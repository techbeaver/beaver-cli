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
	"github.com/techbeaver/beaver-cli/internal/config"
	"github.com/techbeaver/beaver-cli/internal/credential"
	"github.com/techbeaver/beaver-cli/internal/endpoint"
)

// Signing in from a build pipeline without a secret.
//
// Rationale is in ADR 0015.

// ErrNoWorkloadIdentity means this process is not running somewhere that can
// prove who it is.
var ErrNoWorkloadIdentity = fmt.Errorf("no CI workload identity is available here")

// WorkloadOptions configures a pipeline login.
//
// Host is the TechBeaver API the identity token is exchanged at. It must come
// from the profile, BEAVER_HOST or --host, never from a file in the repository
// being built. The audience is not an option: it is discovered from Host and
// pinned to Host's own domain by [pinAudience].
type WorkloadOptions struct {
	Host   string
	Scopes []string
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
			return "", fmt.Errorf("this job cannot request an identity token: add `permissions: id-token: write` to the job or the workflow")
		}

		u, err := url.Parse(endpoint)
		if err != nil {
			return "", fmt.Errorf("the runner gave an unreadable token endpoint: %w", err)
		}
		if u.User != nil {
			return "", fmt.Errorf("the runner's token endpoint is not acceptable: it carries a username or password")
		}
		if _, err := originOfURL(u); err != nil {
			return "", fmt.Errorf("the runner's token endpoint is not acceptable: %w", err)
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

		resp, err := authClient().Do(req)
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

	if _, err := endpoint.Normalise(opts.Host); err != nil {
		return nil, err
	}
	if err := requireTrustedWorkloadHost(opts.Host); err != nil {
		return nil, err
	}
	audience, err := discoverResource(ctx, opts.Host)
	if err != nil {
		return nil, err
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
	target, err := apiURL(host, "/.well-known/oauth-protected-resource")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := authClient().Do(req)
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
	return pinAudience(host, strings.TrimSpace(body.Resource))
}

// pinAudience accepts a discovered audience only if it belongs to the same
// domain as the API host. The identity token is a bearer credential for
// whatever audience it names, so an audience on an unrelated domain is
// refused: a host cannot talk the CLI into minting a token for someone else.
func pinAudience(apiHost, resource string) (string, error) {
	u, err := url.Parse(resource)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("%s published an audience that is not a plain URL", apiHost)
	}
	origin, err := originOfURL(u)
	if err != nil {
		return "", fmt.Errorf("%s published an unacceptable audience: %w", apiHost, err)
	}
	apiOrigin, err := endpoint.Normalise(apiHost)
	if err != nil {
		return "", err
	}
	apiName := hostnameOf(apiOrigin)
	if endpoint.IsLoopback(apiName) {
		return resource, nil
	}
	resName := hostnameOf(origin)
	if underDomain(apiName, platformDomain) {
		if underDomain(resName, platformDomain) {
			return resource, nil
		}
		return "", fmt.Errorf("%s published the audience %s, which is outside %s, so no identity token was requested", apiHost, resName, platformDomain)
	}
	// Exact match only: a guessed parent domain would treat a public suffix like github.io as one owner.
	if resName == apiName {
		return resource, nil
	}
	return "", fmt.Errorf("%s published the audience %s, which is not that host, so no identity token was requested", apiHost, resName)
}

// platformDomain is the domain TechBeaver's own hosts live under.
const platformDomain = "techbeaver.io"

func underDomain(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func hostnameOf(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// originOfURL applies the host rules to the origin of a full URL.
func originOfURL(u *url.URL) (string, error) {
	return endpoint.Normalise(u.Scheme + "://" + u.Host)
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

	target, err := apiURL(opts.Host, "/api/v1/oauth/token")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := authClient().Do(req)
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
		// The server names what did not match, so it is passed through unchanged.
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
		Host:        originOf(opts.Host),
	}
	if body.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	if body.Scope != "" {
		tok.Scopes = strings.Fields(body.Scope)
	}
	// No refresh token, by design: the pipeline can always exchange again.
	return tok, nil
}

// AllowCustomHostEnv lets a CI job exchange its identity with a host other than the default.
const AllowCustomHostEnv = "BEAVER_ALLOW_CUSTOM_HOST"

// requireTrustedWorkloadHost refuses to hand a CI identity to any host but the default one,
// because the host is chosen by the job's configuration, which a pull request can change.
func requireTrustedWorkloadHost(host string) error {
	if endpoint.Same(host, config.DefaultHost) || os.Getenv(AllowCustomHostEnv) == "1" {
		return nil
	}
	return fmt.Errorf("refusing to sign this CI job in at %s: CI identity is only exchanged with %s, because a workflow change could otherwise point the job's identity at a server it controls. If you run your own TechBeaver deployment, set %s=1 in the job",
		host, config.DefaultHost, AllowCustomHostEnv)
}
