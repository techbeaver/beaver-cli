package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDefaultFormatDependsOnWhetherOutputIsATerminal(t *testing.T) {
	spec, err := ParseFormat("", true)
	if err != nil || spec.Format != Table {
		t.Fatalf("a person at a terminal gets a table, got %v (%v)", spec.Format, err)
	}
	spec, err = ParseFormat("", false)
	if err != nil || spec.Format != JSON {
		t.Fatalf("a pipe must get JSON, not a table somebody has to parse, got %v (%v)", spec.Format, err)
	}
}

func TestParseFormatValueColumns(t *testing.T) {
	spec, err := ParseFormat("value(name, status)", true)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Format != Value || len(spec.Columns) != 2 ||
		spec.Columns[0] != "name" || spec.Columns[1] != "status" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	if _, err := ParseFormat("value()", true); err == nil {
		t.Fatal("value() with no field must be rejected")
	}
	if _, err := ParseFormat("xml", true); err == nil {
		t.Fatal("an unknown format must be rejected rather than silently defaulted")
	}
}

func TestListRendersAnEmptyJSONArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	if err := New(&buf, Spec{Format: JSON}).List(nil); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Fatalf("an empty list must be [] so jq and scripts do not break, got %q", got)
	}
}

func TestValueFormatReachesNestedFields(t *testing.T) {
	var buf bytes.Buffer
	rows := []map[string]any{{"name": "api", "spec": map[string]any{"region": "eu"}}}
	w := New(&buf, Spec{Format: Value, Columns: []string{"name", "spec.region"}})
	if err := w.List(rows); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "api\teu" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestTableUsesPreferredColumnsWhenPresent(t *testing.T) {
	var buf bytes.Buffer
	rows := []map[string]any{{"id": "1", "name": "api", "status": "running", "internal": "x"}}
	if err := New(&buf, Spec{Format: Table}).List(rows, "name", "status"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "STATUS") {
		t.Fatalf("preferred columns missing: %q", out)
	}
	if strings.Contains(out, "INTERNAL") {
		t.Fatalf("a table must not widen itself with unrequested columns: %q", out)
	}
}

func TestIntegersDoNotRenderInScientificNotation(t *testing.T) {
	// JSON numbers decode as float64, and 1.234e+06 cannot be pasted back.
	var buf bytes.Buffer
	rows := []map[string]any{{"id": float64(1234567)}}
	if err := New(&buf, Spec{Format: Value, Columns: []string{"id"}}).List(rows); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "1234567" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestJSONOutputIsValid(t *testing.T) {
	var buf bytes.Buffer
	rows := []map[string]any{{"name": "api"}, {"name": "web"}}
	if err := New(&buf, Spec{Format: JSON}).List(rows); err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("output was not valid JSON: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("round trip lost rows: %v", back)
	}
}
