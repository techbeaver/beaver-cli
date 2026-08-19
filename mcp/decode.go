package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/techbeaver/beaver-cli/client"
)

// decodeInto unmarshals an envelope's data into a typed value.
func decodeInto[T any](env *client.Envelope) (*T, error) {
	var out T
	if env == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return &out, nil
	}
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return nil, fmt.Errorf("the API returned an unexpected shape: %w", err)
	}
	return &out, nil
}

// pick returns a string field from a decoded object, or "".
//
// Used to build human-readable summaries out of responses whose exact shape
// this service deliberately does not model. Modelling every response would mean
// this package drifting from the handlers it wraps, which is the failure two
// independently maintained clients against one API reliably produce.
func pick(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			switch typed := v.(type) {
			case string:
				if typed != "" {
					return typed
				}
			case float64:
				return fmt.Sprintf("%g", typed)
			case bool:
				return fmt.Sprintf("%t", typed)
			}
		}
	}
	return ""
}

// omit returns a copy of an object without the named keys.
//
// This is how secrets stay out of a transcript. An agent that pastes a
// production database password into a chat has exfiltrated it, and the
// transcript may belong to a third party.
func omit(obj map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// redactValues replaces every value in a string map with a placeholder,
// preserving the keys.
//
// Environment variables are the case this exists for: knowing that DATABASE_URL
// is set is useful to an agent and to the customer reading its output; knowing
// what it contains is a credential in a chat log.
func redactValues(obj map[string]any) map[string]any {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		switch typed := v.(type) {
		case string:
			if typed == "" {
				out[k] = ""
				continue
			}
			out[k] = fmt.Sprintf("[set, %d characters, hidden]", len(typed))
		default:
			out[k] = "[set, hidden]"
		}
	}
	return out
}
