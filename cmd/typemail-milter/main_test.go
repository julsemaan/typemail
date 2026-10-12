package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"typemail"
)

func TestSuperviseLifecycle(t *testing.T) {
	path, notifications := notificationSocket(t, false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	serve := newServer(ctx, func(context.Context, typemail.Message) (typemail.Result, error) {
		calls.Add(1)
		return typemail.Result{}, nil
	}, nil).Serve
	done := make(chan error, 1)
	go func() {
		done <- supervise(ctx, ln, serve, systemdNotifier{socket: path, watchdog: 90 * time.Millisecond})
	}()
	if got := readNotification(t, notifications); !strings.HasPrefix(got, "READY=1\nSTATUS=Listening") {
		t.Fatal(got)
	}
	if got := readNotification(t, notifications); got != "WATCHDOG=1\nSTATUS=Listening" {
		t.Fatal(got)
	}
	cancel()
	for {
		if got := readNotification(t, notifications); strings.HasPrefix(got, "STOPPING=1") {
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("listener still open")
	}
	if calls.Load() != 0 {
		t.Fatal("lifecycle probes classified mail")
	}
	notifications.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var b [1024]byte
	if _, _, err := notifications.ReadFromUnix(b[:]); err == nil {
		t.Fatal("notification after shutdown")
	}
}

func TestSuperviseStartupFailures(t *testing.T) {
	for _, mode := range []string{"cancelled", "server error", "silent", "notification failure"} {
		t.Run(mode, func(t *testing.T) {
			path, notifications := notificationSocket(t, false)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serve := newServer(ctx, func(context.Context, typemail.Message) (typemail.Result, error) { return typemail.Result{}, nil }, nil).Serve
			if mode == "cancelled" {
				cancel()
			}
			if mode == "server error" {
				serve = func(net.Listener) error { return errors.New("serve failed") }
			}
			if mode == "silent" {
				serve = func(ln net.Listener) error {
					c, err := ln.Accept()
					if err != nil {
						return err
					}
					defer c.Close()
					_, err = io.Copy(io.Discard, c)
					return err
				}
			}
			if mode == "notification failure" {
				notifications.Close()
			}
			err = supervise(ctx, ln, serve, systemdNotifier{socket: path, watchdog: 150 * time.Millisecond})
			if mode != "cancelled" && err == nil {
				t.Fatal("startup failure returned success")
			}
			if mode == "cancelled" && err != nil {
				t.Fatal(err)
			}
			if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
				t.Fatal("failed startup left listener open")
			}
			if mode != "notification failure" {
				notifications.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				var b [1024]byte
				for {
					n, _, err := notifications.ReadFromUnix(b[:])
					if err != nil {
						break
					}
					if strings.Contains(string(b[:n]), "READY=1") {
						t.Fatal("failed startup became ready")
					}
				}
			}
		})
	}
}

func TestWatchdogSuppressionAndRecovery(t *testing.T) {
	path, notifications := notificationSocket(t, false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var unhealthy atomic.Bool
	serve := func(ln net.Listener) error {
		for {
			c, err := ln.Accept()
			if err != nil {
				return err
			}
			var request [17]byte
			c.SetDeadline(time.Now().Add(time.Second))
			_, err = io.ReadFull(c, request[:])
			if err == nil {
				if unhealthy.Load() {
					request[4] = 'X'
				}
				c.Write(request[:])
			}
			c.Close()
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- supervise(ctx, ln, serve, systemdNotifier{socket: path, watchdog: 90 * time.Millisecond})
	}()
	if got := readNotification(t, notifications); !strings.HasPrefix(got, "READY=1") {
		t.Fatal(got)
	}
	if got := readNotification(t, notifications); !strings.HasPrefix(got, "WATCHDOG=1") {
		t.Fatal(got)
	}
	unhealthy.Store(true)
	// Drain any already-completed healthy check, then require consecutive unhealthy checks.
	for {
		if strings.Contains(readNotification(t, notifications), "Unhealthy") {
			break
		}
	}
	for i := 0; i < 3; i++ {
		if got := readNotification(t, notifications); !strings.Contains(got, "Unhealthy") {
			t.Fatalf("heartbeat during failure: %q", got)
		}
	}
	unhealthy.Store(false)
	for {
		if strings.HasPrefix(readNotification(t, notifications), "WATCHDOG=1") {
			break
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSuperviseRuntimeErrors(t *testing.T) {
	for _, notificationFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(notificationFailure), func(t *testing.T) {
			path, notifications := notificationSocket(t, false)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serve := newServer(ctx, func(context.Context, typemail.Message) (typemail.Result, error) { return typemail.Result{}, nil }, nil).Serve
			done := make(chan error, 1)
			go func() {
				done <- supervise(ctx, ln, serve, systemdNotifier{socket: path, watchdog: 90 * time.Millisecond})
			}()
			if got := readNotification(t, notifications); !strings.HasPrefix(got, "READY=1") {
				t.Fatal(got)
			}
			if notificationFailure {
				notifications.Close()
			} else {
				ln.Close()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("runtime failure returned success")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("runtime error not returned")
			}
		})
	}
}

func TestCancellationDuringStartupProbe(t *testing.T) {
	path, notifications := notificationSocket(t, false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accepted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- supervise(ctx, ln, func(ln net.Listener) error {
			c, err := ln.Accept()
			if err != nil {
				return err
			}
			defer c.Close()
			close(accepted)
			_, err = io.Copy(io.Discard, c)
			return err
		}, systemdNotifier{socket: path})
	}()
	<-accepted
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancelled probe did not stop")
	}
	if got := readNotification(t, notifications); !strings.HasPrefix(got, "STOPPING=1") {
		t.Fatal(got)
	}
}

func TestReadinessWaitsForProtocolResponse(t *testing.T) {
	path, notifications := notificationSocket(t, false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requested := make(chan struct{})
	respond := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- supervise(ctx, ln, func(ln net.Listener) error {
			c, err := ln.Accept()
			if err != nil {
				return err
			}
			defer c.Close()
			var packet [17]byte
			if _, err := io.ReadFull(c, packet[:]); err != nil {
				return err
			}
			close(requested)
			select {
			case <-respond:
			case <-ctx.Done():
				return nil
			}
			if _, err := c.Write(packet[:]); err != nil {
				return err
			}
			c.Close()
			c, err = ln.Accept()
			if err != nil {
				return err
			}
			c.Close()
			return nil
		}, systemdNotifier{socket: path})
	}()
	<-requested
	notifications.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var b [1024]byte
	if _, _, err := notifications.ReadFromUnix(b[:]); err == nil {
		t.Fatal("ready before protocol response")
	}
	close(respond)
	if got := readNotification(t, notifications); !strings.HasPrefix(got, "READY=1") {
		t.Fatal(got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunCancelledDuringDiscovery(t *testing.T) {
	path, notifications := notificationSocket(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, nil, systemdNotifier{socket: path}); err != nil {
		t.Fatal(err)
	}
	if got := readNotification(t, notifications); !strings.HasPrefix(got, "STOPPING=1") {
		t.Fatal(got)
	}
}

func TestCancellationDuringWatchdogProbe(t *testing.T) {
	for i := 0; i < 16; i++ {
		path, notifications := notificationSocket(t, false)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		probing := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- supervise(ctx, ln, func(ln net.Listener) error {
				c, err := ln.Accept()
				if err != nil {
					return err
				}
				var packet [17]byte
				if _, err := io.ReadFull(c, packet[:]); err != nil {
					c.Close()
					return err
				}
				c.Write(packet[:])
				c.Close()
				c, err = ln.Accept()
				if err != nil {
					return err
				}
				close(probing)
				io.Copy(io.Discard, c)
				c.Close()
				c, err = ln.Accept()
				if c != nil {
					c.Close()
				}
				return err
			}, systemdNotifier{socket: path, watchdog: 30 * time.Millisecond})
		}()
		readNotification(t, notifications)
		<-probing
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(500 * time.Millisecond):
			ln.Close()
			<-done
			t.Fatal("cancelled watchdog waited for Serve before closing listener")
		}
		notifications.Close()
	}
}

func TestCancellationWhileAccepting(t *testing.T) {
	// Both select branches become ready on cancellation; exercise either ordering.
	for i := 0; i < 32; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		accepting := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- supervise(ctx, ln, func(ln net.Listener) error {
				close(accepting)
				c, err := ln.Accept()
				if c != nil {
					c.Close()
				}
				return err
			}, systemdNotifier{})
		}()
		<-accepting
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(500 * time.Millisecond):
			ln.Close()
			<-done
			t.Fatal("cancellation waited for Serve before closing listener")
		}
	}
}

func TestParseNetworks(t *testing.T) {
	networks, err := parseNetworks("127.0.0.0/8, 192.0.2.0/24 [::1]/128 [2001:db8::]/32 198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1", "192.0.2.255", "::1", "2001:db8::99", "198.51.100.1"} {
		found := false
		for _, n := range networks {
			found = found || n.Contains(net.ParseIP(addr))
		}
		if !found {
			t.Errorf("trusted address %s not found", addr)
		}
	}
	for _, value := range []string{"", "cidr:/etc/postfix/trusted", "127.0.0.1/99", "$other", "bad"} {
		if _, err := parseNetworks(value); err == nil {
			t.Errorf("accepted invalid networks %q", value)
		}
	}
}

func TestLoadClassifier(t *testing.T) {
	env := func(string) string { return "key" }
	if _, err := loadClassifier(nil, env); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-debug"}, {"-concurrency", "10"}, {"extra"}} {
		if _, err := loadClassifier(args, env); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if _, err := loadClassifier(nil, func(string) string { return "" }); err == nil {
		t.Fatal("accepted missing API key")
	}
	path := filepath.Join(t.TempDir(), "typemail.toml")
	if err := os.WriteFile(path, []byte("skip_sender_domains = ['semaan.ca']\n[classes.receipts]\ncriteria = 'Invoices'"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadClassifier([]string{"-config", path}, env)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, class := range c.Classes() {
		found = found || class == "receipts"
	}
	if !found {
		t.Fatal("custom class absent")
	}
	if err := os.WriteFile(path, []byte("[classes.spam]\nEnabled = false"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClassifier([]string{"-config", path}, env); err == nil {
		t.Fatal("accepted unknown setting case")
	}
}
