package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPinAudienceForPlatformHostsStaysUnderTheplatformDomain(t *testing.T) {
	ok := [][2]string{
		{"https://cloudapi.techbeaver.io", "https://mcp.techbeaver.io/mcp"},
		{"https://cloudapi.techbeaver.io", "https://cloudapi.techbeaver.io"},
		{"https://cloudapi.techbeaver.io", "https://techbeaver.io/mcp"},
		{"https://cloudapi.techbeaver.io", "https://a.b.techbeaver.io/x"},
		{"https://cloudapi.techbeaver.io.", "https://mcp.techbeaver.io/mcp"},
		{"https://techbeaver.io", "https://mcp.techbeaver.io/mcp"},
	}
	for _, in := range ok {
		if _, err := pinAudience(in[0], in[1]); err != nil {
			t.Errorf("pinAudience(%q, %q) refused: %v", in[0], in[1], err)
		}
	}
	for _, res := range []string{
		"https://evil.test/mcp", "https://techbeaver.io.evil.test/mcp", "https://eviltechbeaver.io/mcp",
		"https://mcp.techbeaver.io@evil.test/mcp", "https://user@mcp.techbeaver.io/mcp", "http://mcp.techbeaver.io/mcp",
		"ftp://mcp.techbeaver.io/mcp", "https://mcp.techbeaver.io/mcp#frag", "https://127.0.0.1/mcp", "mcp.techbeaver.io", "",
		"https://techbeaver.io.example.com/mcp", "https://xtechbeaver.io/mcp",
	} {
		if got, err := pinAudience("https://cloudapi.techbeaver.io", res); err == nil {
			t.Errorf("platform host accepted audience %q as %q", res, got)
		}
	}
}

func TestPinAudienceForAnyOtherHostMustBeThatExactHost(t *testing.T) {
	ok := [][2]string{
		{"https://api.example.test", "https://api.example.test/mcp"},
		{"https://api.example.test", "https://API.example.test:443/mcp"},
		{"https://api.example.test:8443", "https://api.example.test:9000/x"},
		{"https://api.example.test.", "https://api.example.test/mcp"},
		{"http://127.0.0.1:5000", "https://anything.example.test/mcp"},
		{"http://localhost:5000", "http://localhost:6000/mcp"},
	}
	for _, in := range ok {
		if _, err := pinAudience(in[0], in[1]); err != nil {
			t.Errorf("pinAudience(%q, %q) refused: %v", in[0], in[1], err)
		}
	}
	refused := [][2]string{
		{"https://mine.github.io", "https://victim.github.io/mcp"},
		{"https://evil.co.uk", "https://bank.co.uk/mcp"},
		{"https://api.example.test", "https://mcp.example.test/mcp"},
		{"https://example.test", "https://mcp.example.test/mcp"},
		{"https://api.example.test", "https://example.test/mcp"},
		{"https://api.example.test", "https://other.test/mcp"},
		{"https://api.example.test", "https://test/mcp"},
		{"https://api.example.test", "https://api.example.test.evil.test/mcp"},
		{"https://api.example.test", "https://techbeaver.io/mcp"},
		{"https://api.example.test", "https://mcp.techbeaver.io/mcp"},
		{"https://techbeaver.io.evil.test", "https://mcp.techbeaver.io/mcp"},
		{"https://api.example.test", "https://user@api.example.test/mcp"},
		{"http://evil.test", "https://evil.test/mcp"},
	}
	for _, in := range refused {
		if got, err := pinAudience(in[0], in[1]); err == nil {
			t.Errorf("pinAudience(%q, %q) accepted %q", in[0], in[1], got)
		}
	}
}

func TestACIJobRefusesACustomHostUnlessExplicitlyAllowed(t *testing.T) {
	runner := newFakeRunner(t)
	platform := newFakePlatform(t)
	onGitHubActions(t, runner.server.URL)
	t.Setenv(AllowCustomHostEnv, "")
	for _, v := range []string{"", "0", "true", "yes", "2"} {
		t.Setenv(AllowCustomHostEnv, v)
		_, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.server.URL})
		if err == nil || !strings.Contains(err.Error(), AllowCustomHostEnv) {
			t.Errorf("%q: %v", v, err)
		}
		if runner.audience != "" || platform.subjectToken != "" {
			t.Fatalf("%q: something was minted or sent", v)
		}
	}
	t.Setenv(AllowCustomHostEnv, "1")
	if _, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.server.URL}); err != nil {
		t.Fatalf("allowed host refused: %v", err)
	}
}

func TestTheDefaultHostNeedsNoOverrideForCI(t *testing.T) {
	t.Setenv(AllowCustomHostEnv, "")
	for _, h := range []string{"https://cloudapi.techbeaver.io", "https://CLOUDAPI.techbeaver.io/", "https://cloudapi.techbeaver.io."} {
		if err := requireTrustedWorkloadHost(h); err != nil {
			t.Errorf("%q: %v", h, err)
		}
	}
	for _, h := range []string{"https://cloudapi.techbeaver.io.evil.test", "https://api.techbeaver.io", "http://127.0.0.1:1", "https://techbeaver.io"} {
		if err := requireTrustedWorkloadHost(h); err == nil {
			t.Errorf("%q was trusted", h)
		}
	}
}

func TestAnUnsafeHostIsRefusedBeforeTheRunnerIsAsked(t *testing.T) {
	runner := newFakeRunner(t)
	hits := 0
	runner.server.Config.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ })
	onGitHubActions(t, runner.server.URL)

	for _, host := range []string{
		"http://api.example.test",
		"https://user:pw@api.example.test",
		"https://api.example.test/some/path",
		"ftp://api.example.test",
		"",
	} {
		if _, err := LoginWorkload(context.Background(), WorkloadOptions{Host: host}); err == nil {
			t.Errorf("host %q was accepted", host)
		}
	}
	if hits != 0 {
		t.Fatalf("the runner minted %d identity token(s) for a host that was refused", hits)
	}
}

func TestADiscoveryDocumentCannotPointTheAudienceAtAnotherDomain(t *testing.T) {
	// Loopback hosts accept any audience, but the shape rules still apply.
	runner := newFakeRunner(t)
	onGitHubActions(t, runner.server.URL)
	for _, resource := range []string{"ftp://x.test/mcp", "https://u@x.test/mcp", "http://x.test/mcp", "https://x.test/mcp#f"} {
		platform := newFakePlatform(t)
		platform.resource = resource
		runner.audience = ""
		if _, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.server.URL}); err == nil {
			t.Errorf("audience %q was accepted", resource)
		}
		if runner.audience != "" {
			t.Errorf("the runner was asked to mint a token for %q", resource)
		}
		if platform.subjectToken != "" {
			t.Errorf("an identity token reached the exchange for audience %q", resource)
		}
	}
}

func TestTheRunnerTokenEndpointMustBeSafe(t *testing.T) {
	platform := newFakePlatform(t)
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-secret")
	for _, endpoint := range []string{
		"http://token.example.test/id?api-version=2.0",
		"https://u:p@token.example.test/id",
		"ftp://token.example.test/id",
	} {
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", endpoint)
		_, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.server.URL})
		if err == nil || !strings.Contains(err.Error(), "not acceptable") {
			t.Errorf("runner endpoint %q: %v", endpoint, err)
		}
	}
	if platform.subjectToken != "" {
		t.Fatal("something reached the exchange")
	}
}

func TestTheExchangeNeverFollowsARedirect(t *testing.T) {
	runner := newFakeRunner(t)
	onGitHubActions(t, runner.server.URL)
	stolen := ""
	thief := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		stolen = r.PostForm.Get("subject_token")
	}))
	defer thief.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"resource":"https://mcp.example.test/mcp"}`))
	})
	mux.HandleFunc("/api/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, thief.URL+"/token", http.StatusTemporaryRedirect)
	})
	platform := httptest.NewServer(mux)
	defer platform.Close()

	if _, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.URL}); err == nil {
		t.Fatal("an exchange answered by a redirect succeeded")
	}
	if stolen != "" {
		t.Fatal("the identity token was replayed to the redirect target")
	}
}

func TestRefreshAndDeviceAndRevokeRefuseUnsafeHostsWithoutSendingAnything(t *testing.T) {
	ctx := context.Background()
	for _, host := range []string{"http://api.example.test", "https://u@api.example.test", "https://api.example.test/x"} {
		if _, err := Refresh(ctx, host, "refresh-token"); err == nil {
			t.Errorf("Refresh accepted %q", host)
		}
		if err := Revoke(ctx, host, "tok", HintAccessToken); err == nil {
			t.Errorf("Revoke accepted %q", host)
		}
		if _, err := LoginDevice(ctx, DeviceOptions{Host: host}); err == nil {
			t.Errorf("LoginDevice accepted %q", host)
		}
		if _, err := authorizeURL(host, "http://127.0.0.1:1/callback", "s", "c", nil); err == nil {
			t.Errorf("authorizeURL accepted %q", host)
		}
	}
}

func TestATokenIsRecordedAgainstTheNormalisedHostItWasIssuedFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":60}`))
	}))
	defer srv.Close()
	tok, err := Refresh(context.Background(), srv.URL+"/", "old")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Host != srv.URL {
		t.Fatalf("host recorded as %q, want %q", tok.Host, srv.URL)
	}
}
