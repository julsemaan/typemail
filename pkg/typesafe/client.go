// Package typesafe implements the TypeSafe choice protocol.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config selects a TypeSafe host and model. HTTPClient is optional.
type Config struct {
	Endpoint   string
	Model      string
	APIKey     string
	HTTPClient *http.Client
}

// Client sends bounded, retryable choice requests.
type Client struct {
	endpoint string
	model    string
	apiKey   string
	client   *http.Client
}

// ChoiceRequest supplies policy and state for one choice question.
type ChoiceRequest struct {
	State        string
	Instructions string
	Criteria     map[string]string
}

// ChoiceResult contains a validated answer and probability distribution.
type ChoiceResult struct {
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
	Model         string
}

// New validates host configuration and copies the optional HTTP client.
// Redirects are refused without changing the caller's client.
func New(cfg Config) (*Client, error) {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("typesafe endpoint must be an HTTP or HTTPS URL")
	}
	if cfg.Model == "" {
		return nil, errors.New("typesafe model is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("typesafe API key is required")
	}
	client := http.Client{Timeout: 30 * time.Second}
	if cfg.HTTPClient != nil {
		client = *cfg.HTTPClient
	}
	client.CheckRedirect = refuseRedirects
	return &Client{endpoint: cfg.Endpoint, model: cfg.Model, apiKey: cfg.APIKey, client: &client}, nil
}

const (
	maxErrorResponseBytes = 4 << 10
	maxResponseBytes      = 1 << 20
	maxAttempts           = 3
	retryBaseDelay        = 50 * time.Millisecond
	questionID            = "classification"
)

func refuseRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type choiceQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type classifyRequest struct {
	Model     string                    `json:"model"`
	State     string                    `json:"state"`
	Questions map[string]choiceQuestion `json:"questions"`
}

type choiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type classifyResponse struct {
	Model   string                  `json:"model"`
	Answers map[string]choiceAnswer `json:"answers"`
}

// Choose sends a choice request and validates the complete answer.
func (c *Client) Choose(ctx context.Context, request ChoiceRequest) (ChoiceResult, error) {
	if c == nil {
		return ChoiceResult{}, errors.New("nil typesafe client")
	}
	if ctx == nil {
		return ChoiceResult{}, errors.New("nil context")
	}
	if c.client == nil || c.endpoint == "" || c.model == "" || c.apiKey == "" {
		return ChoiceResult{}, errors.New("typesafe client is not configured")
	}
	if err := ctx.Err(); err != nil {
		return ChoiceResult{}, err
	}
	if len(request.Criteria) == 0 {
		return ChoiceResult{}, errors.New("at least one choice is required")
	}
	requestBody, err := json.Marshal(classifyRequest{
		Model: c.model,
		State: request.State,
		Questions: map[string]choiceQuestion{
			questionID: {
				Type:         "choice",
				Instructions: request.Instructions,
				Criteria:     request.Criteria,
			},
		},
	})
	if err != nil {
		return ChoiceResult{}, fmt.Errorf("encode classifier request: %w", err)
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, retry, err := c.doRequest(ctx, requestBody, len(request.State), request.Criteria)
		if err == nil {
			return result, nil
		}
		if !retry || attempt == maxAttempts {
			return ChoiceResult{}, err
		}
		if err := waitRetry(ctx, attempt); err != nil {
			return ChoiceResult{}, err
		}
	}
	return ChoiceResult{}, errors.New("classifier request failed")
}

func (c *Client) doRequest(ctx context.Context, body []byte, stateBytes int, criteria map[string]string) (ChoiceResult, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return ChoiceResult{}, false, fmt.Errorf("create classifier request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ChoiceResult{}, false, ctx.Err()
		}
		return ChoiceResult{}, true, errors.New("openrouter request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		detail := &HTTPError{
			StatusCode:   resp.StatusCode,
			Model:        c.model,
			StateBytes:   stateBytes,
			RequestBytes: len(body),
		}
		detail.Message, detail.DetailsUnavailable = readErrorMessage(resp.Body, c.apiKey)
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return ChoiceResult{}, retry, detail
	}

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return ChoiceResult{}, true, errors.New("read openrouter response failed")
	}
	if int64(len(responseBody)) > maxResponseBytes {
		return ChoiceResult{}, false, errors.New("openrouter response is too large")
	}

	var response classifyResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return ChoiceResult{}, false, errors.New("decode openrouter response failed")
	}
	return validateResponse(response, criteria)
}

func waitRetry(ctx context.Context, attempt int) error {
	delay := retryBaseDelay * time.Duration(1<<(attempt-1))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateResponse(response classifyResponse, criteria map[string]string) (ChoiceResult, bool, error) {
	if strings.TrimSpace(response.Model) == "" {
		return ChoiceResult{}, false, errors.New("openrouter response has no model")
	}
	answer, ok := response.Answers[questionID]
	if !ok {
		return ChoiceResult{}, false, errors.New("openrouter response has no classification answer")
	}
	if answer.Type != "choice" || answer.Choice == "" || answer.Confidence == nil || answer.Probabilities == nil {
		return ChoiceResult{}, false, errors.New("openrouter response has incomplete classification answer")
	}
	_, ok = criteria[answer.Choice]
	if !ok {
		return ChoiceResult{}, false, errors.New("openrouter response has an unknown classification")
	}
	if math.IsNaN(*answer.Confidence) || math.IsInf(*answer.Confidence, 0) || *answer.Confidence < 0 || *answer.Confidence > 1 {
		return ChoiceResult{}, false, errors.New("openrouter response has invalid confidence")
	}
	if len(answer.Probabilities) != len(criteria) {
		return ChoiceResult{}, false, errors.New("openrouter response has incomplete probabilities")
	}

	probabilities := make(map[string]float64, len(answer.Probabilities))
	var probabilitySum float64
	for name, probability := range answer.Probabilities {
		_, ok := criteria[name]
		if !ok {
			return ChoiceResult{}, false, errors.New("openrouter response has an unknown probability class")
		}
		if math.IsNaN(probability) || math.IsInf(probability, 0) || probability < 0 || probability > 1 {
			return ChoiceResult{}, false, errors.New("openrouter response has an invalid probability")
		}
		probabilitySum += probability
		probabilities[name] = probability
	}
	if math.Abs(probabilitySum-1) > 1e-6 {
		return ChoiceResult{}, false, errors.New("openrouter response probabilities do not sum to one")
	}

	return ChoiceResult{
		Choice:        answer.Choice,
		Confidence:    *answer.Confidence,
		Probabilities: probabilities,
		Model:         response.Model,
	}, false, nil
}
