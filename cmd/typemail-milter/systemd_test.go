package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"typemail/pkg/typemail"
)

func notificationSocket(t *testing.T, abstract bool) (string, *net.UnixConn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify")
	if abstract {
		if runtime.GOOS != "linux" {
			t.Skip("Linux abstract sockets only")
		}
		path = fmt.Sprintf("@typemail-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return path, conn
}

func readNotification(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var buf [1024]byte
	n, _, err := conn.ReadFromUnix(buf[:])
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestSystemdEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, socket, usec, pid string
		interval                time.Duration
		bad                     bool
	}{
		{name: "standalone ignores environment", usec: "bad", pid: "bad"},
		{name: "notify only", socket: "/tmp/notify"},
		{name: "watchdog", socket: "@notify", usec: "30000000", interval: 30 * time.Second},
		{name: "own pid", socket: "@notify", usec: "30000000", pid: strconv.Itoa(os.Getpid()), interval: 30 * time.Second},
		{name: "other pid", socket: "@notify", usec: "30000000", pid: strconv.Itoa(os.Getpid() + 1)},
		{name: "zero", socket: "@notify", usec: "0", bad: true},
		{name: "negative", socket: "@notify", usec: "-1", bad: true},
		{name: "malformed", socket: "@notify", usec: "abc", bad: true},
		{name: "overflow", socket: "@notify", usec: "9223372036854776", bad: true},
		{name: "uint overflow", socket: "@notify", usec: "18446744073709551616", bad: true},
		{name: "invalid pid", socket: "@notify", usec: "1", pid: "bad", bad: true},
		{name: "zero pid", socket: "@notify", usec: "1", pid: "0", bad: true},
		{name: "invalid socket", socket: "relative", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"NOTIFY_SOCKET": tc.socket, "WATCHDOG_USEC": tc.usec, "WATCHDOG_PID": tc.pid}
			n, err := systemdFromEnv(func(k string) string { return env[k] })
			if (err != nil) != tc.bad {
				t.Fatalf("error = %v", err)
			}
			if err == nil && n.watchdog != tc.interval {
				t.Fatalf("interval = %v", n.watchdog)
			}
		})
	}
}

func TestNotifications(t *testing.T) {
	for _, abstract := range []bool{false, true} {
		t.Run(fmt.Sprint(abstract), func(t *testing.T) {
			path, conn := notificationSocket(t, abstract)
			n := systemdNotifier{socket: path}
			if err := n.notify(context.Background(), "READY=1\nSTATUS=Listening"); err != nil {
				t.Fatal(err)
			}
			if got := readNotification(t, conn); got != "READY=1\nSTATUS=Listening" {
				t.Fatal(got)
			}
			conn.Close()
			if err := n.notify(context.Background(), "WATCHDOG=1"); err == nil {
				t.Fatal("accepted missing receiver")
			}
			n.status(context.Background(), "Informational") // Best effort, even without receiver.
		})
	}
	if err := (systemdNotifier{}).notify(context.Background(), "READY=1"); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationDeadlineAndCancellation(t *testing.T) {
	path, receiver := notificationSocket(t, false)
	n := systemdNotifier{socket: path, watchdog: 150 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.notify(ctx, "READY=1"); err == nil {
		t.Fatal("cancelled readiness accepted")
	}
	receiver.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	var b [128]byte
	if _, _, err := receiver.ReadFromUnix(b[:]); err == nil {
		t.Fatal("cancelled readiness delivered")
	}
	attempts := 100
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/sys/net/unix/max_dgram_qlen")
		if err != nil {
			t.Fatalf("read max_dgram_qlen: %v", err)
		}
		limit, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatalf("parse max_dgram_qlen: %v", err)
		}
		// Linux accepts limit + 1 datagrams before blocking the next write.
		attempts = limit + 2
	}
	// Fill the receiver queue to exercise a blocked datagram write, not just dial failure.
	for i := 0; i < attempts; i++ {
		start := time.Now()
		err := n.notify(context.Background(), "WATCHDOG=1")
		if time.Since(start) > 500*time.Millisecond {
			t.Fatal("notification exceeded deadline")
		}
		if err != nil {
			return
		}
	}
	t.Fatal("receiver queue did not fill")
}

func TestProbeBudget(t *testing.T) {
	for _, tc := range []struct{ watchdog, want time.Duration }{{0, time.Second}, {30 * time.Second, time.Second}, {150 * time.Millisecond, 50 * time.Millisecond}} {
		if got := (systemdNotifier{watchdog: tc.watchdog}).probeBudget(); got != tc.want {
			t.Fatalf("budget = %v, want %v", got, tc.want)
		}
	}
}

func TestProbeHealthyNeverClassifies(t *testing.T) {
	var calls atomic.Int32
	addr := startWireServer(t, context.Background(), func(context.Context, typemail.Message) (typemail.Result, error) {
		calls.Add(1)
		return typemail.Result{}, nil
	}, nil)
	if err := probeMilter(context.Background(), addr, time.Second); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("probe classified mail")
	}
}

func TestProbeBadPeers(t *testing.T) {
	for _, response := range [][]byte{nil, {0, 0, 0, 13, 'X'}, {255, 255, 255, 255}, {0, 0, 0, 1, 'O'}, {0, 0, 0, 13, 'O', 0, 0, 0, 6, 0, 0, 0, 17, 0, 0, 0, 0}} {
		t.Run(fmt.Sprintf("%x", response), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				if response != nil {
					c.Write(response)
				}
				var b [128]byte
				for {
					if _, err = c.Read(b[:]); err != nil {
						return
					}
				}
			}()
			start := time.Now()
			if err := probeMilter(context.Background(), ln.Addr().String(), 50*time.Millisecond); err == nil {
				t.Fatal("bad peer passed")
			}
			if time.Since(start) > time.Second {
				t.Fatal("probe exceeded budget")
			}
			<-done
		})
	}
}
