package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A fake runner and a fake platform, wired together the way a real job is.
//
// The point of testing this end to end rather than in pieces: the failure this
// feature has to avoid is a workflow that asks for the wrong audience, and the
// audience travels through three hops before anything checks it.

// TestMain clears the runner's own environment, so a test that wants to be on a
// runner has to say so. See the note in internal/command/command_test.go: this
// repository's CI is GitHub Actions, which is exactly what this package
// detects.
func TestMain(m *testing.M) {
	for _, key := range []string{
		"GITHUB_ACTIONS",
		"ACTIONS_ID_TOKEN_REQUEST_URL",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN",
	} {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

type fakePlatform struct {
	server *httptest.Server
	// requestedAudience is what the exchange said the token was minted for.
	requestedAudience string
	requestedScope    string
	subjectToken      string
	grantType         string
	refuse            string
	refuseDescription string
	resource          string
}

func newFakePlatform(t *testing.T) *fakePlatform {
	t.Helper()
	p := &fakePlatform{resource: "https://mcp.example.test/mcp"}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": p.resource})
	})

	mux.HandleFunc("/api/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.grantType = r.PostForm.Get("grant_type")
		p.subjectToken = r.PostForm.Get("subject_token")
		p.requestedAudience = r.PostForm.Get("resource")
		p.requestedScope = r.PostForm.Get("scope")
		w.Header().Set("Content-Type", "application/json")
		if p.refuse != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": p.refuse, "error_description": p.refuseDescription,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "btk_from_the_exchange",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"scope":        "paas:read paas:deploy",
		})
	})

	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

// fakeRunner stands in for the Actions token service.
type fakeRunner struct {
	server *httptest.Server
	// audience is what the CLI asked the runner to mint the token for.
	audience string
	status   int
}

func newFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	r := &fakeRunner{status: http.StatusOK}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer runner-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.audience = req.URL.Query().Get("audience")
		if r.status != http.StatusOK {
			w.WriteHeader(r.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": "the.id.token"})
	}))
	t.Cleanup(r.server.Close)
	return r
}

// onGitHubActions sets the environment a job with id-token permission sees.
func onGitHubActions(t *testing.T, runnerURL string) {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "true")
	if runnerURL != "" {
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", runnerURL+"?api-version=2.0")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-secret")
	}
}

func TestWorkloadIsNotDetectedOffARunner(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	if name := WorkloadName(); name != "" {
		t.Fatalf("a developer laptop reported itself as %q", name)
	}
	_, err := LoginWorkload(context.Background(), WorkloadOptions{Host: "https://api.example"})
	if err != ErrNoWorkloadIdentity {
		t.Fatalf("expected ErrNoWorkloadIdentity, got %v", err)
	}
}

func TestAJobWithoutIdTokenPermissionIsToldExactlyWhatToAdd(t *testing.T) {
	// The most common failure, and this error is the only documentation read then.
	onGitHubActions(t, "")
	platform := newFakePlatform(t)

	_, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.server.URL})
	if err == nil {
		t.Fatal("a job with no id-token permission signed in")
	}
	if !strings.Contains(err.Error(), "id-token: write") {
		t.Fatalf("the error must name the missing permission: %v", err)
	}
}

func TestThePipelineExchangesItsIdentityForAMachineToken(t *testing.T) {
	runner := newFakeRunner(t)
	platform := newFakePlatform(t)
	onGitHubActions(t, runner.server.URL)

	tok, err := LoginWorkload(context.Background(), WorkloadOptions{
		Host:   platform.server.URL,
		Scopes: []string{"paas:read", "paas:deploy"},
	})
	if err != nil {
		t.Fatalf("a correctly configured job was refused: %v", err)
	}
	if tok.AccessToken != "btk_from_the_exchange" {
		t.Fatalf("token was %q", tok.AccessToken)
	}
	if tok.RefreshToken != "" {
		t.Fatal("a workload login must not keep a refresh token")
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("expiry was not recorded, so nothing can re-exchange on time")
	}

	if platform.grantType != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Fatalf("grant type was %q", platform.grantType)
	}
	if platform.subjectToken != "the.id.token" {
		t.Fatalf("the runner's token did not reach the exchange: %q", platform.subjectToken)
	}
	if platform.requestedScope != "paas:read paas:deploy" {
		t.Fatalf("scope was %q", platform.requestedScope)
	}
}

func TestTheAudienceIsDiscoveredFromThePlatformNotGuessed(t *testing.T) {
	// A hardcoded audience is wrong on every deployment but one, and fails obscurely.
	runner := newFakeRunner(t)
	platform := newFakePlatform(t)
	platform.resource = "https://mcp.some-other-deployment.test/mcp"
	onGitHubActions(t, runner.server.URL)

	if _, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.server.URL}); err != nil {
		t.Fatal(err)
	}
	if runner.audience != platform.resource {
		t.Fatalf("the runner was asked for audience %q, but the platform publishes %q",
			runner.audience, platform.resource)
	}
	if platform.requestedAudience != platform.resource {
		t.Fatalf("the exchange declared resource %q", platform.requestedAudience)
	}
}

func TestTheServersRefusalReachesTheBuildLogIntact(t *testing.T) {
	// The server names what did not match; swallowing it costs an afternoon.
	runner := newFakeRunner(t)
	platform := newFakePlatform(t)
	platform.refuse = "invalid_grant"
	platform.refuseDescription = "no CI identity in this platform is bound to acme/website"
	onGitHubActions(t, runner.server.URL)

	_, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.server.URL})
	if err == nil {
		t.Fatal("a refused exchange returned a token")
	}
	if !strings.Contains(err.Error(), "acme/website") {
		t.Fatalf("the server's description was swallowed: %v", err)
	}
}

func TestAPlatformWithNoResourceIdentifierIsReportedClearly(t *testing.T) {
	runner := newFakeRunner(t)
	platform := newFakePlatform(t)
	platform.resource = ""
	onGitHubActions(t, runner.server.URL)

	_, err := LoginWorkload(context.Background(), WorkloadOptions{Host: platform.server.URL})
	if err == nil || !strings.Contains(err.Error(), "resource identifier") {
		t.Fatalf("expected a clear discovery failure, got %v", err)
	}
}

func TestAnExplicitAudienceSkipsDiscovery(t *testing.T) {
	runner := newFakeRunner(t)
	platform := newFakePlatform(t)
	platform.resource = "https://wrong.test/mcp"
	onGitHubActions(t, runner.server.URL)

	if _, err := LoginWorkload(context.Background(), WorkloadOptions{
		Host:     platform.server.URL,
		Audience: "https://explicit.test/mcp",
	}); err != nil {
		t.Fatal(err)
	}
	if runner.audience != "https://explicit.test/mcp" {
		t.Fatalf("the override was ignored: %q", runner.audience)
	}
}
