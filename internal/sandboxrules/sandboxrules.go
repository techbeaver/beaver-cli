// Package sandboxrules holds the sandbox input rules the CLI and the MCP tools both check before
// sending anything. The server checks again and its answer is still shown.
package sandboxrules

import (
	"strings"
	"unicode"
)

// NameMax is the longest sandbox name, in characters.
const NameMax = 63

// NameRule is the platform's own sentence for a bad name.
const NameRule = "A sandbox name is 1 to 63 characters: letters, digits, spaces, dots, underscores and hyphens, starting with a letter or digit."

// CheckName trims a name and reports whether it is acceptable.
func CheckName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if n := len([]rune(name)); n == 0 || n > NameMax {
		return "", false
	}
	for i, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
		case i > 0 && (r == ' ' || r == '.' || r == '_' || r == '-'):
		default:
			return "", false
		}
	}
	return name, true
}

// Exec bounds the API states for a command.
const (
	ExecMinTimeoutSec = 1
	ExecMaxTimeoutSec = 3600
	ExecMaxStdinBytes = 1 << 20
	// ExecMaxWaitSeconds bounds one call: the edge cuts a silent request, so none may run longer.
	ExecMaxWaitSeconds   = 60
	ExecFirstWaitSeconds = 30
	ExecTimeoutRule      = "timeoutSeconds is between 1 and 3600."
)
