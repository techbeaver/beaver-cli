package mcp

import (
	"net/url"
	"strings"
)

// secretishName matches field names that are a credential by definition, whatever they hold.
func secretishName(name string) bool {
	lowered := strings.ToLower(name)
	for _, needle := range []string{"password", "secret", "token", "dsn", "credential", "connectionstring"} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// embedsCredentials reports whether a value is a URI carrying a password in its userinfo, which is
// the shape of every connection string this API hands out.
func embedsCredentials(value any) bool {
	str, ok := value.(string)
	if !ok || !strings.Contains(str, "://") {
		return false
	}
	parsed, err := url.Parse(str)
	if err != nil {
		return true
	}
	if parsed.User == nil {
		return false
	}
	pw, hasPassword := parsed.User.Password()
	// The API's own withheld marker in the password slot is not a credential.
	if hasPassword && pw == withheldMarker {
		return false
	}
	return hasPassword
}

// omitSecrets drops every field that is a credential by name or by value, recursing into objects and lists.
func omitSecrets(obj map[string]any, alsoDrop ...string) map[string]any {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if secretishName(k) && !isWithheldMarker(v) {
			continue
		}
		cleaned, keep := scrubValue(v)
		if !keep {
			continue
		}
		out[k] = cleaned
	}
	for _, k := range alsoDrop {
		delete(out, k)
	}
	return out
}

// scrubValue applies omitSecrets to one value of any shape. keep is false when the value is
// itself a credential and the field holding it must go.
func scrubValue(v any) (cleaned any, keep bool) {
	switch typed := v.(type) {
	case map[string]any:
		return omitSecrets(typed), true
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			c, ok := scrubValue(item)
			if !ok {
				continue
			}
			out = append(out, c)
		}
		return out, true
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, omitSecrets(item))
		}
		return out, true
	default:
		if embedsCredentials(v) {
			return nil, false
		}
		return v, true
	}
}

// withheldMarker is what the API puts where it withheld a secret; it is kept, since it is not one.
const withheldMarker = "[withheld]"

func isWithheldMarker(v any) bool {
	s, ok := v.(string)
	return ok && s == withheldMarker
}
