package command

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

func TestAddSlotsQuote(t *testing.T) {
	api := newSBServer(t, fixedSB(`{"planName":"Small","slots":2,"currency":"NGN","subtotalMinor":1000,"taxMinor":75,"totalMinor":1075,"daysCharged":12,"walletAvailableMinor":null}`))
	out, _, err := sbRun(t, api, "sandbox", "add-slots", "quote", testSubID, "--slots", "2", "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	r := api.only(t)
	if r.method != "POST" || r.path != "/sandbox/subscriptions/"+testSubID+"/add-slots/quote" || r.body != `{"slots":2}` || r.idem != "" {
		t.Fatalf("request %+v", r)
	}
	for _, want := range []string{"NGN 10.75", "12", "NGN 0.75"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	for _, args := range [][]string{
		{"sandbox", "add-slots", "quote", testSubID, "--slots", "0"},
		{"sandbox", "add-slots", "quote", "nope", "--slots", "1"},
	} {
		if _, _, err := sbRun(t, api, args...); err == nil || exitcode.From(err) != exitcode.Usage {
			t.Fatalf("%v: want usage error, got %v", args, err)
		}
	}
	if len(api.requests()) != 1 {
		t.Fatal("a refused quote reached the network")
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

const purchaseJSON = `{"status":"awaiting_payment","provider":"paystack","currency":"NGN","totalMinor":322500,"invoiceNumber":"INV-1",
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
	for _, want := range []string{"NGN 3,225.00", "INV-1", "awaiting_payment"} {
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
	topUp := newSBServer(t, fixedSB(`{"status":"needs_topup","currency":"USD","totalMinor":215,"shortfallMinor":165,"walletAvailableMinor":50,"paymentUrl":"https://stripe.test/topup","message":"Top up first."}`))
	out, errText, err := sbRun(t, topUp, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "2", "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errText, "https://stripe.test/topup") || !strings.Contains(errText, "top it up") || !strings.Contains(errText, "paid from the wallet when the top-up lands") || strings.Contains(errText, "Open this to pay") || !strings.Contains(out, "USD 1.65") {
		t.Fatalf("top-up case:\n%s\n%s", errText, out)
	}
	none := newSBServer(t, fixedSB(`{"status":"unexpected","currency":"NGN","totalMinor":0,"message":"Nothing to pay."}`))
	_, errText, err = sbRun(t, none, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "1")
	if err != nil || !strings.Contains(errText, "no payment link") {
		t.Fatalf("a missing link must be said plainly: %v\n%s", err, errText)
	}
}

func TestBuyWhenTheServerSaysPaidNeverClaimsToHavePaid(t *testing.T) {
	api := newSBServer(t, fixedSB(`{"status":"paid","currency":"NGN","totalMinor":1000,"message":"Paid."}`))
	_, errText, err := sbRun(t, api, "sandbox", "buy", "--project", testProject, "--plan", testPlan, "--slots", "1")
	if err != nil || !strings.Contains(errText, "reports this purchase as paid") || !strings.Contains(errText, "beaver did not pay it") {
		t.Fatalf("a paid answer must be reported as the server's, not ours: %v\n%s", err, errText)
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
