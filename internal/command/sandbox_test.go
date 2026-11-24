package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

const (
	testProject = "11111111-2222-3333-4444-555555555555"
	testPlan    = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testSubID   = "99999999-8888-7777-6666-555555555555"
)

type sbRequest struct {
	method, path, query, auth, idem string
	body                            string
}

type sbServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []sbRequest
}

func newSBServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *sbServer {
	t.Helper()
	s := &sbServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, sbRequest{
			method: r.Method, path: strings.TrimPrefix(r.URL.Path, "/api/v1"), query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), idem: r.Header.Get("Idempotency-Key"), body: string(raw),
		})
		n := len(s.reqs)
		s.mu.Unlock()
		h(w, r, n)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sbServer) requests() []sbRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sbRequest(nil), s.reqs...)
}

func (s *sbServer) only(t *testing.T) sbRequest {
	t.Helper()
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly one request, got %d: %+v", len(reqs), reqs)
	}
	return reqs[0]
}

func sbOK(w http.ResponseWriter, raw string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":200,"success":true,"message":"ok","data":%s}`, raw)
}

func sbFail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"status": status, "success": false, "message": message}
	if code != "" {
		body["code"] = code
		body["error"] = code
	}
	_ = json.NewEncoder(w).Encode(body)
}

func fixedSB(raw string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) { sbOK(w, raw) }
}

func fastBackoff(t *testing.T) {
	t.Helper()
	saved := chargeBackoff
	chargeBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { chargeBackoff = saved })
}

func sbRun(t *testing.T, api *sbServer, args ...string) (string, string, error) {
	t.Helper()
	env, out, errBuf := newTestEnv(t, api.URL)
	err := run(t, env, args...)
	return out.String(), errBuf.String(), err
}

const plansJSON = `[{"id":"` + testPlan + `","name":"Small","code":"small","vcpu":2,"memoryMb":4096,"diskGb":20,
"maxOutboundGbMonth":100,"idleStopMinutes":30,"maxRunHours":24,"snapshotsPerSlot":1,
"prices":[{"currency":"NGN","monthlyPriceMinor":150000,"hourlyPriceMinor":500},{"currency":"USD","monthlyPriceMinor":100,"hourlyPriceMinor":1}]}]`

func TestPlansListsPricesAndSendsTheRightRequest(t *testing.T) {
	api := newSBServer(t, fixedSB(plansJSON))
	out, _, err := sbRun(t, api, "sandbox", "plans")
	if err != nil {
		t.Fatal(err)
	}
	req := api.only(t)
	if req.method != "GET" || req.path != "/sandbox/plans" || req.query != "" {
		t.Fatalf("unexpected request %+v", req)
	}
	if req.auth != "Bearer btk_test" {
		t.Fatalf("the credential was not sent: %q", req.auth)
	}
	var plans []map[string]any
	if err := json.Unmarshal([]byte(out), &plans); err != nil || len(plans) != 1 || plans[0]["code"] != "small" {
		t.Fatalf("a pipe must receive the API's plans as JSON, got %q (%v)", out, err)
	}
}

func TestPlansTableFormatsMinorUnitsWithTheAPIsCurrency(t *testing.T) {
	api := newSBServer(t, fixedSB(plansJSON))
	out, _, err := sbRun(t, api, "sandbox", "plans", "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NGN 1,500.00", "NGN 5.00", "USD 1.00", "Small"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestPlansListAliasAndCurrencyFilter(t *testing.T) {
	api := newSBServer(t, fixedSB(plansJSON))
	out, _, err := sbRun(t, api, "sandbox", "plans", "list", "--currency", "usd", "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "USD 1.00") || strings.Contains(out, "NGN") {
		t.Fatalf("the currency filter did not apply:\n%s", out)
	}
	out, _, err = sbRun(t, api, "sandbox", "plans", "--currency", "GBP")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("a currency nobody sells in should list nothing, got %q", out)
	}
}

func TestPlansBoundaries(t *testing.T) {
	cases := map[string]string{
		"empty list":       `[]`,
		"null data":        `null`,
		"no prices":        `[{"id":"p","name":"` + strings.Repeat("x", 500) + `"}]`,
		"huge price":       `[{"id":"p","name":"n","prices":[{"currency":"NGN","monthlyPriceMinor":9223372036854775807,"hourlyPriceMinor":0}]}]`,
		"wrapped in items": `{"items":[{"id":"p","name":"n"}]}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			api := newSBServer(t, fixedSB(data))
			for _, format := range []string{"json", "table", "yaml"} {
				if _, _, err := sbRun(t, api, "sandbox", "plans", "--format", format); err != nil {
					t.Fatalf("%s: %v", format, err)
				}
			}
		})
	}
	api := newSBServer(t, fixedSB(cases["huge price"]))
	out, _, _ := sbRun(t, api, "sandbox", "plans", "--format", "table")
	if !strings.Contains(out, "NGN 92,233,720,368,547,758.07") {
		t.Fatalf("a very large amount lost precision:\n%s", out)
	}
	out, _, _ = sbRun(t, api, "sandbox", "plans")
	if !strings.Contains(out, "9223372036854775807") {
		t.Fatalf("raw JSON lost precision:\n%s", out)
	}
	api = newSBServer(t, fixedSB(`[]`))
	out, _, _ = sbRun(t, api, "sandbox", "plans", "--format", "table")
	if !strings.Contains(out, "No results.") {
		t.Fatalf("an empty catalogue should say so, got %q", out)
	}
}

const usageJSON = `{"projectId":"` + testProject + `","subscriptions":[{"id":"` + testSubID + `","projectId":"` + testProject + `",
"planId":"` + testPlan + `","planName":"Small","billingMode":"monthly","status":"active","slots":3,"slotsInUse":1,"cancelAtPeriodEnd":false,
"currency":"NGN","unitMonthlyMinor":150000,"hourlyMinor":500,"currentPeriodEnd":"2026-12-01T00:00:00Z","nextChargeMinor":450000,"nextChargeSlots":3}],
"balance":{"currency":"NGN","availableMinor":1234567,"supported":true},"hourlyBurnMinor":1500,"hoursRemaining":823,"lowBalance":false,
"balanceHidden":false,"periodStart":"2026-11-01T00:00:00Z","periodEnd":"2026-11-24T10:00:00Z",
"usage":[{"sandboxId":"s1","name":"dev box","subscriptionId":"` + testSubID + `","billingMode":"hourly","status":"running","seconds":3600,"minutes":60,"billedSeconds":3600,"estimatedAmountMinor":500}],
"billingRule":"Billed per second, one minute minimum per start."}`

func TestUsageSendsTheProjectAndFormatsTheFigures(t *testing.T) {
	api := newSBServer(t, fixedSB(usageJSON))
	out, _, err := sbRun(t, api, "sandbox", "usage", "--project", testProject, "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	req := api.only(t)
	if req.method != "GET" || req.path != "/sandbox/usage" || req.query != "projectId="+testProject {
		t.Fatalf("unexpected request %+v", req)
	}
	for _, want := range []string{"NGN 12,345.67", "NGN 15.00 per hour", "823", "dev box", "NGN 5.00", "NGN 1,500.00", "NGN 4,500.00", "Billed per second"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output lacks %q:\n%s", want, out)
		}
	}
	jsonOut, _, err := sbRun(t, api, "sandbox", "usage", "--project", testProject)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(jsonOut), &obj) != nil || obj["hourlyBurnMinor"] == nil {
		t.Fatalf("--json style output must pass the API object through: %q", jsonOut)
	}
}

func TestUsageSaysTheBalanceIsHiddenNeverZero(t *testing.T) {
	hidden := `{"projectId":"` + testProject + `","subscriptions":[],"balanceHidden":true,"periodStart":"a","periodEnd":"b","usage":[],"billingRule":"r"}`
	api := newSBServer(t, fixedSB(hidden))
	out, _, err := sbRun(t, api, "sandbox", "usage", "--project", testProject, "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hidden for your role") {
		t.Fatalf("the output must say the balance is hidden:\n%s", out)
	}
	for _, bad := range []string{"0.00", "Hourly burn", "Hours remaining"} {
		if strings.Contains(out, bad) {
			t.Errorf("a hidden balance must not print %q:\n%s", bad, out)
		}
	}
}

func TestUsageBoundaries(t *testing.T) {
	cases := map[string]struct {
		data string
		want []string
	}{
		"nothing running": {
			`{"projectId":"p","subscriptions":[],"balance":{"currency":"USD","availableMinor":0,"supported":true},"hourlyBurnMinor":0,"hoursRemaining":null,"balanceHidden":false,"periodStart":"a","periodEnd":"b","usage":[],"billingRule":"r"}`,
			[]string{"USD 0.00", "none, nothing is running", "None yet", "None used"}},
		"low balance": {
			`{"projectId":"p","subscriptions":[],"balance":{"currency":"NGN","availableMinor":100,"supported":true},"hourlyBurnMinor":500,"hoursRemaining":0,"lowBalance":true,"balanceHidden":false,"usage":[]}`,
			[]string{"0 (low balance)"}},
		"balance unsupported": {
			`{"projectId":"p","subscriptions":[],"balance":{"currency":"NGN","availableMinor":0,"supported":false,"reason":"Your billing details are not set up yet."},"balanceHidden":false,"usage":[]}`,
			[]string{"not available: Your billing details are not set up yet."}},
		"balance missing": {
			`{"projectId":"p","subscriptions":[],"balanceHidden":false,"usage":[]}`,
			[]string{"not reported"}},
		"huge balance": {
			`{"projectId":"p","subscriptions":[],"balance":{"currency":"NGN","availableMinor":9007199254740993,"supported":true},"balanceHidden":false,"usage":[]}`,
			[]string{"NGN 90,071,992,547,409.93"}},
		"odd shapes": {
			`{"projectId":"p","subscriptions":[{"id":"x","unitMonthlyMinor":"oops"}],"balance":{"currency":"NGN","availableMinor":"abc","supported":true},"usage":[{"name":"n","estimatedAmountMinor":1.5}],"balanceHidden":false}`,
			[]string{"abc"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newSBServer(t, fixedSB(tc.data))
			out, _, err := sbRun(t, api, "sandbox", "usage", "--project", testProject, "--format", "table")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestProjectSelectionForSandboxCommands(t *testing.T) {
	api := newSBServer(t, fixedSB(usageJSON))
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no project", []string{"sandbox", "usage"}, "no project selected"},
		{"a project name", []string{"sandbox", "usage", "--project", "my-site"}, "project id"},
		{"an injected path", []string{"sandbox", "usage", "--project", "../x"}, "project id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sbRun(t, api, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
	if n := len(api.requests()); n != 0 {
		t.Fatalf("a bad project selection reached the network %d times", n)
	}

	env, _, _ := newTestEnv(t, api.URL)
	env.Project = testProject
	if err := run(t, env, "sandbox", "usage"); err != nil {
		t.Fatalf("a project from config must be honoured: %v", err)
	}
	if got := api.only(t).query; got != "projectId="+testProject {
		t.Fatalf("query %q", got)
	}
}

func TestSubscriptionsListAndGet(t *testing.T) {
	api := newSBServer(t, fixedSB(usageJSON))
	out, _, err := sbRun(t, api, "sandbox", "subscriptions", "--project", testProject, "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{testSubID, "Small", "NGN 1,500.00", "NGN 4,500.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	out, _, err = sbRun(t, api, "sandbox", "subscriptions", testSubID, "--project", testProject)
	if err != nil {
		t.Fatal(err)
	}
	var one map[string]any
	if json.Unmarshal([]byte(out), &one) != nil || one["id"] != testSubID {
		t.Fatalf("get must return the one subscription as the API gave it: %q", out)
	}
	out, _, _ = sbRun(t, api, "sandbox", "subscriptions", testSubID, "--project", testProject, "--format", "table")
	if !strings.Contains(out, "NGN 4,500.00") {
		t.Fatalf("detail view lacks the formatted next charge:\n%s", out)
	}
	_, _, err = sbRun(t, api, "sandbox", "subscriptions", "nope", "--project", testProject)
	if err == nil || exitcode.From(err) != exitcode.NotFound {
		t.Fatalf("an unknown subscription must exit %d, got %v", exitcode.NotFound, err)
	}
	empty := newSBServer(t, fixedSB(`{"projectId":"p","subscriptions":[],"usage":[]}`))
	out, _, err = sbRun(t, empty, "sandbox", "subscriptions", "--project", testProject)
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("no subscriptions must be an empty list, got %q (%v)", out, err)
	}
}

const quoteJSON = `{"planId":"` + testPlan + `","planName":"Small","slots":2,"currency":"USD","provider":"wallet","unitMonthlyMinor":100,
"hourlyMinor":1,"subtotalMinor":200,"taxMinor":15,"totalMinor":215,"periodStart":"2026-11-24T00:00:00Z","periodEnd":"2026-12-24T00:00:00Z","walletAvailableMinor":50}`

func TestQuoteSendsIdsAndACountNeverMoney(t *testing.T) {
	api := newSBServer(t, fixedSB(quoteJSON))
	out, _, err := sbRun(t, api, "sandbox", "quote", "--project", testProject, "--plan", testPlan, "--slots", "2", "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	req := api.only(t)
	if req.method != "POST" || req.path != "/sandbox/billing/quote" {
		t.Fatalf("unexpected request %+v", req)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(req.body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 3 || body["projectId"] != testProject || body["planId"] != testPlan || body["slots"] != float64(2) {
		t.Fatalf("the quote body must be exactly project, plan and slots: %s", req.body)
	}
	if req.idem != "" {
		t.Fatal("a quote creates nothing and must not carry an idempotency key")
	}
	for _, want := range []string{"USD 2.00", "USD 0.15", "USD 2.15", "USD 0.50", "Small"} {
		if !strings.Contains(out, want) {
			t.Errorf("quote output lacks %q:\n%s", want, out)
		}
	}
}

func TestQuoteSlotBoundaries(t *testing.T) {
	for _, bad := range []string{"0", "-1"} {
		api := newSBServer(t, fixedSB(quoteJSON))
		_, _, err := sbRun(t, api, "sandbox", "quote", "--project", testProject, "--plan", testPlan, "--slots", bad)
		if err == nil || exitcode.From(err) != exitcode.Usage {
			t.Fatalf("--slots %s must be a usage error, got %v", bad, err)
		}
		if len(api.requests()) != 0 {
			t.Fatalf("--slots %s reached the network", bad)
		}
	}
	for _, slots := range []string{"1", "1000"} {
		api := newSBServer(t, fixedSB(quoteJSON))
		if _, _, err := sbRun(t, api, "sandbox", "quote", "--project", testProject, "--plan", testPlan, "--slots", slots); err != nil {
			t.Fatalf("--slots %s: %v", slots, err)
		}
		if !strings.Contains(api.only(t).body, `"slots":`+slots) {
			t.Fatalf("slots %s was not sent: %s", slots, api.only(t).body)
		}
	}
	over := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sbFail(w, 400, "validation_failed", "Check the request: project, plan and a slot count between 1 and 1000 are required.")
	})
	_, _, err := sbRun(t, over, "sandbox", "quote", "--project", testProject, "--plan", testPlan, "--slots", "1001")
	if err == nil || !strings.Contains(err.Error(), "between 1 and 1000") {
		t.Fatalf("the API's own bound must be shown when it refuses, got %v", err)
	}
	if len(over.requests()) != 1 {
		t.Fatal("the CLI must leave the upper bound to the API")
	}
}

func TestQuoteAndBuyNeedAPlanThatLooksLikeOne(t *testing.T) {
	api := newSBServer(t, fixedSB(quoteJSON))
	for _, cmd := range []string{"quote", "buy"} {
		for _, args := range [][]string{
			{"--slots", "1"},
			{"--slots", "1", "--plan", "small"},
			{"--slots", "1", "--plan", testPlan + "/../x"},
		} {
			full := append([]string{"sandbox", cmd, "--project", testProject}, args...)
			if _, _, err := sbRun(t, api, full...); err == nil || exitcode.From(err) != exitcode.Usage {
				t.Fatalf("%v: want a usage error, got %v", full, err)
			}
		}
	}
	if len(api.requests()) != 0 {
		t.Fatal("an invalid plan reached the network")
	}
}

func TestQuoteBoundaries(t *testing.T) {
	zero := `{"planName":"Free","slots":1,"currency":"NGN","unitMonthlyMinor":0,"subtotalMinor":0,"taxMinor":0,"totalMinor":0}`
	api := newSBServer(t, fixedSB(zero))
	out, _, err := sbRun(t, api, "sandbox", "quote", "--project", testProject, "--plan", testPlan, "--slots", "1", "--format", "table")
	if err != nil || !strings.Contains(out, "NGN 0.00") || strings.Contains(out, "Wallet available") {
		t.Fatalf("zero quote: %v\n%s", err, out)
	}
	huge := `{"planName":"` + strings.Repeat("N", 300) + `","slots":1000,"currency":"NGN","totalMinor":9007199254740993}`
	api = newSBServer(t, fixedSB(huge))
	out, _, err = sbRun(t, api, "sandbox", "quote", "--project", testProject, "--plan", testPlan, "--slots", "1000", "--format", "table")
	if err != nil || !strings.Contains(out, "NGN 90,071,992,547,409.93") {
		t.Fatalf("huge quote: %v\n%s", err, out)
	}
}

const purchaseJSON = `{"status":"pending","provider":"paystack","currency":"NGN","totalMinor":322500,"invoiceNumber":"INV-1",
"paymentUrl":"https://checkout.paystack.test/abc","message":"Open the payment page to finish."}`

func TestBuyReturnsALinkAndSaysItCannotPay(t *testing.T) {
	api := newSBServer(t, fixedSB(purchaseJSON))
	out, errText, err := sbRun(t, api, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "3",
		"--success-url", "https://example.test/done", "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	req := api.only(t)
	if req.method != "POST" || req.path != "/sandbox/billing/purchase" {
		t.Fatalf("unexpected request %+v", req)
	}
	if !strings.HasPrefix(req.idem, "cli-") {
		t.Fatalf("a purchase must carry a generated idempotency key, got %q", req.idem)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(req.body), &body)
	if body["paymentSource"] != "checkout" || body["successUrl"] != "https://example.test/done" || body["slots"] != float64(3) || len(body) != 5 {
		t.Fatalf("unexpected body %s", req.body)
	}
	if !strings.Contains(errText, "https://checkout.paystack.test/abc") || !strings.Contains(errText, "beaver cannot pay for you") {
		t.Fatalf("the link and the cannot-pay statement must be printed:\n%s", errText)
	}
	for _, want := range []string{"NGN 3,225.00", "INV-1", "pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestBuyJSONCarriesTheLinkOnStdout(t *testing.T) {
	api := newSBServer(t, fixedSB(purchaseJSON))
	out, errText, err := sbRun(t, api, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "1")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(out), &obj) != nil || obj["paymentUrl"] != "https://checkout.paystack.test/abc" {
		t.Fatalf("stdout must stay machine readable and carry the link: %q", out)
	}
	if !strings.Contains(errText, "cannot pay") {
		t.Fatal("even in JSON mode the statement belongs on stderr")
	}
}

func TestBuyOffersNoWayToSpendBalanceOrWallet(t *testing.T) {
	api := newSBServer(t, fixedSB(purchaseJSON))
	for _, flag := range []string{"--payment-source", "--source", "--pay-from", "--wallet", "--balance"} {
		_, _, err := sbRun(t, api, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "1", flag, "wallet")
		if err == nil {
			t.Fatalf("%s must not exist on buy", flag)
		}
	}
	if len(api.requests()) != 0 {
		t.Fatal("a refused flag still reached the network")
	}
}

func TestBuyWithAWalletTopUpLinkAndWithNoLink(t *testing.T) {
	topUp := newSBServer(t, fixedSB(`{"status":"needs_topup","currency":"USD","totalMinor":215,"shortfallMinor":165,"walletAvailableMinor":50,"topUpUrl":"https://stripe.test/topup","message":"Top up first."}`))
	out, errText, err := sbRun(t, topUp, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "2", "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errText, "https://stripe.test/topup") || !strings.Contains(errText, "topped up") || !strings.Contains(out, "USD 1.65") {
		t.Fatalf("top-up case:\n%s\n%s", errText, out)
	}
	none := newSBServer(t, fixedSB(`{"status":"paid","currency":"NGN","totalMinor":0,"message":"Nothing to pay."}`))
	_, errText, err = sbRun(t, none, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "1")
	if err != nil || !strings.Contains(errText, "no payment link") {
		t.Fatalf("a missing link must be said plainly: %v\n%s", err, errText)
	}
}

func TestBuyReusesOneKeyAcrossAttemptsAndNeverResubmitsAfterAReusedKey(t *testing.T) {
	fastBackoff(t)
	flaky := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		switch n {
		case 1:
			sbFail(w, 503, "billing_unavailable", "Sandbox billing is starting up. Try again in a moment.")
		case 2:
			sbFail(w, 409, "idempotency_in_progress", "An identical request is in progress.")
		default:
			sbOK(w, purchaseJSON)
		}
	})
	if _, _, err := sbRun(t, flaky, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "1"); err != nil {
		t.Fatal(err)
	}
	reqs := flaky.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(reqs))
	}
	for _, r := range reqs {
		if r.idem == "" || r.idem != reqs[0].idem || r.body != reqs[0].body {
			t.Fatalf("every attempt must repeat the same key and body: %+v", reqs)
		}
	}

	reused := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sbFail(w, 422, "idempotency_key_reused", "That Idempotency-Key was already used for a different request.")
	})
	_, _, err := sbRun(t, reused, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "1", "--idempotency-key", "mine-1")
	if err == nil || len(reused.requests()) != 1 {
		t.Fatalf("a reused key must never be resubmitted: %v after %d requests", err, len(reused.requests()))
	}
	if reused.requests()[0].idem != "mine-1" {
		t.Fatal("a caller supplied key must be sent verbatim")
	}
	if !strings.Contains(err.Error(), "without --idempotency-key") {
		t.Fatalf("the message must say how to start fresh: %v", err)
	}
}

func TestBuyAfterAnAmbiguousFailureNamesTheKeyToReuse(t *testing.T) {
	fastBackoff(t)
	resets := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	_, _, err := sbRun(t, resets, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "1")
	if err == nil {
		t.Fatal("a connection reset must fail")
	}
	reqs := resets.requests()
	if len(reqs) != 3 {
		t.Fatalf("a reset is ambiguous and should be retried under one key, got %d attempts", len(reqs))
	}
	if !strings.Contains(err.Error(), "--idempotency-key "+reqs[0].idem) {
		t.Fatalf("the error must name the key to reuse: %v", err)
	}
	if !strings.Contains(err.Error(), "Could not reach TechBeaver") {
		t.Fatalf("a reset must read as a calm connection problem: %v", err)
	}
	if strings.Contains(err.Error(), "\n\n") || strings.Contains(err.Error(), "btk_") {
		t.Fatalf("unexpected text: %q", err.Error())
	}
}

type failCase struct {
	name     string
	status   int
	code     string
	message  string
	wantText string
	wantExit int
}

var sandboxFailures = []failCase{
	{"unauthorized", 401, "", "Unauthorized", "beaver auth login", exitcode.Unauthenticated},
	{"forbidden", 403, "forbidden", "You do not have access to this project.", "You do not have access to this project.", exitcode.Unauthenticated},
	{"insufficient scope", 403, "insufficient_scope", "This needs billing:read.", "billing:read", exitcode.Unauthenticated},
	{"route not permitted", 403, "route_not_permitted", "no", "does not allow the CLI", exitcode.Unauthenticated},
	{"agent cannot pay", 403, "agent_cannot_pay", "x", "cannot pay", exitcode.Unauthenticated},
	{"project not found", 404, "", "Project not found", "Project not found", exitcode.NotFound},
	{"plan not found", 404, "sandbox_plan_not_on_sale", "That sandbox plan does not exist.", "does not exist", exitcode.NotFound},
	{"usage owed", 402, "usage_owed", "You owe 12 minutes of sandbox use.", "You owe 12 minutes", exitcode.PaymentRequired},
	{"balance short", 402, "prepaid_balance_insufficient", "Your balance is too low.", "balance is too low", exitcode.PaymentRequired},
	{"sandboxes exist", 409, "sandboxes_exist", "Destroy the sandboxes first.", "Destroy the sandboxes first.", exitcode.Error},
	{"subscription live", 409, "sandbox_subscription_live", "A subscription is live.", "A subscription is live.", exitcode.Error},
	{"hourly cap", 409, "hourly_cap", "Hourly cap reached.", "Hourly cap reached.", exitcode.Error},
	{"hourly subscription cap", 409, "hourly_subscription_cap", "Too many hourly subscriptions.", "Too many hourly", exitcode.Error},
	{"open payments", 409, "sandbox_too_many_open_payments", "Finish your open payments first.", "Finish your open payments", exitcode.Error},
	{"open payments no message", 409, "sandbox_too_many_open_payments", "", "payments are waiting", exitcode.Error},
	{"credit unsupported", 422, "sandbox_credit_not_supported", "Credit cannot pay for sandboxes.", "Credit cannot pay", exitcode.Error},
	{"runtime failed", 502, "sandbox_runtime_failed", "The runtime failed.", "The runtime failed.", exitcode.Error},
	{"service paused", 503, "service_paused", "New sandboxes are paused for maintenance.", "New sandboxes are paused for maintenance", exitcode.Error},
	{"check unavailable", 503, "sandbox_check_unavailable", "internal detail", "cannot be checked right now", exitcode.Error},
	{"billing unavailable", 503, "billing_unavailable", "internal detail", "cannot be checked right now", exitcode.Error},
	{"internal error", 500, "internal_error", "Something went wrong on our side.", "Something went wrong on our side.", exitcode.Error},
	{"rate limited", 429, "", "slow down", "rate limiting", exitcode.RateLimited},
	{"bare 500", 500, "", "", "problem on its side", exitcode.Error},
}

func sandboxInvocations() map[string][]string {
	common := []string{"--project", testProject}
	buy := append([]string{"sandbox", "buy", "--plan", testPlan, "--slots", "1"}, common...)
	return map[string][]string{
		"plans":         {"sandbox", "plans"},
		"plans list":    {"sandbox", "plans", "list"},
		"usage":         append([]string{"sandbox", "usage"}, common...),
		"subscriptions": append([]string{"sandbox", "subscriptions"}, common...),
		"quote":         append([]string{"sandbox", "quote", "--plan", testPlan, "--slots", "1"}, common...),
		"buy":           buy,
	}
}

func TestEverySandboxCommandMapsEveryFailureToOneCalmLine(t *testing.T) {
	fastBackoff(t)
	for _, fc := range sandboxFailures {
		for name, args := range sandboxInvocations() {
			t.Run(fc.name+"/"+name, func(t *testing.T) {
				api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, fc.status, fc.code, fc.message) })
				_, _, err := sbRun(t, api, args...)
				if err == nil {
					t.Fatal("a failed call must exit non-zero")
				}
				msg := err.Error()
				if !strings.Contains(msg, fc.wantText) {
					t.Fatalf("message %q lacks %q", msg, fc.wantText)
				}
				if got := exitcode.From(err); got != fc.wantExit {
					t.Fatalf("exit code %d, want %d (%v)", got, fc.wantExit, err)
				}
				first, _, _ := strings.Cut(msg, "\n")
				if strings.Contains(first, "(") && strings.Contains(first, "_") {
					t.Fatalf("the machine code leaked into the message: %q", msg)
				}
				if strings.Contains(msg, "btk_") || strings.Contains(strings.ToLower(msg), "bearer") {
					t.Fatalf("a credential leaked: %q", msg)
				}
				if strings.Contains(msg, "Sandboxes are not available on this server yet") {
					t.Fatalf("a real API refusal was reported as a missing API: %q", msg)
				}
			})
		}
	}
}

func TestMalformedAnswersAreReportedNotCrashed(t *testing.T) {
	bodies := map[string]string{
		"not json":         `<html>oops</html>`,
		"truncated json":   `{"status":200,"success":true,"data":{"projectId":`,
		"data is a string": `{"status":200,"success":true,"data":"hello"}`,
		"data is a list":   `{"status":200,"success":true,"data":[1,2]}`,
	}
	for name, body := range bodies {
		for cmdName, args := range sandboxInvocations() {
			t.Run(name+"/"+cmdName, func(t *testing.T) {
				api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, body)
				})
				_, _, err := sbRun(t, api, args...)
				if name == "data is a list" && (cmdName == "plans" || cmdName == "plans list") {
					return
				}
				if err == nil {
					t.Fatal("an unreadable answer must not look like success")
				}
				if strings.Contains(err.Error(), "goroutine") || strings.Contains(err.Error(), "panic") {
					t.Fatalf("a stack trace reached the user: %v", err)
				}
			})
		}
	}
}

func TestATimeoutIsAOneLineMessage(t *testing.T) {
	release := make(chan struct{})
	api := newSBServer(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	env, _, _ := newTestEnv(t, api.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	root := NewRoot(env)
	root.SetArgs([]string{"sandbox", "usage", "--project", testProject})
	err := root.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "did not answer in time") {
		t.Fatalf("want the timeout line, got %v", err)
	}
	if strings.Contains(err.Error(), "btk_") || strings.Contains(err.Error(), "context deadline") {
		t.Fatalf("raw detail leaked: %v", err)
	}
}

func TestAnUnreachableServerIsAConnectionMessage(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	env, _, _ := newTestEnv(t, "http://"+addr)
	err = run(t, env, "sandbox", "plans")
	if err == nil || !strings.Contains(err.Error(), "Could not reach TechBeaver") {
		t.Fatalf("got %v", err)
	}
}

func allSandboxLeaves(root *cobra.Command) map[string]bool {
	out := map[string]bool{}
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if c.Runnable() {
			out[path] = true
		}
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			walk(sub, strings.TrimSpace(path+" "+sub.Name()))
		}
	}
	for _, c := range root.Commands() {
		if c.Name() == "sandbox" {
			walk(c, "")
		}
	}
	return out
}

func TestEverySandboxSubcommandFailsCalmlyWhenTheServerHasNoSandboxAPI(t *testing.T) {
	fastBackoff(t)
	flavours := map[string]func(http.ResponseWriter, *http.Request, int){
		"fiber text": func(w http.ResponseWriter, r *http.Request, _ int) {
			http.Error(w, "Cannot "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		},
		"empty body": func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusNotFound) },
		"json no code": func(w http.ResponseWriter, _ *http.Request, _ int) {
			sbFail(w, 404, "", "route does not exist")
		},
		"html": func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<html><body>404</body></html>")
		},
	}
	invocations := sandboxInvocations()
	root := NewRoot(&Env{})
	leaves := allSandboxLeaves(root)
	covered := map[string]bool{}
	for name := range invocations {
		covered[name] = true
	}
	for leaf := range leaves {
		if !covered[leaf] {
			t.Errorf("sandbox %q has no entry in sandboxInvocations, so the 404 guard does not cover it", leaf)
		}
	}
	for covers := range covered {
		if !leaves[covers] {
			t.Errorf("sandboxInvocations names %q, which is not a sandbox command", covers)
		}
	}
	for flavour, handler := range flavours {
		for name, args := range invocations {
			t.Run(flavour+"/"+name, func(t *testing.T) {
				api := newSBServer(t, handler)
				var panicked any
				var err error
				var errText string
				func() {
					defer func() { panicked = recover() }()
					_, errText, err = sbRun(t, api, args...)
				}()
				if panicked != nil {
					t.Fatalf("panic: %v", panicked)
				}
				if err == nil {
					t.Fatal("must exit non-zero")
				}
				if !strings.Contains(err.Error(), "Sandboxes are not available on this server yet") {
					t.Fatalf("not the calm message: %q", err.Error())
				}
				if exitcode.From(err) == exitcode.Unauthenticated || strings.Contains(err.Error()+errText, "auth login") {
					t.Fatalf("a missing API must not send the user to log in again: %v", err)
				}
				if len(api.requests()) > 1 {
					t.Fatalf("a 404 must not be retried, saw %d requests", len(api.requests()))
				}
				if strings.Contains(err.Error()+errText, "btk_") {
					t.Fatal("a credential leaked")
				}
			})
		}
	}
}

func TestSandboxCommandsRunConcurrentlyWithoutSharedState(t *testing.T) {
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/plans"):
			sbOK(w, plansJSON)
		case strings.HasSuffix(r.URL.Path, "/usage"):
			sbOK(w, usageJSON)
		case strings.HasSuffix(r.URL.Path, "/quote"):
			sbOK(w, quoteJSON)
		default:
			sbOK(w, purchaseJSON)
		}
	})
	invocations := sandboxInvocations()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 8; i++ {
		for name, args := range invocations {
			wg.Add(1)
			go func() {
				defer wg.Done()
				env, out, _ := newTestEnv(t, api.URL)
				var errBuf bytes.Buffer
				env.Err = &errBuf
				root := NewRoot(env)
				root.SetArgs(args)
				root.SetOut(env.Out)
				root.SetErr(env.Err)
				if err := root.ExecuteContext(context.Background()); err != nil {
					errs <- fmt.Errorf("%s: %w", name, err)
					return
				}
				if out.Len() == 0 {
					errs <- fmt.Errorf("%s: no output", name)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	keys := map[string]bool{}
	for _, r := range api.requests() {
		if r.path == "/sandbox/billing/purchase" {
			if keys[r.idem] {
				t.Fatalf("two separate purchase attempts shared key %q", r.idem)
			}
			keys[r.idem] = true
		}
	}
	if len(keys) != 8 {
		t.Fatalf("expected 8 distinct keys for 8 purchases, got %d", len(keys))
	}
}

func TestSandboxCommandsNeverEchoTheCredentialEvenWithDebug(t *testing.T) {
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, 500, "", "boom") })
	for name, args := range sandboxInvocations() {
		env, out, errBuf := newTestEnv(t, api.URL)
		err := run(t, env, append(args, "--debug")...)
		all := out.String() + errBuf.String()
		if err != nil {
			all += err.Error()
		}
		if strings.Contains(all, "btk_test") || strings.Contains(strings.ToLower(all), "authorization") {
			t.Fatalf("%s printed credential material: %q", name, all)
		}
	}
}

func TestSandboxHelpDoesNotPromiseWhatTheCLICannotDo(t *testing.T) {
	root := NewRoot(&Env{})
	for _, c := range root.Commands() {
		if c.Name() != "sandbox" {
			continue
		}
		for _, banned := range []string{"create", "destroy", "start", "stop", "cancel", "resume", "hourly", "add-slots", "reduce-slots"} {
			for _, sub := range c.Commands() {
				if sub.Name() == banned {
					t.Errorf("sandbox %s exists, but the platform does not let the CLI use that route", banned)
				}
			}
		}
	}
}

func TestFormatMinor(t *testing.T) {
	cases := []struct {
		in       any
		currency string
		want     string
	}{
		{json.Number("0"), "NGN", "NGN 0.00"},
		{json.Number("1"), "NGN", "NGN 0.01"},
		{json.Number("99"), "USD", "USD 0.99"},
		{json.Number("100"), "USD", "USD 1.00"},
		{json.Number("123456789"), "ngn", "NGN 1,234,567.89"},
		{json.Number("-5"), "USD", "USD -0.05"},
		{json.Number("9223372036854775807"), "NGN", "NGN 92,233,720,368,547,758.07"},
		{json.Number("-9223372036854775808"), "NGN", "NGN -92,233,720,368,547,758.08"},
		{json.Number("500"), "JPY", "JPY 500"},
		{json.Number("1500"), "KWD", "KWD 1.500"},
		{json.Number("12"), "", "12 (minor units, currency not stated)"},
		{float64(250), "USD", "USD 2.50"},
		{nil, "USD", ""},
		{"abc", "USD", "abc"},
		{json.Number("1.5"), "USD", "1.5"},
	}
	for _, tc := range cases {
		if got := formatMinor(tc.in, tc.currency); got != tc.want {
			t.Errorf("formatMinor(%v, %q) = %q, want %q", tc.in, tc.currency, got, tc.want)
		}
	}
}
