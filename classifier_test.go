package typemail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func validResponse() map[string]any {
	return map[string]any{
		"model": "typesafe/jev-1.13",
		"answers": map[string]any{
			questionID: map[string]any{
				"type":       "choice",
				"choice":     "phishing",
				"confidence": 0.98,
				"probabilities": map[string]float64{
					"legitimate": 0.01,
					"spam":       0.01,
					"phishing":   0.98,
					"selling":    0,
					"newsletter": 0,
					"suspicious": 0,
				},
			},
		},
	}
}

func classifierForServer(t *testing.T, handler http.Handler) (*Classifier, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	classifier, err := NewClassifier("test-key")
	if err != nil {
		server.Close()
		t.Fatalf("NewClassifier() error = %v", err)
	}
	classifier.endpoint = server.URL + "/api/v1/systemone"
	classifier.client = server.Client()
	return classifier, server
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
}

func TestClassifierRequestAndResponse(t *testing.T) {
	classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/systemone" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var request classifyRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("request JSON error = %v", err)
			return
		}
		if request.Model != defaultJevModel {
			t.Errorf("model = %q", request.Model)
		}
		if !strings.Contains(request.State, "Subject: Verify your account") {
			t.Errorf("state = %q", request.State)
		}
		question, ok := request.Questions[questionID]
		if !ok || question.Type != "choice" || question.Instructions == "" {
			t.Errorf("question = %#v", question)
		}
		if len(question.Criteria) != 6 || question.Criteria["phishing"] == "" {
			t.Errorf("criteria = %#v", question.Criteria)
		}
		writeJSON(t, w, validResponse())
	}))
	defer server.Close()

	result, err := classifier.Classify(context.Background(), Message{
		Headers: map[string][]string{"Subject": {"Verify your account"}},
		TextParts: []TextPart{{
			ContentType: "text/plain",
			Text:        "Please send your password.",
		}},
	})
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if result.Class != ClassPhishing || result.Confidence != 0.98 || result.Model != "typesafe/jev-1.13" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Probabilities) != 6 || result.Probabilities[ClassPhishing] != 0.98 {
		t.Fatalf("probabilities = %#v", result.Probabilities)
	}
}

func TestClassifierRejectsInvalidAnswers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing answer", mutate: func(response map[string]any) {
			response["answers"] = map[string]any{}
		}},
		{name: "wrong type", mutate: func(response map[string]any) {
			response["answers"].(map[string]any)[questionID].(map[string]any)["type"] = "score"
		}},
		{name: "missing confidence", mutate: func(response map[string]any) {
			delete(response["answers"].(map[string]any)[questionID].(map[string]any), "confidence")
		}},
		{name: "unknown class", mutate: func(response map[string]any) {
			response["answers"].(map[string]any)[questionID].(map[string]any)["choice"] = "other"
		}},
		{name: "invalid probability", mutate: func(response map[string]any) {
			response["answers"].(map[string]any)[questionID].(map[string]any)["probabilities"].(map[string]float64)["spam"] = 1.1
		}},
		{name: "probabilities do not sum", mutate: func(response map[string]any) {
			response["answers"].(map[string]any)[questionID].(map[string]any)["probabilities"].(map[string]float64)["spam"] = 0.2
		}},
		{name: "missing probability", mutate: func(response map[string]any) {
			delete(response["answers"].(map[string]any)[questionID].(map[string]any)["probabilities"].(map[string]float64), "spam")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validResponse()
			test.mutate(response)
			classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, response)
			}))
			defer server.Close()
			if _, err := classifier.Classify(context.Background(), Message{}); err == nil {
				t.Fatal("Classify() error = nil, want validation error")
			}
		})
	}
}

func TestClassifierDoesNotRetryAuthenticationOrValidationErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("contains email body that must not appear"))
			}))
			defer server.Close()
			if _, err := classifier.Classify(context.Background(), Message{}); err == nil {
				t.Fatal("Classify() error = nil, want error")
			} else if strings.Contains(err.Error(), "email body") {
				t.Fatalf("error leaks response body: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestClassifierRetriesRateLimitAndTransientServerErrors(t *testing.T) {
	for _, firstStatus := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(firstStatus), func(t *testing.T) {
			var calls atomic.Int32
			classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(firstStatus)
					return
				}
				writeJSON(t, w, validResponse())
			}))
			defer server.Close()
			if _, err := classifier.Classify(context.Background(), Message{}); err != nil {
				t.Fatalf("Classify() error = %v", err)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls = %d, want 2", calls.Load())
			}
		})
	}
}

func TestClassifierCancellationStopsRetry(t *testing.T) {
	called := make(chan struct{}, 1)
	classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := classifier.Classify(ctx, Message{})
		done <- err
	}()
	select {
	case <-called:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("classifier did not make a request")
	}
	select {
	case err := <-done:
		if err == nil || err != context.Canceled {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Classify() did not stop after cancellation")
	}
}

func TestClassifierConfiguration(t *testing.T) {
	criteria := "Purchase receipts."
	disabled := false
	cfg := ClassifierConfig{Classes: map[string]ClassConfig{
		"selling":    {Criteria: &criteria},
		"suspicious": {Enabled: &disabled},
		"Receipts":   {Criteria: &criteria},
		"archived":   {Enabled: &disabled, Criteria: &criteria},
	}}
	classifier, err := NewClassifierWithConfig("key", cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []Class{"Receipts", "legitimate", "newsletter", "phishing", "selling", "spam"}
	if !reflect.DeepEqual(classifier.Classes(), want) || classifier.criteria["selling"] != criteria {
		t.Fatalf("classes = %v, criteria = %v", classifier.Classes(), classifier.criteria)
	}
	criteria = "changed"
	disabled = true
	delete(cfg.Classes, "Receipts")
	names := classifier.Classes()
	names[0] = "changed"
	other, err := NewClassifier("key")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(classifier.Classes(), want) || classifier.criteria["selling"] != "Purchase receipts." || other.criteria["selling"] != classCriteria["selling"] || len(other.Classes()) != 6 {
		t.Fatal("classifiers share mutable configuration")
	}
	for _, cfg := range []ClassifierConfig{{}, {Classes: map[string]ClassConfig{}}, {Classes: map[string]ClassConfig{"selling": {}}}} {
		c, err := NewClassifierWithConfig("key", cfg)
		if err != nil || !reflect.DeepEqual(c.criteria, classCriteria) {
			t.Fatalf("default criteria = %v, error = %v", c, err)
		}
	}
}

func TestClassifierRejectsInvalidConfiguration(t *testing.T) {
	blank, criteria, disabled := " \n", "Receipts", false
	for _, name := range []string{"", " ", " receipts", "receipts ", "a\nb", "a\u007fb"} {
		if _, err := NewClassifierWithConfig("key", ClassifierConfig{Classes: map[string]ClassConfig{name: {Criteria: &criteria}}}); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
	for _, config := range []ClassConfig{{}, {Enabled: &disabled}, {Criteria: &blank}, {Enabled: &disabled, Criteria: &blank}} {
		if _, err := NewClassifierWithConfig("key", ClassifierConfig{Classes: map[string]ClassConfig{"receipts": config}}); err == nil {
			t.Errorf("accepted invalid custom config %#v", config)
		}
	}
	if _, err := NewClassifierWithConfig("key", ClassifierConfig{Classes: map[string]ClassConfig{"selling": {Criteria: &blank}}}); err == nil {
		t.Fatal("accepted blank built-in criteria")
	}
	cfg := ClassifierConfig{Classes: map[string]ClassConfig{}}
	for name := range classCriteria {
		cfg.Classes[name] = ClassConfig{Enabled: &disabled}
	}
	if _, err := NewClassifierWithConfig("key", cfg); err == nil {
		t.Fatal("accepted no active classes")
	}
	cfg.Classes["receipts"] = ClassConfig{Enabled: &disabled, Criteria: &criteria}
	if _, err := NewClassifierWithConfig("key", cfg); err == nil {
		t.Fatal("disabled custom class must not make configuration active")
	}
}

func TestConfiguredClassifierRequestAndResponse(t *testing.T) {
	criteria, disabled := "Purchase receipts.", false
	cfg := ClassifierConfig{Classes: map[string]ClassConfig{
		"receipts": {Criteria: &criteria}, "selling": {Criteria: &criteria}, "suspicious": {Enabled: &disabled},
	}}
	for _, mode := range []string{"valid", "disabled choice", "disabled probability", "missing probability"} {
		t.Run(mode, func(t *testing.T) {
			classifier, err := NewClassifierWithConfig("key", cfg)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request classifyRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if !reflect.DeepEqual(request.Questions[questionID].Criteria, classifier.criteria) || request.Questions[questionID].Criteria["receipts"] != criteria || request.Questions[questionID].Criteria["selling"] != criteria {
					t.Errorf("criteria = %v", request.Questions[questionID].Criteria)
				}
				response := validResponse()
				answer := response["answers"].(map[string]any)[questionID].(map[string]any)
				answer["choice"] = "receipts"
				probabilities := map[string]float64{"receipts": 1, "selling": 0, "spam": 0, "phishing": 0, "legitimate": 0, "newsletter": 0}
				answer["probabilities"] = probabilities
				switch mode {
				case "disabled choice":
					answer["choice"] = "suspicious"
				case "disabled probability":
					delete(probabilities, "spam")
					probabilities["suspicious"] = 0
				case "missing probability":
					delete(probabilities, "receipts")
				}
				writeJSON(t, w, response)
			}))
			defer server.Close()
			classifier.endpoint, classifier.client = server.URL, server.Client()
			result, err := classifier.Classify(context.Background(), Message{})
			if mode == "valid" {
				if err != nil || result.Class != "receipts" || result.Probabilities["receipts"] != 1 || len(result.Probabilities) != 6 {
					t.Fatalf("result = %#v, error = %v", result, err)
				}
			} else if err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestNewsletterClass(t *testing.T) {
	classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request classifyRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Questions[questionID].Criteria["newsletter"] == "" {
			t.Error("request has no newsletter criteria")
		}
		response := validResponse()
		answer := response["answers"].(map[string]any)[questionID].(map[string]any)
		answer["choice"] = "newsletter"
		probabilities := answer["probabilities"].(map[string]float64)
		probabilities["phishing"], probabilities["newsletter"] = 0, 0.98
		writeJSON(t, w, response)
	}))
	defer server.Close()
	result, err := classifier.Classify(context.Background(), Message{})
	if err != nil || result.Class != ClassNewsletter || result.Probabilities[ClassNewsletter] != 0.98 {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	criteria, disabled := "Editorial newsletters only.", false
	for _, config := range []ClassConfig{{}, {Criteria: &criteria}, {Enabled: &disabled}} {
		c, err := NewClassifierWithConfig("key", ClassifierConfig{Classes: map[string]ClassConfig{"newsletter": config}})
		if err != nil {
			t.Fatal(err)
		}
		_, active := c.parseClass("newsletter")
		if active != (config.Enabled == nil) || config.Criteria != nil && c.criteria["newsletter"] != criteria {
			t.Fatalf("newsletter config = %#v, criteria = %v", config, c.criteria)
		}
	}
}

func TestNewClassifierRequiresAPIKey(t *testing.T) {
	if _, err := NewClassifier(" "); err == nil {
		t.Fatal("NewClassifier() error = nil, want error")
	}
}

func TestHTTPErrorDiagnostics(t *testing.T) {
	message := Message{TextParts: []TextPart{{ContentType: "text/plain", Text: "café"}}}
	var requestBytes int
	classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBytes = len(body)
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(t, w, map[string]any{"error": map[string]any{"message": "rejected test-key email", "metadata": "private metadata"}})
	}))
	defer server.Close()
	_, err := classifier.Classify(context.Background(), message)
	var detail *HTTPError
	if !errors.As(fmt.Errorf("wrapped: %w", err), &detail) {
		t.Fatalf("not HTTPError: %v", err)
	}
	if err.Error() != "openrouter request returned HTTP 400" {
		t.Fatalf("private error = %q", err.Error())
	}
	if detail.StatusCode != 400 || detail.Message != "rejected [redacted] email" || detail.DetailsUnavailable != "" || detail.Model != defaultJevModel || detail.StateBytes != len(formatState(message)) || detail.RequestBytes != requestBytes {
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
			classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			_, err := classifier.Classify(context.Background(), Message{})
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
	classifier, _ := NewClassifier("test-key")
	classifier.client = &http.Client{Transport: errorTransport{}}
	_, err := classifier.Classify(context.Background(), Message{})
	var detail *HTTPError
	if !errors.As(err, &detail) || detail.Message != "" || detail.DetailsUnavailable != "could not read error response" {
		t.Fatalf("diagnostics = %#v, error = %v", detail, err)
	}
}

func TestHTTPErrorRetryExhaustion(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			classifier, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				writeJSON(t, w, map[string]any{"error": map[string]string{"message": "try later"}})
			}))
			defer server.Close()
			_, err := classifier.Classify(context.Background(), Message{})
			var detail *HTTPError
			if calls.Load() != maxAttempts || !errors.As(err, &detail) || detail.StatusCode != status || detail.Message != "try later" {
				t.Fatalf("calls = %d, error = %v", calls.Load(), err)
			}
		})
	}
}
