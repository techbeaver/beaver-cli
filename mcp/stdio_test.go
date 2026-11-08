package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
)

// connectLocal runs the stdio server's code path over an in-memory transport.
func connectLocal(t *testing.T, api *fakeAPI) *mcpsdk.ClientSession {
	t.Helper()
	return connectLocalWith(t, api, Surface{})
}

// connectLocalWith is connectLocal with the operator's choice of surface.
func connectLocalWith(t *testing.T, api *fakeAPI, surface Surface) *mcpsdk.ClientSession {
	t.Helper()
	apiServer := httptest.NewServer(api.handler())
	t.Cleanup(apiServer.Close)

	apiClient := client.New(apiServer.URL, "test")
	deps := &Deps{Client: apiClient, Auth: NewAuthenticator(apiClient), Config: Config{APIBaseURL: apiServer.URL}, Surface: surface}
	identity, err := deps.Auth.Resolve(context.Background(), "btk_test-token")
	if err != nil {
		t.Fatal(err)
	}

	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = serve(ctx, deps, "btk_test-token", identity, serverTransport) }()

	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "local-agent", Version: "1"}, nil).
		Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestTheLocalServerSendsTheSignedInCredentialWithEveryCall(t *testing.T) {
	api := &fakeAPI{scopes: scopes.All}
	session := connectLocal(t, api)

	res := callTool(t, session, "list_projects", map[string]any{})
	if res.IsError {
		t.Fatalf("a signed-in local server refused the call: %s", resultText(res))
	}
	if got := api.sawHeader("/api/v1/paas/projects", "Authorization"); got != "Bearer btk_test-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestSettingOneVariableSendsOnlyTheNamedOnesAndNeverReads(t *testing.T) {
	api := &fakeAPI{scopes: scopes.All}
	session := connectLocal(t, api)

	res := callTool(t, session, "set_env_vars", map[string]any{"appId": "app-1", "vars": map[string]any{"LOG_LEVEL": "debug", "NEW": "1"}})
	if res.IsError {
		t.Fatalf("set_env_vars: %s", resultText(res))
	}
	var body struct {
		EnvVars map[string]string `json:"envVars"`
	}
	if err := json.Unmarshal([]byte(api.lastBody()), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"LOG_LEVEL": "debug", "NEW": "1"}
	if len(body.EnvVars) != len(want) {
		t.Fatalf("sent %v, want only the named variables %v", body.EnvVars, want)
	}
	for k, v := range want {
		if body.EnvVars[k] != v {
			t.Errorf("%s = %q, want %q", k, body.EnvVars[k], v)
		}
	}
	for _, req := range api.snapshot() {
		if strings.Contains(req.Query, "reveal") {
			t.Fatalf("a write path asked for reveal: %s?%s", req.Path, req.Query)
		}
	}
	if strings.Contains(resultText(res), "hunter2") {
		t.Error("the result leaked a value")
	}
}

func TestRestartAppRollsWithoutStopping(t *testing.T) {
	api := &fakeAPI{scopes: scopes.All}
	session := connectLocal(t, api)

	res := callTool(t, session, "restart_app", map[string]any{"appId": "app-1"})
	if res.IsError {
		t.Fatalf("restart_app: %s", resultText(res))
	}
	if api.countCalls(http.MethodPost, "/api/v1/paas/apps/app-1/restart") != 1 {
		t.Error("restart_app did not ask for a rolling restart")
	}
	if api.countCalls(http.MethodPost, "/api/v1/paas/apps/app-1/stop") != 0 {
		t.Error("restart_app stopped the app")
	}
}
