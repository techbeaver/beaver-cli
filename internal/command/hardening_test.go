package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/config"
	"github.com/techbeaver/beaver-cli/internal/credential"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
	"golang.org/x/term"
)

// counting is a server that only counts and remembers what reached it.
type counting struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	auth  []string
}

func newCounting(t *testing.T, h http.HandlerFunc) *counting {
	t.Helper()
	c := &counting{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths = append(c.paths, r.Method+" "+r.URL.Path)
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		c.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		writeEnvelope(w, 200, []map[string]any{})
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *counting) hits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.paths)
}

func writeProjectFile(t *testing.T, env *Env, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(env.Loader.WorkDir, config.ProjectFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- HIGH 1: a credential only goes to the host that issued it ----

func TestAMaliciousProjectFileCannotRedirectTheToken(t *testing.T) {
	good := newCounting(t, nil)
	attacker := newCounting(t, nil)
	env, _, errBuf := newTestEnv(t, good.URL)
	writeProjectFile(t, env, fmt.Sprintf("project = \"p1\"\nhost = %q\n", attacker.URL))

	if err := run(t, env, "projects", "list"); err != nil {
		t.Fatalf("the command should still run against the configured host: %v", err)
	}
	if attacker.hits() != 0 {
		t.Fatalf("the attacker named in beaver.toml received %d request(s): %v", attacker.hits(), attacker.paths)
	}
	if good.hits() != 1 || good.auth[0] != "Bearer btk_test" {
		t.Fatalf("the real host was not used: %v %v", good.paths, good.auth)
	}
	if !strings.Contains(errBuf.String(), "ignored") || !strings.Contains(errBuf.String(), attacker.URL) {
		t.Fatalf("the ignored host must be reported: %q", errBuf.String())
	}
}

func TestAMaliciousProjectFileCannotRedirectARefresh(t *testing.T) {
	attacker := newCounting(t, nil)
	good := newCounting(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/oauth/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new", "refresh_token": "r2", "expires_in": 3600})
			return
		}
		writeEnvelope(w, 200, []map[string]any{})
	})
	env, _, _ := newTestEnv(t, good.URL)
	writeProjectFile(t, env, fmt.Sprintf("host = %q\n", attacker.URL))
	saveToken(t, env, &credential.Token{AccessToken: "old", RefreshToken: "r1", Host: good.URL, ExpiresAt: time.Now().Add(-time.Hour)})

	if err := run(t, env, "projects", "list"); err != nil {
		t.Fatal(err)
	}
	if attacker.hits() != 0 {
		t.Fatalf("the refresh token reached the attacker: %v", attacker.paths)
	}
}

func saveToken(t *testing.T, env *Env, tok *credential.Token) {
	t.Helper()
	if err := env.Store.Save("default", tok); err != nil {
		t.Fatal(err)
	}
}

func TestATokenIsNeverSentToAHostItWasNotIssuedFor(t *testing.T) {
	issuer := newCounting(t, nil)
	other := newCounting(t, nil)
	cases := map[string]string{
		"another port":    "http://127.0.0.1:1",
		"another name":    strings.Replace(issuer.URL, "127.0.0.1", "localhost", 1),
		"another server":  other.URL,
		"plain http host": "http://cloudapi.techbeaver.io",
		"the default":     "",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			env, _, _ := newTestEnv(t, target)
			saveToken(t, env, &credential.Token{AccessToken: "secret", Host: issuer.URL})
			err := run(t, env, "projects", "list")
			if err == nil {
				t.Fatal("the command ran against the wrong host")
			}
			if name != "plain http host" && exitcode.From(err) != exitcode.Unauthenticated {
				t.Errorf("exit code %d, want %d", exitcode.From(err), exitcode.Unauthenticated)
			}
			if issuer.hits() != 0 || other.hits() != 0 {
				t.Fatalf("something was sent: %v %v", issuer.paths, other.paths)
			}
		})
	}
}

func TestAHostFlagCannotRedirectAStoredCredential(t *testing.T) {
	issuer := newCounting(t, nil)
	attacker := newCounting(t, nil)
	env, _, _ := newTestEnv(t, issuer.URL)
	err := run(t, env, "--host", attacker.URL, "projects", "list")
	if err == nil || exitcode.From(err) != exitcode.Unauthenticated {
		t.Fatalf("err = %v", err)
	}
	if attacker.hits() != 0 {
		t.Fatal("the credential was sent to the --host target")
	}
}

func TestAnExpiredCredentialIsNotRefreshedAtAnotherHost(t *testing.T) {
	issuer := newCounting(t, nil)
	attacker := newCounting(t, nil)
	env, _, _ := newTestEnv(t, issuer.URL)
	saveToken(t, env, &credential.Token{AccessToken: "old", RefreshToken: "r1", Host: issuer.URL, ExpiresAt: time.Now().Add(-time.Hour)})
	if err := run(t, env, "--host", attacker.URL, "projects", "list"); err == nil {
		t.Fatal("a refresh went to a host the token was not issued for")
	}
	if attacker.hits() != 0 {
		t.Fatalf("the refresh token reached %v", attacker.paths)
	}
}

func TestACredentialWithNoRecordedHostOnlyGoesToTheDefault(t *testing.T) {
	api := newCounting(t, nil)
	env, _, _ := newTestEnv(t, api.URL)
	saveToken(t, env, &credential.Token{AccessToken: "legacy"})
	if err := run(t, env, "projects", "list"); err == nil {
		t.Fatal("a hostless credential was sent to a non-default host")
	}
	if api.hits() != 0 {
		t.Fatal("something was sent")
	}
}

func TestAnUnsafeHostIsRefusedByEveryCommandThatUsesIt(t *testing.T) {
	env, _, _ := newTestEnv(t, "http://cloudapi.techbeaver.io")
	for _, args := range [][]string{{"projects", "list"}, {"auth", "whoami"}} {
		err := run(t, env, args...)
		if err == nil || !strings.Contains(err.Error(), "beaver config set host") || !strings.Contains(err.Error(), "--host") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestConfigSetHostRefusesAnUnsafeHost(t *testing.T) {
	env, _, _ := newTestEnv(t, "")
	for _, h := range []string{"http://api.example.test", "https://u:p@api.example.test", "api.example.test", "https://api.example.test/x"} {
		if err := run(t, env, "config", "set", "host", h); err == nil {
			t.Errorf("config set host %q was accepted", h)
		}
	}
	if err := run(t, env, "config", "set", "host", "HTTPS://API.Example.test/"); err != nil {
		t.Fatal(err)
	}
	f, _ := env.Loader.Load()
	if got := f.Profiles["default"].Host; got != "https://api.example.test" {
		t.Errorf("stored %q", got)
	}
}

func TestAnInvalidIdentifierInProjectFileStopsTheCommand(t *testing.T) {
	api := newCounting(t, nil)
	env, _, _ := newTestEnv(t, api.URL)
	writeProjectFile(t, env, "project = \"p1\"\napp = \"../../admin\"\n")
	if err := run(t, env, "apps", "describe"); err == nil {
		t.Fatal("a traversal id from beaver.toml was used")
	}
	if api.hits() != 0 {
		t.Fatalf("a request was sent: %v", api.paths)
	}
}

// ---- Medium: unescaped path segments ----

func TestEveryCommandEscapesWhatItPutsInAPath(t *testing.T) {
	const nasty = "x/../y?z=1#f"
	api := newCounting(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 200, map[string]any{"id": "x"})
	})
	var raws []string
	api.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		raws = append(raws, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		api.mu.Unlock()
		writeEnvelope(w, 200, map[string]any{"id": "x"})
	})
	env, _, _ := newTestEnv(t, api.URL)

	commands := [][]string{
		{"apps", "describe", nasty}, {"apps", "start", nasty}, {"apps", "stop", nasty}, {"apps", "rollback", nasty},
		{"apps", "instances", nasty}, {"apps", "env", "list", nasty}, {"apps", "scale", nasty, "--replicas", "1", "--quote"},
		{"apps", "set-port", nasty, "--port", "80"}, {"apps", "restart", nasty}, {"apps", "deploy", nasty},
		{"apps", "domains", "list", nasty}, {"apps", "domains", "verify", nasty, "--app", "app-1"}, {"apps", "list", "--project", nasty},
		{"projects", "describe", nasty},
		{"db", "describe", nasty}, {"db", "usage", nasty}, {"db", "stats", nasty}, {"db", "roles", "list", nasty},
		{"db", "roles", "create", nasty, "r"}, {"db", "databases", "list", nasty}, {"db", "extensions", "list", nasty, nasty},
		{"db", "extensions", "disable", nasty, nasty, nasty, "--yes"}, {"db", "backups", "list", nasty},
		{"db", "connection-string", nasty, nasty}, {"db", "allowlist", "list", nasty}, {"db", "list", "--project", nasty},
		{"billing", "invoices", "describe", nasty}, {"billing", "invoices", "pay", nasty},
	}
	for _, args := range commands {
		before := len(raws)
		_ = run(t, env, args...)
		api.mu.Lock()
		got := append([]string(nil), raws[before:]...)
		api.mu.Unlock()
		if len(got) == 0 {
			t.Errorf("%v sent nothing, so its path was not checked", args)
		}
		for _, p := range got {
			path := strings.SplitN(p, "?", 2)[0]
			if strings.Contains(path, "/../") || strings.Contains(path, "#") {
				t.Errorf("%v produced an unescaped path %q", args, p)
			}
			if !strings.Contains(path, url.PathEscape(nasty)) {
				t.Errorf("%v: %q does not carry the escaped id", args, p)
			}
			if q := strings.SplitN(p, "?", 2)[1]; strings.Contains(q, "z=1") {
				t.Errorf("%v smuggled a query string: %q", args, p)
			}
		}
	}
}

func TestDotSegmentIdentifiersNeverReachTheNetwork(t *testing.T) {
	api := newCounting(t, nil)
	env, _, _ := newTestEnv(t, api.URL)
	for _, id := range []string{"..", "."} {
		for _, args := range [][]string{
			{"apps", "describe", id}, {"db", "describe", id}, {"projects", "describe", id}, {"billing", "invoices", "describe", id},
		} {
			if err := run(t, env, args...); err == nil {
				t.Errorf("%v with %q was sent", args, id)
			}
		}
	}
	if api.hits() != 0 {
		t.Fatalf("requests escaped the guard: %v", api.paths)
	}
}

// ---- Medium: token prompt, logout ----

func TestTheTokenPromptDoesNotEchoOnATerminal(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, map[string]any{"scopes": []any{"paas:read"}})
	})
	env, _, errBuf := newTestEnv(t, api.URL)
	_ = env.Store.Delete("default")
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	env.In = pr

	calledOn := -1
	oldIs, oldRead := isTerminal, readPassword
	isTerminal = func(fd int) bool { return fd == int(pr.Fd()) }
	readPassword = func(fd int) ([]byte, error) { calledOn = fd; return []byte("  btk_typed\n"), nil }
	t.Cleanup(func() { isTerminal, readPassword = oldIs, oldRead })

	if err := run(t, env, "auth", "login", "--token"); err != nil {
		t.Fatal(err)
	}
	if calledOn != int(pr.Fd()) {
		t.Fatal("the no-echo reader was not used on a terminal")
	}
	tok, err := env.Store.Load("default")
	if err != nil || tok.AccessToken != "btk_typed" || tok.Host != api.URL {
		t.Fatalf("stored %+v %v", tok, err)
	}
	if strings.Contains(errBuf.String(), "btk_typed") {
		t.Fatal("the token was printed")
	}
}

func TestTheTokenIsReadFromStdinWhenNotATerminal(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) { writeEnvelope(w, 200, map[string]any{}) })
	env, _, _ := newTestEnv(t, api.URL)
	_ = env.Store.Delete("default")
	env.In = strings.NewReader("btk_piped\nsecond line ignored\n")
	if err := run(t, env, "auth", "login", "--token"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := env.Store.Load("default"); tok == nil || tok.AccessToken != "btk_piped" {
		t.Fatalf("stored %+v", tok)
	}
}

func TestAnEmptyOrFailedTokenReadStoresNothing(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) { writeEnvelope(w, 200, map[string]any{}) })
	for name, in := range map[string]string{"empty": "", "blank": "  \n", "bad token": "nope\n"} {
		env, _, _ := newTestEnv(t, api.URL)
		_ = env.Store.Delete("default")
		env.In = strings.NewReader(in)
		if name == "bad token" {
			bad := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
				writeEnvelope(w, 401, map[string]any{"code": "invalid_token"})
			})
			env.Host = bad.URL
		}
		if err := run(t, env, "auth", "login", "--token"); err == nil {
			t.Errorf("%s: login succeeded", name)
		}
		if _, err := env.Store.Load("default"); err != credential.ErrNotFound {
			t.Errorf("%s: a credential was stored (%v)", name, err)
		}
	}
}

func TestLogoutRevokesBothTokensThenDeletesLocally(t *testing.T) {
	var mu sync.Mutex
	var forms []url.Values
	srv := newCounting(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
	})
	env, _, errBuf := newTestEnv(t, srv.URL)
	saveToken(t, env, &credential.Token{AccessToken: "acc", RefreshToken: "ref", Host: srv.URL})
	if err := run(t, env, "auth", "logout"); err != nil {
		t.Fatal(err)
	}
	if len(forms) != 2 {
		t.Fatalf("%d revocation calls: %v", len(forms), srv.paths)
	}
	got := map[string]string{}
	for _, f := range forms {
		got[f.Get("token_type_hint")] = f.Get("token")
	}
	if got["refresh_token"] != "ref" || got["access_token"] != "acc" {
		t.Errorf("revoked %v", got)
	}
	if forms[0].Get("token_type_hint") != "refresh_token" {
		t.Error("the refresh token should be revoked first")
	}
	if _, err := env.Store.Load("default"); err != credential.ErrNotFound {
		t.Fatal("the credential is still stored")
	}
	if strings.Contains(errBuf.String(), "Warning") {
		t.Errorf("a clean logout warned: %q", errBuf.String())
	}
}

func TestLogoutDeletesLocallyWhateverTheNetworkDid(t *testing.T) {
	cases := map[string]func(t *testing.T) (host string){
		"server error": func(t *testing.T) string {
			return newCounting(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }).URL
		},
		"unreachable": func(t *testing.T) string {
			s := httptest.NewServer(http.NotFoundHandler())
			u := s.URL
			s.Close()
			return u
		},
		"malformed": func(t *testing.T) string {
			return newCounting(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500); _, _ = w.Write([]byte("<html>")) }).URL
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			host := mk(t)
			env, _, errBuf := newTestEnv(t, host)
			saveToken(t, env, &credential.Token{AccessToken: "acc", RefreshToken: "ref", Host: host})
			if err := run(t, env, "auth", "logout"); err != nil {
				t.Fatalf("logout failed: %v", err)
			}
			if _, err := env.Store.Load("default"); err != credential.ErrNotFound {
				t.Fatal("the local credential survived a failed revocation")
			}
			if !strings.Contains(errBuf.String(), "Warning") || !strings.Contains(errBuf.String(), "still be valid") {
				t.Fatalf("the user was not told revocation failed: %q", errBuf.String())
			}
			if strings.Contains(errBuf.String(), "acc") && strings.Contains(errBuf.String(), "ref\"") {
				t.Error("a token was printed")
			}
		})
	}
}

func TestLogoutAttemptsTheAccessTokenEvenIfTheRefreshRevocationFails(t *testing.T) {
	srv := newCounting(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("token_type_hint") == "refresh_token" {
			w.WriteHeader(500)
		}
	})
	env, _, errBuf := newTestEnv(t, srv.URL)
	saveToken(t, env, &credential.Token{AccessToken: "acc", RefreshToken: "ref", Host: srv.URL})
	_ = run(t, env, "auth", "logout")
	if srv.hits() != 2 {
		t.Fatalf("%d calls, want 2", srv.hits())
	}
	if !strings.Contains(errBuf.String(), "refresh token") {
		t.Errorf("the warning should say which half failed: %q", errBuf.String())
	}
}

func TestLogoutRevokesAtTheIssuerNotAtWhateverTheProfilePointsAtNow(t *testing.T) {
	issuer := newCounting(t, nil)
	other := newCounting(t, nil)
	env, _, errBuf := newTestEnv(t, issuer.URL)
	saveToken(t, env, &credential.Token{AccessToken: "acc", RefreshToken: "ref", Host: issuer.URL})
	if err := run(t, env, "--host", other.URL, "auth", "logout"); err != nil {
		t.Fatal(err)
	}
	if other.hits() != 0 {
		t.Fatalf("a token was sent to the profile host: %v", other.paths)
	}
	if issuer.hits() != 2 {
		t.Fatalf("the issuer saw %d revocations, want 2", issuer.hits())
	}
	if strings.Contains(errBuf.String(), "Warning") {
		t.Errorf("stderr %q", errBuf.String())
	}
	if _, err := env.Store.Load("default"); err != credential.ErrNotFound {
		t.Error("the local copy should be removed")
	}
}

func TestLogoutWithAnUnusableRecordedHostDeletesLocallyAndWarns(t *testing.T) {
	srv := newCounting(t, nil)
	env, _, errBuf := newTestEnv(t, srv.URL)
	saveToken(t, env, &credential.Token{AccessToken: "acc", RefreshToken: "ref", Host: "http://evil.example.test"})
	if err := run(t, env, "auth", "logout"); err != nil {
		t.Fatal(err)
	}
	if srv.hits() != 0 || !strings.Contains(errBuf.String(), "Warning") {
		t.Fatalf("hits %d stderr %q", srv.hits(), errBuf.String())
	}
	if _, err := env.Store.Load("default"); err != credential.ErrNotFound {
		t.Error("the local copy should be removed")
	}
}

func TestLogoutHelpSaysRevocationEndsAPastedTokenEverywhere(t *testing.T) {
	env, out, _ := newTestEnv(t, "")
	if err := run(t, env, "auth", "logout", "--help"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "everywhere it is") || !strings.Contains(out.String(), "--token") {
		t.Fatalf("%s", out.String())
	}
}

func TestLogoutWithNothingStoredIsQuietAndSendsNothing(t *testing.T) {
	srv := newCounting(t, nil)
	env, _, errBuf := newTestEnv(t, srv.URL)
	_ = env.Store.Delete("default")
	if err := run(t, env, "auth", "logout"); err != nil {
		t.Fatal(err)
	}
	if srv.hits() != 0 || strings.Contains(errBuf.String(), "Warning") {
		t.Fatalf("hits %d, stderr %q", srv.hits(), errBuf.String())
	}
}

func TestLogoutStillDeletesAnUnreadableCredential(t *testing.T) {
	srv := newCounting(t, nil)
	env, _, errBuf := newTestEnv(t, srv.URL)
	path := filepath.Join(env.Loader.Dir, "credentials-default.json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(t, env, "auth", "logout"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the unreadable credential file was left behind")
	}
	if !strings.Contains(errBuf.String(), "Warning") {
		t.Errorf("stderr %q", errBuf.String())
	}
}

// ---- Medium: refresh, rotation, locking ----

type tokenEndpoint struct {
	*httptest.Server
	mu      sync.Mutex
	current string
	calls   int
	mode    string
}

func newTokenEndpoint(t *testing.T) *tokenEndpoint {
	t.Helper()
	te := &tokenEndpoint{current: "r1"}
	te.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/oauth/token" {
			writeEnvelope(w, 200, []map[string]any{})
			return
		}
		_ = r.ParseForm()
		te.mu.Lock()
		defer te.mu.Unlock()
		te.calls++
		time.Sleep(5 * time.Millisecond)
		switch te.mode {
		case "500":
			w.WriteHeader(500)
			return
		case "malformed":
			_, _ = w.Write([]byte("<html>"))
			return
		case "empty":
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		if r.PostForm.Get("refresh_token") != te.current {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		resp := map[string]any{"access_token": fmt.Sprintf("acc-%d", te.calls), "expires_in": 3600, "scope": "paas:read"}
		if te.mode != "omit-refresh" {
			te.current = fmt.Sprintf("r%d", te.calls+1)
			resp["refresh_token"] = te.current
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(te.Close)
	return te
}

func expiredFor(host string) *credential.Token {
	return &credential.Token{AccessToken: "old", RefreshToken: "r1", Host: host, Scopes: []string{"paas:read", "paas:deploy"}, ExpiresAt: time.Now().Add(-time.Hour)}
}

func TestConcurrentProcessesRefreshOnceAndNobodyIsLoggedOut(t *testing.T) {
	te := newTokenEndpoint(t)
	base, _, _ := newTestEnv(t, te.URL)
	saveToken(t, base, expiredFor(te.URL))

	const n = 10
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out, errBuf bytes.Buffer
			env := &Env{Out: &out, Err: &errBuf, In: strings.NewReader(""), Store: &credential.FileStore{Dir: base.Loader.Dir},
				Loader: &config.Loader{Dir: base.Loader.Dir, WorkDir: base.Loader.Dir}, Host: te.URL}
			s, err := env.Authenticate(t.Context())
			errs[i] = err
			if s != nil {
				results[i] = s.Token
			}
		}()
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("process %d: %v", i, errs[i])
		}
		if results[i] != "acc-1" {
			t.Errorf("process %d used %q, want the single refreshed token", i, results[i])
		}
	}
	if te.calls != 1 {
		t.Fatalf("%d refresh calls, want exactly 1 (the refresh token rotates, so a second would sign someone out)", te.calls)
	}
	stored, _ := base.Store.Load("default")
	if stored.RefreshToken != "r2" || stored.AccessToken != "acc-1" {
		t.Fatalf("stored %+v", stored)
	}
}

func TestAResponseWithoutARefreshTokenKeepsTheStoredOne(t *testing.T) {
	te := newTokenEndpoint(t)
	te.mode = "omit-refresh"
	env, _, _ := newTestEnv(t, te.URL)
	saveToken(t, env, expiredFor(te.URL))
	s, err := env.Authenticate(t.Context())
	if err != nil || s.Token != "acc-1" {
		t.Fatalf("%v %v", s, err)
	}
	stored, _ := env.Store.Load("default")
	if stored.RefreshToken != "r1" {
		t.Fatalf("the refresh token was dropped: %q", stored.RefreshToken)
	}
	if len(stored.Scopes) == 0 {
		t.Error("scopes were dropped")
	}
}

func TestRefreshFailuresLeaveTheStoredCredentialAndAskForALogin(t *testing.T) {
	for _, mode := range []string{"500", "malformed", "empty"} {
		te := newTokenEndpoint(t)
		te.mode = mode
		env, _, _ := newTestEnv(t, te.URL)
		saveToken(t, env, expiredFor(te.URL))
		_, err := env.Authenticate(t.Context())
		if err == nil || exitcode.From(err) != exitcode.Unauthenticated || !strings.Contains(err.Error(), "beaver auth login") {
			t.Errorf("%s: %v", mode, err)
		}
		stored, _ := env.Store.Load("default")
		if stored.RefreshToken != "r1" || stored.AccessToken != "old" {
			t.Errorf("%s: a failed refresh changed the stored credential: %+v", mode, stored)
		}
	}
}

func TestAnotherProcessThatAlreadyRefreshedIsNotRefreshedAgain(t *testing.T) {
	te := newTokenEndpoint(t)
	env, _, _ := newTestEnv(t, te.URL)
	resolved := &config.Resolved{Profile: "default", Host: te.URL}
	fresh := &credential.Token{AccessToken: "from-the-other-process", RefreshToken: "r9", Host: te.URL, ExpiresAt: time.Now().Add(time.Hour)}
	saveToken(t, env, fresh)

	got, err := env.refresh(t.Context(), resolved, expiredFor(te.URL))
	if err != nil || got.AccessToken != "from-the-other-process" {
		t.Fatalf("%v %v", got, err)
	}
	if te.calls != 0 {
		t.Fatalf("%d refresh calls for a credential that was already fresh", te.calls)
	}
}

func TestRefreshSpendsTheNewestRefreshTokenNotTheStaleOne(t *testing.T) {
	te := newTokenEndpoint(t)
	te.current = "r5"
	env, _, _ := newTestEnv(t, te.URL)
	stale := expiredFor(te.URL)
	newer := expiredFor(te.URL)
	newer.RefreshToken = "r5"
	saveToken(t, env, newer)
	got, err := env.refresh(t.Context(), &config.Resolved{Profile: "default", Host: te.URL}, stale)
	if err != nil || got.AccessToken != "acc-1" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestTheCredentialFileStaysPrivateAfterARefresh(t *testing.T) {
	te := newTokenEndpoint(t)
	env, _, _ := newTestEnv(t, te.URL)
	saveToken(t, env, expiredFor(te.URL))
	if _, err := env.Authenticate(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(env.Loader.Dir, "credentials-default.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info, err)
	}
	left, _ := filepath.Glob(filepath.Join(env.Loader.Dir, ".credentials-*"))
	for _, f := range left {
		if !strings.HasSuffix(f, ".lock") {
			t.Errorf("a temp file was left behind: %s", f)
		}
	}
}

// ---- Medium: confirmations ----

func TestEveryConfirmationStatusHasAClearOutcome(t *testing.T) {
	pending := &pendingConfirmation{Token: "from-create"}
	type want struct {
		done bool
		tok  string
		err  string
	}
	cases := map[string]struct {
		status, token string
		want          want
	}{
		"approved":                  {"approved", "ct", want{true, "ct", ""}},
		"approved, token at create": {"approved", "", want{true, "from-create", ""}},
		"pending":                   {"pending", "", want{false, "", ""}},
		"rejected":                  {"rejected", "", want{false, "", "rejected"}},
		"declined":                  {"declined", "", want{false, "", "rejected"}},
		"consumed":                  {"consumed", "", want{false, "", "already used"}},
		"expired":                   {"expired", "", want{false, "", "expired"}},
		"unknown":                   {"cancelled", "", want{false, "", "does not understand"}},
		"empty":                     {"", "", want{false, "", "does not understand"}},
		"wrong case":                {"APPROVED", "ct", want{false, "", "does not understand"}},
	}
	for name, c := range cases {
		done, tok, err := interpretStatus(c.status, c.token, pending)
		if done != c.want.done || tok != c.want.tok {
			t.Errorf("%s: done=%v tok=%q", name, done, tok)
		}
		if (err == nil) != (c.want.err == "") || (err != nil && !strings.Contains(err.Error(), c.want.err)) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want.err)
		}
	}
	if _, _, err := interpretStatus("approved", "", &pendingConfirmation{}); err == nil {
		t.Error("an approval with no token anywhere was accepted")
	}
}

func TestNoNonApprovedStatusEverSendsTheDestructiveCall(t *testing.T) {
	for _, status := range []string{"rejected", "consumed", "expired", "declined", "mystery"} {
		t.Run(status, func(t *testing.T) {
			api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					writeEnvelope(w, 201, map[string]any{"id": "c1", "status": "pending", "approvalUrl": "https://x/a"})
				case http.MethodGet:
					writeEnvelope(w, 200, map[string]any{"status": status, "confirmationToken": "ct-should-not-matter"})
				default:
					writeEnvelope(w, 200, map[string]any{})
				}
			})
			env, _, _ := newTestEnv(t, api.URL)
			err := run(t, env, "apps", "delete", "app-1", "--yes", "--wait", "30s")
			if err == nil {
				t.Fatal("a non-approved confirmation reported success")
			}
			if exitcode.From(err) == exitcode.OK {
				t.Fatal("exit 0")
			}
			if api.calls(http.MethodDelete, "/paas/apps/app-1") != 0 {
				t.Fatal("the delete was sent without an approval")
			}
		})
	}
}

func TestTheReplayedCallCarriesTheExactBytesTheConfirmationDeclared(t *testing.T) {
	var confirmed json.RawMessage
	var replayed []byte
	api := newFakeAPI(t, func(a *fakeAPI, w http.ResponseWriter, r *http.Request) {
		rec := a.requests[len(a.requests)-1]
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mcp/confirmations"):
			var reg struct {
				Body json.RawMessage `json:"body"`
			}
			_ = json.Unmarshal(rec.raw, &reg)
			confirmed = reg.Body
			writeEnvelope(w, 201, map[string]any{"id": "c1", "status": "pending", "approvalUrl": "https://x/a"})
		case r.Method == http.MethodGet:
			writeEnvelope(w, 200, map[string]any{"status": "approved", "confirmationToken": "ct"})
		default:
			replayed = rec.raw
			writeEnvelope(w, 200, map[string]any{})
		}
	})
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "db", "backups", "restore", "db-1", "--backup", "b <1> & é", "--yes", "--wait", "30s"); err != nil {
		t.Fatal(err)
	}
	if len(confirmed) == 0 || !bytes.Equal(confirmed, replayed) {
		t.Fatalf("confirmation declared %q, replay sent %q", confirmed, replayed)
	}
}

func TestACallWithNoBodyDeclaresNoneAndReplaysNone(t *testing.T) {
	var declared bool
	replayBody := -1
	api := newFakeAPI(t, func(a *fakeAPI, w http.ResponseWriter, r *http.Request) {
		rec := a.requests[len(a.requests)-1]
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mcp/confirmations"):
			declared = bytes.Contains(rec.raw, []byte(`"body"`))
			writeEnvelope(w, 201, map[string]any{"id": "c1", "status": "pending", "approvalUrl": "https://x/a"})
		case r.Method == http.MethodGet:
			writeEnvelope(w, 200, map[string]any{"status": "approved", "confirmationToken": "ct"})
		default:
			replayBody = len(rec.raw)
			writeEnvelope(w, 200, map[string]any{})
		}
	})
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "db", "roles", "reset-password", "db-1", "role-1", "--yes", "--wait", "30s"); err != nil {
		t.Fatal(err)
	}
	if declared || replayBody != 0 {
		t.Fatalf("declared a body: %v, replayed %d bytes", declared, replayBody)
	}
	if api.calls(http.MethodPost, "/dbaas/instances/db-1/roles/role-1/password") != 1 {
		t.Fatal("the password rotation was not replayed with its confirmation")
	}
}

func TestRotatingARolePasswordNeedsAnApproval(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mcp/confirmations") {
			writeEnvelope(w, 201, map[string]any{"id": "c1", "status": "pending", "approvalUrl": "https://x/a"})
			return
		}
		writeEnvelope(w, 200, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)
	err := run(t, env, "db", "roles", "reset-password", "db-1", "role-1", "--yes")
	if exitcode.From(err) != exitcode.AwaitingHuman {
		t.Fatalf("exit %d: %v", exitcode.From(err), err)
	}
	if api.calls(http.MethodPost, "/dbaas/instances/db-1/roles/role-1/password") != 0 {
		t.Fatal("the rotation was sent without an approval")
	}
}

// ---- Medium: secrets ----

func envAPI(t *testing.T, scopeOK bool) *fakeAPI {
	return newFakeAPI(t, func(a *fakeAPI, w http.ResponseWriter, r *http.Request) {
		rec := a.requests[len(a.requests)-1]
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/env"):
			if strings.Contains(rec.query, "reveal=true") && !scopeOK {
				writeEnvelope(w, 403, map[string]any{"code": "secrets_scope_required"})
				return
			}
			writeEnvelope(w, 200, []map[string]any{{"key": "DB", "value": "hunter2"}, {"key": "LOG", "value": "info"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/env/check"):
			writeEnvelope(w, 200, map[string]any{"set": true, "matches": rec.body["value"] == "hunter2"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/connection-info"):
			writeEnvelope(w, 200, map[string]any{"host": "h", "password": "********"})
		default:
			writeEnvelope(w, 200, map[string]any{})
		}
	})
}

func TestEnvListMasksValuesEvenIfTheAPISendsThem(t *testing.T) {
	api := envAPI(t, true)
	env, out, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "list", "app-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hunter2") || !strings.Contains(out.String(), "DB") || !strings.Contains(out.String(), "********") {
		t.Fatalf("%s", out.String())
	}
	for _, r := range api.requests {
		if strings.Contains(r.query, "reveal") {
			t.Fatalf("a plain list asked for reveal: %q", r.query)
		}
	}
}

func TestEnvListRevealAsksForRevealAndPrintsValues(t *testing.T) {
	for _, flag := range []string{"--reveal", "--show-values"} {
		api := envAPI(t, true)
		env, out, _ := newTestEnv(t, api.URL)
		if err := run(t, env, "apps", "env", "list", "app-1", flag); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "hunter2") {
			t.Errorf("%s: %s", flag, out.String())
		}
		if api.requests[len(api.requests)-1].query != "reveal=true" {
			t.Errorf("%s: query %q", flag, api.requests[len(api.requests)-1].query)
		}
	}
}

func TestRevealWithoutTheScopeTellsTheUserHowToFixIt(t *testing.T) {
	api := envAPI(t, false)
	env, out, _ := newTestEnv(t, api.URL)
	err := run(t, env, "apps", "env", "list", "app-1", "--reveal")
	if err == nil {
		t.Fatal("reveal succeeded without the scope")
	}
	for _, want := range []string{"secrets:read", "beaver auth login --scope", "paas:read"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should contain %q: %v", want, err)
		}
	}
	if exitcode.From(err) != exitcode.Unauthenticated {
		t.Errorf("exit %d", exitcode.From(err))
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Error("a value leaked")
	}
	conn := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 403, map[string]any{"code": "secrets_scope_required"})
	})
	env2, _, _ := newTestEnv(t, conn.URL)
	if err := run(t, env2, "db", "connection-string", "db-1", "d-1", "--reveal"); err == nil || !strings.Contains(err.Error(), "beaver auth login --scope") {
		t.Errorf("connection-string --reveal: %v", err)
	}
}

func TestTheSuggestedLoginScopesIncludeSecretsReadButNoDestroyScope(t *testing.T) {
	err := explainSecretsScope(&clientAPIError{})
	_ = err
	got := explainSecretsScope(apiError(403, SecretsScopeRequired)).Error()
	if !strings.Contains(got, "secrets:read") || strings.Contains(got, "destroy") {
		t.Fatalf("%s", got)
	}
}

func TestConnectionStringIsMaskedUnlessRevealed(t *testing.T) {
	api := envAPI(t, true)
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "db", "connection-string", "db-1", "d-1"); err != nil {
		t.Fatal(err)
	}
	if q := api.requests[len(api.requests)-1].query; q != "" {
		t.Fatalf("query %q", q)
	}
	if err := run(t, env, "db", "connection-string", "db-1", "d-1", "--reveal"); err != nil {
		t.Fatal(err)
	}
	if q := api.requests[len(api.requests)-1].query; q != "reveal=true" {
		t.Fatalf("query %q", q)
	}
}

func TestSettingVariablesNeedsNoSecretsScope(t *testing.T) {
	api := envAPI(t, false)
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "set", "--app", "app-1", "NEW=1"); err != nil {
		t.Fatal(err)
	}
	if api.calls(http.MethodPost, "/paas/apps/app-1/env/upsert") != 1 {
		t.Fatal("no upsert was sent")
	}
	if api.calls(http.MethodPatch, "/paas/apps/app-1/env")+api.calls(http.MethodPut, "/paas/apps/app-1/env") != 0 {
		t.Fatal("a whole-set write was sent")
	}
	for _, r := range api.requests {
		if strings.Contains(r.query, "reveal") {
			t.Fatalf("reveal sent: %s", r.query)
		}
	}
}

func TestEnvCheckReadsTheValueFromStdinAndNeverShowsIt(t *testing.T) {
	api := envAPI(t, true)
	env, out, errBuf := newTestEnv(t, api.URL)
	env.In = strings.NewReader("hunter2\n")
	if err := run(t, env, "apps", "env", "check", "DB", "app-1"); err != nil {
		t.Fatal(err)
	}
	last := api.requests[len(api.requests)-1]
	if last.path != "/paas/apps/app-1/env/check" || last.body["name"] != "DB" || last.body["value"] != "hunter2" || last.body["kind"] != "runtime" {
		t.Fatalf("%+v", last)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res["matches"] != true || res["set"] != true {
		t.Fatalf("%s %v", out.String(), err)
	}
	if strings.Contains(out.String()+errBuf.String(), "hunter2") {
		t.Fatal("the checked value was echoed")
	}
}

func TestEnvCheckKindsAndMismatch(t *testing.T) {
	api := envAPI(t, true)
	env, out, _ := newTestEnv(t, api.URL)
	env.In = strings.NewReader("wrong")
	if err := run(t, env, "apps", "env", "check", "DB", "app-1", "--build"); err != nil {
		t.Fatal(err)
	}
	last := api.requests[len(api.requests)-1]
	if last.body["kind"] != "build" || last.body["value"] != "wrong" {
		t.Fatalf("%+v", last.body)
	}
	if !strings.Contains(out.String(), `"matches": false`) && !strings.Contains(out.String(), `"matches":false`) {
		t.Fatalf("%s", out.String())
	}
}

func TestEnvCheckRefusesAValueOnTheCommandLine(t *testing.T) {
	api := envAPI(t, true)
	env, _, _ := newTestEnv(t, api.URL)
	env.In = strings.NewReader("hunter2\n")
	for _, args := range [][]string{
		{"apps", "env", "check", "DB=hunter2", "app-1"},
		{"apps", "env", "check", "DB", "app-1", "hunter2"},
		{"apps", "env", "check"},
		{"apps", "env", "check", "DB", "--value", "hunter2"},
	} {
		if err := run(t, env, args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	for _, r := range api.requests {
		if strings.HasSuffix(r.path, "/env/check") {
			t.Fatalf("a check was sent: %+v", r)
		}
	}
}

func TestEnvCheckWithNoInputOrTooMuchFailsWithoutSending(t *testing.T) {
	api := envAPI(t, true)
	for name, in := range map[string]string{"empty": "", "newline only": "\n"} {
		env, _, _ := newTestEnv(t, api.URL)
		env.In = strings.NewReader(in)
		if err := run(t, env, "apps", "env", "check", "DB", "app-1"); exitcode.From(err) != exitcode.Usage {
			t.Errorf("%s: %v", name, err)
		}
	}
	env, _, _ := newTestEnv(t, api.URL)
	env.In = strings.NewReader(strings.Repeat("a", maxSecretBytes+10))
	if err := run(t, env, "apps", "env", "check", "DB", "app-1"); err == nil {
		t.Error("an oversized value was accepted")
	}
	for _, r := range api.requests {
		if strings.HasSuffix(r.path, "/env/check") {
			t.Fatal("a check was sent")
		}
	}
}

func TestEnvCheckPromptsWithoutEchoOnATerminalAndKeepsMultilineValues(t *testing.T) {
	api := envAPI(t, true)
	env, _, errBuf := newTestEnv(t, api.URL)
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	env.In = pr
	oldIs, oldRead := isTerminal, readPassword
	isTerminal = func(int) bool { return true }
	readPassword = func(int) ([]byte, error) { return []byte("line1\nline2"), nil }
	t.Cleanup(func() { isTerminal, readPassword = oldIs, oldRead })
	if err := run(t, env, "apps", "env", "check", "KEY", "app-1"); err != nil {
		t.Fatal(err)
	}
	if got := api.requests[len(api.requests)-1].body["value"]; got != "line1\nline2" {
		t.Fatalf("value %q", got)
	}
	if !strings.Contains(errBuf.String(), "not echoed") || strings.Contains(errBuf.String(), "line1") {
		t.Fatalf("prompt %q", errBuf.String())
	}

	env.In = strings.NewReader("a\nb\n")
	if err := run(t, env, "apps", "env", "check", "KEY", "app-1"); err != nil {
		t.Fatal(err)
	}
	if got := api.requests[len(api.requests)-1].body["value"]; got != "a\nb" {
		t.Fatalf("a multi-line value was cut: %q", got)
	}
}

func TestEnvCheckSurfacesAPIFailures(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500} {
		api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
			writeEnvelope(w, status, map[string]any{"code": "x"})
		})
		env, out, _ := newTestEnv(t, api.URL)
		env.In = strings.NewReader("v")
		if err := run(t, env, "apps", "env", "check", "K", "app-1"); err == nil {
			t.Errorf("status %d reported as success", status)
		}
		if strings.Contains(out.String(), "matches") {
			t.Errorf("status %d printed a result", status)
		}
	}
}

func apiError(status int, code string) error {
	return &client.APIError{StatusCode: status, Code: code, Message: "m"}
}

type clientAPIError = client.APIError

func TestARefreshNeverSpendsACredentialReloadedForAnotherHost(t *testing.T) {
	te := newTokenEndpoint(t)
	env, _, _ := newTestEnv(t, te.URL)
	saveToken(t, env, &credential.Token{AccessToken: "x", RefreshToken: "r1", Host: "https://other.example.test", ExpiresAt: time.Now().Add(-time.Hour)})
	_, err := env.refresh(t.Context(), &config.Resolved{Profile: "default", Host: te.URL}, expiredFor(te.URL))
	if err == nil || exitcode.From(err) != exitcode.Unauthenticated {
		t.Fatalf("err = %v", err)
	}
	if te.calls != 0 {
		t.Fatalf("%d refresh call(s) were made", te.calls)
	}
}

// ---- repairing a bad host ----

func writeProfileHost(t *testing.T, env *Env, host string) {
	t.Helper()
	body := fmt.Sprintf("[profiles.default]\nhost = %q\n", host)
	if err := os.WriteFile(filepath.Join(env.Loader.Dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestABadStoredHostCanBeRepairedByConfigAndLogout(t *testing.T) {
	good := newCounting(t, nil)
	env, out, errBuf := newTestEnv(t, "")
	writeProfileHost(t, env, "http://plain.example.test")
	saveToken(t, env, &credential.Token{AccessToken: "acc", RefreshToken: "ref", Host: good.URL})

	if err := run(t, env, "projects", "list"); err == nil || !strings.Contains(err.Error(), "beaver config set host https://") || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("an ordinary command should explain the fix: %v", err)
	}
	if err := run(t, env, "config", "list"); err != nil || !strings.Contains(out.String(), "host_error") {
		t.Fatalf("config list: %v %s", err, out.String())
	}
	out.Reset()
	if err := run(t, env, "config", "get", "host"); err != nil || !strings.Contains(out.String(), "plain.example.test") {
		t.Fatalf("config get: %v %s", err, out.String())
	}
	if err := run(t, env, "config", "set", "host", "https://api.example.test"); err != nil {
		t.Fatalf("config set could not repair the host: %v", err)
	}
	if err := run(t, env, "config", "get", "host"); err != nil {
		t.Fatalf("after repair: %v", err)
	}
	if good.hits() != 0 {
		t.Fatalf("something was sent while the host was bad: %v", good.paths)
	}

	writeProfileHost(t, env, "http://plain.example.test")
	if err := run(t, env, "auth", "logout"); err != nil {
		t.Fatalf("logout is locked out by a bad host: %v", err)
	}
	if _, err := env.Store.Load("default"); err != credential.ErrNotFound {
		t.Error("logout did not delete locally")
	}
	if good.hits() != 2 {
		t.Errorf("revocation should still reach the issuer: %v (stderr %q)", good.paths, errBuf.String())
	}
}

func TestABadHostNeverReceivesATokenFromAnyCommand(t *testing.T) {
	bad := newCounting(t, nil)
	for _, raw := range []string{"http://" + strings.TrimPrefix(bad.URL, "http://") + ".evil.test", "ftp://x", "https://u:p@x.test"} {
		env, _, _ := newTestEnv(t, "")
		writeProfileHost(t, env, raw)
		for _, args := range [][]string{{"projects", "list"}, {"auth", "whoami"}, {"auth", "print-access-token"}, {"mcp"}} {
			if err := run(t, env, args...); err == nil {
				t.Errorf("%q %v ran", raw, args)
			}
		}
	}
	if bad.hits() != 0 {
		t.Fatal("a bad host received a request")
	}
}

// ---- Ctrl-C at the prompt ----

func TestCancellingAtTheSecretPromptReturnsAndRestoresTheTerminal(t *testing.T) {
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	env := &Env{In: pr, Err: &bytes.Buffer{}}

	release := make(chan struct{})
	defer close(release)
	var restored bool
	oldIs, oldRead, oldGet, oldRestore := isTerminal, readPassword, getState, restoreState
	isTerminal = func(int) bool { return true }
	readPassword = func(int) ([]byte, error) { <-release; return nil, nil }
	getState = func(int) (*term.State, error) { return new(term.State), nil }
	restoreState = func(int, *term.State) error { restored = true; return nil }
	t.Cleanup(func() { isTerminal, readPassword, getState, restoreState = oldIs, oldRead, oldGet, oldRestore })

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := readSecret(ctx, env, "Value: ", false); result <- err }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the prompt kept blocking after cancel")
	}
	if !restored {
		t.Fatal("the terminal state was not restored")
	}
}

func TestACancelledContextNeverLeaksAValueTypedAfterwards(t *testing.T) {
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	env := &Env{In: pr, Err: &bytes.Buffer{}}
	oldIs, oldRead, oldGet, oldRestore := isTerminal, readPassword, getState, restoreState
	isTerminal = func(int) bool { return true }
	readPassword = func(int) ([]byte, error) { return []byte("typed"), nil }
	getState = func(int) (*term.State, error) { return nil, errors.New("no state") }
	restoreState = func(int, *term.State) error { return nil }
	t.Cleanup(func() { isTerminal, readPassword, getState, restoreState = oldIs, oldRead, oldGet, oldRestore })

	got, err := readSecret(context.Background(), env, "Value: ", false)
	if err != nil || got != "typed" {
		t.Fatalf("a normal read broke: %q %v", got, err)
	}
}

// ---- login and logout take the credential lock ----

func heldLock(t *testing.T, env *Env) func() {
	t.Helper()
	release, err := credential.Lock(context.Background(), env.Loader.Dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func TestLogoutWaitsForAnInFlightRefreshInsteadOfRacingIt(t *testing.T) {
	srv := newCounting(t, nil)
	env, _, _ := newTestEnv(t, srv.URL)
	release := heldLock(t, env)
	done := make(chan error, 1)
	go func() { done <- run(t, env, "auth", "logout") }()
	time.Sleep(150 * time.Millisecond)
	if _, err := env.Store.Load("default"); err != nil {
		t.Fatal("logout deleted the credential while another process held the lock")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.Load("default"); err != credential.ErrNotFound {
		t.Fatal("logout did not complete after the lock was released")
	}
}

func TestLoginWaitsForAnInFlightRefreshBeforeSaving(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) { writeEnvelope(w, 200, map[string]any{}) })
	env, _, _ := newTestEnv(t, api.URL)
	_ = env.Store.Delete("default")
	env.In = strings.NewReader("btk_new\n")
	release := heldLock(t, env)
	done := make(chan error, 1)
	go func() { done <- run(t, env, "auth", "login", "--token") }()
	time.Sleep(150 * time.Millisecond)
	if _, err := env.Store.Load("default"); err != credential.ErrNotFound {
		t.Fatal("login saved while another process held the lock")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if tok, err := env.Store.Load("default"); err != nil || tok.AccessToken != "btk_new" {
		t.Fatalf("%v %v", tok, err)
	}
}

func TestLogoutGivesUpCleanlyIfTheLockCannotBeTaken(t *testing.T) {
	srv := newCounting(t, nil)
	env, _, _ := newTestEnv(t, srv.URL)
	release := heldLock(t, env)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	root := NewRoot(env)
	root.SetArgs([]string{"auth", "logout"})
	if err := root.ExecuteContext(ctx); err == nil {
		t.Fatal("logout proceeded without the lock")
	}
	if _, err := env.Store.Load("default"); err != nil {
		t.Fatal("the credential was deleted without the lock")
	}
	if srv.hits() != 0 {
		t.Fatal("something was revoked without the lock")
	}
}
