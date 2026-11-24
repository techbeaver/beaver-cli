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

	"github.com/techbeaver/beaver-cli/scopes"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	sbProject = "11111111-2222-3333-4444-555555555555"
	sbPlan    = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

type sbSeen struct {
	method, path, query, idem, body string
}

type sbAPI struct {
	mu      sync.Mutex
	seen    []sbSeen
	handler func(w http.ResponseWriter, r *http.Request, n int)
}

func (a *sbAPI) calls() []sbSeen {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sbSeen(nil), a.seen...)
}

func connectSandbox(t *testing.T, granted []string, handler func(w http.ResponseWriter, r *http.Request, n int)) (*mcpsdk.ClientSession, *sbAPI) {
	t.Helper()
	sb := &sbAPI{handler: handler}
	base := &fakeAPI{scopes: granted}
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/sandbox") {
			base.handler().ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		sb.mu.Lock()
		sb.seen = append(sb.seen, sbSeen{r.Method, strings.TrimPrefix(r.URL.Path, "/api/v1"), r.URL.RawQuery, r.Header.Get("Idempotency-Key"), string(raw)})
		n := len(sb.seen)
		sb.mu.Unlock()
		sb.handler(w, r, n)
	}))
	t.Cleanup(apiServer.Close)
	mcpServer := httptest.NewServer(NewHandler(Config{
		APIBaseURL: apiServer.URL, PublicURL: "https://mcp.example.test", AuthorizationServer: apiServer.URL,
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(mcpServer.Close)
	c := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-agent", Version: "1"}, nil)
	session, err := c.Connect(context.Background(), &mcpsdk.StreamableClientTransport{
		Endpoint:   mcpServer.URL + MCPPath,
		HTTPClient: &http.Client{Transport: bearerTransport{token: "btk_test-token", base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, sb
}

func sbJSON(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":200,"success":true,"message":"ok","data":`+data+`}`)
}

func sbFailure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"status": status, "success": false, "message": message}
	if code != "" {
		body["code"] = code
	}
	_ = json.NewEncoder(w).Encode(body)
}

func okSandbox(w http.ResponseWriter, r *http.Request, _ int) {
	switch r.URL.Path {
	case "/api/v1/sandbox/plans":
		sbJSON(w, `[{"id":"`+sbPlan+`","name":"Small","prices":[{"currency":"NGN","monthlyPriceMinor":150000,"hourlyPriceMinor":500}]}]`)
	case "/api/v1/sandbox/usage":
		sbJSON(w, `{"projectId":"`+sbProject+`","balance":{"currency":"NGN","availableMinor":1000,"supported":true},"balanceHidden":false}`)
	case "/api/v1/sandbox/billing/quote":
		sbJSON(w, `{"slots":2,"currency":"NGN","totalMinor":322500}`)
	default:
		sbJSON(w, `{"status":"pending","paymentUrl":"https://checkout.paystack.test/sbx","message":"Open it."}`)
	}
}

var sandboxToolNames = []string{"list_sandbox_plans", "get_sandbox_usage", "quote_sandbox_slots", "create_sandbox_checkout_link"}

func TestSandboxToolsExistAndAreClassifiedByWhatTheyDo(t *testing.T) {
	specs := map[string]toolSpec{}
	for _, s := range AllTools().Specs() {
		specs[s.Name] = s
	}
	readOnly := map[string]bool{"list_sandbox_plans": true, "get_sandbox_usage": true, "quote_sandbox_slots": true}
	for _, name := range sandboxToolNames {
		spec, ok := specs[name]
		if !ok {
			t.Fatalf("tool %s is not registered", name)
		}
		if spec.ReadOnly != readOnly[name] {
			t.Errorf("%s ReadOnly=%v", name, spec.ReadOnly)
		}
		if spec.Destructive {
			t.Errorf("%s must not be destructive", name)
		}
	}
	if got := specs["create_sandbox_checkout_link"].Scopes; len(got) != 1 || got[0] != scopes.BillingCheckout {
		t.Errorf("checkout scopes %v", got)
	}
	for name := range specs {
		if strings.Contains(name, "sandbox") && !contains(sandboxToolNames, name) {
			t.Errorf("tool %q is a sandbox tool the platform does not let machine credentials use", name)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestSandboxReadToolsReachTheirRoutes(t *testing.T) {
	session, api := connectSandbox(t, scopes.Default, okSandbox)

	if text := resultText(callTool(t, session, "list_sandbox_plans", map[string]any{})); !strings.Contains(text, "Small") {
		t.Errorf("plans: %s", text)
	}
	if text := resultText(callTool(t, session, "get_sandbox_usage", map[string]any{"projectId": sbProject})); !strings.Contains(text, "availableMinor") {
		t.Errorf("usage: %s", text)
	}
	text := resultText(callTool(t, session, "quote_sandbox_slots", map[string]any{"projectId": sbProject, "planId": sbPlan, "slots": 2}))
	if !strings.Contains(text, "322500") {
		t.Errorf("quote: %s", text)
	}
	calls := api.calls()
	if len(calls) != 3 {
		t.Fatalf("calls: %+v", calls)
	}
	if calls[0].method != "GET" || calls[0].path != "/sandbox/plans" {
		t.Errorf("plans call %+v", calls[0])
	}
	if calls[1].method != "GET" || calls[1].path != "/sandbox/usage" || calls[1].query != "projectId="+sbProject {
		t.Errorf("usage call %+v", calls[1])
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(calls[2].body), &body)
	if calls[2].method != "POST" || calls[2].path != "/sandbox/billing/quote" || len(body) != 3 || calls[2].idem != "" {
		t.Errorf("quote call %+v", calls[2])
	}
}

func TestSandboxUsageSaysAHiddenBalanceIsHidden(t *testing.T) {
	session, _ := connectSandbox(t, scopes.Default, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sbJSON(w, `{"projectId":"p","balanceHidden":true,"subscriptions":[]}`)
	})
	text := resultText(callTool(t, session, "get_sandbox_usage", map[string]any{"projectId": sbProject}))
	if !strings.Contains(text, "hidden") || !strings.Contains(text, "not as zero") {
		t.Fatalf("the model must be told the balance is hidden: %s", text)
	}
}

func TestSandboxCheckoutReturnsALinkSendsTheKeyAndOnlyEverAsksForCheckout(t *testing.T) {
	session, api := connectSandbox(t, scopes.Default, okSandbox)
	res := callTool(t, session, "create_sandbox_checkout_link", map[string]any{
		"projectId": sbProject, "planId": sbPlan, "slots": 3, "idempotencyKey": "key-sbx", "successUrl": "https://example.test/ok",
	})
	text := resultText(res)
	if !strings.Contains(text, "checkout.paystack.test/sbx") || !strings.Contains(text, "cannot pay it for them") {
		t.Fatalf("link and cannot-pay statement expected: %s", text)
	}
	calls := api.calls()
	if len(calls) != 1 || calls[0].idem != "key-sbx" || calls[0].path != "/sandbox/billing/purchase" {
		t.Fatalf("calls: %+v", calls)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(calls[0].body), &body)
	if body["paymentSource"] != "checkout" || body["slots"] != float64(3) || body["successUrl"] != "https://example.test/ok" {
		t.Fatalf("body %s", calls[0].body)
	}
}

func TestSandboxCheckoutRefusesBeforeTheNetwork(t *testing.T) {
	session, api := connectSandbox(t, scopes.Default, okSandbox)
	base := func() map[string]any {
		return map[string]any{"projectId": sbProject, "planId": sbPlan, "slots": 1, "idempotencyKey": "k"}
	}
	cases := map[string]func(map[string]any){
		"no key":         func(m map[string]any) { delete(m, "idempotencyKey") },
		"blank key":      func(m map[string]any) { m["idempotencyKey"] = "  " },
		"balance source": func(m map[string]any) { m["paymentSource"] = "balance" },
		"wallet source":  func(m map[string]any) { m["paymentSource"] = "wallet" },
		"zero slots":     func(m map[string]any) { m["slots"] = 0 },
		"negative slots": func(m map[string]any) { m["slots"] = -4 },
		"bad project":    func(m map[string]any) { m["projectId"] = "my-site" },
		"bad plan":       func(m map[string]any) { m["planId"] = "../x" },
	}
	for name, mutate := range cases {
		args := base()
		mutate(args)
		res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "create_sandbox_checkout_link", Arguments: args})
		if err == nil && !res.IsError {
			t.Errorf("%s: must be refused", name)
		}
	}
	if n := len(api.calls()); n != 0 {
		t.Fatalf("refused calls reached the API %d times", n)
	}
	for _, args := range []map[string]any{
		{"projectId": "x"}, {"projectId": sbProject, "planId": sbPlan, "slots": 0},
	} {
		res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "quote_sandbox_slots", Arguments: args})
		if err == nil && !res.IsError {
			t.Errorf("quote %v must be refused", args)
		}
	}
	if n := len(api.calls()); n != 0 {
		t.Fatalf("refused quotes reached the API %d times", n)
	}
}

func TestSandboxCheckoutAcceptsTheCheckoutSourceAndHandlesAWalletTopUp(t *testing.T) {
	session, _ := connectSandbox(t, scopes.Default, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sbJSON(w, `{"status":"needs_topup","topUpUrl":"https://stripe.test/topup","message":"Top up first."}`)
	})
	res := callTool(t, session, "create_sandbox_checkout_link", map[string]any{
		"projectId": sbProject, "planId": sbPlan, "slots": 1, "idempotencyKey": "k", "paymentSource": "Checkout",
	})
	text := resultText(res)
	if res.IsError || !strings.Contains(text, "stripe.test/topup") || !strings.Contains(text, "topped up") {
		t.Fatalf("top-up case: %s", text)
	}
}

func TestSandboxToolsMapEveryFailureToACalmMessage(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		code, msg    string
		raw          string
		want, notWan string
	}{
		{"no sandbox api", 404, "", "", "Cannot GET /api/v1/sandbox/plans", "not available on this server yet", ""},
		{"real 404", 404, "", "Project not found", "", "Project not found", "not available on this server"},
		{"401", 401, "invalid_token", "bad", "", "no longer authorised", ""},
		{"403", 403, "", "No access to this project.", "", "No access to this project.", ""},
		{"usage owed", 402, "usage_owed", "You owe sandbox use.", "", "You owe sandbox use.", ""},
		{"sandboxes exist", 409, "sandboxes_exist", "Destroy them first.", "", "Destroy them first.", ""},
		{"too many payments", 409, "sandbox_too_many_open_payments", "Finish open payments.", "", "Finish open payments.", ""},
		{"credit", 422, "sandbox_credit_not_supported", "No credit.", "", "No credit.", ""},
		{"runtime", 502, "sandbox_runtime_failed", "Runtime failed.", "", "Runtime failed.", ""},
		{"paused", 503, "service_paused", "Paused for maintenance.", "", "do not retry", ""},
		{"check unavailable", 503, "sandbox_check_unavailable", "detail", "", "cannot be checked right now", "detail"},
		{"billing unavailable", 503, "billing_unavailable", "detail", "", "cannot be checked right now", "detail"},
		{"key reused", 422, "idempotency_key_reused", "x", "", "start the operation again", ""},
		{"key in progress", 409, "idempotency_in_progress", "x", "", "still running", ""},
		{"html", 502, "", "", "<html>bad gateway</html>", "HTTP 502", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, _ := connectSandbox(t, scopes.Default, func(w http.ResponseWriter, _ *http.Request, _ int) {
				if tc.raw != "" {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.raw)
					return
				}
				sbFailure(w, tc.status, tc.code, tc.msg)
			})
			for tool, args := range map[string]map[string]any{
				"list_sandbox_plans":           {},
				"get_sandbox_usage":            {"projectId": sbProject},
				"quote_sandbox_slots":          {"projectId": sbProject, "planId": sbPlan, "slots": 1},
				"create_sandbox_checkout_link": {"projectId": sbProject, "planId": sbPlan, "slots": 1, "idempotencyKey": "k"},
			} {
				res := callTool(t, session, tool, args)
				text := resultText(res)
				if !res.IsError || !strings.Contains(text, tc.want) {
					t.Errorf("%s: want error containing %q, got error=%v %q", tool, tc.want, res.IsError, text)
				}
				if tc.notWan != "" && strings.Contains(text, tc.notWan) {
					t.Errorf("%s: %q must not appear in %q", tool, tc.notWan, text)
				}
				if strings.Contains(text, "btk_") {
					t.Errorf("%s leaked a credential", tool)
				}
			}
		})
	}
}

func TestSandboxToolsEveryCallAgainstAServerWithNoSandboxAPIIsCalm(t *testing.T) {
	session, _ := connectSandbox(t, scopes.Default, func(w http.ResponseWriter, r *http.Request, _ int) {
		http.Error(w, "Cannot "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	})
	for tool, args := range map[string]map[string]any{
		"list_sandbox_plans":           {},
		"get_sandbox_usage":            {"projectId": sbProject},
		"quote_sandbox_slots":          {"projectId": sbProject, "planId": sbPlan, "slots": 1},
		"create_sandbox_checkout_link": {"projectId": sbProject, "planId": sbPlan, "slots": 1, "idempotencyKey": "k"},
	} {
		res := callTool(t, session, tool, args)
		if !res.IsError || !strings.Contains(resultText(res), "Sandboxes are not available on this server yet") {
			t.Errorf("%s: %q", tool, resultText(res))
		}
	}
}

func TestSandboxToolsHandleMalformedAndEmptyAnswers(t *testing.T) {
	session, _ := connectSandbox(t, scopes.Default, func(w http.ResponseWriter, _ *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		if n%2 == 0 {
			_, _ = io.WriteString(w, `{"status":200,"data":`)
			return
		}
		_, _ = io.WriteString(w, `{"status":200,"success":true,"data":[]}`)
	})
	res := callTool(t, session, "list_sandbox_plans", map[string]any{})
	if res.IsError || !strings.Contains(resultText(res), "No sandbox plans") {
		t.Fatalf("an empty catalogue is a fact, not an error: %s", resultText(res))
	}
	res = callTool(t, session, "list_sandbox_plans", map[string]any{})
	if !res.IsError {
		t.Fatalf("a truncated body must be an error: %s", resultText(res))
	}
}

func TestSandboxToolsAreHiddenWithoutTheirScopes(t *testing.T) {
	session, _ := connectSandbox(t, []string{scopes.ProjectsRead, scopes.BillingRead}, okSandbox)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		seen[tool.Name] = true
	}
	if seen["create_sandbox_checkout_link"] {
		t.Error("a connection without billing:checkout can see the checkout tool")
	}
	for _, name := range []string{"list_sandbox_plans", "get_sandbox_usage", "quote_sandbox_slots"} {
		if !seen[name] {
			t.Errorf("%s should be visible to a read connection", name)
		}
	}
}

func TestSandboxToolsRunConcurrently(t *testing.T) {
	session, api := connectSandbox(t, scopes.Default, okSandbox)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
				Name: "create_sandbox_checkout_link",
				Arguments: map[string]any{"projectId": sbProject, "planId": sbPlan, "slots": 1,
					"idempotencyKey": "key-" + string(rune('a'+i))},
			})
			if err != nil || res.IsError {
				t.Errorf("call failed: %v", err)
			}
		}()
	}
	wg.Wait()
	keys := map[string]bool{}
	for _, c := range api.calls() {
		keys[c.idem] = true
	}
	if len(keys) != 12 {
		t.Fatalf("each call must carry its own caller key, saw %d distinct", len(keys))
	}
}
