package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/scopes"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// End to end: a real MCP client, over the real streamable HTTP transport,
// against the real handler, in front of a stand-in for the TechBeaver API.
//
// The stand-in is deliberately strict. It refuses a delete with no confirmation
// header and a checkout with no idempotency key, exactly as the API does, so
// these tests fail if the MCP server ever stops sending them rather than
// passing against a mock that accepts anything.

// fakeAPI is a stand-in for /api/v1.
type fakeAPI struct {
	mu       sync.Mutex
	requests []recordedRequest

	scopes    []string
	deleted   bool
	approved  bool
	checkouts int
}

type recordedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Body    string
}

func (f *fakeAPI) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone(), Body: string(body),
	})
}

func (f *fakeAPI) sawHeader(path, header string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if req.Path == path {
			if v := req.Headers.Get(header); v != "" {
				return v
			}
		}
	}
	return ""
}

func (f *fakeAPI) countCalls(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if req.Method == method && req.Path == path {
			n++
		}
	}
	return n
}

func ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": 200, "success": true, "message": "ok", "data": data,
	})
}

func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status, "success": false, "message": message, "error": code, "code": code,
	})
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)

		if r.Header.Get("Authorization") != "Bearer btk_test-token" {
			fail(w, http.StatusUnauthorized, "invalid_token", "not a valid token")
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")

		switch {
		case path == "/mcp/whoami":
			ok(w, map[string]any{
				"userId": "user-1", "email": "ada@example.test", "principal": "machine",
				"clientName": "Test client.Client", "scopes": f.scopes, "spendCapMinor": 0,
				"canSpendWithoutCheckout": false,
			})

		case path == "/paas/projects":
			ok(w, []map[string]any{{"id": "proj-1", "name": "storefront", "regionCode": "eu"}})

		case path == "/paas/plans":
			// maxAppsPerProject is returned by the real API and enforced by
			// nothing. It is here so the test can prove the tool strips it.
			ok(w, []map[string]any{{"id": "plan-1", "name": "starter", "monthlyPriceMinor": 500000, "maxAppsPerProject": 3}})

		case path == "/paas/apps/app-1":
			if r.Method == http.MethodDelete {
				if r.Header.Get("X-Confirmation-Token") == "" {
					fail(w, http.StatusPreconditionRequired, "confirmation_required",
						"the account owner has to approve this in their browser first")
					return
				}
				f.mu.Lock()
				f.deleted = true
				f.mu.Unlock()
				ok(w, map[string]any{"id": "app-1", "deleted": true})
				return
			}
			ok(w, map[string]any{"id": "app-1", "name": "checkout-service", "status": "running", "url": "https://checkout.example.test"})

		case path == "/paas/apps/app-1/custom-domains":
			ok(w, []map[string]any{})

		case path == "/paas/apps/app-1/env":
			ok(w, map[string]any{"DATABASE_URL": "postgres://user:hunter2@db.example.test/app", "LOG_LEVEL": "info"})

		case path == "/paas/apps/app-1/checkout":
			if r.Header.Get("Idempotency-Key") == "" {
				fail(w, http.StatusBadRequest, "idempotency_key_required", "this call needs an Idempotency-Key")
				return
			}
			f.mu.Lock()
			f.checkouts++
			f.mu.Unlock()
			ok(w, map[string]any{"authorizationUrl": "https://checkout.paystack.test/abc", "invoiceId": "inv-1"})

		case path == "/mcp/confirmations" && r.Method == http.MethodPost:
			ok(w, map[string]any{
				"id": "conf-1", "status": "pending",
				"approvalUrl":       "https://portal.example.test/portal/confirm/ref-1",
				"confirmationToken": "bcf_secret",
				"expiresAt":         time.Now().Add(15 * time.Minute).Format(time.RFC3339),
				"recoveryNote":      "recreate it from your repository",
			})

		case strings.HasPrefix(path, "/mcp/confirmations/"):
			f.mu.Lock()
			approved := f.approved
			f.mu.Unlock()
			status := "pending"
			if approved {
				status = "approved"
			}
			ok(w, map[string]any{"id": "conf-1", "status": status})

		case path == "/dbaas/plans", path == "/paas/regions":
			ok(w, []map[string]any{})

		default:
			fail(w, http.StatusNotFound, "not_found", "no such thing on this account")
		}
	})
}

// bearerTransport puts the customer's token on every request the MCP client
// makes, which is what a real client does after its OAuth flow.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// connectAgent stands up the whole chain and returns a connected MCP session.
func connectAgent(t *testing.T, api *fakeAPI, token string) (*mcpsdk.ClientSession, *httptest.Server) {
	t.Helper()
	apiServer := httptest.NewServer(api.handler())
	t.Cleanup(apiServer.Close)

	mcpServer := httptest.NewServer(NewHandler(Config{
		APIBaseURL:          apiServer.URL,
		PublicURL:           "https://mcp.example.test",
		AuthorizationServer: apiServer.URL,
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(mcpServer.Close)

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-agent", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcpsdk.StreamableClientTransport{
		Endpoint:   mcpServer.URL + MCPPath,
		HTTPClient: &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatalf("connecting the agent: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, mcpServer
}

func callTool(t *testing.T, session *mcpsdk.ClientSession, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	return res
}

func resultText(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------

func TestAnUnauthenticatedClientIsChallenged(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	apiServer := httptest.NewServer(api.handler())
	defer apiServer.Close()
	mcpServer := httptest.NewServer(NewHandler(Config{
		APIBaseURL:          apiServer.URL,
		PublicURL:           "https://mcp.example.test",
		AuthorizationServer: apiServer.URL,
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer mcpServer.Close()

	resp, err := http.Post(mcpServer.URL+MCPPath, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	// Without this header a client cannot discover how to authenticate and the
	// connector simply fails.
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, "resource_metadata=") {
		t.Fatalf("the challenge must point at the protected-resource metadata, got %q", challenge)
	}
	if !strings.Contains(challenge, ProtectedResourceMetadataPath) {
		t.Fatalf("the challenge names the wrong document: %q", challenge)
	}
}

func TestDiscoveryDocumentIsServed(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	apiServer := httptest.NewServer(api.handler())
	defer apiServer.Close()
	mcpServer := httptest.NewServer(NewHandler(Config{
		APIBaseURL:          apiServer.URL,
		PublicURL:           "https://mcp.example.test",
		AuthorizationServer: "https://api.example.test",
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer mcpServer.Close()

	for _, path := range []string{ProtectedResourceMetadataPath, ProtectedResourceMetadataPath + MCPPath} {
		resp, err := http.Get(mcpServer.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		if doc["resource"] != "https://mcp.example.test/mcp" {
			t.Fatalf("%s named the wrong resource: %v", path, doc["resource"])
		}
		servers, _ := doc["authorization_servers"].([]any)
		if len(servers) != 1 || servers[0] != "https://api.example.test" {
			t.Fatalf("%s named the wrong authorization server: %v", path, doc["authorization_servers"])
		}
	}
}

func TestAgentSeesOnlyTheToolsItWasGranted(t *testing.T) {
	api := &fakeAPI{scopes: []string{scopes.ProjectsRead, scopes.PaaSRead}}
	session, _ := connectAgent(t, api, "btk_test-token")

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	if !names["get_app"] || !names["whoami"] {
		t.Fatalf("a read connection should see the read tools, got %v", names)
	}
	for _, forbidden := range []string{"delete_app", "delete_database", "create_app", "pay_invoice", "deploy_app"} {
		if names[forbidden] {
			t.Errorf("a read-only connection was shown %q. A model that can see a tool will eventually suggest it", forbidden)
		}
	}
}

func TestToolsCarryAnnotationsAndOutputSchemas(t *testing.T) {
	api := &fakeAPI{scopes: append(append([]string{}, scopes.Default...), scopes.PaaSDestroy)}
	session, _ := connectAgent(t, api, "btk_test-token")

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Annotations == nil {
			t.Errorf("tool %q has no annotations, so a client cannot tell whether it is safe to auto-approve", tool.Name)
			continue
		}
		if tool.OutputSchema == nil {
			t.Errorf("tool %q has no output schema, so a client gets prose to re-parse", tool.Name)
		}
		switch tool.Name {
		case "get_app", "list_projects", "whoami":
			if !tool.Annotations.ReadOnlyHint {
				t.Errorf("tool %q reads nothing but is not marked read-only", tool.Name)
			}
		case "delete_app":
			if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Errorf("delete_app is not marked destructive")
			}
		}
	}
}

func TestWhoamiStatesWhatTheAgentCannotDo(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "whoami", map[string]any{})
	text := resultText(res)
	for _, phrase := range []string{"cannot complete a payment", "cannot reach anything administrative", "approving that exact deletion"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("whoami should say plainly that the agent %q; got: %s", phrase, text)
		}
	}
}

func TestEnvironmentVariablesAreRedactedByDefault(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "get_env_vars", map[string]any{"appId": "app-1"})
	text := resultText(res)
	if strings.Contains(text, "hunter2") {
		t.Fatalf("a secret value reached the transcript: %s", text)
	}
	if !strings.Contains(text, "DATABASE_URL") {
		t.Fatalf("the variable names should still be visible: %s", text)
	}

	res = callTool(t, session, "get_env_vars", map[string]any{"appId": "app-1", "includeValues": true})
	if !strings.Contains(resultText(res), "hunter2") {
		t.Fatal("asking explicitly for values should return them")
	}
}

func TestListPlansHidesTheLimitThatEnforcesNothing(t *testing.T) {
	// The API returns maxAppsPerProject and nothing enforces it: apps are billed
	// individually, so their count bounds itself and there is deliberately no
	// cap. An agent shown that field would refuse to create a fourth app.
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "list_plans", map[string]any{})
	text := resultText(res)
	if strings.Contains(text, "maxAppsPerProject") {
		t.Fatalf("list_plans passed a limit that enforces nothing to the model: %s", text)
	}
	if !strings.Contains(text, "starter") {
		t.Fatalf("the plan itself should still be there: %s", text)
	}
}

func TestCheckoutReturnsALinkAndSaysItCannotPay(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "create_checkout_link", map[string]any{
		"resourceType": "app", "resourceId": "app-1", "idempotencyKey": "key-1",
	})
	text := resultText(res)
	if !strings.Contains(text, "checkout.paystack.test") {
		t.Fatalf("expected a payment link, got: %s", text)
	}
	if !strings.Contains(text, "cannot pay it for them") {
		t.Fatalf("the result must say the agent cannot complete the payment, got: %s", text)
	}
	if api.sawHeader("/api/v1/paas/apps/app-1/checkout", "Idempotency-Key") != "key-1" {
		t.Fatal("the idempotency key was not forwarded, so a retry would charge twice")
	}
}

func TestCheckoutRefusesToInventAnIdempotencyKey(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "create_checkout_link", map[string]any{
		"resourceType": "app", "resourceId": "app-1",
	})
	if !res.IsError {
		t.Fatal("a money call with no idempotency key must fail rather than proceeding")
	}
	if api.countCalls("POST", "/api/v1/paas/apps/app-1/checkout") != 0 {
		t.Fatal("the call reached the API despite having no idempotency key")
	}
	// Two layers refuse it, and either is enough: the input schema marks the
	// key required, and the handler refuses an empty one. The message names the
	// field either way, which is what lets the model fix its own call.
	if !strings.Contains(resultText(res), "idempotencyKey") {
		t.Fatalf("the error should tell the agent what to send: %s", resultText(res))
	}

	// The second layer, reached when a client sends the field but leaves it
	// blank rather than omitting it.
	blank := callTool(t, session, "create_checkout_link", map[string]any{
		"resourceType": "app", "resourceId": "app-1", "idempotencyKey": "",
	})
	if !blank.IsError {
		t.Fatal("an empty idempotency key must be refused too, not treated as absent")
	}
	if api.countCalls("POST", "/api/v1/paas/apps/app-1/checkout") != 0 {
		t.Fatal("a blank idempotency key still reached the API")
	}
}

func TestDeletionReturnsAPreviewAndAnApprovalLinkAndDeletesNothing(t *testing.T) {
	api := &fakeAPI{scopes: append(append([]string{}, scopes.Default...), scopes.PaaSDestroy)}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "delete_app", map[string]any{"appId": "app-1"})
	text := resultText(res)

	if api.deleted {
		t.Fatal("the app was deleted on the first call, with no human approval")
	}
	if !strings.Contains(text, "awaiting_approval") {
		t.Fatalf("expected to be told approval is pending, got: %s", text)
	}
	if !strings.Contains(text, "portal.example.test/portal/confirm/ref-1") {
		t.Fatalf("the approval link has to reach the customer, got: %s", text)
	}
	if !strings.Contains(text, "recreate it from your repository") && !strings.Contains(text, "git repository") {
		t.Fatalf("the preview must say how to recover, got: %s", text)
	}
	if !strings.Contains(text, "checkout-service") {
		t.Fatalf("the preview must name what is about to be deleted, got: %s", text)
	}
}

func TestDeletionProceedsOnceApproved(t *testing.T) {
	api := &fakeAPI{scopes: append(append([]string{}, scopes.Default...), scopes.PaaSDestroy)}
	session, _ := connectAgent(t, api, "btk_test-token")

	first := callTool(t, session, "delete_app", map[string]any{"appId": "app-1"})
	if api.deleted {
		t.Fatal("deleted without approval")
	}
	// The customer approves in their browser. The agent replays the token.
	api.mu.Lock()
	api.approved = true
	api.mu.Unlock()
	_ = first

	second := callTool(t, session, "delete_app", map[string]any{
		"appId": "app-1", "confirmationToken": "bcf_secret",
	})
	if !api.deleted {
		t.Fatalf("an approved deletion should have gone through: %s", resultText(second))
	}
	if api.sawHeader("/api/v1/paas/apps/app-1", "X-Confirmation-Token") != "bcf_secret" {
		t.Fatal("the confirmation token was not forwarded")
	}
	text := resultText(second)
	if !strings.Contains(text, "done") {
		t.Fatalf("expected a completed status, got: %s", text)
	}
	// The recovery path has to be restated at the moment of deletion, not only
	// in the preview the customer has by now scrolled past.
	if !strings.Contains(text, "git repository") {
		t.Fatalf("the result must restate how to recover, got: %s", text)
	}
}

func TestAPausedServiceIsReportedRatherThanRetried(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.record(r)
		if strings.HasSuffix(r.URL.Path, "/mcp/whoami") {
			ok(w, map[string]any{"userId": "user-1", "email": "ada@example.test", "principal": "machine", "scopes": scopes.Default})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 503, "success": false,
			"message": "Deploying new apps is temporarily paused while we perform maintenance.",
			"error":   "service temporarily paused", "code": "service_paused",
			"data": map[string]any{"code": "SERVICE_PAUSED", "gateKey": "paas_app_create"},
		})
	}))
	defer apiServer.Close()

	mcpServer := httptest.NewServer(NewHandler(Config{
		APIBaseURL: apiServer.URL, PublicURL: "https://mcp.example.test", AuthorizationServer: apiServer.URL,
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer mcpServer.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-agent", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcpsdk.StreamableClientTransport{
		Endpoint:   mcpServer.URL + MCPPath,
		HTTPClient: &http.Client{Transport: bearerTransport{token: "btk_test-token", base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res := callTool(t, session, "create_app", map[string]any{
		"projectId": "proj-1", "name": "new-app", "idempotencyKey": "key-1",
	})
	if !res.IsError {
		t.Fatal("a paused service should surface as an error the agent reports")
	}
	text := resultText(res)
	if !strings.Contains(text, "do not retry") {
		t.Fatalf("the agent must be told not to retry into a circuit breaker, got: %s", text)
	}
	if !strings.Contains(text, "temporarily paused") {
		t.Fatalf("the operator's own message must reach the customer, got: %s", text)
	}
}

func TestCrossOriginRequestsFromUnknownOriginsAreRefused(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	apiServer := httptest.NewServer(api.handler())
	defer apiServer.Close()
	mcpServer := httptest.NewServer(NewHandler(Config{
		APIBaseURL: apiServer.URL, PublicURL: "https://mcp.example.test", AuthorizationServer: apiServer.URL,
		AllowedOrigins: []string{"https://console.example.test"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer mcpServer.Close()

	req, _ := http.NewRequest(http.MethodPost, mcpServer.URL+MCPPath, strings.NewReader("{}"))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer btk_test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an unrecognised origin must be refused (DNS rebinding), got %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, mcpServer.URL+MCPPath, strings.NewReader("{}"))
	req.Header.Set("Origin", "https://console.example.test")
	req.Header.Set("Authorization", "Bearer btk_test-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusForbidden {
		t.Fatal("an allowed origin was refused")
	}
}

func TestTheKillSwitchRefusesEverything(t *testing.T) {
	mcpServer := httptest.NewServer(NewHandler(Config{
		APIBaseURL: "http://unused.invalid", PublicURL: "https://mcp.example.test", Disabled: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer mcpServer.Close()

	req, _ := http.NewRequest(http.MethodPost, mcpServer.URL+MCPPath, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer btk_test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the kill switch must refuse everything, got %d", resp.StatusCode)
	}
}

func TestHealthCheckDoesNotDependOnTheAPI(t *testing.T) {
	// A health check that fails when a dependency blips gets the container
	// killed, which turns a blip into a restart loop.
	mcpServer := httptest.NewServer(NewHandler(Config{
		APIBaseURL: "http://definitely-not-listening.invalid",
		PublicURL:  "https://mcp.example.test",
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer mcpServer.Close()

	resp, err := http.Get(mcpServer.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health check should pass with the API unreachable, got %d", resp.StatusCode)
	}
}

func TestSearchAndFetchWithholdSecrets(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "fetch", map[string]any{"id": "app:app-1"})
	if strings.Contains(resultText(res), "hunter2") {
		t.Fatal("fetch leaked a secret that get_env_vars would have withheld")
	}
}
