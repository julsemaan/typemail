package typemail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"typemail/pkg/typesafe"
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

const questionID = "classification"
const testModel = "test-model"

type classifyRequest struct {
	Model     string
	State     string
	Questions map[string]struct {
		Type         string
		Instructions string
		Criteria     map[string]string
	}
}

func testClient(t *testing.T, endpoint string, httpClient *http.Client) *typesafe.Client {
	t.Helper()
	client, err := typesafe.New(typesafe.Config{Endpoint: endpoint, Model: testModel, APIKey: "test-key", HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func testClassifier(t *testing.T, cfg Config) (*Classifier, error) {
	t.Helper()
	return New(testClient(t, "http://localhost", nil), cfg)
}
func classifierForServer(t *testing.T, handler http.Handler) (*Classifier, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	classifier, err := New(testClient(t, server.URL+"/api/v1/systemone", server.Client()), Config{})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return classifier, server
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
}

func TestClassifierSkipSenderDomains(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string][]string
		skip    bool
	}{
		{"exact", map[string][]string{"From": {"julien@semaan.ca"}}, true},
		{"nested", map[string][]string{"From": {"julien@a.mail.semaan.ca"}}, true},
		{"case and display name", map[string][]string{"fRoM": {"Julien <JULIEN@SEMAAN.CA>"}}, true},
		{"lookalike", map[string][]string{"From": {"a@notsemaan.ca"}}, false},
		{"suffix", map[string][]string{"From": {"a@semaan.ca.evil.com"}}, false},
		{"missing", nil, false},
		{"malformed", map[string][]string{"From": {"not a mailbox"}}, false},
		{"malformed domain", map[string][]string{"From": {"a@bad..semaan.ca"}}, false},
		{"invalid domain label", map[string][]string{"From": {"a@-bad.semaan.ca"}}, false},
		{"multiple", map[string][]string{"From": {"a@semaan.ca, b@semaan.ca"}}, false},
		{"duplicate", map[string][]string{"From": {"a@semaan.ca", "b@semaan.ca"}}, false},
		{"duplicate case", map[string][]string{"From": {"a@semaan.ca"}, "from": {"b@semaan.ca"}}, false},
		{"multiple in group", map[string][]string{"From": {"Team: a@semaan.ca, b@semaan.ca;"}}, false},
		{"quoted local part", map[string][]string{"From": {`"a@b"@semaan.ca`}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeJSON(t, w, validResponse())
			}))
			defer server.Close()
			domains := []string{" SeMaAn.Ca "}
			c, err := testClassifier(t, Config{SkipSenderDomains: domains})
			if err != nil {
				t.Fatal(err)
			}
			domains[0] = "evil.com" // The classifier owns a normalized copy.
			c.client = testClient(t, server.URL, server.Client())
			result, err := c.Classify(context.Background(), Message{Headers: tc.headers})
			if err != nil {
				t.Fatal(err)
			}
			if tc.skip {
				if !reflect.DeepEqual(result, Result{Skipped: true}) || calls.Load() != 0 {
					t.Fatalf("result=%#v requests=%d", result, calls.Load())
				}
			} else if result.Skipped || result.Class != ClassPhishing || calls.Load() != 1 {
				t.Fatalf("result=%#v requests=%d", result, calls.Load())
			}
		})
	}
}

func TestSkipSenderDomainsValidation(t *testing.T) {
	for _, domain := range []string{"", " ", "a@semaan.ca", "https://semaan.ca", "*.semaan.ca", ".semaan.ca", "semaan.ca.", "bad..ca", "-bad.ca", "bad-.ca", "bad_ca", "a/b.ca", strings.Repeat("a", 64) + ".ca"} {
		if _, err := testClassifier(t, Config{SkipSenderDomains: []string{domain}}); err == nil {
			t.Errorf("accepted domain %q", domain)
		}
	}
	for _, domains := range [][]string{nil, {}} {
		c, server := classifierForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, validResponse()) }))
		c.skipSenderDomains = domains
		result, err := c.Classify(context.Background(), Message{Headers: map[string][]string{"From": {"a@semaan.ca"}}})
		server.Close()
		if err != nil || result.Skipped || result.Class != ClassPhishing {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	}
	c, err := testClassifier(t, Config{SkipSenderDomains: []string{"semaan.ca"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := c.Classify(ctx, Message{Headers: map[string][]string{"From": {"a@semaan.ca"}}})
	if !errors.Is(err, context.Canceled) || result.Skipped {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	c.client = nil
	if result, err := c.Classify(context.Background(), Message{Headers: map[string][]string{"From": {"a@semaan.ca"}}}); err == nil || result.Skipped {
		t.Fatalf("result=%#v err=%v", result, err)
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
		if request.Model != testModel {
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

func TestConfiguredClassifierRequestAndResponse(t *testing.T) {
	criteria, disabled := "Purchase receipts.", false
	cfg := Config{Classes: map[string]ClassConfig{
		"receipts": {Criteria: &criteria}, "selling": {Criteria: &criteria}, "suspicious": {Enabled: &disabled},
	}}
	for _, mode := range []string{"valid", "disabled choice", "disabled probability", "missing probability"} {
		t.Run(mode, func(t *testing.T) {
			classifier, err := testClassifier(t, cfg)
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
				probabilities := map[string]float64{"receipts": 1, "selling": 0, "spam": 0, "phishing": 0, "regular": 0, "newsletter": 0}
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
			classifier.client = testClient(t, server.URL, server.Client())
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
		c, err := testClassifier(t, Config{Classes: map[string]ClassConfig{"newsletter": config}})
		if err != nil {
			t.Fatal(err)
		}
		_, active := c.criteria["newsletter"]
		if active != (config.Enabled == nil) || config.Criteria != nil && c.criteria["newsletter"] != criteria {
			t.Fatalf("newsletter config = %#v, criteria = %v", config, c.criteria)
		}
	}
}
