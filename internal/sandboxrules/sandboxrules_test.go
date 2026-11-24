package sandboxrules

import (
	"strings"
	"testing"
)

func TestCheckName(t *testing.T) {
	good := map[string]string{
		"a": "a", "  box  ": "box", "my box.1_x-y": "my box.1_x-y", "9lives": "9lives",
		strings.Repeat("a", 63): strings.Repeat("a", 63), strings.Repeat("é", 63): strings.Repeat("é", 63),
	}
	for in, want := range good {
		if got, ok := CheckName(in); !ok || got != want {
			t.Errorf("%q: got %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", " ", "-a", ".a", "_a", "a/b", "a:b", "a\tb", "a\nb", "a\u200bb", "a\u202eb", "box\U0001F600", strings.Repeat("a", 64), strings.Repeat("é", 64), "a\u0301"} {
		if got, ok := CheckName(in); ok {
			t.Errorf("%q must be refused, got %q", in, got)
		}
	}
}
