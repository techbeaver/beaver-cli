package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/credential"
)

// Signing in from a build pipeline without a secret.
//
// Rationale is in docs/adr/0015-ci-workload-identity.md.

// ErrNoWorkloadIdentity means this process is not running somewhere that can
// prove who it is.
var ErrNoWorkloadIdentity = fmt.Errorf("no CI workload identity is available here")

// WorkloadOptions configures a pipeline login.
type WorkloadOptions struct {
	Host   string
	Scopes []string
	// Audience overrides discovery. Left empty in normal use.
	Audience string
}

// workloadSource is one CI system that can issue an identity token.
type workloadSource struct {
	// Name is what the CLI calls it when reporting.
	Name string
	// detect reports whether we are running there, separately from whether a
	// token can actually be fetched. The distinction matters: being on a GitHub
	// runner with no id-token permission is by far the most common failure, and
	// it deserves its own message rather than "not available".
	detect func() bool
	// fetch retrieves an identity token for an audience.
	fetch func(ctx context.Context, audience string) (string, error)
}

var workloadSources = []workloadSource{githubActions}

// githubActions reads the token the runner offers a job that asked for it.
var githubActions = workloadSource{
	Name:   "GitHub Actions",
	detect: func() bool { return os.Getenv("GITHUB_ACTIONS") == "true" },
	fetch: func(ctx context.Context, audience string) (string, error) {
		endpoint := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
		bearer := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
		if endpoint == "" || bearer == "" {
			return "", fmt.Errorf("this job cannot request an identity token. Add this to the job or the workflow:\n\n" +
				"  permissions:\n    id-token: write\n    contents: read\n")
		}

		u, err := url.Parse(endpoint)
		if err != nil {
			return "", fmt.Errorf("the runner gave an unreadable token endpoint: %w", err)
		}
		q := u.Query()
		q.Set("audience", audience)
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Accept", "application/json; api-version=2.0")

		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return "", fmt.Errorf("asking the runner for an identity token: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("the runner refused to issue an identity token (status %d)", resp.StatusCode)
		}

		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Value == "" {
			return "", fmt.Errorf("the runner returned no identity token")
		}
		return body.Value, nil
	},
}

// WorkloadName returns the CI system this process is running on, or "" for
// none. Used to decide whether an unauthenticated command should try to sign
// itself in before giving up.
func WorkloadName() string {
	for _, s := range workloadSources {
		if s.detect() {
			return s.Name
		}
	}
	return ""
}

// LoginWorkload exchanges this pipeline's identity token for a machine token.
//
// Nothing is stored by this function. The caller decides, and on a runner the
// answer is usually "keep it for the rest of the job".
func LoginWorkload(ctx context.Context, opts WorkloadOptions) (*credential.Token, error) {
	var source *workloadSource
	for i := range workloadSources {
		if workloadSources[i].detect() {
			source = &workloadSources[i]
			break
		}
	}
	if source == nil {
		return nil, ErrNoWorkloadIdentity
	}

	audience := strings.TrimSpace(opts.Audience)
	if audience == "" {
		discovered, err := discoverResource(ctx, opts.Host)
		if err != nil {
			return nil, err
		}
		audience = discovered
	}

	idToken, err := source.fetch(ctx, audience)
	if err != nil {
		return nil, err
	}

	return exchangeWorkloadToken(ctx, opts, audience, idToken)
}

// discoverResource asks the platform which audience its tokens are bound to.
//
// Asking beats hardcoding: the value differs between deployments, and a
// customer copying a workflow from the docs should never have to know which one
// they are on.
func discoverResource(ctx context.Context, host string) (string, error) {
	endpoint := strings.TrimRight(host, "/") + "/.well-known/oauth-protected-resource"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching %s: %w", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s does not publish a resource identifier (status %d), so no audience can be requested",
			host, resp.StatusCode)
	}

	var body struct {
		Resource string `json:"resource"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || strings.TrimSpace(body.Resource) == "" {
		return "", fmt.Errorf("%s published no resource identifier", host)
	}
	return strings.TrimSpace(body.Resource), nil
}

// exchangeWorkloadToken presents the identity token at the token endpoint.
func exchangeWorkloadToken(ctx context.Context, opts WorkloadOptions, audience, idToken string) (*credential.Token, error) {
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {idToken},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
		"resource":           {audience},
	}
	if len(opts.Scopes) > 0 {
		form.Set("scope", strings.Join(opts.Scopes, " "))
	}

	endpoint := strings.TrimRight(opts.Host, "/") + "/api/v1/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the authorization server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		tokenResponse
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("the authorization server returned something unreadable")
	}

	if body.AccessToken == "" {
		// The server's description names what did not match: the repository, the
		// ref, the audience. Passing it through unchanged is the difference
		// between a five-minute fix and an afternoon.
		message := body.Description
		if message == "" {
			message = "this pipeline is not bound to any TechBeaver account. " +
				"Create a CI identity for this repository under Portal, AI and CLI"
		}
		code := body.Error
		if code == "" {
			code = "token_exchange_failed"
		}
		return nil, &client.APIError{StatusCode: resp.StatusCode, Code: code, Message: message}
	}

	tok := &credential.Token{
		AccessToken: body.AccessToken,
		TokenType:   body.TokenType,
		Host:        strings.TrimRight(opts.Host, "/"),
	}
	if body.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	if body.Scope != "" {
		tok.Scopes = strings.Fields(body.Scope)
	}
	// No refresh token comes back, and none is wanted: the pipeline holds the
	// thing that mints tokens, so it can always exchange again.
	return tok, nil
}
