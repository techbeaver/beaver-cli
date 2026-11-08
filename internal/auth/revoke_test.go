package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRevokeSendsAnRFC7009Request(t *testing.T) {
	var got map[string]string
	var ctype, method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = map[string]string{
			"token": r.PostForm.Get("token"), "hint": r.PostForm.Get("token_type_hint"), "client": r.PostForm.Get("client_id"),
		}
		ctype, method, path = r.Header.Get("Content-Type"), r.Method, r.URL.Path
		if r.Header.Get("Authorization") != "" {
			t.Error("a bearer header was sent to the revocation endpoint")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := Revoke(context.Background(), srv.URL, "the-token", HintRefreshToken); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/v1/oauth/revoke" || ctype != "application/x-www-form-urlencoded" {
		t.Errorf("%s %s %s", method, path, ctype)
	}
	if got["token"] != "the-token" || got["hint"] != "refresh_token" || got["client"] != ClientID {
		t.Errorf("form = %v", got)
	}
}

func TestRevokeReportsEveryWayItCanFail(t *testing.T) {
	for _, status := range []int{400, 401, 404, 429, 500, 502, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		if err := Revoke(context.Background(), srv.URL, "t", HintAccessToken); err == nil {
			t.Errorf("status %d was reported as success", status)
		}
		srv.Close()
	}

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()
	if err := Revoke(context.Background(), url, "t", HintAccessToken); err == nil {
		t.Error("an unreachable server was reported as success")
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer slow.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Revoke(ctx, slow.URL, "t", HintAccessToken); err == nil {
		t.Error("a timeout was reported as success")
	}
}

func TestRevokeDoesNotFollowARedirectWithTheToken(t *testing.T) {
	seen := ""
	thief := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen = r.PostForm.Get("token")
	}))
	defer thief.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, thief.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if err := Revoke(context.Background(), srv.URL, "secret", HintAccessToken); err == nil {
		t.Error("a redirect was reported as success")
	}
	if seen != "" {
		t.Fatal("the token followed a redirect to another host")
	}
}
