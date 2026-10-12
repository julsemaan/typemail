package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientConfiguration(t *testing.T) {
	valid := Config{Endpoint: "https://typesafe.example/choose", Model: "custom-model", APIKey: " key "}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Endpoint = "" },
		func(c *Config) { c.Endpoint = "ftp://example.com" },
		func(c *Config) { c.Endpoint = "http://" },
		func(c *Config) { c.Model = " " },
		func(c *Config) { c.APIKey = " " },
	} {
		cfg := valid
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Fatalf("accepted invalid config %#v", cfg)
		}
	}
	client, err := New(valid)
	if err != nil || client.apiKey != "key" || client.client.Timeout != 30*time.Second {
		t.Fatalf("client=%#v err=%v", client, err)
	}
	if _, err := client.Choose(context.Background(), ChoiceRequest{}); err == nil {
		t.Fatal("accepted empty criteria")
	}
	if _, err := client.Choose(nil, testRequest()); err == nil {
		t.Fatal("accepted nil context")
	}
	var nilClient *Client
	if _, err := nilClient.Choose(context.Background(), testRequest()); err == nil {
		t.Fatal("accepted nil client")
	}
	if _, err := new(Client).Choose(context.Background(), testRequest()); err == nil {
		t.Fatal("accepted zero client")
	}
}

func TestChooseWirePayload(t *testing.T) {
	request := testRequest()
	client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/systemone" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s %v", r.Method, r.URL, r.Header)
		}
		var wire classifyRequest
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
			return
		}
		want := classifyRequest{Model: testModel, State: request.State, Questions: map[string]choiceQuestion{questionID: {Type: "choice", Instructions: request.Instructions, Criteria: request.Criteria}}}
		if !reflect.DeepEqual(wire, want) {
			t.Errorf("payload=%#v want=%#v", wire, want)
		}
		writeJSON(t, w, validResponse())
	}))
	defer server.Close()
	result, err := client.Choose(context.Background(), request)
	if err != nil || result.Choice != "phishing" || result.Confidence != .98 || result.Probabilities["phishing"] != .98 || result.Model != "typesafe/jev-1.13" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestInjectedClientRefusesRedirectsWithoutMutation(t *testing.T) {
	var destinationCalls, redirectCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	injected := source.Client()
	injected.Timeout = time.Second
	injected.CheckRedirect = func(*http.Request, []*http.Request) error { redirectCalls.Add(1); return nil }
	client, err := New(Config{Endpoint: source.URL, Model: testModel, APIKey: "test-key", HTTPClient: injected})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Choose(context.Background(), testRequest())
	var detail *HTTPError
	if !errors.As(err, &detail) || detail.StatusCode != 307 || destinationCalls.Load() != 0 || redirectCalls.Load() != 0 {
		t.Fatalf("error=%v destination=%d redirects=%d", err, destinationCalls.Load(), redirectCalls.Load())
	}
	if injected.Timeout != time.Second || injected.CheckRedirect == nil || injected.Transport != client.client.Transport {
		t.Fatal("caller client was modified")
	}
	_ = injected.CheckRedirect(nil, nil)
	if redirectCalls.Load() != 1 {
		t.Fatal("caller redirect policy changed")
	}
}

func TestChooseResponseLimit(t *testing.T) {
	var calls atomic.Int32
	client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(strings.Repeat(" ", maxResponseBytes+1)))
	}))
	defer server.Close()
	if _, err := client.Choose(context.Background(), testRequest()); err == nil || !strings.Contains(err.Error(), "too large") || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestChooseCancellationDuringRequest(t *testing.T) {
	called := make(chan struct{})
	client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(called)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Choose(ctx, testRequest()); done <- err }()
	select {
	case <-called:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not cancel")
	}
}

func TestResponseRequiredFieldsAndFiniteValues(t *testing.T) {
	for _, mutate := range []func(*classifyResponse){
		func(r *classifyResponse) { r.Model = " " },
		func(r *classifyResponse) { a := r.Answers[questionID]; a.Choice = ""; r.Answers[questionID] = a },
		func(r *classifyResponse) {
			a := r.Answers[questionID]
			a.Probabilities = nil
			r.Answers[questionID] = a
		},
		func(r *classifyResponse) { *r.Answers[questionID].Confidence = math.NaN() },
		func(r *classifyResponse) { *r.Answers[questionID].Confidence = math.Inf(1) },
		func(r *classifyResponse) { *r.Answers[questionID].Confidence = -0.1 },
		func(r *classifyResponse) { *r.Answers[questionID].Confidence = 1.1 },
		func(r *classifyResponse) { r.Answers[questionID].Probabilities["spam"] = math.NaN() },
		func(r *classifyResponse) { r.Answers[questionID].Probabilities["spam"] = math.Inf(1) },
		func(r *classifyResponse) { r.Answers[questionID].Probabilities["spam"] = -0.1 },
		func(r *classifyResponse) {
			delete(r.Answers[questionID].Probabilities, "spam")
			r.Answers[questionID].Probabilities["disabled"] = .01
		},
	} {
		var response classifyResponse
		data, _ := json.Marshal(validResponse())
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		mutate(&response)
		if _, _, err := validateResponse(response, testRequest().Criteria); err == nil {
			t.Fatal("accepted invalid response")
		}
	}
}

func validResponse() map[string]any {
	return map[string]any{
		"model": "typesafe/jev-1.13",
		"answers": map[string]any{
			questionID: map[string]any{
				"type":       "choice",
				"choice":     "phishing",
				"confidence": 0.98,
				"probabilities": map[string]float64{
					"regular":    0.01,
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

const testModel = "test-model"

func testRequest() ChoiceRequest {
	return ChoiceRequest{State: "sample state café", Instructions: "Choose one category.", Criteria: map[string]string{"regular": "a", "spam": "b", "phishing": "c", "selling": "d", "newsletter": "e", "suspicious": "f"}}
}
func clientForServer(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := New(Config{Endpoint: server.URL + "/api/v1/systemone", Model: testModel, APIKey: "test-key", HTTPClient: server.Client()})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}
func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
}

func TestChooseRejectsInvalidAnswers(t *testing.T) {
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
			classifier, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, response)
			}))
			defer server.Close()
			if _, err := classifier.Choose(context.Background(), testRequest()); err == nil {
				t.Fatal("Classify() error = nil, want validation error")
			}
		})
	}
}

func TestChooseDoesNotRetryAuthenticationOrValidationErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			classifier, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("contains email body that must not appear"))
			}))
			defer server.Close()
			if _, err := classifier.Choose(context.Background(), testRequest()); err == nil {
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

func TestChooseRetriesRateLimitAndTransientServerErrors(t *testing.T) {
	for _, firstStatus := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(firstStatus), func(t *testing.T) {
			var calls atomic.Int32
			classifier, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(firstStatus)
					return
				}
				writeJSON(t, w, validResponse())
			}))
			defer server.Close()
			if _, err := classifier.Choose(context.Background(), testRequest()); err != nil {
				t.Fatalf("Classify() error = %v", err)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls = %d, want 2", calls.Load())
			}
		})
	}
}

func TestChooseCancellationStopsRetry(t *testing.T) {
	called := make(chan struct{}, 1)
	classifier, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := classifier.Choose(ctx, testRequest())
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
