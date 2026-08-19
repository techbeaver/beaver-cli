package command

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/internal/config"
	"github.com/techbeaver/beaver-cli/internal/credential"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

// fakeAPI records what the CLI sent and replies with envelopes.
type fakeAPI struct {
	*httptest.Server
	requests []recorded
	handler  func(*fakeAPI, http.ResponseWriter, *http.Request)
}

type recorded struct {
	method, path, confirmation string
	body                       map[string]any
}

func newFakeAPI(t *testing.T, handler func(*fakeAPI, http.ResponseWriter, *http.Request)) *fakeAPI {
	t.Helper()
	api := &fakeAPI{handler: handler}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{
			method:       r.Method,
			path:         strings.TrimPrefix(r.URL.Path, "/api/v1"),
			confirmation: r.Header.Get(ConfirmationHeader),
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&rec.body)
		}
		api.requests = append(api.requests, rec)
		api.handler(api, w, r)
	}))
	t.Cleanup(api.Close)
	return api
}

func (a *fakeAPI) calls(method, path string) int {
	n := 0
	for _, r := range a.requests {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func writeEnvelope(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoded, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status, "success": status < 400, "data": json.RawMessage(encoded),
	})
}

// newTestEnv builds an environment pointed at a fake API with a valid stored
// credential and a non-terminal stdout, which is how CI runs.
func newTestEnv(t *testing.T, host string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	store := &credential.FileStore{Dir: dir}
	if err := store.Save("default", &credential.Token{AccessToken: "btk_test"}); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	return &Env{
		Out:    &out,
		Err:    &errBuf,
		In:     strings.NewReader(""),
		Store:  store,
		Loader: &config.Loader{Dir: dir, WorkDir: dir},
		IsTTY:  false,
		Host:   host,
	}, &out, &errBuf
}

func run(t *testing.T, env *Env, args ...string) error {
	t.Helper()
	root := NewRoot(env)
	root.SetArgs(args)
	root.SetOut(env.Out)
	root.SetErr(env.Err)
	return root.ExecuteContext(context.Background())
}

func TestListRendersJSONWhenOutputIsNotATerminal(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, []map[string]any{{"id": "p1", "name": "site"}})
	})
	env, out, _ := newTestEnv(t, api.URL)

	if err := run(t, env, "projects", "list"); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("a pipe must receive JSON, got %q", out.String())
	}
	if len(rows) != 1 || rows[0]["name"] != "site" {
		t.Fatalf("unexpected rows: %v", rows)
	}
}

func TestDeleteWaitsForAHumanAndExitsSixWhenUnapproved(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mcp/confirmations") {
			writeEnvelope(w, 201, map[string]any{
				"id": "conf-1", "status": "pending",
				"approvalUrl": "https://techbeaver.io/portal/confirm/abc",
			})
			return
		}
		writeEnvelope(w, 200, map[string]any{})
	})
	env, _, errBuf := newTestEnv(t, api.URL)

	err := run(t, env, "apps", "delete", "app-1", "--yes")
	if err == nil {
		t.Fatal("an unapproved deletion must not report success")
	}
	if code := exitcode.From(err); code != exitcode.AwaitingHuman {
		t.Fatalf("exit code was %d, want %d (awaiting a human)", code, exitcode.AwaitingHuman)
	}
	if api.calls(http.MethodDelete, "/paas/apps/app-1") != 0 {
		t.Fatal("the delete was sent without an approval")
	}
	if !strings.Contains(errBuf.String(), "portal/confirm/abc") {
		t.Fatalf("the approval link must be shown, got %q", errBuf.String())
	}
}

func TestYesDoesNotBypassTheServerSideGate(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mcp/confirmations") {
			writeEnvelope(w, 201, map[string]any{
				"id": "conf-1", "status": "pending", "approvalUrl": "https://example/approve",
			})
			return
		}
		writeEnvelope(w, 200, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)

	_ = run(t, env, "db", "delete", "db-1", "--yes")

	if api.calls(http.MethodPost, "/mcp/confirmations") != 1 {
		t.Fatal("--yes must still register a confirmation with the server")
	}
	if api.calls(http.MethodDelete, "/dbaas/instances/db-1") != 0 {
		t.Fatal("--yes bypassed the human approval, which it must never do")
	}
}

func TestAnApprovedDeletionCarriesTheConfirmationToken(t *testing.T) {
	approved := false
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mcp/confirmations"):
			writeEnvelope(w, 201, map[string]any{
				"id": "conf-1", "status": "pending", "approvalUrl": "https://example/approve",
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/mcp/confirmations/"):
			approved = true
			writeEnvelope(w, 200, map[string]any{"status": "approved", "confirmationToken": "ct-xyz"})
		default:
			writeEnvelope(w, 200, map[string]any{"deleted": true})
		}
	})
	env, _, _ := newTestEnv(t, api.URL)

	if err := run(t, env, "apps", "delete", "app-1", "--yes", "--wait", "30s"); err != nil {
		t.Fatalf("an approved deletion should succeed: %v", err)
	}
	if !approved {
		t.Fatal("the CLI never polled for approval")
	}
	for _, r := range api.requests {
		if r.method == http.MethodDelete && r.path == "/paas/apps/app-1" {
			if r.confirmation != "ct-xyz" {
				t.Fatalf("the delete carried %q instead of the approved token", r.confirmation)
			}
			return
		}
	}
	t.Fatal("the delete was never sent after approval")
}

func TestAnExpiredCredentialWithNoRefreshTokenAsksForALogin(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)
	if err := env.Store.Save("default", &credential.Token{
		AccessToken: "old", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	err := run(t, env, "projects", "list")
	if code := exitcode.From(err); code != exitcode.Unauthenticated {
		t.Fatalf("exit code was %d, want %d", code, exitcode.Unauthenticated)
	}
	if len(api.requests) != 0 {
		t.Fatal("an expired credential must not be sent to the API")
	}
}

func TestEnvSetRejectsSomethingThatIsNotKeyValue(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)
	env.App = "app-1"

	err := run(t, env, "apps", "env", "set", "NOT_A_PAIR")
	if code := exitcode.From(err); code != exitcode.Usage {
		t.Fatalf("exit code was %d, want %d", code, exitcode.Usage)
	}
}

func TestACommandNeedingAProjectSaysEveryWayToSupplyOne(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)

	err := run(t, env, "apps", "list")
	if err == nil {
		t.Fatal("expected an error when no project is set")
	}
	for _, expected := range []string{"--project", "beaver.toml", "beaver config set"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("the error should mention %q, got: %v", expected, err)
		}
	}
}

func TestRestartIsAStopThenAStart(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)

	if err := run(t, env, "apps", "restart", "app-1"); err != nil {
		t.Fatal(err)
	}
	if api.calls(http.MethodPost, "/paas/apps/app-1/stop") != 1 {
		t.Fatal("restart did not stop the app")
	}
	if api.calls(http.MethodPost, "/paas/apps/app-1/start") != 1 {
		t.Fatal("restart did not start the app")
	}
	if api.requests[len(api.requests)-1].path != "/paas/apps/app-1/start" {
		t.Fatal("start must come after stop")
	}
}
