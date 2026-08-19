package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPKCEIsS256AndUnique(t *testing.T) {
	a, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if a.Verifier == b.Verifier {
		t.Fatal("two attempts produced the same verifier")
	}
	sum := sha256.Sum256([]byte(a.Verifier))
	if a.Challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("the challenge is not the S256 hash of the verifier")
	}
	if strings.ContainsAny(a.Challenge, "+/=") {
		t.Fatalf("the challenge must be base64url without padding, got %q", a.Challenge)
	}
}

func TestAuthorizeURLAlwaysDeclaresS256(t *testing.T) {
	raw := authorizeURL("https://api.example", "http://127.0.0.1:5000/callback", "st", "ch", []string{"paas:read"})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("method was %q; plain is not permitted", q.Get("code_challenge_method"))
	}
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type was %q", q.Get("response_type"))
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatal("challenge and state are both required")
	}
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") {
		t.Fatalf("redirect must be a loopback literal, got %q", q.Get("redirect_uri"))
	}
	if strings.Contains(raw, "client_secret") {
		t.Fatal("a public client must not send a secret")
	}
}

// serve drives the callback handler the way a browser would.
func serve(t *testing.T, wantState, query string) (chan callbackResult, *http.Response) {
	t.Helper()
	results := make(chan callbackResult, 1)
	srv := httptest.NewServer(callbackHandler(wantState, results))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + CallbackPath + "?" + query)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return results, resp
}

func TestAMismatchedStateIsRefusedWithoutExchanging(t *testing.T) {
	results, _ := serve(t, "expected", "state=attacker&code=stolen")
	res := <-results
	if !errors.Is(res.err, ErrStateMismatch) {
		t.Fatalf("want ErrStateMismatch, got %v", res.err)
	}
	if res.code != "" {
		t.Fatal("a mismatched state must not yield a code to exchange")
	}
}

func TestTheCallbackPageNeverContainsTheCode(t *testing.T) {
	const code = "authcode-must-not-appear"
	_, resp := serve(t, "st", "state=st&code="+code)
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if strings.Contains(string(buf[:n]), code) {
		t.Fatal("the authorization code was echoed into the page served back to the browser")
	}
}

func TestTheListenerAnswersExactlyOneRequest(t *testing.T) {
	results := make(chan callbackResult, 1)
	srv := httptest.NewServer(callbackHandler("st", results))
	defer srv.Close()

	first, err := http.Get(srv.URL + CallbackPath + "?state=st&code=one")
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Body.Close()
	if res := <-results; res.code != "one" {
		t.Fatalf("first request should have succeeded, got %+v", res)
	}

	second, err := http.Get(srv.URL + CallbackPath + "?state=st&code=two")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusGone {
		t.Fatalf("a replayed callback must be refused, got %d", second.StatusCode)
	}
}

func TestADeclinedConsentIsReportedAsSuch(t *testing.T) {
	results, _ := serve(t, "st", "state=st&error=access_denied&error_description=nope")
	if res := <-results; !errors.Is(res.err, ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", res.err)
	}
}

func TestLoginBindsLoopbackOnlyAndSendsNoSecret(t *testing.T) {
	var gotRedirect, gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form.Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"btk_a","refresh_token":"btk_r","expires_in":3600,"scope":"paas:read"}`))
	}))
	defer srv.Close()

	opened := make(chan string, 1)
	tok, err := Login(context.Background(), LoginOptions{
		Host:    srv.URL,
		Scopes:  []string{"paas:read"},
		Timeout: 10 * time.Second,
		OpenURL: func(target string) error {
			u, _ := url.Parse(target)
			gotRedirect = u.Query().Get("redirect_uri")
			opened <- target
			// Play the browser: complete the callback on the loopback port.
			go func() {
				redirect, _ := url.Parse(u.Query().Get("redirect_uri"))
				resp, err := http.Get(redirect.String() + "?state=" + u.Query().Get("state") + "&code=abc")
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	<-opened

	if !strings.HasPrefix(gotRedirect, "http://127.0.0.1:") {
		t.Fatalf("the listener must be on the loopback literal, got %q", gotRedirect)
	}
	if strings.Contains(gotForm, "client_secret") {
		t.Fatalf("a secret was sent to the token endpoint: %s", gotForm)
	}
	if !strings.Contains(gotForm, "code_verifier") {
		t.Fatalf("the verifier is what proves this exchange; form was %s", gotForm)
	}
	if tok.AccessToken != "btk_a" || tok.RefreshToken != "btk_r" {
		t.Fatalf("unexpected token: %+v", tok)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("expires_in must be turned into an absolute expiry")
	}
}

func TestOpenBrowserRefusesAMalformedURL(t *testing.T) {
	if err := OpenBrowser("not a url; rm -rf /"); err == nil {
		t.Fatal("a malformed target must be refused rather than handed to a command")
	}
}
