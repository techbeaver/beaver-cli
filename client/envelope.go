package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Envelope is the response shape every endpoint on this platform returns.
type Envelope struct {
	Status  int             `json:"status"`
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
	Code    string          `json:"code,omitempty"`
}

// APIError is a failed call, carrying the machine-readable code alongside the
// message so a caller can branch without reading prose.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Data       map[string]any
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Code)
	}
	return e.Message
}

// Paused reports whether the platform has deliberately gated this action. It
// arrives as a 503 but is an operator decision, not a transient fault.
func (e *APIError) Paused() bool {
	return strings.EqualFold(strings.TrimSpace(e.Code), "service_paused")
}

// Retryable reports whether repeating the call unchanged could plausibly
// succeed: a 429 or a 5xx, and nothing else. A paused service is excluded even
// though it is a 503, because retrying into an operator's circuit breaker turns
// a maintenance window into a thundering herd. See ADR 0014.
func (e *APIError) Retryable() bool {
	if e.Paused() {
		return false
	}
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func envelopeError(status int, env *Envelope) *APIError {
	out := &APIError{StatusCode: status, Code: env.Code, Message: env.Message}
	if out.Message == "" {
		out.Message = env.Error
	}
	if len(env.Data) > 0 {
		var data map[string]any
		if err := json.Unmarshal(env.Data, &data); err == nil {
			out.Data = data
			if out.Code == "" {
				if code, ok := data["code"].(string); ok {
					out.Code = code
				}
			}
		}
	}
	if out.Code == "" && env.Error != "" {
		out.Code = env.Error
	}
	if out.Message == "" {
		out.Message = fmt.Sprintf("The TechBeaver API returned %d", status)
	}
	return out
}

// DecodeObject unmarshals an envelope's data into a map. An absent or null data
// field yields an empty map rather than an error.
func DecodeObject(env *Envelope) (map[string]any, error) {
	if env == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return nil, fmt.Errorf("the API returned something other than an object: %w", err)
	}
	return out, nil
}

// DecodeList unmarshals an envelope's data into a list of objects. It tolerates
// a single object, and a paginated wrapper holding the list under a
// conventional key. See ADR 0014.
func DecodeList(env *Envelope) ([]map[string]any, error) {
	if env == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, nil
	}
	var list []map[string]any
	if err := json.Unmarshal(env.Data, &list); err == nil {
		return list, nil
	}
	var wrapper map[string]any
	if err := json.Unmarshal(env.Data, &wrapper); err != nil {
		return nil, fmt.Errorf("the API returned something other than a list: %w", err)
	}
	for _, key := range []string{"items", "data", "results", "rows", "records"} {
		if nested, ok := wrapper[key].([]any); ok {
			out := make([]map[string]any, 0, len(nested))
			for _, item := range nested {
				if obj, ok := item.(map[string]any); ok {
					out = append(out, obj)
				}
			}
			return out, nil
		}
	}
	return []map[string]any{wrapper}, nil
}
