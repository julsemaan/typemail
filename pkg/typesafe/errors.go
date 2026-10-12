package typesafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// HTTPError retains opt-in diagnostics for a failed TypeSafe request.
// Message may quote submitted state. Error returns only the HTTP status.
type HTTPError struct {
	StatusCode         int
	Message            string
	DetailsUnavailable string
	Model              string
	StateBytes         int
	RequestBytes       int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("openrouter request returned HTTP %d", e.StatusCode)
}

func readErrorMessage(body io.Reader, apiKey string) (string, string) {
	data, err := io.ReadAll(io.LimitReader(body, maxErrorResponseBytes+1))
	if err != nil {
		return "", "could not read error response"
	}
	if len(data) > maxErrorResponseBytes {
		return "", "error response exceeds 4096 bytes"
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", "empty error response"
	}
	var response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return "", "malformed error response"
	}
	if strings.TrimSpace(response.Error.Message) == "" {
		return "", "error response has no message"
	}
	return strings.ReplaceAll(response.Error.Message, apiKey, "[redacted]"), ""
}
