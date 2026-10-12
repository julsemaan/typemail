package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/emersion/go-milter"
)

type systemdNotifier struct {
	socket   string
	watchdog time.Duration
}

func systemdFromEnv(getenv func(string) string) (systemdNotifier, error) {
	n := systemdNotifier{socket: getenv("NOTIFY_SOCKET")}
	if n.socket == "" {
		return n, nil
	}
	if len(n.socket) < 2 || (n.socket[0] != '/' && n.socket[0] != '@') {
		return n, errors.New("invalid NOTIFY_SOCKET")
	}
	if value := getenv("WATCHDOG_USEC"); value != "" {
		usec, err := strconv.ParseUint(value, 10, 64)
		if err != nil || usec == 0 || usec > uint64((1<<63-1)/time.Microsecond) {
			return n, errors.New("invalid WATCHDOG_USEC")
		}
		n.watchdog = time.Duration(usec) * time.Microsecond
		if value := getenv("WATCHDOG_PID"); value != "" {
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return n, errors.New("invalid WATCHDOG_PID")
			}
			if pid != os.Getpid() {
				n.watchdog = 0
			}
		}
	}
	return n, nil
}

func (n systemdNotifier) probeBudget() time.Duration {
	if n.watchdog > 0 {
		return min(time.Second, n.watchdog/3)
	}
	return time.Second
}

func (n systemdNotifier) notify(ctx context.Context, state string) error {
	if n.socket == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, n.probeBudget())
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unixgram", n.socket)
	if err != nil {
		return fmt.Errorf("systemd notification: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(conn, state)
	if err != nil {
		return fmt.Errorf("systemd notification: %w", err)
	}
	// A successful write commits delivery, even if cancellation arrives immediately afterward.
	return nil
}

func (n systemdNotifier) status(ctx context.Context, status string) {
	_ = n.notify(ctx, "STATUS="+status)
}

// Negotiation alone exercises the accept loop and protocol handler without mail or classification.
func probeMilter(ctx context.Context, address string, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, min(time.Second, budget))
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	var request [17]byte
	binary.BigEndian.PutUint32(request[:4], 13)
	request[4] = 'O'
	binary.BigEndian.PutUint32(request[5:9], 2)
	binary.BigEndian.PutUint32(request[9:13], uint32(milter.OptAddHeader|milter.OptChangeHeader))
	if _, err := io.Copy(conn, bytes.NewReader(request[:])); err != nil {
		return err
	}
	// Fixed-size reads reject oversized packets without allocating based on peer input.
	var size [4]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(size[:]) != 13 {
		return errors.New("invalid Milter negotiation length")
	}
	var response [13]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		return err
	}
	if !bytes.Equal(response[:], request[4:]) {
		return errors.New("invalid Milter negotiation response")
	}
	return ctx.Err()
}
