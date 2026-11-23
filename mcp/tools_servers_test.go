package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/techbeaver/beaver-cli/client"
)

// The server and wallet tools, against a stand-in for the API that is as strict as the API is: it refuses
// a write with no Idempotency-Key, dedupes a repeated key the way IdempotencyMiddleware does, and, for a
// dollar order, treats a call to the checkout route as the wallet being charged. A test that ends with
// charges == 0 therefore proves the tool never reached the route that spends.

type orderFake struct {
	mu sync.Mutex

	// dollar is what GET /billing/wallet/status answers (available).
	dollar bool
	// catalog overrides the catalog body; nil uses the default.
	catalog map[string]any
	// walletAvailable and shortfall drive the quote of a dollar order. noBalance omits the balance.
	walletAvailable, shortfall int64
	suggested                  *int64
	noBalance                  bool
	// omitPortalLink makes the order answer without portalCheckoutUrl.
	omitPortalLink bool
	// failure, when set, answers the named route (METHOD path) with a status and code.
	failure map[string]fakeFailure
	// extraOrderFields are added to the order answer (to prove secrets are scrubbed).
	extraOrderFields map[string]any
	// checkoutURL is what the naira checkout answers; empty means the field is missing.
	checkoutURL string

	requests []recordedRequest
	orders   map[string]string // idempotency key -> response body, as the API stores it
	created  int
	// charges counts calls to the checkout route of a dollar order: the wallet being charged.
	charges int
	// topUps counts top-up pages created.
	topUps  int
	topUpBy map[string]string
}

type fakeFailure struct {
	status int
	code   string
	msg    string
	data   map[string]any
}

func newOrderFake() *orderFake {
	return &orderFake{
		orders: map[string]string{}, topUpBy: map[string]string{}, failure: map[string]fakeFailure{},
		checkoutURL: "https://checkout.paystack.test/abc", walletAvailable: 500_000,
	}
}

func defaultServerCatalog(dollar bool) map[string]any {
	plan := map[string]any{"id": "plan-1", "name": "Small", "vcpu": 2, "ramMb": 4096, "storageGb": 80, "currency": "NGN", "monthlyPriceMinor": 900000, "usdMonthlyPriceMinor": 900, "stripeEligible": true}
	if dollar {
		plan = map[string]any{"id": "plan-1", "name": "Small", "vcpu": 2, "ramMb": 4096, "storageGb": 80, "currency": "USD", "monthlyPriceMinor": 900}
	}
	return map[string]any{
		"regions":          []any{map[string]any{"id": "r1", "city": "Lagos", "code": "lag"}},
		"plans":            []any{plan, map[string]any{"id": "plan-2", "name": "Big", "vcpu": 8, "ramMb": 16384, "storageGb": 320, "currency": plan["currency"], "monthlyPriceMinor": 1}},
		"operatingSystems": []any{map[string]any{"id": "os-1", "planId": "plan-1", "name": "Ubuntu", "version": "24.04"}, map[string]any{"id": "os-9", "planId": "plan-2", "name": "Debian", "version": "12"}},
	}
}

func (f *orderFake) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

func (f *orderFake) last(method, path string) recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].Method == method && f.requests[i].Path == path {
			return f.requests[i]
		}
	}
	return recordedRequest{}
}

func (f *orderFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: path, Headers: r.Header.Clone(), Body: string(raw)})
		fail1, failing := f.failure[r.Method+" "+path]
		f.mu.Unlock()

		if r.Header.Get("Authorization") != "Bearer btk_test-token" {
			fail(w, http.StatusUnauthorized, "invalid_token", "not a valid token")
			return
		}
		if failing {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fail1.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": fail1.status, "success": false, "message": fail1.msg, "code": fail1.code, "data": fail1.data})
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if r.Method == http.MethodPost && key == "" && path != "/servers/orders/quote" {
			fail(w, http.StatusBadRequest, "idempotency_key_required", "this call needs an Idempotency-Key")
			return
		}

		switch {
		case path == "/mcp/whoami":
			ok(w, map[string]any{"userId": "user-1", "email": "ada@example.test", "principal": "machine", "clientName": "Test",
				"scopes": []string{"billing:read", "billing:checkout"}, "canSpendWithoutCheckout": false})
		case path == "/billing/wallet/status":
			ok(w, map[string]any{"available": f.dollar, "reason": "", "currency": "USD"})
		case path == "/servers/catalog" && !f.dollar:
			ok(w, f.catalogBody(false))
		case path == "/servers/catalog/usd" && f.dollar:
			ok(w, f.catalogBody(true))
		case path == "/servers/catalog/usd":
			fail(w, http.StatusNotFound, "wallet_not_available", "The wallet is not available.")
		case path == "/servers/orders/quote":
			f.quote(w)
		case path == "/servers/orders" && r.Method == http.MethodPost:
			f.createOrder(w, key, raw)
		case strings.HasPrefix(path, "/servers/orders/") && strings.HasSuffix(path, "/checkout"):
			f.checkout(w, key)
		case path == "/billing/wallet/top-ups" && r.Method == http.MethodPost:
			f.topUp(w, key, raw)
		default:
			fail(w, http.StatusNotFound, "not_found", "no such thing")
		}
	})
}

func (f *orderFake) catalogBody(dollar bool) map[string]any {
	if f.catalog != nil {
		return f.catalog
	}
	return defaultServerCatalog(dollar)
}

func (f *orderFake) quote(w http.ResponseWriter) {
	if !f.dollar {
		ok(w, map[string]any{"currency": "NGN", "cycleTotalPriceMinor": 900000, "taxMinor": 67500, "finalTotalMinor": 967500})
		return
	}
	q := map[string]any{"currency": "USD", "payWith": "wallet", "cycleTotalPriceMinor": 900}
	if !f.noBalance {
		q["walletAvailableMinor"] = f.walletAvailable
	}
	if f.shortfall > 0 {
		q["shortfallMinor"] = f.shortfall
		if f.suggested != nil {
			q["suggestedTopUpMinor"] = *f.suggested
		} else {
			q["multipleTopUps"] = true
		}
	}
	ok(w, q)
}

// createOrder behaves like the global idempotency middleware around the handler: the first call with a
// key runs and is stored, a repeat gets the stored answer, and a repeat with a different body is a 422.
func (f *orderFake) createOrder(w http.ResponseWriter, key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if stored, seen := f.orders[key]; seen {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, stored)
		return
	}
	f.created++
	data := map[string]any{"orderId": fmt.Sprintf("00000000-0000-0000-0000-%012d", f.created), "status": "draft", "billingCycle": "monthly", "monthlyPriceMinor": 900, "totalPriceMinor": 900}
	if f.dollar {
		data["currency"] = "USD"
		if !f.omitPortalLink {
			data["portalCheckoutUrl"] = "https://portal.example.test/portal/billing/checkout?orderId=" + data["orderId"].(string)
		}
	} else {
		data["currency"] = "NGN"
	}
	for k, v := range f.extraOrderFields {
		data[k] = v
	}
	enc, _ := json.Marshal(map[string]any{"status": 201, "success": true, "message": "Server order created", "data": data})
	f.orders[key] = string(enc)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(enc)
}

func (f *orderFake) checkout(w http.ResponseWriter, key string) {
	f.mu.Lock()
	dollar, url := f.dollar, f.checkoutURL
	if dollar {
		f.charges++
	}
	f.mu.Unlock()
	if dollar {
		ok(w, map[string]any{"paid": true, "provider": "wallet"})
		return
	}
	data := map[string]any{"invoiceId": "inv-1", "provider": "paystack", "status": "pending"}
	if url != "" {
		data["authorizationUrl"] = url
	}
	ok(w, data)
}

func (f *orderFake) topUp(w http.ResponseWriter, key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	if stored, seen := f.topUpBy[key]; seen {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, stored)
		return
	}
	f.topUps++
	view := map[string]any{"id": "tu-1", "status": "open", "amountMinor": in["amountMinor"], "currency": "USD", "checkoutUrl": "https://checkout.stripe.test/c/pay/cs_test_1"}
	enc, _ := json.Marshal(map[string]any{"status": 201, "success": true, "message": "Top-up created", "data": view})
	f.topUpBy[key] = string(enc)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(enc)
}

// directDeps builds Deps and a Call that reach the fake with the given client, bypassing the MCP
// transport so a test can set a short client timeout.
func directDeps(t *testing.T, f *orderFake, timeout time.Duration) (*Deps, *Call, func()) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	cl := client.New(srv.URL, "test")
	cl.HTTP.Timeout = timeout
	return &Deps{Client: cl}, &Call{Session: &Session{Token: "btk_test-token", Identity: &Identity{}}}, srv.Close
}

func newDirect(t *testing.T, f *orderFake) (*Deps, *Call) {
	t.Helper()
	deps, call, closeFn := directDeps(t, f, 5*time.Second)
	t.Cleanup(closeFn)
	return deps, call
}

func orderArgs() (name, plan, region, os, cycle, key string) {
	return "web-1", "plan-1", "lag", "os-1", "", "key-1"
}

func placeOrder(t *testing.T, f *orderFake, key string) (*ServerOrderResult, error) {
	t.Helper()
	deps, call := newDirect(t, f)
	name, plan, region, os, cycle, _ := orderArgs()
	return orderServer(context.Background(), deps, call, name, plan, region, os, cycle, key)
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestNairaOrderReturnsAHostedLinkAndCreatesOneDraft(t *testing.T) {
	f := newOrderFake()
	res, err := placeOrder(t, f, "key-1")
	require.NoError(t, err)

	assert.Equal(t, "https://checkout.paystack.test/abc", res.PaymentURL)
	assert.Empty(t, res.PortalCheckoutURL, "a naira order is paid on a hosted page, not on the portal")
	assert.Equal(t, "NGN", res.Currency)
	assert.Equal(t, 1, f.created, "state change: exactly one draft order")
	assert.Equal(t, 0, f.count("GET", "/servers/catalog/usd"), "a naira account never reads the dollar catalog")

	orderReq := f.last("POST", "/servers/orders")
	assert.Equal(t, "key-1", orderReq.Headers.Get("Idempotency-Key"))
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(orderReq.Body), &body))
	assert.Equal(t, "Small", body["planName"], "names and hardware come from the catalog, not from the agent")
	assert.EqualValues(t, 2, body["vcpu"])
	assert.Equal(t, "NGN", body["currency"])
	assert.Equal(t, "monthly", body["billingCycle"], "monthly when left out")
	assert.Equal(t, "Ubuntu", body["osName"])

	checkout := f.last("POST", "/servers/orders/00000000-0000-0000-0000-000000000001/checkout")
	assert.Equal(t, "key-1:checkout", checkout.Headers.Get("Idempotency-Key"), "the checkout has its own derived key")
	assert.Equal(t, "{}", strings.TrimSpace(checkout.Body), "the tool never chooses the return address")
	assert.Contains(t, res.NextStep, "cannot pay it for them")
}

func TestDollarOrderNeverChargesTheWalletAndHandsOverThePortalLink(t *testing.T) {
	f := newOrderFake()
	f.dollar = true
	res, err := placeOrder(t, f, "key-usd")
	require.NoError(t, err)

	assert.Equal(t, 0, f.charges, "the wallet was charged: the tool called the checkout route of a dollar order")
	assert.Equal(t, 0, f.count("POST", "/servers/orders/00000000-0000-0000-0000-000000000001/checkout"))
	assert.Equal(t, "https://portal.example.test/portal/billing/checkout?orderId=00000000-0000-0000-0000-000000000001", res.PortalCheckoutURL)
	assert.Empty(t, res.PaymentURL)
	assert.Equal(t, "USD", res.Currency)
	require.NotNil(t, res.WalletCoversOrder)
	assert.True(t, *res.WalletCoversOrder)
	assert.Nil(t, res.ShortfallMinor)
	assert.Contains(t, res.NextStep, "confirm the payment there themselves")
	assert.Equal(t, 1, f.count("GET", "/servers/catalog/usd"))
	assert.Equal(t, 0, f.count("GET", "/servers/catalog"))
}

func TestDollarOrderWithAShortWalletSaysSoAndPointsAtTheTopUp(t *testing.T) {
	f := newOrderFake()
	f.dollar, f.walletAvailable, f.shortfall = true, 200, 700
	s := int64(1000)
	f.suggested = &s
	res, err := placeOrder(t, f, "key-short")
	require.NoError(t, err)

	require.NotNil(t, res.WalletCoversOrder)
	assert.False(t, *res.WalletCoversOrder)
	require.NotNil(t, res.ShortfallMinor)
	assert.EqualValues(t, 700, *res.ShortfallMinor)
	require.NotNil(t, res.SuggestedTopUpMinor)
	assert.EqualValues(t, 1000, *res.SuggestedTopUpMinor)
	assert.Contains(t, res.NextStep, "create_wallet_topup_link")
	assert.Contains(t, res.NextStep, "suggestedTopUpMinor")
	assert.Equal(t, 0, f.charges)
}

func TestDollarOrderWhereOneTopUpCannotCoverItSaysSo(t *testing.T) {
	f := newOrderFake()
	f.dollar, f.shortfall = true, 99_999_999
	res, err := placeOrder(t, f, "key-multi")
	require.NoError(t, err)
	require.NotNil(t, res.WalletCoversOrder)
	assert.Nil(t, res.SuggestedTopUpMinor)
	assert.Contains(t, res.NextStep, "more than once")
}

func TestListServerPlansReadsTheCatalogOfTheAccountsCurrency(t *testing.T) {
	// naira: the dollar price the public catalog carries is not this account's price and is removed
	f := newOrderFake()
	deps, call := newDirect(t, f)
	spec := findSpec(t, "list_server_plans")
	assert.True(t, spec.ReadOnly)
	cur, err := serverBillingCurrency(context.Background(), deps, call)
	require.NoError(t, err)
	assert.Equal(t, "NGN", cur)
	raw, _, err := serverCatalogFor(context.Background(), deps, call, cur)
	require.NoError(t, err)
	item := omitSecrets(raw)
	stripUsdFields(item)
	plan := item["plans"].([]any)[0].(map[string]any)
	assert.NotContains(t, plan, "usdMonthlyPriceMinor")
	assert.NotContains(t, plan, "stripeEligible")
	assert.EqualValues(t, 900000, plan["monthlyPriceMinor"])

	// dollar: the dollar catalog is read
	f2 := newOrderFake()
	f2.dollar = true
	deps2, call2 := newDirect(t, f2)
	cur2, err := serverBillingCurrency(context.Background(), deps2, call2)
	require.NoError(t, err)
	assert.Equal(t, "USD", cur2)
	_, _, err = serverCatalogFor(context.Background(), deps2, call2, cur2)
	require.NoError(t, err)
	assert.Equal(t, 1, f2.count("GET", "/servers/catalog/usd"))
}

func TestPartnerKeyDoesNotChangeTheMoneyRule(t *testing.T) {
	f := newOrderFake()
	f.dollar = true
	deps, _ := newDirect(t, f)
	call := &Call{Session: &Session{Token: "btk_test-token", Identity: &Identity{CanSpendWithoutCheckout: true, SpendCapMinor: 5_000_000}}}
	name, plan, region, os, cycle, _ := orderArgs()
	res, err := orderServer(context.Background(), deps, call, name, plan, region, os, cycle, "key-partner")
	require.NoError(t, err)
	assert.Equal(t, 0, f.charges)
	assert.NotEmpty(t, res.PortalCheckoutURL)
}

func findSpec(t *testing.T, name string) toolSpec {
	t.Helper()
	for _, s := range AllTools().Specs() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no tool %s", name)
	return toolSpec{}
}

func TestWalletTopUpToolSendsOnlyTheAmountAndReturnsThePage(t *testing.T) {
	f := newOrderFake()
	f.dollar = true
	session := connectToHandler(t, f.handler())
	res := callTool(t, session, "create_wallet_topup_link", map[string]any{"amountMinor": 2000, "idempotencyKey": "tu-key"})
	require.False(t, res.IsError, resultText(res))

	req := f.last("POST", "/billing/wallet/top-ups")
	assert.Equal(t, "tu-key", req.Headers.Get("Idempotency-Key"))
	assert.JSONEq(t, `{"amountMinor":2000}`, req.Body, "no consent, no auto top-up and no order id can be sent through the tool")
	assert.Equal(t, 1, f.topUps)
	text := resultText(res)
	assert.Contains(t, text, "https://checkout.stripe.test/c/pay/cs_test_1")
	assert.Contains(t, text, "cannot pay it for them")
}

func TestServerToolsExistWithTheScopesTheyNeed(t *testing.T) {
	for name, want := range map[string][]string{
		"list_server_plans": {"billing:read"}, "quote_server_order": {"billing:read"},
		"order_server": {"billing:read", "billing:checkout"}, "create_wallet_topup_link": {"billing:checkout"},
	} {
		assert.ElementsMatch(t, want, findSpec(t, name).Scopes, name)
	}
	assert.True(t, findSpec(t, "quote_server_order").ReadOnly)
	assert.False(t, findSpec(t, "order_server").ReadOnly)
	assert.False(t, findSpec(t, "order_server").Destructive)
}

func TestQuoteServerOrderPricesLiveAndCreatesNothing(t *testing.T) {
	f := newOrderFake()
	session := connectToHandler(t, f.handler())
	res := callTool(t, session, "quote_server_order", map[string]any{"planId": "plan-1", "osId": "os-1", "billingCycle": "annual"})
	require.False(t, res.IsError, resultText(res))
	assert.Contains(t, resultText(res), "967500")
	assert.Equal(t, 0, f.created)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.last("POST", "/servers/orders/quote").Body), &body))
	assert.Equal(t, "annual", body["billingCycle"])
	assert.Equal(t, "NGN", body["currency"])
}

// ---------------------------------------------------------------------------
// Boundaries
// ---------------------------------------------------------------------------

func TestOrderBoundariesAreRefusedBeforeAnythingIsCreated(t *testing.T) {
	cases := []struct {
		label                         string
		name, plan, region, os, cycle string
		key                           string
		want                          string
	}{
		{"empty key", "web-1", "plan-1", "lag", "os-1", "", "", "idempotency"},
		{"blank key", "web-1", "plan-1", "lag", "os-1", "", "   ", "idempotency"},
		{"key too long", "web-1", "plan-1", "lag", "os-1", "", strings.Repeat("k", 250), "too long"},
		{"key with a colon", "web-1", "plan-1", "lag", "os-1", "", "abc:checkout", "colon"},
		{"name too short", "a", "plan-1", "lag", "os-1", "", "k", "name has to be"},
		{"name empty", "", "plan-1", "lag", "os-1", "", "k", "name has to be"},
		{"name too long", strings.Repeat("n", 256), "plan-1", "lag", "os-1", "", "k", "name has to be"},
		{"bad cycle", "web-1", "plan-1", "lag", "os-1", "weekly", "k", "billingCycle"},
		{"unknown plan", "web-1", "nope", "lag", "os-1", "", "k", "no plan with id"},
		{"empty plan", "web-1", "", "lag", "os-1", "", "k", "no plan with id"},
		{"unknown region", "web-1", "plan-1", "xx", "os-1", "", "k", "no region with code"},
		{"empty region", "web-1", "plan-1", "", "os-1", "", "k", "regionCode is required"},
		{"os of another plan", "web-1", "plan-1", "lag", "os-9", "", "k", "not offered with that plan"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			f := newOrderFake()
			deps, call := newDirect(t, f)
			_, err := orderServer(context.Background(), deps, call, c.name, c.plan, c.region, c.os, c.cycle, c.key)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
			assert.Equal(t, 0, f.created, "nothing was created")
			assert.Equal(t, 0, f.count("POST", "/servers/orders"))
		})
	}
}

func TestOrderBoundaryAcceptsEveryBillingCycleAndTheLengthLimits(t *testing.T) {
	for _, cycle := range []string{"", "monthly", "QUARTERLY", " semiannual ", "annual"} {
		f := newOrderFake()
		deps, call := newDirect(t, f)
		_, err := orderServer(context.Background(), deps, call, "ab", "plan-1", "lag", "os-1", cycle, "k")
		require.NoError(t, err, cycle)
	}
	f := newOrderFake()
	deps, call := newDirect(t, f)
	_, err := orderServer(context.Background(), deps, call, strings.Repeat("n", 255), "plan-1", "lag", "os-1", "", strings.Repeat("k", 246))
	require.NoError(t, err, "a 255 character name and a key whose derived checkout key is exactly 255 characters (246 plus the 9 characters of :checkout)")
}

func TestEmptyCatalogIsRefusedWithAnInstructionNotAnOrder(t *testing.T) {
	f := newOrderFake()
	f.catalog = map[string]any{"regions": []any{}, "plans": []any{}, "operatingSystems": []any{}}
	_, err := placeOrder(t, f, "k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list_server_plans")
	assert.Equal(t, 0, f.created)

	plans := callListPlans(t, f)
	assert.Contains(t, plans, "0 server plan(s)")
}

func callListPlans(t *testing.T, f *orderFake) string {
	t.Helper()
	session := connectToHandler(t, f.handler())
	res := callTool(t, session, "list_server_plans", map[string]any{})
	require.False(t, res.IsError, resultText(res))
	return resultText(res)
}

func TestTopUpBoundariesAreRefusedWithoutCallingTheAPI(t *testing.T) {
	for _, amount := range []int64{0, -1, -1000} {
		f := newOrderFake()
		f.dollar = true
		session := connectToHandler(t, f.handler())
		res := callTool(t, session, "create_wallet_topup_link", map[string]any{"amountMinor": amount, "idempotencyKey": "k"})
		assert.True(t, res.IsError, "amount %d", amount)
		assert.Contains(t, resultText(res), "positive number of cents")
		assert.Equal(t, 0, f.topUps)
		assert.Equal(t, 0, f.count("POST", "/billing/wallet/top-ups"))
	}
	f := newOrderFake()
	session := connectToHandler(t, f.handler())
	res := callTool(t, session, "create_wallet_topup_link", map[string]any{"amountMinor": 1000, "idempotencyKey": "  "})
	assert.True(t, res.IsError)
	assert.Contains(t, resultText(res), "idempotency")
	assert.Equal(t, 0, f.count("POST", "/billing/wallet/top-ups"))
}

func TestTopUpAtTheSmallestAndLargestAmountsIsPassedThrough(t *testing.T) {
	// The tool does not know the limits: the API decides, so the boundary values reach it unchanged.
	for _, amount := range []int64{1, 1000, 100000, 9_999_999_999} {
		f := newOrderFake()
		session := connectToHandler(t, f.handler())
		res := callTool(t, session, "create_wallet_topup_link", map[string]any{"amountMinor": amount, "idempotencyKey": "k"})
		require.False(t, res.IsError, resultText(res))
		assert.JSONEq(t, fmt.Sprintf(`{"amountMinor":%d}`, amount), f.last("POST", "/billing/wallet/top-ups").Body)
	}
}

// ---------------------------------------------------------------------------
// Error and fault injection
// ---------------------------------------------------------------------------

func TestTopUpRefusalsBecomeInstructionsTheAgentCanActOn(t *testing.T) {
	limits := map[string]any{"minMinor": 1000, "maxMinor": 100000, "stepMinor": 1000}
	cases := []struct {
		label  string
		status int
		code   string
		data   map[string]any
		want   []string
	}{
		{"wallet off", 404, "wallet_not_available", nil, []string{"not open for this account", "create_topup_link"}},
		{"naira account", 422, "wallet_wrong_billing_track", nil, []string{"billed in naira", "create_topup_link"}},
		{"profile incomplete", 422, "profile_incomplete", nil, []string{"billing country", "Tell them and stop"}},
		{"org role", 403, "wallet_org_role_required", nil, []string{"organisation owner or admin"}},
		{"below minimum", 422, "amount_out_of_range", limits, []string{"minimum of 1000", "maximum of 100000", "multiples of 1000", "cents"}},
		{"off step", 422, "amount_not_on_step", limits, []string{"multiples of 1000"}},
		{"frozen", 403, "wallet_frozen", nil, []string{"frozen", "contact support"}},
		{"rate limited", 429, "wallet_topup_rate_limited", nil, []string{"Wait a few minutes"}},
		{"card payments down", 503, "card_payments_unavailable", nil, []string{"not available right now"}},
		{"consent forbidden", 403, "wallet_consent_requires_human", nil, []string{"only be done by the account owner"}},
		{"upstream 500", 500, "internal_error", nil, []string{"temporary error", "retry once"}},
		{"upstream 502", 502, "", nil, []string{"temporary error"}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			f := newOrderFake()
			f.dollar = true
			f.failure["POST /billing/wallet/top-ups"] = fakeFailure{status: c.status, code: c.code, msg: "refused", data: c.data}
			session := connectToHandler(t, f.handler())
			res := callTool(t, session, "create_wallet_topup_link", map[string]any{"amountMinor": 500, "idempotencyKey": "k"})
			require.True(t, res.IsError)
			for _, w := range c.want {
				assert.Contains(t, resultText(res), w)
			}
			assert.Equal(t, 0, f.topUps, "no top-up was created")
		})
	}
}

func TestOrderRefusalsAreInstructionsAndCreateNothing(t *testing.T) {
	cases := []struct {
		label  string
		route  string
		status int
		code   string
		want   string
	}{
		{"currency mismatch", "POST /servers/orders", 422, "currency_mismatch", "billing currency changed"},
		{"plan not in dollars", "POST /servers/orders", 422, "plan_not_available_in_usd", "cannot be bought in dollars"},
		{"spec mismatch", "POST /servers/orders", 422, "spec_mismatch", "hardware differs"},
		{"paused supplier", "POST /servers/orders", 503, "service_paused", "do not retry"},
		{"order 5xx", "POST /servers/orders", 500, "internal_error", "temporary error"},
		{"profile incomplete", "POST /servers/orders", 422, "profile_incomplete", "Tell them and stop"},
		{"idempotency reuse", "POST /servers/orders", 422, "idempotency_key_reused", "start the operation again"},
		{"idempotency in flight", "POST /servers/orders", 409, "idempotency_in_progress", "still running"},
		{"status 5xx", "GET /billing/wallet/status", 500, "internal_error", "temporary error"},
		{"catalog 5xx", "GET /servers/catalog", 503, "", "temporary error"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			f := newOrderFake()
			f.failure[c.route] = fakeFailure{status: c.status, code: c.code, msg: "refused"}
			_, err := placeOrder(t, f, "k")
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
			assert.Equal(t, 0, f.created)
			assert.Equal(t, 0, f.charges)
		})
	}
}

func TestPaystackCheckoutFailureAfterTheDraftNamesTheOrderAndSaysToRetryWithTheSameKey(t *testing.T) {
	for _, status := range []int{500, 422, 503} {
		f := newOrderFake()
		f.failure["POST /servers/orders/00000000-0000-0000-0000-000000000001/checkout"] = fakeFailure{status: status, code: "", msg: "paystack down"}
		_, err := placeOrder(t, f, "key-x")
		require.Error(t, err, status)
		assert.Contains(t, err.Error(), "00000000-0000-0000-0000-000000000001", "the draft that exists is named")
		assert.Contains(t, err.Error(), "same idempotencyKey")
		assert.Equal(t, 1, f.created)
	}
	// and the retry with that key reuses the draft and succeeds once the checkout works
	f := newOrderFake()
	failing := "POST /servers/orders/00000000-0000-0000-0000-000000000001/checkout"
	f.failure[failing] = fakeFailure{status: 500, msg: "down"}
	deps, call := newDirect(t, f)
	name, plan, region, os, cycle, _ := orderArgs()
	_, err := orderServer(context.Background(), deps, call, name, plan, region, os, cycle, "key-r")
	require.Error(t, err)
	f.mu.Lock()
	delete(f.failure, failing)
	f.mu.Unlock()
	res, err := orderServer(context.Background(), deps, call, name, plan, region, os, cycle, "key-r")
	require.NoError(t, err)
	assert.Equal(t, 1, f.created, "the retry did not create a second order")
	assert.NotEmpty(t, res.PaymentURL)
}

func TestMissingPaymentPageIsNeverPromised(t *testing.T) {
	f := newOrderFake()
	f.checkoutURL = ""
	// the fake leaves authorizationUrl out when checkoutURL is empty
	_, err := placeOrder(t, f, "k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no payment page")
	assert.Contains(t, err.Error(), "do not say a page is waiting")

	// a top-up answer without checkoutUrl
	res := walletTopUpResult(map[string]any{"id": "tu-1", "status": "credited", "amountMinor": float64(1000)})
	assert.Empty(t, res.PaymentURL)
	assert.Contains(t, res.NextStep, "Do not tell the customer anything was added")
	assert.NotContains(t, res.NextStep, "Give the customer this link")
}

func TestDollarOrderWithoutAPortalLinkAdmitsItAndInventsNone(t *testing.T) {
	f := newOrderFake()
	f.dollar, f.omitPortalLink = true, true
	res, err := placeOrder(t, f, "k")
	require.NoError(t, err)
	assert.Empty(t, res.PortalCheckoutURL)
	assert.Contains(t, res.NextStep, "Do not invent a link")
	assert.Equal(t, 0, f.charges)
}

func TestDollarOrderWhoseQuoteFailsLeavesTheBalanceUnknown(t *testing.T) {
	for _, mode := range []string{"5xx", "no balance"} {
		f := newOrderFake()
		f.dollar = true
		if mode == "5xx" {
			f.failure["POST /servers/orders/quote"] = fakeFailure{status: 500, msg: "boom"}
		} else {
			f.noBalance = true
		}
		res, err := placeOrder(t, f, "k-"+mode)
		require.NoError(t, err, mode)
		assert.Nil(t, res.WalletCoversOrder, mode)
		assert.Contains(t, res.NextStep, "do not say whether it does", mode)
		assert.NotEmpty(t, res.PortalCheckoutURL)
	}
}

func TestOrderAnswerWithNoOrderIdIsNotTreatedAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/api/v1") {
		case "/billing/wallet/status":
			ok(w, map[string]any{"available": false})
		case "/servers/catalog":
			ok(w, defaultServerCatalog(false))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"status":201,"success":true,"message":"x","data":{"status":"draft"}}`)
		}
	}))
	defer srv.Close()
	deps := &Deps{Client: client.New(srv.URL, "test")}
	call := &Call{Session: &Session{Token: "t", Identity: &Identity{}}}
	_, err := orderServer(context.Background(), deps, call, "web-1", "plan-1", "lag", "os-1", "", "k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no order id")
}

func TestMalformedAndSlowAPIAnswersAreReportedNotCrashedOn(t *testing.T) {
	// malformed JSON from an edge page
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	defer bad.Close()
	deps := &Deps{Client: client.New(bad.URL, "test")}
	call := &Call{Session: &Session{Token: "t", Identity: &Identity{}}}
	_, err := orderServer(context.Background(), deps, call, "web-1", "plan-1", "lag", "os-1", "", "k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not JSON")

	// a catalog whose plans are the wrong type
	f := newOrderFake()
	f.catalog = map[string]any{"plans": "none"}
	_, err = placeOrder(t, f, "k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected shape")

	// a timeout: the API takes longer than the client allows
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		ok(w, map[string]any{"available": false})
	}))
	defer slow.Close()
	cl := client.New(slow.URL, "test")
	cl.HTTP.Timeout = 50 * time.Millisecond
	_, err = orderServer(context.Background(), &Deps{Client: cl}, call, "web-1", "plan-1", "lag", "os-1", "", "k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "calling the TechBeaver API")

	// a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f2 := newOrderFake()
	deps2, call2 := newDirect(t, f2)
	_, err = orderServer(ctx, deps2, call2, "web-1", "plan-1", "lag", "os-1", "", "k")
	require.Error(t, err)
	assert.Equal(t, 0, f2.created)

	// a connection reset mid-answer
	reset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer reset.Close()
	_, err = orderServer(context.Background(), &Deps{Client: client.New(reset.URL, "test")}, call, "web-1", "plan-1", "lag", "os-1", "", "k")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

func TestOrderAndTopUpResultsCarryNoCredential(t *testing.T) {
	f := newOrderFake()
	f.extraOrderFields = map[string]any{
		"clientSecret": "sek-1", "providerToken": "tok-1",
		"leakedUri": "postgres://u:pw-1@db.example.test/x",
	}
	res, err := placeOrder(t, f, "k")
	require.NoError(t, err)
	encoded, _ := json.Marshal(res)
	for _, secret := range []string{"sek-1", "tok-1", "pw-1"} {
		assert.NotContains(t, string(encoded), secret)
	}

	top := walletTopUpResult(omitSecrets(map[string]any{"checkoutUrl": "https://x.test/p", "clientSecret": "sek-2", "amountMinor": float64(1000)}))
	encoded, _ = json.Marshal(top)
	assert.NotContains(t, string(encoded), "sek-2")
	assert.Contains(t, string(encoded), "https://x.test/p")
}

// ---------------------------------------------------------------------------
// Races
// ---------------------------------------------------------------------------

func TestConcurrentOrdersWithOneKeyCreateOneDraft(t *testing.T) {
	for _, dollar := range []bool{false, true} {
		f := newOrderFake()
		f.dollar = dollar
		deps, call := newDirect(t, f)
		const n = 24
		ids := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res, err := orderServer(context.Background(), deps, call, "web-1", "plan-1", "lag", "os-1", "", "same-key")
				errs[i] = err
				if res != nil {
					ids[i] = res.OrderID
				}
			}(i)
		}
		wg.Wait()
		for i := range errs {
			require.NoError(t, errs[i])
			assert.Equal(t, ids[0], ids[i], "every retry returns the first order")
		}
		f.mu.Lock()
		created, charges := f.created, f.charges
		f.mu.Unlock()
		assert.Equal(t, 1, created, "dollar=%v: concurrent retries of one key made %d drafts", dollar, created)
		assert.Equal(t, 0, charges)
	}
}

func TestConcurrentOrdersWithDifferentKeysAreIndependent(t *testing.T) {
	f := newOrderFake()
	deps, call := newDirect(t, f)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := orderServer(context.Background(), deps, call, "web-1", "plan-1", "lag", "os-1", "", fmt.Sprintf("key-%d", i))
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 10, f.created)
}

func TestConcurrentTopUpsWithOneKeyMakeOnePage(t *testing.T) {
	f := newOrderFake()
	f.dollar = true
	session := connectToHandler(t, f.handler())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "create_wallet_topup_link", Arguments: map[string]any{"amountMinor": 3000, "idempotencyKey": "one"}})
			assert.NoError(t, err)
			assert.False(t, res.IsError)
		}()
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, 1, f.topUps)
}

func connectToHandler(t *testing.T, h http.Handler) *mcpsdk.ClientSession {
	t.Helper()
	apiServer := httptest.NewServer(h)
	t.Cleanup(apiServer.Close)
	apiClient := client.New(apiServer.URL, "test")
	deps := &Deps{Client: apiClient, Auth: NewAuthenticator(apiClient), Config: Config{APIBaseURL: apiServer.URL}}
	identity, err := deps.Auth.Resolve(context.Background(), "btk_test-token")
	require.NoError(t, err)
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = serve(ctx, deps, "btk_test-token", identity, serverTransport) }()
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-agent", Version: "1"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}
