package command

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

const testStatement = "12121212-3434-5656-7878-909090909090"

const statementSummaryJSON = `"id":"` + testStatement + `","number":"SBX-2026-11-0001","currency":"NGN","month":"2026-11",
"periodStart":"2026-11-01T00:00:00Z","periodEnd":"2026-12-01T00:00:00Z","status":"closed","subtotalMinor":1000000,
"discountMinor":0,"taxMinor":75000,"totalMinor":1075000,"paymentState":"paid","paidMinor":1075000,"unpaidMinor":0,
"adjustmentsMinor":-200000,"netTotalMinor":875000,"chargeCount":3,"asOf":"2026-12-01T00:05:00Z"`

const statementListJSON = `{"items":[{` + statementSummaryJSON + `}],"page":1,"limit":20,"total":1}`

const statementDetailJSON = `{` + statementSummaryJSON + `,
"sandboxes":[{"sandboxId":"` + testSandbox + `","name":"build box","planName":"Small","vcpu":2,"memoryMb":4096,"diskGb":20,"hourlyMinor":50000,"hours":20,"amountMinor":1000000}],
"charges":[{"invoiceNumber":"INV-9","invoiceStatus":"paid","periodStart":"2026-11-01T00:00:00Z","periodEnd":"2026-11-15T00:00:00Z","sandboxes":1,"hours":20,"subtotalMinor":1000000,"discountMinor":0,"taxMinor":75000,"totalMinor":1075000}],
"adjustments":[{"invoiceId":"` + testStatement + `","invoiceNumber":"INV-9","reason":"refunded","amountMinor":-200000}]}`

const statementCurrentJSON = `{"month":"2026-11","periodStart":"2026-11-01T00:00:00Z","periodEnd":"2026-12-01T00:00:00Z","statements":[` + statementDetailJSON + `]}`

func TestStatementsListSendsTheProjectAndFormatsTheFigures(t *testing.T) {
	api := newSBServer(t, fixedSB(statementListJSON))
	out, _, err := sbRun(t, api, "sandbox", "statements", "list", "--project", testProject, "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	req := api.only(t)
	if req.method != "GET" || req.path != "/sandbox/statements" || req.query != "projectId="+testProject {
		t.Fatalf("unexpected request %+v", req)
	}
	for _, want := range []string{"SBX-2026-11-0001", "2026-11", "NGN 10,750.00", "NGN 8,750.00", "paid", "closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestStatementsListPagingAndEmpty(t *testing.T) {
	api := newSBServer(t, fixedSB(`{"items":[{`+statementSummaryJSON+`}],"page":2,"limit":1,"total":5}`))
	_, errText, err := sbRun(t, api, "sandbox", "statements", "list", "--project", testProject, "--page", "2", "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if q := api.only(t).query; !strings.Contains(q, "page=2") || !strings.Contains(q, "limit=1") || !strings.Contains(q, "projectId="+testProject) {
		t.Fatalf("paging must reach the API: %q", q)
	}
	if !strings.Contains(errText, "Showing 1 of 5") {
		t.Fatalf("a partial page must say so: %q", errText)
	}

	empty := newSBServer(t, fixedSB(`{"items":[],"page":1,"limit":20,"total":0}`))
	out, errText, err := sbRun(t, empty, "sandbox", "statements", "list", "--project", testProject, "--format", "table")
	if err != nil || !strings.Contains(out, "No statements yet") || strings.Contains(errText, "Showing") {
		t.Fatalf("an empty list is a sentence, not a blank screen: %v\n%s\n%s", err, out, errText)
	}
	js, _, err := sbRun(t, empty, "sandbox", "statements", "list", "--project", testProject)
	if err != nil || strings.TrimSpace(js) != "[]" {
		t.Fatalf("empty JSON must be an empty list: %v %q", err, js)
	}
}

func TestStatementsListBoundaries(t *testing.T) {
	cases := []struct {
		name string
		args []string
		ok   bool
		want string
	}{
		{"page zero", []string{"--page", "0"}, false, "--page counts from 1"},
		{"page negative", []string{"--page", "-3"}, false, "--page counts from 1"},
		{"limit zero", []string{"--limit", "0"}, false, "--limit is between 1 and 100"},
		{"limit over", []string{"--limit", "101"}, false, "--limit is between 1 and 100"},
		{"limit negative", []string{"--limit", "-1"}, false, "--limit is between 1 and 100"},
		{"page one", []string{"--page", "1"}, true, "page=1"},
		{"limit one", []string{"--limit", "1"}, true, "limit=1"},
		{"limit hundred", []string{"--limit", "100"}, true, "limit=100"},
		{"neither", nil, true, "projectId="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newSBServer(t, fixedSB(statementListJSON))
			_, _, err := sbRun(t, api, append([]string{"sandbox", "statements", "list", "--project", testProject}, tc.args...)...)
			if !tc.ok {
				if err == nil || !strings.Contains(err.Error(), tc.want) || exitcode.From(err) != exitcode.Usage || len(api.requests()) != 0 {
					t.Fatalf("want a usage error before any request, got %v with %d requests", err, len(api.requests()))
				}
				return
			}
			if err != nil || !strings.Contains(api.only(t).query, tc.want) {
				t.Fatalf("err %v, requests %+v", err, api.requests())
			}
		})
	}
}

func TestStatementsNeedAProjectThatLooksLikeOne(t *testing.T) {
	for _, args := range [][]string{
		{"sandbox", "statements", "list"},
		{"sandbox", "statements", "current", "--project", "my-project"},
		{"sandbox", "statements", "show", testStatement, "--project", "my-project"},
	} {
		api := newSBServer(t, fixedSB(statementListJSON))
		_, _, err := sbRun(t, api, args...)
		if err == nil || len(api.requests()) != 0 {
			t.Fatalf("%v must stop before the network: %v", args, err)
		}
	}
}

func TestStatementsCurrentShowsTheMonthSoFar(t *testing.T) {
	api := newSBServer(t, fixedSB(statementCurrentJSON))
	out, _, err := sbRun(t, api, "sandbox", "statements", "current", "--project", testProject, "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	if req := api.only(t); req.path != "/sandbox/statements/current" || req.query != "projectId="+testProject {
		t.Fatalf("unexpected request %+v", req)
	}
	for _, want := range []string{"2026-11", "build box", "NGN 500.00", "INV-9", "refunded", "NGN -2,000.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	empty := newSBServer(t, fixedSB(`{"month":"2026-11","periodStart":"2026-11-01T00:00:00Z","periodEnd":"2026-12-01T00:00:00Z","statements":[]}`))
	out, _, err = sbRun(t, empty, "sandbox", "statements", "current", "--project", testProject, "--format", "table")
	if err != nil || !strings.Contains(out, "Nothing has been settled this month yet") {
		t.Fatalf("an empty month is a sentence: %v\n%s", err, out)
	}
}

func TestAnOpenStatementSaysRunningHoursAreNotOnIt(t *testing.T) {
	open := strings.Replace(statementDetailJSON, `"status":"closed"`, `"status":"open"`, 1)
	api := newSBServer(t, fixedSB(open))
	out, _, err := sbRun(t, api, "sandbox", "statements", "show", testStatement, "--project", testProject, "--format", "table")
	if err != nil || !strings.Contains(out, "Hours still running are not on it") {
		t.Fatalf("an open statement must say what it leaves out: %v\n%s", err, out)
	}
	closed := newSBServer(t, fixedSB(statementDetailJSON))
	out, _, err = sbRun(t, closed, "sandbox", "statements", "show", testStatement, "--project", testProject, "--format", "table")
	if err != nil || strings.Contains(out, "Hours still running") {
		t.Fatalf("a closed statement has no such caveat: %v\n%s", err, out)
	}
}

func TestStatementsShowSendsTheIDAndTheProject(t *testing.T) {
	api := newSBServer(t, fixedSB(statementDetailJSON))
	out, _, err := sbRun(t, api, "sandbox", "statements", "show", testStatement, "--project", testProject)
	if err != nil {
		t.Fatal(err)
	}
	if req := api.only(t); req.path != "/sandbox/statements/"+testStatement || req.query != "projectId="+testProject {
		t.Fatalf("unexpected request %+v", req)
	}
	if !strings.Contains(out, `"netTotalMinor": 875000`) && !strings.Contains(out, `"netTotalMinor":875000`) {
		t.Fatalf("JSON must carry the API's own figures untouched:\n%s", out)
	}
	for _, bad := range []string{"", "not-an-id", testStatement + "0", "../" + testStatement} {
		api := newSBServer(t, fixedSB(statementDetailJSON))
		if _, _, err := sbRun(t, api, "sandbox", "statements", "show", bad, "--project", testProject); err == nil || len(api.requests()) != 0 {
			t.Fatalf("id %q must be refused before any request: %v", bad, err)
		}
	}
}

func TestStatementsAreReadOnlyAndNeverSendABodyOrAKey(t *testing.T) {
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/current"):
			sbOK(w, statementCurrentJSON)
		case strings.HasSuffix(r.URL.Path, "/statements"):
			sbOK(w, statementListJSON)
		default:
			sbOK(w, statementDetailJSON)
		}
	})
	for _, args := range [][]string{
		{"sandbox", "statements", "list", "--project", testProject},
		{"sandbox", "statements", "current", "--project", testProject},
		{"sandbox", "statements", "show", testStatement, "--project", testProject},
	} {
		if _, _, err := sbRun(t, api, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range api.requests() {
		if r.method != "GET" || r.body != "" || r.idem != "" || r.confirm != "" {
			t.Fatalf("a statements read must be a bare GET: %+v", r)
		}
	}
	for _, flag := range []string{"--pay", "--void", "--refund", "--auto-renew", "--hourly-auto-topup"} {
		if _, _, err := sbRun(t, api, "sandbox", "statements", "list", "--project", testProject, flag); err == nil {
			t.Fatalf("%s must not exist on statements", flag)
		}
	}
}

func TestStatementsFaultInjection(t *testing.T) {
	fastBackoff(t)
	invocations := map[string][]string{
		"list":    {"sandbox", "statements", "list", "--project", testProject},
		"current": {"sandbox", "statements", "current", "--project", testProject},
		"show":    {"sandbox", "statements", "show", testStatement, "--project", testProject},
	}
	for name, args := range invocations {
		t.Run(name+"/server errors", func(t *testing.T) {
			for _, status := range []int{500, 502, 503} {
				api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, status, "", "") })
				_, _, err := sbRun(t, api, args...)
				if err == nil || !strings.Contains(err.Error(), "problem on its side") || exitcode.From(err) != exitcode.Error {
					t.Fatalf("%d: %v", status, err)
				}
			}
		})
		t.Run(name+"/not json", func(t *testing.T) {
			api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = w.Write([]byte("<html>gateway</html>")) })
			if _, _, err := sbRun(t, api, args...); err == nil {
				t.Fatal("an HTML answer must not look like success")
			}
		})
		t.Run(name+"/truncated", func(t *testing.T) {
			api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":200,"success":true,"data":{"items":[`))
			})
			if _, _, err := sbRun(t, api, args...); err == nil {
				t.Fatal("a cut off body must not look like success")
			}
		})
		t.Run(name+"/unauthorized", func(t *testing.T) {
			api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, 401, "", "Unauthorized") })
			_, _, err := sbRun(t, api, args...)
			if err == nil || !strings.Contains(err.Error(), "beaver auth login") || exitcode.From(err) != exitcode.Unauthenticated {
				t.Fatalf("%v", err)
			}
		})
		t.Run(name+"/grant message", func(t *testing.T) {
			api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				sbFailWith(w, 403, "insufficient_scope", "needs billing:read", map[string]any{"code": "insufficient_scope", "missingScope": []string{"billing:read"}})
			})
			_, _, err := sbRun(t, api, args...)
			if err == nil || !strings.Contains(err.Error(), "not granted billing:read") || !strings.Contains(err.Error(), "--scope billing:read") {
				t.Fatalf("the grant message must name the scope and how to add it: %v", err)
			}
			if len(api.requests()) != 1 {
				t.Fatalf("a refusal is not retried, saw %d requests", len(api.requests()))
			}
		})
		t.Run(name+"/not on the allowlist", func(t *testing.T) {
			api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, 403, "route_not_permitted", "no") })
			if _, _, err := sbRun(t, api, args...); err == nil || !strings.Contains(err.Error(), "does not allow the CLI") {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestStatementsRateLimitHonoursRetryAfterOnce(t *testing.T) {
	waits := recordWaits(t)
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "3")
			sbFail(w, 429, "sandbox_call_rate_limited", "Slow down.")
			return
		}
		sbOK(w, statementListJSON)
	})
	if _, _, err := sbRun(t, api, "sandbox", "statements", "list", "--project", testProject); err != nil {
		t.Fatal(err)
	}
	if got := waits.all(); len(api.requests()) != 2 || len(got) != 1 || got[0] != 3*time.Second {
		t.Fatalf("one retry after exactly the asked wait: %d requests, waits %v", len(api.requests()), got)
	}
	always := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "3")
		sbFail(w, 429, "sandbox_call_rate_limited", "Slow down.")
	})
	_, _, err := sbRun(t, always, "sandbox", "statements", "current", "--project", testProject)
	if err == nil || len(always.requests()) != 2 || exitcode.From(err) != exitcode.RateLimited || !strings.Contains(err.Error(), "Try again in 3 seconds") {
		t.Fatalf("never a third attempt, and the wait is told: %d requests, %v", len(always.requests()), err)
	}
	long := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "600")
		sbFail(w, 429, "sandbox_call_rate_limited", "Slow down.")
	})
	_, _, err = sbRun(t, long, "sandbox", "statements", "show", testStatement, "--project", testProject)
	if err == nil || len(long.requests()) != 1 || !strings.Contains(err.Error(), "600 seconds") {
		t.Fatalf("a long wait is told, not slept: %d requests, %v", len(long.requests()), err)
	}
}

func TestStatementsTimeout(t *testing.T) {
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
	root.SetArgs([]string{"sandbox", "statements", "list", "--project", testProject})
	if err := root.ExecuteContext(ctx); err == nil || !strings.Contains(err.Error(), "did not answer in time") {
		t.Fatalf("want the timeout line, got %v", err)
	}
}

func TestStatementsRunConcurrentlyWithoutSharedState(t *testing.T) {
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/current"):
			sbOK(w, statementCurrentJSON)
		case strings.HasSuffix(r.URL.Path, "/statements"):
			sbOK(w, statementListJSON)
		default:
			sbOK(w, statementDetailJSON)
		}
	})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 8; i++ {
		for _, args := range [][]string{
			{"sandbox", "statements", "list", "--project", testProject, "--format", "table"},
			{"sandbox", "statements", "current", "--project", testProject, "--format", "table"},
			{"sandbox", "statements", "show", testStatement, "--project", testProject, "--format", "table"},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				env, out, _ := newTestEnv(t, api.URL)
				root := NewRoot(env)
				root.SetArgs(args)
				if err := root.ExecuteContext(context.Background()); err != nil {
					errs <- err
					return
				}
				if !strings.Contains(out.String(), "2026-11") {
					errs <- context.DeadlineExceeded
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(api.requests()); got != 24 {
		t.Fatalf("want 24 requests, got %d", got)
	}
}
