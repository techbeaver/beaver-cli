package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOmitSecretsRecursesIntoNestedObjectsAndArrays(t *testing.T) {
	in := map[string]any{
		"name": "db",
		"nested": map[string]any{
			"host":     "h",
			"password": "sentinel-pw",
			"deeper":   map[string]any{"directUri": "postgres://u:sentinel-pw@h/db", "port": 5432},
		},
		"list": []any{
			map[string]any{"apiToken": "sentinel-tok", "ok": 1},
			"postgres://u:sentinel-pw@h/db",
			"plain",
			[]any{map[string]any{"clientSecret": "sentinel-sec"}},
		},
		"typed": []map[string]any{{"dsn": "sentinel-dsn", "keep": true}},
	}
	out := omitSecrets(in)
	raw, _ := json.Marshal(out)
	for _, s := range []string{"sentinel-pw", "sentinel-tok", "sentinel-sec", "sentinel-dsn"} {
		if strings.Contains(string(raw), s) {
			t.Fatalf("%s survived in %s", s, raw)
		}
	}
	for _, keep := range []string{`"host":"h"`, `"port":5432`, `"ok":1`, `"plain"`, `"keep":true`} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("non-secret %s was dropped: %s", keep, raw)
		}
	}
	// The input must not be mutated.
	if in["nested"].(map[string]any)["password"] != "sentinel-pw" {
		t.Error("omitSecrets mutated its input")
	}
}

func TestOmitSecretsKeepsTheAPIsWithheldMarker(t *testing.T) {
	out := omitSecrets(map[string]any{
		"password":  withheldMarker,
		"directUri": "postgres://u:%5Bwithheld%5D@h:5432/db",
		"x":         "postgres://u:real@h/db",
	})
	if out["password"] != withheldMarker {
		t.Errorf("the marker says a value exists and was withheld, and must survive: %#v", out)
	}
	if _, ok := out["directUri"]; !ok {
		t.Errorf("a connection string carrying only the marker is not a credential: %#v", out)
	}
	if _, ok := out["x"]; ok {
		t.Errorf("a real credential was kept: %#v", out)
	}
}
