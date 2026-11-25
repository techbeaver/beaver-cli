package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HeaderRequestID is the response header the platform uses to identify a request.
const HeaderRequestID = "X-Request-ID"

const maxRequestIDLen = 128

// logCall records a finished request. It is given the method, the path (never a
// query), the response and the timing, and nothing else, so it cannot print a
// header, a body or a query value. resp is nil when no response arrived.
func (c *Client) logCall(method, path string, resp *http.Response, started time.Time, err error) {
	if c.Debug == nil {
		return
	}
	if method == "" {
		method = http.MethodGet
	}
	c.debugMu.Lock()
	defer c.debugMu.Unlock()
	// Whole milliseconds, so a sub-millisecond request reads "0ms", never "0s".
	elapsed := fmt.Sprintf("%dms", time.Since(started).Milliseconds())
	switch {
	case resp != nil:
		line := fmt.Sprintf("beaver debug: %s /api/v1%s -> %d in %s", method, path, resp.StatusCode, elapsed)
		if id := sanitizeRequestID(resp.Header.Get(HeaderRequestID)); id != "" {
			line += " request-id=" + id
		}
		_, _ = fmt.Fprintln(c.Debug, line)
	default:
		_, _ = fmt.Fprintf(c.Debug, "beaver debug: %s /api/v1%s -> %s after %s\n", method, path, failureKind(err), elapsed)
	}
}

// failureKind names a transport failure without quoting it: a transport error
// carries the full request URL, query string included.
func failureKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "network error"
	}
}

// sanitizeRequestID keeps a header value printable and short, so a hostile
// server cannot inject terminal escapes or flood the line.
func sanitizeRequestID(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	for _, r := range id {
		if r < 0x21 || r > 0x7e {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxRequestIDLen {
			break
		}
	}
	return b.String()
}
