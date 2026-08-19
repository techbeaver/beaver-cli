// Package output renders results. It knows nothing about HTTP, and client
// knows nothing about rendering.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"
)

// Format is how a result is rendered.
type Format string

// The supported formats. Value carries the requested columns, so it is parsed
// rather than compared.
const (
	JSON  Format = "json"
	YAML  Format = "yaml"
	Table Format = "table"
	Value Format = "value"
)

var valuePattern = regexp.MustCompile(`^value\(([^)]*)\)$`)

// Spec is a parsed --format argument.
type Spec struct {
	Format  Format
	Columns []string
}

// ParseFormat reads a --format argument. An empty argument resolves by whether
// out is a terminal: a table for a person, JSON for a pipe. See ADR 0008.
func ParseFormat(raw string, isTTY bool) (Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if isTTY {
			return Spec{Format: Table}, nil
		}
		return Spec{Format: JSON}, nil
	}
	if m := valuePattern.FindStringSubmatch(raw); m != nil {
		var cols []string
		for _, c := range strings.Split(m[1], ",") {
			if c = strings.TrimSpace(c); c != "" {
				cols = append(cols, c)
			}
		}
		if len(cols) == 0 {
			return Spec{}, fmt.Errorf("value(...) needs at least one field, such as value(name,status)")
		}
		return Spec{Format: Value, Columns: cols}, nil
	}
	switch Format(strings.ToLower(raw)) {
	case JSON:
		return Spec{Format: JSON}, nil
	case YAML:
		return Spec{Format: YAML}, nil
	case Table:
		return Spec{Format: Table}, nil
	}
	return Spec{}, fmt.Errorf("unknown format %q: use json, yaml, table or value(field,...)", raw)
}

// IsTTY reports whether f is a terminal.
func IsTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Writer renders results in one format.
type Writer struct {
	Out  io.Writer
	Spec Spec
}

// New builds a writer.
func New(out io.Writer, spec Spec) *Writer {
	return &Writer{Out: out, Spec: spec}
}

// Object renders a single record.
func (w *Writer) Object(obj map[string]any) error {
	switch w.Spec.Format {
	case JSON:
		return w.writeJSON(obj)
	case YAML:
		return w.writeYAML(obj)
	case Value:
		return w.writeValues([]map[string]any{obj})
	default:
		return w.writeKeyValue(obj)
	}
}

// List renders a collection. Preferred columns are used for a table when
// present in the data; anything else is discovered from the rows.
func (w *Writer) List(rows []map[string]any, preferred ...string) error {
	switch w.Spec.Format {
	case JSON:
		if rows == nil {
			rows = []map[string]any{}
		}
		return w.writeJSON(rows)
	case YAML:
		return w.writeYAML(rows)
	case Value:
		return w.writeValues(rows)
	default:
		return w.writeTable(rows, preferred)
	}
}

func (w *Writer) writeJSON(v any) error {
	enc := json.NewEncoder(w.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (w *Writer) writeYAML(v any) error {
	enc := yaml.NewEncoder(w.Out)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return enc.Close()
}

func (w *Writer) writeValues(rows []map[string]any) error {
	for _, row := range rows {
		fields := make([]string, 0, len(w.Spec.Columns))
		for _, c := range w.Spec.Columns {
			fields = append(fields, scalar(lookup(row, c)))
		}
		if _, err := fmt.Fprintln(w.Out, strings.Join(fields, "\t")); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) writeKeyValue(obj map[string]any) error {
	tw := tabwriter.NewWriter(w.Out, 0, 0, 2, ' ', 0)
	keys := sortedKeys(obj)
	for _, k := range keys {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", k, scalar(obj[k])); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (w *Writer) writeTable(rows []map[string]any, preferred []string) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w.Out, "No results.")
		return err
	}
	cols := chooseColumns(rows, preferred)
	tw := tabwriter.NewWriter(w.Out, 0, 0, 3, ' ', 0)
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = strings.ToUpper(c)
	}
	if _, err := fmt.Fprintln(tw, strings.Join(header, "\t")); err != nil {
		return err
	}
	for _, row := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = scalar(lookup(row, c))
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func chooseColumns(rows []map[string]any, preferred []string) []string {
	var cols []string
	for _, c := range preferred {
		for _, row := range rows {
			if _, ok := row[c]; ok {
				cols = append(cols, c)
				break
			}
		}
	}
	if len(cols) > 0 {
		return cols
	}
	seen := map[string]bool{}
	for _, row := range rows {
		for k, v := range row {
			if !seen[k] && isScalar(v) {
				seen[k] = true
			}
		}
	}
	return sortedSet(seen)
}

// lookup resolves a dotted path, so value(spec.region) reaches a nested field.
func lookup(row map[string]any, path string) any {
	var current any = row
	for _, part := range strings.Split(path, ".") {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = obj[part]
	}
	return current
}

func isScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any, nil:
		return false
	}
	return true
}

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return fmt.Sprintf("%t", t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case map[string]any, []any:
		encoded, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(encoded)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
