package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The CLI does not depend on testify. This is the small slice of it the ported server tests use.
type asserter struct{ fatal bool }

var (
	assert  = asserter{}
	require = asserter{fatal: true}
)

func (a asserter) report(t *testing.T, what string, msgAndArgs []any) bool {
	t.Helper()
	extra := ""
	if len(msgAndArgs) > 0 {
		if f, ok := msgAndArgs[0].(string); ok {
			extra = ": " + fmt.Sprintf(f, msgAndArgs[1:]...)
		}
	}
	t.Errorf("%s%s", what, extra)
	if a.fatal {
		t.FailNow()
	}
	return false
}

func (a asserter) Equal(t *testing.T, want, got any, m ...any) bool {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		return true
	}
	return a.report(t, fmt.Sprintf("not equal: want %#v, got %#v", want, got), m)
}

func (a asserter) EqualValues(t *testing.T, want, got any, m ...any) bool {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		return true
	}
	w, wok := toFloat(want)
	g, gok := toFloat(got)
	if wok && gok && w == g {
		return true
	}
	if fmt.Sprint(want) == fmt.Sprint(got) {
		return true
	}
	return a.report(t, fmt.Sprintf("values differ: want %#v, got %#v", want, got), m)
}

func toFloat(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}

func contains(container, item any) bool {
	if s, ok := container.(string); ok {
		return strings.Contains(s, fmt.Sprint(item))
	}
	rv := reflect.ValueOf(container)
	switch rv.Kind() {
	case reflect.Map:
		for _, k := range rv.MapKeys() {
			if reflect.DeepEqual(k.Interface(), item) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if reflect.DeepEqual(rv.Index(i).Interface(), item) {
				return true
			}
		}
	}
	return false
}

func (a asserter) Contains(t *testing.T, container, item any, m ...any) bool {
	t.Helper()
	if contains(container, item) {
		return true
	}
	return a.report(t, fmt.Sprintf("%#v does not contain %#v", container, item), m)
}

func (a asserter) NotContains(t *testing.T, container, item any, m ...any) bool {
	t.Helper()
	if !contains(container, item) {
		return true
	}
	return a.report(t, fmt.Sprintf("%#v contains %#v", container, item), m)
}

func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Map, reflect.Slice, reflect.Array:
		return rv.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	}
	return rv.IsZero()
}

func (a asserter) Empty(t *testing.T, v any, m ...any) bool {
	t.Helper()
	if isEmpty(v) {
		return true
	}
	return a.report(t, fmt.Sprintf("expected empty, got %#v", v), m)
}

func (a asserter) NotEmpty(t *testing.T, v any, m ...any) bool {
	t.Helper()
	if !isEmpty(v) {
		return true
	}
	return a.report(t, "expected not empty", m)
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

func (a asserter) Nil(t *testing.T, v any, m ...any) bool {
	t.Helper()
	if isNil(v) {
		return true
	}
	return a.report(t, fmt.Sprintf("expected nil, got %#v", v), m)
}

func (a asserter) NotNil(t *testing.T, v any, m ...any) bool {
	t.Helper()
	if !isNil(v) {
		return true
	}
	return a.report(t, "expected not nil", m)
}

func (a asserter) True(t *testing.T, v bool, m ...any) bool {
	t.Helper()
	if v {
		return true
	}
	return a.report(t, "expected true", m)
}

func (a asserter) False(t *testing.T, v bool, m ...any) bool {
	t.Helper()
	if !v {
		return true
	}
	return a.report(t, "expected false", m)
}

func (a asserter) Len(t *testing.T, v any, n int, m ...any) bool {
	t.Helper()
	if got := reflect.ValueOf(v).Len(); got != n {
		return a.report(t, fmt.Sprintf("length %d, want %d", got, n), m)
	}
	return true
}

func (a asserter) NoError(t *testing.T, err error, m ...any) bool {
	t.Helper()
	if err == nil {
		return true
	}
	return a.report(t, "unexpected error: "+err.Error(), m)
}

func (a asserter) Error(t *testing.T, err error, m ...any) bool {
	t.Helper()
	if err != nil {
		return true
	}
	return a.report(t, "expected an error", m)
}

func (a asserter) JSONEq(t *testing.T, want, got string, m ...any) bool {
	t.Helper()
	var w, g any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		return a.report(t, "want is not JSON: "+err.Error(), m)
	}
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		return a.report(t, "got is not JSON: "+err.Error(), m)
	}
	return a.Equal(t, w, g, m...)
}

func (a asserter) ElementsMatch(t *testing.T, want, got any, m ...any) bool {
	t.Helper()
	norm := func(v any) []string {
		rv := reflect.ValueOf(v)
		out := make([]string, rv.Len())
		for i := range out {
			out[i] = fmt.Sprintf("%#v", rv.Index(i).Interface())
		}
		sort.Strings(out)
		return out
	}
	return a.Equal(t, norm(want), norm(got), m...)
}

func (a asserter) NotPanics(t *testing.T, f func(), m ...any) (ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			ok = a.report(t, fmt.Sprintf("panicked: %v", r), m)
		}
	}()
	f()
	return true
}
