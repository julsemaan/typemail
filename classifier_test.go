package typemail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
					"marketing":  0,
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
		if len(question.Criteria) != 5 || question.Criteria["phishing"] == "" {
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
	if len(result.Probabilities) != 5 || result.Probabilities[ClassPhishing] != 0.98 {
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
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest} {
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

func TestNewClassifierRequiresAPIKey(t *testing.T) {
	if _, err := NewClassifier(" "); err == nil {
		t.Fatal("NewClassifier() error = nil, want error")
	}
}
