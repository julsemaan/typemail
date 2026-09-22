package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"typemail"
)

func testEnvironment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func completeEnvironment() map[string]string {
	return map[string]string{
		"TYPEMAIL_IMAP_ADDR":     "imap.example.com:993",
		"TYPEMAIL_IMAP_USERNAME": "user",
		"TYPEMAIL_IMAP_PASSWORD": "password",
		"OPENROUTER_API_KEY":     "key",
	}
}

func TestLoadConfigDefaultsToFiveWorkers(t *testing.T) {
	cfg, err := loadConfig(nil, testEnvironment(completeEnvironment()))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want %d", cfg.concurrency, defaultConcurrency)
	}
}

func TestLoadConfigHonorsExplicitConcurrency(t *testing.T) {
	cfg, err := loadConfig([]string{"-concurrency", "10"}, testEnvironment(completeEnvironment()))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.concurrency != 10 {
		t.Fatalf("concurrency = %d, want 10", cfg.concurrency)
	}
}

func TestLoadConfigRejectsInvalidConcurrency(t *testing.T) {
	if _, err := loadConfig([]string{"-concurrency", "0"}, testEnvironment(completeEnvironment())); err == nil {
		t.Fatal("loadConfig() error = nil, want error")
	}
}

func TestWorkerPoolHonorsConcurrencyLimit(t *testing.T) {
	const (
		limit = 3
		jobs  = 12
	)
	ctx := context.Background()
	input := make(chan classificationJob)
	started := make(chan struct{}, jobs)
	var active atomic.Int32
	var maximum atomic.Int32

	results := startWorkers(ctx, limit, input, func(context.Context, typemail.Message) (typemail.Result, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		return typemail.Result{Class: typemail.ClassSpam}, nil
	})

	go func() {
		for i := 0; i < jobs; i++ {
			input <- classificationJob{uid: 1}
		}
		close(input)
	}()
	for range results {
	}
	if got := maximum.Load(); got > limit {
		t.Fatalf("maximum active classifications = %d, want <= %d", got, limit)
	}
	if len(started) != jobs {
		t.Fatalf("started classifications = %d, want %d", len(started), jobs)
	}
}

func TestWorkerPoolStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input := make(chan classificationJob)
	started := make(chan struct{}, 1)
	results := startWorkers(ctx, 1, input, func(ctx context.Context, _ typemail.Message) (typemail.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return typemail.Result{}, ctx.Err()
	})

	go func() {
		input <- classificationJob{uid: 1}
		close(input)
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}

	closed := make(chan struct{})
	go func() {
		for range results {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("worker pool did not stop after cancellation")
	}
}

func TestWorkerErrorsReachFinalSummary(t *testing.T) {
	input := make(chan classificationJob, 1)
	input <- classificationJob{uid: 42, err: errors.New("malformed message")}
	close(input)
	results := startWorkers(context.Background(), 1, input, nil)

	var stdout, stderr bytes.Buffer
	summary := outputResults(&stdout, &stderr, results)
	writeSummary(&stdout, summary)
	if summary.failures != 1 {
		t.Fatalf("failures = %d, want 1", summary.failures)
	}
	if !strings.Contains(stdout.String(), "Failures: 1") {
		t.Fatalf("summary = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "UID 42") || !strings.Contains(stderr.String(), "malformed message") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestOutputReplacesTerminalControlCharacters(t *testing.T) {
	input := make(chan classificationResult, 1)
	input <- classificationResult{
		uid:     7,
		from:    "sender\x1b[31m@example.com\nnext",
		subject: "subject\r\nnext",
		result: typemail.Result{
			Class:      typemail.ClassSpam,
			Confidence: 0.94,
		},
	}
	close(input)

	var stdout, stderr bytes.Buffer
	outputResults(&stdout, &stderr, input)
	if strings.Contains(stdout.String(), "\x1b") || strings.Contains(stdout.String(), "\r") {
		t.Fatalf("stdout contains terminal controls: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "\nnext") {
		t.Fatalf("stdout contains injected row: %q", stdout.String())
	}
}
