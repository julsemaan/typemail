package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"typemail"
	"typemail/internal/cli"
)

const listenAddress = "127.0.0.1:8892"

func parseNetworks(value string) ([]*net.IPNet, error) {
	var networks []*net.IPNet
	for _, entry := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		entry = strings.NewReplacer("[", "", "]", "").Replace(entry)
		if ip := net.ParseIP(entry); ip != nil {
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("mynetworks must contain literal IP addresses or CIDRs: %q", entry)
		}
		networks = append(networks, network)
	}
	if len(networks) == 0 {
		return nil, errors.New("Postfix mynetworks is empty")
	}
	return networks, nil
}

func loadClassifier(args []string, getenv func(string) string) (*typemail.Classifier, error) {
	flags := flag.NewFlagSet("typemail-milter", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "path to classifier TOML configuration")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, errors.New("unexpected positional arguments")
	}
	cfg, err := cli.LoadClassifierConfig(*path)
	if err != nil {
		return nil, err
	}
	return typemail.NewClassifierWithConfig(getenv("OPENROUTER_API_KEY"), cfg)
}

func run(ctx context.Context, classifier *typemail.Classifier) error {
	startupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// -x expands parameter references, available since Postfix 2.10.
	output, err := exec.CommandContext(startupCtx, "/usr/sbin/postconf", "-xh", "mynetworks").Output()
	if err != nil {
		return fmt.Errorf("read Postfix mynetworks: %w", err)
	}
	networks, err := parseNetworks(string(output))
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return err
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()
	// Close the listener, not Server.Close, which races with Serve in this library.
	err = newServer(ctx, classifier.Classify, networks).Serve(listener)
	if ctx.Err() != nil && errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func main() {
	classifier, err := loadClassifier(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, classifier); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
