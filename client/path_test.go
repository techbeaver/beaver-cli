package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCheckPathRefusesAnythingThatCouldLeaveItsSegment(t *testing.T) {
	bad := []string{
		"", "paas/apps", "/paas/apps?x=1", "/paas/apps#f", "/paas\\apps", "/paas/../admin", "/paas/./apps",
		"/paas/%2e%2e/admin", "/paas/%2E%2E/admin", "/paas/.%2e/admin", "/paas/%2e./admin", "/paas//apps", "/paas/apps/", "//x",
		"/paas/apps/..", "/..",
	}
	for _, p := range bad {
		if err := checkPath(p); err == nil {
			t.Errorf("checkPath(%q) accepted", p)
		}
	}
	good := []string{"/paas/apps", "/paas/apps/abc-123/env", "/paas/apps/a%2Fb", "/paas/apps/a%3Fb%23c", "/x/..a", "/x/a..", "/mcp/confirmations/ct_1"}
	for _, p := range good {
		if err := checkPath(p); err != nil {
			t.Errorf("checkPath(%q) refused: %v", p, err)
		}
	}
}

func TestEveryEscapedSegmentIsAcceptedAndStaysOneSegmentOnTheWire(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"status":200,"success":true,"data":{}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "test")
	for _, id := range []string{"a/b", "a?b=1", "a#b", "a b", "100%", "a\\b", "..%2f", "x/../y", "ünï", "a%2e%2e", "\n"} {
		seen = ""
		if _, err := c.Do(context.Background(), "t", Request{Method: http.MethodGet, Path: "/paas/apps/" + url.PathEscape(id) + "/env"}); err != nil {
			t.Errorf("%q: %v", id, err)
			continue
		}
		if got := strings.Count(strings.Trim(seen, "/"), "/"); got != 5 {
			t.Errorf("%q became %q on the wire", id, seen)
		}
	}
}

func TestUnescapedDotSegmentsNeverReachTheNetwork(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	c := New(srv.URL, "test")
	for _, id := range []string{"..", "."} {
		_, err := c.Do(context.Background(), "t", Request{Method: http.MethodDelete, Path: "/paas/apps/" + url.PathEscape(id)})
		if err == nil {
			t.Errorf("%q was sent", id)
		}
	}
	if _, _, err := c.Tail(context.Background(), "t", Request{Path: "/paas/apps/../logs"}, 10, 0); err == nil {
		t.Error("a stream path with a dot segment was opened")
	}
	if hits != 0 {
		t.Fatalf("%d request(s) reached the server", hits)
	}
}

func TestACredentialDoesNotFollowARedirectToAnotherHost(t *testing.T) {
	stolen := ""
	thief := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stolen = r.Header.Get("Authorization")
	}))
	defer thief.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, thief.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer api.Close()
	c := New(api.URL, "test")
	if _, err := c.Do(context.Background(), "secret-token", Request{Method: http.MethodGet, Path: "/paas/apps"}); err == nil {
		t.Fatal("a redirect to another host was followed")
	}
	if _, _, err := c.Tail(context.Background(), "secret-token", Request{Path: "/paas/apps/a/logs/runtime"}, 5, 0); err == nil {
		t.Fatal("a stream redirected to another host was followed")
	}
	if stolen != "" {
		t.Fatal("the bearer token reached another host")
	}
}
