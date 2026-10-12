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

func run(ctx context.Context, classifier *typemail.Classifier, notifier systemdNotifier) error {
	supervised := false
	defer func() {
		if !supervised && ctx.Err() != nil {
			_ = notifier.notify(context.Background(), "STOPPING=1\nSTATUS=Stopping")
		}
	}()
	notifier.status(ctx, "Discovering Postfix networks")
	startupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// -x expands parameter references, available since Postfix 2.10.
	output, err := exec.CommandContext(startupCtx, "/usr/sbin/postconf", "-xh", "mynetworks").Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("read Postfix mynetworks: %w", err)
	}
	networks, err := parseNetworks(string(output))
	if err != nil {
		return err
	}
	notifier.status(ctx, "Setting up Milter listener")
	if ctx.Err() != nil {
		return nil
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return err
	}
	supervised = true
	return supervise(ctx, listener, newServer(ctx, classifier.Classify, networks).Serve, notifier)
}

func supervise(ctx context.Context, listener net.Listener, serve func(net.Listener) error, notifier systemdNotifier) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = serve(listener)
		close(done)
		cancel() // Interrupt a probe if the serving loop exits.
	}()
	var ticker *time.Ticker
	var ticks <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
		cancel()
		if ctx.Err() != nil {
			_ = notifier.notify(context.Background(), "STOPPING=1\nSTATUS=Stopping")
		}
		// Close the listener, not Server.Close, which races with Serve in this library.
		_ = listener.Close()
		<-done
	}()
	serverError := func() error {
		// Parent cancellation also cancels serveCtx; cleanup must close the listener before waiting.
		if ctx.Err() != nil {
			return nil
		}
		<-done
		if serveErr == nil {
			return errors.New("Milter serving loop stopped unexpectedly")
		}
		return serveErr
	}
	if ctx.Err() != nil {
		return nil
	}
	if notifier.socket != "" {
		if err := probeMilter(serveCtx, listener.Addr().String(), notifier.probeBudget()); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if serveCtx.Err() != nil {
				return serverError()
			}
			return fmt.Errorf("Milter startup probe: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-done:
			return serverError()
		default:
		}
		if err := notifier.notify(serveCtx, "READY=1\nSTATUS=Listening"); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	if notifier.watchdog > 0 {
		ticker = time.NewTicker(notifier.watchdog / 3)
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-serveCtx.Done():
			return serverError()
		case <-ticks:
			if err := probeMilter(serveCtx, listener.Addr().String(), notifier.probeBudget()); err != nil {
				notifier.status(serveCtx, "Unhealthy: Milter responsiveness check failed")
				continue
			}
			if err := notifier.notify(serveCtx, "WATCHDOG=1\nSTATUS=Listening"); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	notifier, err := systemdFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	notifier.status(ctx, "Loading configuration")
	classifier, err := loadClassifier(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(ctx, classifier, notifier); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
