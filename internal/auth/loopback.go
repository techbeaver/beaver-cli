package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/credential"
)

// ClientID is this CLI's pre-registered public OAuth client. It is an
// identifier, not a secret: see docs/adr/0005-pre-registered-oauth-client.md.
const ClientID = "beaver-cli"

// CallbackPath is the loopback redirect path. Everything but the port must
// match what is registered, per RFC 8252.
const CallbackPath = "/callback"

// ErrStateMismatch means the authorization server returned a state this
// process did not generate. The attempt is abandoned without exchanging.
var ErrStateMismatch = errors.New("the authorization response did not match this login attempt")

// ErrDenied means the customer declined on the consent screen.
var ErrDenied = errors.New("authorization was declined")

// LoginOptions configures a browser login.
type LoginOptions struct {
	Host    string
	Scopes  []string
	OpenURL func(string) error
	Prompt  func(string)
	Timeout time.Duration
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// Login runs authorization code with PKCE over a loopback redirect and returns
// the resulting tokens.
func Login(ctx context.Context, opts LoginOptions) (*credential.Token, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.OpenURL == nil {
		opts.OpenURL = OpenBrowser
	}
	if opts.Prompt == nil {
		opts.Prompt = func(string) {}
	}

	// Loopback explicitly. All interfaces would put the callback on the network.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("opening a local port for the login callback: %w", err)
	}
	defer func() { _ = listener.Close() }()

	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath)

	pkce, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	state, err := NewState()
	if err != nil {
		return nil, err
	}

	results := make(chan callbackResult, 1)
	server := &http.Server{
		Handler:           callbackHandler(state, results),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authURL := authorizeURL(opts.Host, redirectURI, state, pkce.Challenge, opts.Scopes)
	opts.Prompt(authURL)
	if err := opts.OpenURL(authURL); err != nil {
		opts.Prompt("Could not open a browser. Open the link above by hand.")
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	select {
	case <-waitCtx.Done():
		return nil, fmt.Errorf("timed out waiting for the browser to complete the login")
	case res := <-results:
		if res.err != nil {
			return nil, res.err
		}
		return exchange(ctx, opts.Host, res.code, redirectURI, pkce.Verifier)
	}
}

type callbackResult struct {
	code string
	err  error
}

// callbackHandler answers exactly one authorization response. The page it
// serves never contains the authorization code.
func callbackHandler(wantState string, results chan<- callbackResult) http.Handler {
	var done bool
	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		if done {
			http.Error(w, "This login has already completed.", http.StatusGone)
			return
		}
		done = true

		q := r.URL.Query()
		switch {
		case q.Get("state") != wantState:
			writePage(w, "Login failed", "That response did not match this login attempt. Nothing was granted. Run beaver auth login again.")
			results <- callbackResult{err: ErrStateMismatch}
		case q.Get("error") != "":
			writePage(w, "Login declined", "You declined, or the platform refused. Nothing was granted.")
			results <- callbackResult{err: fmt.Errorf("%w: %s", ErrDenied, q.Get("error_description"))}
		case q.Get("code") == "":
			writePage(w, "Login failed", "The response carried no authorization code.")
			results <- callbackResult{err: errors.New("the authorization response carried no code")}
		default:
			writePage(w, "You are signed in", "You can close this tab and go back to your terminal.")
			results <- callbackResult{code: q.Get("code")}
		}
	})
	return mux
}

func writePage(w http.ResponseWriter, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head>
<body style="font-family:system-ui,sans-serif;max-width:34rem;margin:5rem auto;padding:0 1rem">
<h1 style="font-size:1.25rem">%s</h1><p style="color:#555">%s</p></body></html>`, title, title, message)
}

func authorizeURL(host, redirectURI, state, challenge string, scopes []string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {ClientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	return strings.TrimRight(host, "/") + "/api/v1/oauth/authorize?" + q.Encode()
}

func exchange(ctx context.Context, host, code, redirectURI, verifier string) (*credential.Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {ClientID},
		"code_verifier": {verifier},
	}
	return postToken(ctx, host, form)
}

// Refresh exchanges a refresh token for a new access token.
func Refresh(ctx context.Context, host, refreshToken string) (*credential.Token, error) {
	return postToken(ctx, host, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {ClientID},
	})
}

func postToken(ctx context.Context, host string, form url.Values) (*credential.Token, error) {
	endpoint := strings.TrimRight(host, "/") + "/api/v1/oauth/token"
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

	var body tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("the authorization server returned something unreadable")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || body.AccessToken == "" {
		return nil, &client.APIError{
			StatusCode: resp.StatusCode,
			Code:       "token_exchange_failed",
			Message:    "the authorization server refused this login",
		}
	}

	tok := &credential.Token{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		TokenType:    body.TokenType,
		Host:         strings.TrimRight(host, "/"),
	}
	if body.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	if body.Scope != "" {
		tok.Scopes = strings.Fields(body.Scope)
	}
	return tok, nil
}

// OpenBrowser opens a URL in the customer's default browser. The URL is passed
// as an argument to a fixed command, never through a shell.
func OpenBrowser(target string) error {
	if _, err := url.ParseRequestURI(target); err != nil {
		return fmt.Errorf("refusing to open a malformed URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
