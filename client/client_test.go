package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDoSendsBearerTokenAndUserAgent(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		_ = json.NewEncoder(w).Encode(Envelope{Success: true, Data: json.RawMessage(`{"id":"a"}`)})
	}))
	defer srv.Close()

	c := New(srv.URL, "beaver/1.2.3")
	if _, err := c.Do(context.Background(), "btk_secret", Request{Method: http.MethodGet, Path: "/paas/apps"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer btk_secret" {
		t.Fatalf("authorization header was %q", gotAuth)
	}
	if gotUA != "beaver/1.2.3" {
		t.Fatalf("user agent was %q", gotUA)
	}
}

func TestCallerIPIsForwardedOnlyWhenSet(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(HeaderCFConnectingIP)
		_ = json.NewEncoder(w).Encode(Envelope{Success: true})
	}))
	defer srv.Close()
	c := New(srv.URL, "test")

	if _, err := c.Do(context.Background(), "t", Request{Method: http.MethodGet, Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("a CLI must not claim a caller address, got %q", got)
	}

	ctx := WithCallerIP(context.Background(), "203.0.113.7")
	if _, err := c.Do(ctx, "t", Request{Method: http.MethodGet, Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	if got != "203.0.113.7" {
		t.Fatalf("caller address was not forwarded, got %q", got)
	}
}

func TestErrorNeverContainsTheToken(t *testing.T) {
	const token = "btk_thismustnotleak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(Envelope{Message: "not yours", Code: "forbidden"})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "test").Do(context.Background(), token, Request{Method: http.MethodGet, Path: "/x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "thismustnotleak") {
		t.Fatalf("the credential leaked into an error message: %v", err)
	}
}

func TestNonJSONUpstreamIsReportedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>edge error</html>"))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "test").Do(context.Background(), "t", Request{Method: http.MethodGet, Path: "/x"})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected an APIError, got %T", err)
	}
	if apiErr.Code != "upstream_not_json" {
		t.Fatalf("a non-JSON body must be named as such, got code %q", apiErr.Code)
	}
}

func TestPausedServiceIsNotRetryableDespiteBeingA503(t *testing.T) {
	for _, code := range []string{"service_paused", "SERVICE_PAUSED", " Service_Paused "} {
		paused := &APIError{StatusCode: http.StatusServiceUnavailable, Code: code}
		if !paused.Paused() {
			t.Fatalf("%q must be recognised as a deliberate gate", code)
		}
		if paused.Retryable() {
			t.Fatalf("%q is an operator decision and must not be retried", code)
		}
	}
	ordinary := &APIError{StatusCode: http.StatusServiceUnavailable, Code: "upstream_down"}
	if !ordinary.Retryable() {
		t.Fatal("an ordinary 503 is still retryable")
	}
}

func TestRetryableIsConservative(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadRequest, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusPaymentRequired, false},
	} {
		got := (&APIError{StatusCode: tc.status}).Retryable()
		if got != tc.want {
			t.Errorf("status %d: Retryable() = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestDecodeListToleratesTheShapesThisAPIReturns(t *testing.T) {
	cases := map[string]struct {
		data string
		want int
	}{
		"a plain list":        {`[{"id":"1"},{"id":"2"}]`, 2},
		"a paginated wrapper": {`{"items":[{"id":"1"}]}`, 1},
		"a single object":     {`{"id":"1"}`, 1},
		"null":                {`null`, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeList(&Envelope{Data: json.RawMessage(tc.data)})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d rows, want %d", len(got), tc.want)
			}
		})
	}
}

func TestTailReturnsOnlyDataLinesAndBoundsThem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": heartbeat\n"))
		for i := 0; i < 50; i++ {
			_, _ = w.Write([]byte("data: line\n"))
		}
	}))
	defer srv.Close()

	lines, truncated, err := New(srv.URL, "test").
		Tail(context.Background(), "t", Request{Path: "/logs"}, 10, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 10 {
		t.Fatalf("expected the tail to be bounded to 10, got %d", len(lines))
	}
	if !truncated {
		t.Fatal("a bounded read must report that it truncated")
	}
	for _, l := range lines {
		if l != "line" {
			t.Fatalf("a non-data line leaked through: %q", l)
		}
	}
}
