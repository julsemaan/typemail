package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
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

func defaultClasses(t *testing.T) []typemail.Class {
	t.Helper()
	classifier, err := typemail.NewClassifier("key")
	if err != nil {
		t.Fatal(err)
	}
	return classifier.Classes()
}

func TestLoadConfigClassifierFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "empty"},
		{name: "empty classes table", content: "[classes]"},
		{name: "empty built-in section", content: "[classes.selling]"},
		{name: "wrong enabled type", content: "[classes.spam]\nenabled = 'false'", wantErr: "decode config"},
		{name: "wrong criteria type", content: "[classes.spam]\ncriteria = false", wantErr: "decode config"},
		{name: "malformed", content: "[classes", wantErr: "decode config"},
		{name: "unknown top-level key", content: "concurrency = 10", wantErr: "concurrency"},
		{name: "uppercase classes", content: "[CLASSES.spam]\nenabled = false", wantErr: "CLASSES"},
		{name: "uppercase enabled", content: "[classes.spam]\nEnabled = false", wantErr: "Enabled"},
		{name: "uppercase criteria", content: "[classes.spam]\nCriteria = 'junk'", wantErr: "Criteria"},
		{name: "unknown class key", content: "[classes.spam]\ncriterai = 'typo'", wantErr: "classes.spam.criterai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig([]string{"-config", path}, testEnvironment(completeEnvironment()))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("loadConfig() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			classifier, err := typemail.NewClassifierWithConfig("key", cfg.classifierConfig)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(classifier.Classes(), defaultClasses(t)) {
				t.Fatalf("empty config classes = %v, want defaults", classifier.Classes())
			}
		})
	}
}

func TestLoadConfigMultilineCriteria(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := "[classes.receipts]\nenabled = true\ncriteria = \"\"\"\nPurchase receipts.\nInclude invoices.\n\"\"\"\n[classes.spam]\nenabled = false\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig([]string{"-config", path}, testEnvironment(completeEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	receipts := cfg.classifierConfig.Classes["receipts"]
	if receipts.Enabled == nil || !*receipts.Enabled || receipts.Criteria == nil || *receipts.Criteria != "Purchase receipts.\nInclude invoices.\n" {
		t.Fatalf("receipts config = %#v", receipts)
	}
	spam := cfg.classifierConfig.Classes["spam"]
	if spam.Enabled == nil || *spam.Enabled || spam.Criteria != nil {
		t.Fatalf("spam config = %#v", spam)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	_, err := loadConfig([]string{"-config", path}, testEnvironment(completeEnvironment()))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loadConfig() error = %v, want missing file", err)
	}
}

func TestLoadConfigEmptyPathUsesDefaults(t *testing.T) {
	for _, args := range [][]string{nil, {"-config", ""}} {
		cfg, err := loadConfig(args, testEnvironment(completeEnvironment()))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.classifierConfig.Classes) != 0 {
			t.Fatalf("default classifier config = %#v", cfg.classifierConfig)
		}
	}
}

func TestRunRejectsInvalidClassifierConfigBeforeIMAP(t *testing.T) {
	criteria := ""
	cfg := config{
		openRouterKey: "key",
		imapAddr:      "invalid IMAP address",
		classifierConfig: typemail.ClassifierConfig{Classes: map[string]typemail.ClassConfig{
			"receipts": {Criteria: &criteria},
		}},
	}
	_, wantErr := typemail.NewClassifierWithConfig(cfg.openRouterKey, cfg.classifierConfig)
	if wantErr == nil {
		t.Fatal("empty custom criteria should be invalid")
	}
	err := run(context.Background(), cfg, io.Discard, io.Discard)
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("run() error = %v, want classifier validation error %v", err, wantErr)
	}
}

func TestWriteSummaryUsesActiveClasses(t *testing.T) {
	active := []typemail.Class{"receipts", typemail.ClassSpam}
	summary := runSummary{
		counts:   map[typemail.Class]int{"receipts": 2, typemail.ClassLegitimate: 7},
		total:    10,
		failures: 1,
	}
	var output bytes.Buffer
	writeSummary(&output, summary, active)
	want := "\nTotals:\nMessages: 10\nreceipts: 2\nspam: 0\nFailures: 1\n"
	if output.String() != want {
		t.Fatalf("summary = %q, want %q", output.String(), want)
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

func TestLoadConfigMaxEmailBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int64
	}{
		{name: "default", want: 16384},
		{name: "override", args: []string{"-max-email-bytes", "8192"}, want: 8192},
		{name: "minimum", args: []string{"-max-email-bytes", "1"}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(tc.args, testEnvironment(completeEnvironment()))
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			if cfg.maxEmailBytes != tc.want {
				t.Fatalf("maxEmailBytes = %d, want %d", cfg.maxEmailBytes, tc.want)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidMaxEmailBytes(t *testing.T) {
	for _, value := range []string{"0", "-1", "invalid"} {
		t.Run(value, func(t *testing.T) {
			if _, err := loadConfig([]string{"-max-email-bytes", value}, testEnvironment(completeEnvironment())); err == nil {
				t.Fatal("loadConfig() error = nil, want error")
			}
		})
	}
}

func TestLoadConfigRewriteSubjectIsOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var args []string
		if enabled {
			args = []string{"-rewrite-subject"}
		}
		cfg, err := loadConfig(args, testEnvironment(completeEnvironment()))
		if err != nil || cfg.rewriteSubject != enabled {
			t.Fatalf("rewriteSubject = %v, error = %v", cfg.rewriteSubject, err)
		}
	}
}

func TestWorkersForwardAdmittedJobsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := make(chan classificationJob, 3)
	for uid := 1; uid <= 3; uid++ {
		input <- classificationJob{uid: imap.UID(uid)}
	}
	close(input)
	seen := make(map[imap.UID]bool)
	for result := range startWorkers(ctx, 2, input, func(context.Context, typemail.Message) (typemail.Result, error) {
		t.Error("classification called after cancellation")
		return typemail.Result{}, nil
	}) {
		if seen[result.uid] || !errors.Is(result.err, context.Canceled) {
			t.Fatalf("unexpected result = %#v", result)
		}
		seen[result.uid] = true
	}
	if len(seen) != 3 {
		t.Fatalf("forwarded %d results, want 3", len(seen))
	}
}

func TestLoadConfigDebugIsOptIn(t *testing.T) {
	for _, debug := range []bool{false, true} {
		var args []string
		if debug {
			args = []string{"-debug"}
		}
		cfg, err := loadConfig(args, testEnvironment(completeEnvironment()))
		if err != nil || cfg.debug != debug {
			t.Fatalf("loadConfig(%v) = %#v, %v", args, cfg, err)
		}
	}
}

func TestOutputHTTPDiagnostics(t *testing.T) {
	for _, debug := range []bool{false, true} {
		input := make(chan classificationResult, 4)
		input <- classificationResult{uid: 64866, err: fmt.Errorf("classify: %w", &typemail.HTTPError{
			StatusCode: 400, Message: "private email\x1b[31m\nforged\r\t\u009b", Model: "jev\nmodel", StateBytes: 123, RequestBytes: 456,
		})}
		input <- classificationResult{uid: 64867, err: &typemail.HTTPError{StatusCode: 400, DetailsUnavailable: "malformed error response"}}
		input <- classificationResult{uid: 9, err: errors.New("parse failed")}
		input <- classificationResult{uid: 10, from: "sender", subject: "subject", result: typemail.Result{Class: typemail.ClassSpam, Confidence: 0.94}}
		close(input)
		var stdout, stderr bytes.Buffer
		summary := outputResults(&stdout, &stderr, input, debug)
		writeSummary(&stdout, summary, defaultClasses(t))
		if summary.total != 4 || summary.failures != 3 || summary.counts[typemail.ClassSpam] != 1 || !strings.Contains(stdout.String(), "Failures: 3") || !strings.Contains(stdout.String(), "0.94") {
			t.Fatalf("summary = %#v, stdout = %q", summary, stdout.String())
		}
		if debug {
			for _, want := range []string{"UID 64866: HTTP diagnostics", "model=jev model", "state_bytes=123", "request_bytes=456", "message=private email [31m forged", "UID 64867: HTTP diagnostics", "details unavailable: malformed error response"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q: %q", want, stderr.String())
				}
			}
			if strings.Count(stderr.String(), "\n") != 5 || strings.ContainsAny(stderr.String(), "\x1b\r\t\u009b") {
				t.Fatalf("unsafe stderr = %q", stderr.String())
			}
		} else if strings.Contains(stderr.String(), "private email") || strings.Contains(stderr.String(), "malformed error response") || strings.Contains(stderr.String(), "state_bytes") {
			t.Fatalf("default stderr leaks diagnostics: %q", stderr.String())
		}
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
	summary := outputResults(&stdout, &stderr, results, false)
	writeSummary(&stdout, summary, defaultClasses(t))
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
	outputResults(&stdout, &stderr, input, false)
	if strings.Contains(stdout.String(), "\x1b") || strings.Contains(stdout.String(), "\r") {
		t.Fatalf("stdout contains terminal controls: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "\nnext") {
		t.Fatalf("stdout contains injected row: %q", stdout.String())
	}
}
