package typesafe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPErrorDiagnostics(t *testing.T) {
	message := testRequest()
	var requestBytes int
	classifier, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBytes = len(body)
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(t, w, map[string]any{"error": map[string]any{"message": "rejected test-key email", "metadata": "private metadata"}})
	}))
	defer server.Close()
	_, err := classifier.Choose(context.Background(), message)
	var detail *HTTPError
	if !errors.As(fmt.Errorf("wrapped: %w", err), &detail) {
		t.Fatalf("not HTTPError: %v", err)
	}
	if err.Error() != "openrouter request returned HTTP 400" {
		t.Fatalf("private error = %q", err.Error())
	}
	if detail.StatusCode != 400 || detail.Message != "rejected [redacted] email" || detail.DetailsUnavailable != "" || detail.Model != testModel || detail.StateBytes != len(message.State) || detail.RequestBytes != requestBytes {
		t.Fatalf("diagnostics = %#v, request bytes = %d", detail, requestBytes)
	}
}

func TestHTTPErrorUnavailableDetails(t *testing.T) {
	for _, test := range []struct{ name, body, reason string }{
		{"malformed", "private raw response", "malformed error response"},
		{"oversized", strings.Repeat("x", maxErrorResponseBytes+1), "error response exceeds 4096 bytes"},
		{"valid oversized", `{"error":{"message":"private"}}` + strings.Repeat(" ", maxErrorResponseBytes), "error response exceeds 4096 bytes"},
		{"empty", "", "empty error response"},
		{"missing message", `{"error":{"metadata":"private"}}`, "error response has no message"},
		{"blank message", `{"error":{"message":"  "}}`, "error response has no message"},
	} {
		t.Run(test.name, func(t *testing.T) {
			classifier, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			_, err := classifier.Choose(context.Background(), testRequest())
			var detail *HTTPError
			if !errors.As(err, &detail) || detail.Message != "" || detail.DetailsUnavailable != test.reason {
				t.Fatalf("diagnostics = %#v, error = %v", detail, err)
			}
		})
	}
}

type unreadableBody struct{}

func (unreadableBody) Read([]byte) (int, error) { return 0, errors.New("private read error test-key") }
func (unreadableBody) Close() error             { return nil }

type errorTransport struct{}

func (errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusBadRequest, Body: unreadableBody{}, Header: make(http.Header)}, nil
}
func TestHTTPErrorUnreadableDetails(t *testing.T) {
	classifier, _ := New(Config{Endpoint: "http://localhost", Model: testModel, APIKey: "test-key", HTTPClient: &http.Client{Transport: errorTransport{}}})
	_, err := classifier.Choose(context.Background(), testRequest())
	var detail *HTTPError
	if !errors.As(err, &detail) || detail.Message != "" || detail.DetailsUnavailable != "could not read error response" {
		t.Fatalf("diagnostics = %#v, error = %v", detail, err)
	}
}

func TestHTTPErrorRetryExhaustion(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			classifier, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				writeJSON(t, w, map[string]any{"error": map[string]string{"message": "try later"}})
			}))
			defer server.Close()
			_, err := classifier.Choose(context.Background(), testRequest())
			var detail *HTTPError
			if calls.Load() != maxAttempts || !errors.As(err, &detail) || detail.StatusCode != status || detail.Message != "try later" {
				t.Fatalf("calls = %d, error = %v", calls.Load(), err)
			}
		})
	}
}
