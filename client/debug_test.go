package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	secretToken = "btk_supersecrettoken"
	secretQuery = "hunter2-query-value"
	secretBody  = "hunter2-body-value"
	secretReply = "hunter2-reply-value"
	secretCook  = "hunter2-cookie-value"
)

func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range []string{secretToken, secretQuery, secretBody, secretReply, secretCook, "Bearer", "Authorization", "Cookie", "?"} {
		if strings.Contains(out, s) {
			t.Errorf("debug output leaked %q: %q", s, out)
		}
	}
}

func debugServer(t *testing.T, handler http.HandlerFunc) (*Client, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	var buf bytes.Buffer
	c := New(srv.URL, "test")
	c.Debug = &buf
	return c, &buf
}

func okReply(w http.ResponseWriter, status int, requestID string) {
	w.Header().Set("Set-Cookie", "session="+secretCook)
	if requestID != "" {
		w.Header().Set(HeaderRequestID, requestID)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status, "success": status < 400, "message": secretReply,
		"data": map[string]any{"leak": secretReply},
	})
}

func TestDebugLinesCarryMethodPathStatusDurationAndRequestID(t *testing.T) {
	c, buf := debugServer(t, func(w http.ResponseWriter, _ *http.Request) { okReply(w, 200, "req-abc123") })
	_, err := c.Do(context.Background(), secretToken, Request{
		Method: http.MethodPost, Path: "/paas/apps/a1/restart",
		Query:   url.Values{"token": {secretQuery}},
		Body:    map[string]string{"password": secretBody},
		Headers: map[string]string{"Cookie": "s=" + secretCook},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"POST", "/api/v1/paas/apps/a1/restart", "200", "request-id=req-abc123", "ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("want one line per request, got %d: %q", n, out)
	}
	assertNoSecrets(t, out)
}

func TestDebugOmitsRequestIDWhenTheAPIReturnsNone(t *testing.T) {
	c, buf := debugServer(t, func(w http.ResponseWriter, _ *http.Request) { okReply(w, 200, "") })
	if _, err := c.Do(context.Background(), secretToken, Request{Method: http.MethodGet, Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "request-id") {
		t.Errorf("no id was sent, none should be printed: %q", buf.String())
	}
}

func TestDebugIsSilentByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { okReply(w, 200, "id") }))
	defer srv.Close()
	c := New(srv.URL, "test")
	if c.Debug != nil {
		t.Fatal("debug must be off unless asked for")
	}
	if _, err := c.Do(context.Background(), secretToken, Request{Method: http.MethodGet, Path: "/x"}); err != nil {
		t.Fatal(err)
	}
}

func TestDebugRequestIDIsSanitised(t *testing.T) {
	cases := map[string]string{
		"plain":                  "plain",
		"  padded  ":             "padded",
		"esc\x1b[31mred":         "esc[31mred",
		"two words":              "twowords",
		strings.Repeat("a", 500): strings.Repeat("a", maxRequestIDLen),
		strings.Repeat("a", 128): strings.Repeat("a", 128),
		strings.Repeat("a", 129): strings.Repeat("a", 128),
		"":                       "",
		"\x00\x01":               "",
		"unicode\u202eevil":      "unicodeevil",
	}
	for in, want := range cases {
		if got := sanitizeRequestID(in); got != want {
			t.Errorf("sanitizeRequestID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDebugRecordsFailuresWithoutLeaking(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, _ *http.Request) { okReply(w, 503, "req-5xx") },
		"not json": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(502)
			_, _ = w.Write([]byte("<html>" + secretReply))
		},
		"malformed envelope": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":`)) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			c, buf := debugServer(t, h)
			_, err := c.Do(context.Background(), secretToken, Request{
				Method: http.MethodGet, Path: "/x", Query: url.Values{"k": {secretQuery}},
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(buf.String(), "/api/v1/x") {
				t.Errorf("the request was not logged: %q", buf.String())
			}
			assertNoSecrets(t, buf.String())
		})
	}
	t.Run("503 status is shown", func(t *testing.T) {
		c, buf := debugServer(t, cases["server error"])
		_, _ = c.Do(context.Background(), secretToken, Request{Method: http.MethodGet, Path: "/x"})
		if !strings.Contains(buf.String(), "503") || !strings.Contains(buf.String(), "request-id=req-5xx") {
			t.Errorf("got %q", buf.String())
		}
	})
}

func TestDebugTimeoutDoesNotQuoteTheURL(t *testing.T) {
	c, buf := debugServer(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c.HTTP.Timeout = 50 * time.Millisecond
	_, err := c.Do(context.Background(), secretToken, Request{
		Method: http.MethodGet, Path: "/slow", Query: url.Values{"k": {secretQuery}},
	})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	out := buf.String()
	if !strings.Contains(out, "/api/v1/slow") || !strings.Contains(out, "network error") && !strings.Contains(out, "timeout") {
		t.Errorf("the failure was not logged: %q", out)
	}
	assertNoSecrets(t, out)
}

func TestDebugCancelledContextIsNamed(t *testing.T) {
	c, buf := debugServer(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if _, err := c.Do(ctx, secretToken, Request{Method: http.MethodGet, Path: "/slow"}); err == nil {
		t.Fatal("expected cancellation")
	}
	if !strings.Contains(buf.String(), "canceled") {
		t.Errorf("got %q", buf.String())
	}
}

func TestDebugCoversStreams(t *testing.T) {
	c, buf := debugServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(HeaderRequestID, "req-stream")
		_, _ = w.Write([]byte("data: line\n\n"))
	})
	var lines []string
	err := c.Stream(context.Background(), secretToken, Request{Path: "/paas/apps/a/logs/build", Query: url.Values{"k": {secretQuery}}},
		func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "GET /api/v1/paas/apps/a/logs/build") || !strings.Contains(out, "request-id=req-stream") {
		t.Errorf("the stream was not logged: %q", out)
	}
	assertNoSecrets(t, out)
}

func TestDebugIsSafeUnderConcurrentRequests(t *testing.T) {
	c, buf := debugServer(t, func(w http.ResponseWriter, _ *http.Request) { okReply(w, 200, "id") })
	var wg sync.WaitGroup
	const n = 40
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Do(context.Background(), secretToken, Request{Method: http.MethodGet, Path: "/x"})
		}()
	}
	wg.Wait()
	c.debugMu.Lock()
	defer c.debugMu.Unlock()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != n {
		t.Fatalf("want %d whole lines, got %d", n, len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "beaver debug: GET /api/v1/x -> 200 in ") {
			t.Errorf("interleaved or malformed line: %q", l)
		}
	}
}
