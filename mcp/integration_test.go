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
	// created counts apps actually created, i.e. invoices actually raised.
	created int

	// gitAccess is what /paas/git/access answers. Nil means the route is not
	// there at all, which is the older API a released binary may still meet.
	gitAccess map[string]any
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

func (f *fakeAPI) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return ""
	}
	return f.requests[len(f.requests)-1].Body
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
			// Returned by the real API and enforced by nothing; here so the strip is proven.
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

		case path == "/paas/apps/app-1/deployments":
			ok(w, []map[string]any{
				{"id": "dep-3", "revision": "checkout-service-00003", "status": "failed",
					"failureStage": "build", "isActive": false, "createdAt": "2026-08-25T11:00:00Z"},
				{"id": "dep-2", "revision": "checkout-service-00002", "status": "active",
					"isActive": true, "createdAt": "2026-08-24T09:00:00Z"},
			})

		case path == "/paas/apps/app-1/rollback":
			// An empty revision reaches a lookup that matches nothing and 500s, exactly as the API does.
			var body map[string]any
			_ = json.Unmarshal([]byte(f.lastBody()), &body)
			revision, _ := body["revisionName"].(string)
			if strings.TrimSpace(revision) == "" {
				fail(w, http.StatusInternalServerError, "rollback_failed", "Rollback failed")
				return
			}
			ok(w, map[string]any{"revision": revision, "status": "rolling-back"})

		case path == "/paas/apps/app-1/root-directory":
			var body map[string]any
			_ = json.Unmarshal([]byte(f.lastBody()), &body)
			root, _ := body["rootDirectory"].(string)
			ok(w, map[string]any{"id": "app-1", "rootDirectory": root})

		case path == "/paas/git/access":
			f.mu.Lock()
			answer := f.gitAccess
			f.mu.Unlock()
			if answer == nil {
				fail(w, http.StatusNotFound, "not_found", "no such route")
				return
			}
			ok(w, answer)

		case path == "/paas/git/github/installations":
			ok(w, []map[string]any{})

		case path == "/paas/apps" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.Unmarshal([]byte(f.lastBody()), &body)
			f.mu.Lock()
			f.created++
			f.mu.Unlock()
			ok(w, map[string]any{"id": "app-2", "name": body["name"], "status": "building",
				"rootDirectory": body["rootDirectory"]})

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
	// Without this header a client cannot discover how to authenticate.
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
	// Nothing enforces it, so an agent shown it would refuse to create a fourth app.
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
	// Two layers refuse it, and the message names the field either way.
	if !strings.Contains(resultText(res), "idempotencyKey") {
		t.Fatalf("the error should tell the agent what to send: %s", resultText(res))
	}

	// The second layer: the field sent but left blank rather than omitted.
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
	// Restated at deletion, not only in the preview already scrolled past.
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
	// Failing on a dependency blip gets the container killed. ADR 0016.
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

// ---------------------------------------------------------------------------
// Rolling back
// ---------------------------------------------------------------------------

/*
rollback_app sent an empty body for its whole life. The API requires a revision
name, so the lookup ran for the empty string, matched nothing, and every call
came back "Rollback failed" with a 500.

It could not have been fixed in the tool alone: nothing in this surface listed
an app's deployments, so nothing could have told it what to send. That is why
list_deployments arrives with it.
*/

func TestRollingBackNamesTheRevisionItIsGoingTo(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "rollback_app", map[string]any{
		"appId": "app-1", "revisionName": "checkout-service-00002",
	})

	if res.IsError {
		t.Fatalf("rollback failed: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "checkout-service-00002") {
		t.Errorf("the result does not say what it rolled back to: %s", resultText(res))
	}
}

// TestRollingBackWithNoRevisionIsRefusedTwiceOver covers both ways a revision
// goes missing. Refusing beats the 500 it used to produce, and beats guessing:
// rolling back to something the customer did not choose is a second incident.
//
// Omitting the field entirely does not even reach the tool, because the schema
// marks it required and the client refuses to make the call. An empty string
// satisfies the schema and is what produced the 500, so the tool checks it too.
func TestRollingBackWithNoRevisionIsRefusedTwiceOver(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	omitted := callTool(t, session, "rollback_app", map[string]any{"appId": "app-1"})
	if !omitted.IsError {
		t.Fatal("a rollback with no revision must not be attempted")
	}
	if !strings.Contains(resultText(omitted), "revisionName") {
		t.Errorf("the schema refusal does not name the missing field: %s", resultText(omitted))
	}

	empty := callTool(t, session, "rollback_app", map[string]any{
		"appId": "app-1", "revisionName": "   ",
	})
	if !empty.IsError {
		t.Fatal("an empty revision reaches a lookup that matches nothing; it must be refused here")
	}
	if !strings.Contains(resultText(empty), "list_deployments") {
		t.Errorf("the refusal does not say where to find one: %s", resultText(empty))
	}
	if n := api.countCalls("POST", "/api/v1/paas/apps/app-1/rollback"); n != 0 {
		t.Errorf("a refused rollback still reached the API %d time(s)", n)
	}
}

// TestTheDeploymentListSaysWhichOneIsLive checks the question an agent is
// actually answering, which is "what do I go back to". That is the last
// revision that worked, not simply the previous one.
func TestTheDeploymentListSaysWhichOneIsLive(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "list_deployments", map[string]any{"appId": "app-1"})

	if res.IsError {
		t.Fatalf("listing deployments failed: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "is live") {
		t.Errorf("the summary does not name the live revision: %s", text)
	}
	if !strings.Contains(text, "failureStage") {
		t.Errorf("which half failed is what separates a bad Dockerfile from a bad port: %s", text)
	}
}

// ---------------------------------------------------------------------------
// Repositories this platform cannot read
// ---------------------------------------------------------------------------

/*
create_app on a priced plan raises an invoice immediately, and the clone happens
later in a build pod. So a private repository the platform has no access to went:
agent creates the app, customer pays, build fails on code nobody could ever have
cloned, explanation sits in a log. The fix is on GitHub, where only the customer
can perform it.

The console had a Connect GitHub step in front of the person the whole time. This
surface had no tool and no way for an agent even to ask.
*/

func createArgs(repo string) map[string]any {
	return map[string]any{
		"projectId": "proj-1", "name": "new-app", "gitRepo": repo,
		"idempotencyKey": "key-1",
	}
}

func TestCreateAppRefusesARepoThePlatformCannotRead(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default, gitAccess: map[string]any{
		"provider": "github", "owner": "acme", "repo": "secret",
		"checked": true, "reachable": false, "reason": "not_installed",
		"detail":     "the TechBeaver GitHub App is not installed on \"acme\", so it cannot read acme/secret.",
		"installUrl": "https://github.com/apps/techbeaver/installations/new",
	}}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "create_app", createArgs("https://github.com/acme/secret"))

	if !res.IsError {
		t.Fatal("creating an app against an unreadable repository must fail before anything is created")
	}
	// The whole point: no app, so no invoice.
	if api.created != 0 {
		t.Errorf("the app was created anyway (%d times), so the customer was charged for something that cannot build", api.created)
	}
	text := resultText(res)
	for _, want := range []string{"acme", "github.com/apps/techbeaver", "nothing has been charged"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("the refusal does not mention %q: %s", want, text)
		}
	}
}

func TestCreateAppProceedsWhenTheRepoIsReachable(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default, gitAccess: map[string]any{
		"provider": "github", "owner": "acme", "repo": "public-thing",
		"checked": true, "reachable": true, "public": true,
	}}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "create_app", createArgs("https://github.com/acme/public-thing"))

	if res.IsError {
		t.Fatalf("a reachable repository must not be blocked: %s", resultText(res))
	}
	if api.created != 1 {
		t.Errorf("expected the app to be created once, got %d", api.created)
	}
}

// TestCreateAppIsNotBlockedByACheckItCouldNotRun covers the two ways the check
// itself is unavailable: the route is missing (an older API than this binary),
// and the provider cannot be judged ahead of time (GitLab clones with the
// project owner's own connection). Neither is evidence that the repository is
// unreachable, and refusing on either would make this guard worse than the
// problem it was added for.
func TestCreateAppIsNotBlockedByACheckItCouldNotRun(t *testing.T) {
	for name, api := range map[string]*fakeAPI{
		"route missing": {scopes: scopes.Default, gitAccess: nil},
		"not checkable": {scopes: scopes.Default, gitAccess: map[string]any{
			"provider": "gitlab", "checked": false, "reachable": false,
			"detail": "gitlab repositories are cloned with the connection the project owner made in the console.",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			session, _ := connectAgent(t, api, "btk_test-token")
			res := callTool(t, session, "create_app", createArgs("https://gitlab.com/acme/api"))
			if res.IsError {
				t.Fatalf("creation was blocked by an inconclusive check: %s", resultText(res))
			}
			if api.created != 1 {
				t.Errorf("expected the app to be created, got %d creations", api.created)
			}
		})
	}
}

// TestCreateAppWithNoRepositoryNeverAsksAboutOne covers how the AI builder
// deploys: source this platform already holds, no repository at all. Asking
// GitHub about an empty string would be a wasted call at best and a spurious
// refusal at worst.
func TestCreateAppWithNoRepositoryNeverAsksAboutOne(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "create_app", map[string]any{
		"projectId": "proj-1", "name": "generated-site", "idempotencyKey": "key-2",
	})

	if res.IsError {
		t.Fatalf("an app with no repository must still be creatable: %s", resultText(res))
	}
	if n := api.countCalls("GET", "/api/v1/paas/git/access"); n != 0 {
		t.Errorf("the access check ran %d times for an app with no repository", n)
	}
}

// TestCreateAppIsNotBlockedByGitHubItselfBeingBroken separates the check
// answering "no" from the check answering "GitHub would not talk to me".
// Treating the second as a refusal would mean an outage at GitHub stops
// customers creating apps here at all, including from public repositories that
// clone without any credential.
func TestCreateAppIsNotBlockedByGitHubItselfBeingBroken(t *testing.T) {
	for _, reason := range []string{"github_error", "app_not_configured"} {
		t.Run(reason, func(t *testing.T) {
			api := &fakeAPI{scopes: scopes.Default, gitAccess: map[string]any{
				"provider": "github", "owner": "acme", "repo": "thing",
				"checked": true, "reachable": false, "reason": reason,
				"detail": "could not obtain GitHub credentials for acme/thing",
			}}
			session, _ := connectAgent(t, api, "btk_test-token")

			res := callTool(t, session, "create_app", createArgs("https://github.com/acme/thing"))

			if res.IsError {
				t.Fatalf("creation was refused over a failure that says nothing about the repository: %s", resultText(res))
			}
			if api.created != 1 {
				t.Errorf("expected the app to be created, got %d creations", api.created)
			}
		})
	}
}

func TestCheckRepoAccessHandsOverTheInstallLink(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default, gitAccess: map[string]any{
		"provider": "github", "owner": "acme", "repo": "secret",
		"checked": true, "reachable": false, "reason": "not_installed",
		"detail":     "the TechBeaver GitHub App is not installed on \"acme\".",
		"installUrl": "https://github.com/apps/techbeaver/installations/new",
	}}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "check_repo_access", map[string]any{
		"gitRepo": "https://github.com/acme/secret",
	})

	if res.IsError {
		t.Fatalf("the check itself must not error just because the answer is no: %s", resultText(res))
	}
	text := resultText(res)
	// The link is what the customer needs, and only they can act on it.
	if !strings.Contains(text, "https://github.com/apps/techbeaver/installations/new") {
		t.Errorf("the install link is not in the result: %s", text)
	}
	if !strings.Contains(strings.ToLower(text), "do not create the app yet") {
		t.Errorf("the result does not tell the agent to stop: %s", text)
	}
}

// TestCheckRepoAccessTellsAnAgentToStopTalkingWhenThereIsNothingToDo pins that
// a reachable repository produces no next step at all. Otherwise an agent
// recites a GitHub connection procedure at somebody whose repo already works.
func TestCheckRepoAccessTellsAnAgentToStopTalkingWhenThereIsNothingToDo(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default, gitAccess: map[string]any{
		"provider": "github", "owner": "acme", "repo": "thing",
		"checked": true, "reachable": true,
		"installUrl": "https://github.com/apps/techbeaver/installations/new",
	}}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "check_repo_access", map[string]any{"gitRepo": "https://github.com/acme/thing"})

	if strings.Contains(strings.ToLower(resultText(res)), "do not create") {
		t.Errorf("a working repository must not come with an instruction: %s", resultText(res))
	}
}

// ---------------------------------------------------------------------------
// Monorepos
// ---------------------------------------------------------------------------

/*
A repository whose app lives in apps/web could not be deployed from this surface
at all. Everything after the checkout was pinned to the repository root, so the
build found the wrong project or none, and there was no tool to correct it after
a first build looked in the wrong place.
*/

func TestCreateAppCarriesTheRootDirectoryForAMonorepo(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default, gitAccess: map[string]any{
		"provider": "github", "owner": "acme", "repo": "monorepo",
		"checked": true, "reachable": true, "public": true,
	}}
	session, _ := connectAgent(t, api, "btk_test-token")

	args := createArgs("https://github.com/acme/monorepo")
	args["rootDirectory"] = "apps/web"
	res := callTool(t, session, "create_app", args)

	if res.IsError {
		t.Fatalf("create_app failed: %s", resultText(res))
	}
	if !strings.Contains(api.lastBody(), `"rootDirectory":"apps/web"`) {
		t.Errorf("the root directory never reached the API: %s", api.lastBody())
	}
}

// TestSettingTheRootDirectoryDoesNotRedeployByItself pins that the change waits
// for a deploy. Applying it unasked would rebuild an app the customer had not
// finished configuring.
func TestSettingTheRootDirectoryDoesNotRedeployByItself(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "set_root_directory", map[string]any{
		"appId": "app-1", "rootDirectory": "apps/web",
	})

	if res.IsError {
		t.Fatalf("set_root_directory failed: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "apps/web") {
		t.Errorf("the result does not say what it now builds from: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "deploy_app") {
		t.Errorf("the result does not say that nothing rebuilds by itself: %s", resultText(res))
	}
	if n := api.countCalls("POST", "/api/v1/paas/apps/app-1/deploy"); n != 0 {
		t.Errorf("setting the root directory triggered %d deploy(s)", n)
	}
}

// TestAnEmptyRootDirectoryMeansTheRepositoryRoot pins that empty is a real
// value here, not an absent one: it is how somebody undoes a wrong
// subdirectory. Dropping it would leave the app pinned to the old one.
func TestAnEmptyRootDirectoryMeansTheRepositoryRoot(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session, _ := connectAgent(t, api, "btk_test-token")

	res := callTool(t, session, "set_root_directory", map[string]any{
		"appId": "app-1", "rootDirectory": "",
	})

	if res.IsError {
		t.Fatalf("clearing the root directory failed: %s", resultText(res))
	}
	if !strings.Contains(api.lastBody(), `"rootDirectory":""`) {
		t.Errorf("the empty value was dropped instead of sent: %s", api.lastBody())
	}
	if !strings.Contains(strings.ToLower(resultText(res)), "repository root") {
		t.Errorf("the result does not say it now builds from the root: %s", resultText(res))
	}
}
