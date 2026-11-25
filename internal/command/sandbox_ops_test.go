package command

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

const sandboxJSON = `{"id":"` + testSandbox + `","projectId":"` + testProject + `","subscriptionId":"` + testSubID + `","planId":"` + testPlan + `",
"name":"box","status":"running","vcpu":2,"memoryMb":4096,"diskGb":20,"idleStopMinutes":30,"maxRunHours":24,"createdAt":"2026-11-24T10:00:00Z"}`

const execOKJSON = `{"status":"exited","exitCode":0,"stdout":"hi\n","stderr":"","stdoutTruncated":false,"stderrTruncated":false,"timedOut":false,"durationMs":5}`

func okAll(w http.ResponseWriter, r *http.Request, _ int) {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/plans"):
		sbOK(w, plansJSON)
	case strings.HasSuffix(p, "/usage"):
		sbOK(w, usageJSON)
	case strings.HasSuffix(p, "/statements"):
		sbOK(w, statementListJSON)
	case strings.HasSuffix(p, "/statements/current"):
		sbOK(w, statementCurrentJSON)
	case strings.Contains(p, "/statements/"):
		sbOK(w, statementDetailJSON)
	case strings.HasSuffix(p, "/add-slots/quote"), strings.HasSuffix(p, "/billing/quote"):
		sbOK(w, quoteJSON)
	case strings.HasSuffix(p, "/billing/purchase"):
		sbOK(w, purchaseJSON)
	case strings.HasSuffix(p, "/exec"):
		sbOK(w, execOKJSON)
	case strings.HasSuffix(p, "/files/list"):
		sbOK(w, `{"path":"/workspace","entries":[{"name":"a.txt","type":"file","size":5,"modTime":"2026-11-24T10:00:00Z"}],"truncated":false}`)
	case strings.HasSuffix(p, "/files/mkdir"):
		sbOK(w, `{"path":"/workspace/new"}`)
	case strings.HasSuffix(p, "/files") && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "hello")
	case strings.HasSuffix(p, "/files"):
		sbOK(w, `{"path":"/workspace/up.txt","bytes":5}`)
	case strings.HasSuffix(p, "/sandbox/sandboxes") && r.Method == http.MethodGet:
		sbOK(w, `[`+sandboxJSON+`]`)
	case strings.HasSuffix(p, "/sandbox/subscriptions"):
		sbOK(w, `[`+subJSON+`]`)
	case strings.Contains(p, "/sandbox/subscriptions/"):
		sbOK(w, subJSON)
	default:
		sbOK(w, sandboxJSON)
	}
}

type waitLog struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (w *waitLog) all() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...)
}

func recordWaits(t *testing.T) *waitLog {
	t.Helper()
	log := &waitLog{}
	saved := retryWait
	retryWait = func(_ context.Context, d time.Duration) bool {
		log.mu.Lock()
		log.waits = append(log.waits, d)
		log.mu.Unlock()
		return true
	}
	t.Cleanup(func() { retryWait = saved })
	return log
}

func decodeBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("body %q: %v", raw, err)
	}
	return m
}

func TestLifecycleReadsAndTransitions(t *testing.T) {
	api := newSBServer(t, okAll)
	for _, tc := range []struct {
		args         []string
		method, path string
		query        string
	}{
		{[]string{"sandbox", "list", "--project", testProject}, "GET", "/sandbox/sandboxes", "projectId=" + testProject},
		{[]string{"sandbox", "list", "--all", "--include-destroyed", "--page", "2", "--limit", "5"}, "GET", "/sandbox/sandboxes", "includeDestroyed=true&limit=5&page=2"},
		{[]string{"sandbox", "list"}, "GET", "/sandbox/sandboxes", ""},
		{[]string{"sandbox", "get", testSandbox}, "GET", "/sandbox/sandboxes/" + testSandbox, ""},
		{[]string{"sandbox", "start", testSandbox}, "POST", "/sandbox/sandboxes/" + testSandbox + "/start", ""},
		{[]string{"sandbox", "stop", testSandbox}, "POST", "/sandbox/sandboxes/" + testSandbox + "/stop", ""},
	} {
		before := len(api.requests())
		out, _, err := sbRun(t, api, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		reqs := api.requests()
		if len(reqs) != before+1 {
			t.Fatalf("%v: expected one request", tc.args)
		}
		r := reqs[before]
		if r.method != tc.method || r.path != tc.path || r.query != tc.query || r.auth != "Bearer btk_test" {
			t.Errorf("%v: request %+v", tc.args, r)
		}
		if r.idem != "" || r.body != "" {
			t.Errorf("%v: a read or transition carries no key or body: %+v", tc.args, r)
		}
		if !strings.Contains(out, testSandbox) {
			t.Errorf("%v: output lacks the sandbox: %q", tc.args, out)
		}
	}
}

func TestListBoundaries(t *testing.T) {
	empty := newSBServer(t, fixedSB(`[]`))
	out, _, err := sbRun(t, empty, "sandbox", "list")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty list: %q %v", out, err)
	}
	out, _, _ = sbRun(t, empty, "sandbox", "list", "--format", "table")
	if !strings.Contains(out, "No results.") {
		t.Fatalf("table: %q", out)
	}
	if _, _, err := sbRun(t, empty, "sandbox", "list", "--project", "my-site"); err == nil || exitcode.From(err) != exitcode.Usage {
		t.Fatalf("a project name is a usage error, got %v", err)
	}
	for _, args := range [][]string{{"sandbox", "get", "x"}, {"sandbox", "start", "../x"}, {"sandbox", "stop", ""}} {
		if _, _, err := sbRun(t, empty, args...); err == nil || exitcode.From(err) != exitcode.Usage {
			t.Fatalf("%v: want usage error, got %v", args, err)
		}
	}
	if len(empty.requests()) != 2 {
		t.Fatalf("only the two valid lists may reach the network, got %d", len(empty.requests()))
	}
}

func TestCreateSendsKeyAndBodyAndValidatesTheName(t *testing.T) {
	ok := newSBServer(t, okAll)
	if _, _, err := sbRun(t, ok, "sandbox", "create", "  my box.1_x-y ", "--project", testProject, "--subscription", testSubID); err != nil {
		t.Fatal(err)
	}
	r := ok.only(t)
	if r.method != "POST" || r.path != "/sandbox/sandboxes" || !strings.HasPrefix(r.idem, "cli-") {
		t.Fatalf("request %+v", r)
	}
	b := decodeBody(t, r.body)
	if len(b) != 3 || b["projectId"] != testProject || b["name"] != "my box.1_x-y" || b["subscriptionId"] != testSubID {
		t.Fatalf("body %s", r.body)
	}
	plain := newSBServer(t, okAll)
	if _, _, err := sbRun(t, plain, "sandbox", "create", "box", "--project", testProject, "--idempotency-key", "mine"); err != nil {
		t.Fatal(err)
	}
	r = plain.only(t)
	if r.idem != "mine" || strings.Contains(r.body, "subscriptionId") {
		t.Fatalf("request %+v", r)
	}

	bad := map[string]string{
		"empty": "", "blank": "   ", "leading hyphen": "-box", "leading dot": ".box",
		"too long": strings.Repeat("a", 64), "slash": "a/b", "emoji": "box\U0001F600", "bidi": "box\u202e", "zero width": "a\u200bb",
		"newline": "a\nb", "tab": "a\tb", "colon": "a:b", "too long multibyte": strings.Repeat("é", 64),
	}
	for name, n := range bad {
		srv := newSBServer(t, okAll)
		_, _, err := sbRun(t, srv, "sandbox", "create", "--project", testProject, "--", n)
		if err == nil || exitcode.From(err) != exitcode.Usage || !strings.Contains(err.Error(), "1 to 63 characters") {
			t.Errorf("%s: want the naming rule, got %v", name, err)
		}
		if len(srv.requests()) != 0 {
			t.Errorf("%s: an invalid name reached the network", name)
		}
	}
	for _, n := range []string{"a", strings.Repeat("a", 63), "9lives", "été 2026", strings.Repeat("é", 63)} {
		srv := newSBServer(t, okAll)
		if _, _, err := sbRun(t, srv, "sandbox", "create", n, "--project", testProject); err != nil {
			t.Errorf("%q must be accepted: %v", n, err)
		}
	}
	if _, _, err := sbRun(t, ok, "sandbox", "create", "box", "--project", testProject, "--subscription", "nope"); err == nil || exitcode.From(err) != exitcode.Usage {
		t.Fatalf("bad subscription id: %v", err)
	}
}

func TestCreateStillShowsTheServersOwnRefusal(t *testing.T) {
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sbFail(w, 400, "validation_failed", "A sandbox name is 1 to 63 characters: server wording.")
	})
	_, _, err := sbRun(t, api, "sandbox", "create", "box", "--project", testProject)
	if err == nil || !strings.Contains(err.Error(), "server wording") {
		t.Fatalf("got %v", err)
	}
}

func TestCreateHonoursRetryAfterOnceAndKeepsTheKey(t *testing.T) {
	waits := recordWaits(t)
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			sbFailWith(w, 429, "sandbox_create_rate_limited", "Created too fast.", map[string]any{"retryAfterSeconds": 2})
			return
		}
		sbOK(w, sandboxJSON)
	})
	if _, _, err := sbRun(t, api, "sandbox", "create", "box", "--project", testProject); err != nil {
		t.Fatal(err)
	}
	reqs := api.requests()
	if len(reqs) != 2 || reqs[0].idem == "" || reqs[0].idem != reqs[1].idem {
		t.Fatalf("one retry under one key expected: %+v", reqs)
	}
	if w := waits.all(); len(w) != 1 || w[0] != 2*time.Second {
		t.Fatalf("waits %v", w)
	}

	always := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "2")
		sbFail(w, 429, "sandbox_create_rate_limited", "Created too fast.")
	})
	_, _, err := sbRun(t, always, "sandbox", "create", "box", "--project", testProject)
	if err == nil || len(always.requests()) != 2 || exitcode.From(err) != exitcode.RateLimited {
		t.Fatalf("never more than one automatic retry, got %d requests, %v", len(always.requests()), err)
	}
	if !strings.Contains(err.Error(), "Try again in 2 seconds") {
		t.Fatalf("the wait must be told: %v", err)
	}

	hourly := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "1800")
		sbFail(w, 429, "sandbox_create_rate_limited", "Created too fast.")
	})
	_, _, err = sbRun(t, hourly, "sandbox", "create", "box", "--project", testProject)
	if err == nil || len(hourly.requests()) != 1 || !strings.Contains(err.Error(), "1800 seconds") {
		t.Fatalf("a long wait must not be slept inside the command: %d requests, %v", len(hourly.requests()), err)
	}
	none := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, 429, "sandbox_busy", "Busy.") })
	if _, _, err = sbRun(t, none, "sandbox", "get", testSandbox); err == nil || len(none.requests()) != 1 {
		t.Fatalf("with no Retry-After there is no automatic retry: %v", err)
	}
}

func TestCreateRetriesAServerErrorUnderTheSameKey(t *testing.T) {
	fastBackoff(t)
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			sbFail(w, 503, "billing_unavailable", "x")
			return
		}
		sbOK(w, sandboxJSON)
	})
	if _, _, err := sbRun(t, api, "sandbox", "create", "box", "--project", testProject); err != nil {
		t.Fatal(err)
	}
	reqs := api.requests()
	if len(reqs) != 2 || reqs[0].idem != reqs[1].idem {
		t.Fatalf("%+v", reqs)
	}
	dead := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sbFail(w, 502, "sandbox_runtime_failed", "Runtime failed.")
	})
	_, _, err := sbRun(t, dead, "sandbox", "create", "box", "--project", testProject)
	if err == nil || !strings.Contains(err.Error(), "without risking a second sandbox") {
		t.Fatalf("the message must name the key to reuse: %v", err)
	}
}

func destroyServer(t *testing.T, approved bool) *sbServer {
	t.Helper()
	return newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/mcp/confirmations"):
			w.WriteHeader(201)
			sbOK(w, `{"id":"conf-1","status":"pending","approvalUrl":"https://techbeaver.io/portal/confirm/abc"}`)
		case strings.Contains(r.URL.Path, "/mcp/confirmations/"):
			if approved {
				sbOK(w, `{"id":"conf-1","status":"approved","confirmationToken":"tok-1"}`)
				return
			}
			sbOK(w, `{"id":"conf-1","status":"pending"}`)
		case r.Method == "DELETE":
			if r.Header.Get("X-Confirmation-Token") == "" {
				sbFail(w, 428, "confirmation_required", "needs approval")
				return
			}
			sbOK(w, `{"id":"`+testSandbox+`","status":"destroyed"}`)
		default:
			sbOK(w, sandboxJSON)
		}
	})
}

func TestDestroyNeedsAHumanAndSendsNothingUntilApproved(t *testing.T) {
	api := destroyServer(t, false)
	_, errText, err := sbRun(t, api, "sandbox", "destroy", testSandbox, "--yes")
	if err == nil || exitcode.From(err) != exitcode.AwaitingHuman {
		t.Fatalf("an unapproved destroy must exit %d, got %v", exitcode.AwaitingHuman, err)
	}
	if !strings.Contains(errText, "portal/confirm/abc") {
		t.Fatalf("the approval link must be shown:\n%s", errText)
	}
	var reg *sbRequest
	for _, r := range api.requests() {
		if r.method == "DELETE" {
			t.Fatal("--yes must not bypass the approval: DELETE was sent")
		}
		if r.path == "/mcp/confirmations" {
			r := r
			reg = &r
		}
	}
	if reg == nil {
		t.Fatal("the confirmation was not registered")
	}
	b := decodeBody(t, reg.body)
	if b["method"] != "DELETE" || b["path"] != "/sandbox/sandboxes/"+testSandbox || b["resourceType"] != "sandbox" || b["action"] != "destroy" {
		t.Fatalf("registration %s", reg.body)
	}
	if sum, _ := b["summary"].(map[string]any); sum["name"] != "box" || sum["status"] != "running" {
		t.Fatalf("the preview must show what goes: %s", reg.body)
	}
}

func TestAnApprovedDestroyReplaysWithTheToken(t *testing.T) {
	api := destroyServer(t, true)
	out, _, err := sbRun(t, api, "sandbox", "destroy", testSandbox, "--yes", "--wait", "5s")
	if err != nil {
		t.Fatal(err)
	}
	var del *sbRequest
	for _, r := range api.requests() {
		if r.method == "DELETE" {
			r := r
			del = &r
		}
	}
	if del == nil || del.confirm != "tok-1" || del.path != "/sandbox/sandboxes/"+testSandbox || del.body != "" {
		t.Fatalf("replay %+v", del)
	}
	if !strings.Contains(out, "destroyed") {
		t.Fatalf("output %q", out)
	}
}

func TestDestroyOfAnUnknownSandboxNeverBothersTheOwner(t *testing.T) {
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, 404, "", "Sandbox not found") })
	_, _, err := sbRun(t, api, "sandbox", "destroy", testSandbox, "--yes")
	if err == nil || exitcode.From(err) != exitcode.NotFound {
		t.Fatalf("got %v", err)
	}
	if len(api.requests()) != 1 {
		t.Fatal("no confirmation may be registered for a sandbox that is not there")
	}
	if _, _, err := sbRun(t, api, "sandbox", "destroy", "x"); err == nil || exitcode.From(err) != exitcode.Usage {
		t.Fatalf("got %v", err)
	}
}

func execRun(t *testing.T, api *sbServer, stdin string, args ...string) (string, string, error) {
	t.Helper()
	env, out, errBuf := newTestEnv(t, api.URL)
	env.In = strings.NewReader(stdin)
	err := run(t, env, append([]string{"sandbox", "exec"}, args...)...)
	return out.String(), errBuf.String(), err
}

func TestExecSendsTheRequestAndSplitsOutput(t *testing.T) {
	api := newSBServer(t, fixedSB(`{"exitCode":0,"stdout":"out\n","stderr":"warn\n","stdoutTruncated":false,"stderrTruncated":false,"timedOut":false,"durationMs":7}`))
	out, errText, err := execRun(t, api, "", testSandbox, "--cwd", "/workspace/app", "--env", "A=1", "--env", "B=x=y", "--timeout", "30s", "--", "python3", "-c", "print(1)")
	if err != nil {
		t.Fatal(err)
	}
	if out != "out\n" || errText != "warn\n" {
		t.Fatalf("stdout %q stderr %q", out, errText)
	}
	r := api.only(t)
	if r.method != "POST" || r.path != "/sandbox/sandboxes/"+testSandbox+"/exec" || r.idem != "" {
		t.Fatalf("request %+v", r)
	}
	b := decodeBody(t, r.body)
	cmdv, _ := b["command"].([]any)
	env, _ := b["env"].(map[string]any)
	if len(cmdv) != 3 || cmdv[0] != "python3" || cmdv[2] != "print(1)" || b["cwd"] != "/workspace/app" || b["timeoutSeconds"] != float64(30) || env["B"] != "x=y" || len(env) != 2 {
		t.Fatalf("body %s", r.body)
	}
	if _, has := b["stdin"]; has {
		t.Fatal("an empty stdin must not be sent")
	}
}

func TestExecShellLineAndExactlyOneForm(t *testing.T) {
	api := newSBServer(t, fixedSB(execOKJSON))
	if _, _, err := execRun(t, api, "", testSandbox, "--sh", "make test | tail"); err != nil {
		t.Fatal(err)
	}
	if b := decodeBody(t, api.only(t).body); b["command"] != "make test | tail" {
		t.Fatalf("shell body %v", b)
	}
	bad := [][]string{
		{testSandbox},
		{testSandbox, "--sh", "x", "--", "ls"},
		{testSandbox, "ls"},
		{testSandbox, "ls", "-la"},
		{testSandbox, "--", ""},
		{testSandbox, "--sh", "   "},
		{"nope", "--", "ls"},
		{testSandbox, "--cwd", "relative", "--", "ls"},
		{testSandbox, "--env", "NOEQUALS", "--", "ls"},
		{testSandbox, "--env", "1BAD=x", "--", "ls"},
	}
	for _, args := range bad {
		if _, _, err := execRun(t, api, "", args...); err == nil || exitcode.From(err) != exitcode.Usage {
			t.Errorf("%v: want usage error, got %v", args, err)
		}
	}
	if len(api.requests()) != 1 {
		t.Fatalf("bad invocations reached the network: %d", len(api.requests())-1)
	}
}

func TestExecTimeoutBounds(t *testing.T) {
	for flag, want := range map[string]float64{"500ms": 1, "1s": 1, "1500ms": 2, "10m": 600, "601s": 601, "1h": 3600, "3600s": 3600, "1ns": 1} {
		api := newSBServer(t, fixedSB(execOKJSON))
		if _, _, err := execRun(t, api, "", testSandbox, "--timeout", flag, "--", "ls"); err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if got := decodeBody(t, api.only(t).body)["timeoutSeconds"]; got != want {
			t.Errorf("--timeout %s sent %v, want %v", flag, got, want)
		}
	}
	for _, flag := range []string{"3601s", "1h1s", "-5s"} {
		api := newSBServer(t, fixedSB(execOKJSON))
		_, _, err := execRun(t, api, "", testSandbox, "--timeout", flag, "--", "ls")
		if err == nil || exitcode.From(err) != exitcode.Usage || !strings.Contains(err.Error(), "between 1 and 3600") || len(api.requests()) != 0 {
			t.Errorf("--timeout %s: %v (%d requests)", flag, err, len(api.requests()))
		}
	}
}

func TestExecExitCodesTimeoutAndTruncation(t *testing.T) {
	cases := []struct {
		name, data string
		wantExit   int
		wantErr    string
	}{
		{"zero", `{"exitCode":0,"stdout":"","stderr":""}`, 0, ""},
		{"three", `{"exitCode":3,"stdout":"","stderr":""}`, 3, ""},
		{"255", `{"exitCode":255}`, 255, ""},
		{"too big", `{"exitCode":300}`, 255, ""},
		{"negative", `{"exitCode":-1}`, 255, ""},
		{"timed out", `{"exitCode":-1,"timedOut":true,"stdout":"partial"}`, ExitTimedOut, "ran past its timeout"},
		{"timed out with zero", `{"exitCode":0,"timedOut":true}`, ExitTimedOut, "ran past its timeout"},
	}
	for _, tc := range cases {
		api := newSBServer(t, fixedSB(tc.data))
		_, _, err := execRun(t, api, "", testSandbox, "--", "x")
		if tc.wantExit == 0 {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || exitcode.From(err) != tc.wantExit || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: exit %d err %v", tc.name, exitcode.From(err), err)
		}
	}
	api := newSBServer(t, fixedSB(`{"exitCode":0,"stdout":"abc","stderr":"d","stdoutTruncated":true,"stderrTruncated":true}`))
	out, errText, err := execRun(t, api, "", testSandbox, "--", "x")
	if err != nil || out != "abc" {
		t.Fatalf("%q %v", out, err)
	}
	if !strings.Contains(errText, "stdout was cut off") || !strings.Contains(errText, "stderr was cut off") {
		t.Fatalf("truncation must be warned on stderr: %q", errText)
	}
}

func TestExecJSONPrintsTheWholeResultAndStillExitsWithTheCommandsCode(t *testing.T) {
	api := newSBServer(t, fixedSB(`{"exitCode":4,"stdout":"o","stderr":"e","stdoutTruncated":false,"stderrTruncated":false,"timedOut":false,"durationMs":9}`))
	out, errText, err := execRun(t, api, "", testSandbox, "--json", "--", "x")
	if exitcode.From(err) != 4 {
		t.Fatalf("exit %d", exitcode.From(err))
	}
	res := decodeBody(t, out)
	if res["stdout"] != "o" || res["stderr"] != "e" || res["exitCode"] != float64(4) || res["durationMs"] != float64(9) {
		t.Fatalf("json %s", out)
	}
	if errText != "" || err.Error() != "" {
		t.Fatalf("with --json nothing else is printed: %q / %q", errText, err.Error())
	}
}

func TestExecStdin(t *testing.T) {
	api := newSBServer(t, fixedSB(execOKJSON))
	if _, _, err := execRun(t, api, "line1\nline2\n", testSandbox, "--", "cat"); err != nil {
		t.Fatal(err)
	}
	if b := decodeBody(t, api.only(t).body); b["stdin"] != "line1\nline2\n" {
		t.Fatalf("stdin not sent: %v", b)
	}
	skip := newSBServer(t, fixedSB(execOKJSON))
	if _, _, err := execRun(t, skip, "data", testSandbox, "--no-stdin", "--", "cat"); err != nil {
		t.Fatal(err)
	}
	if _, has := decodeBody(t, skip.only(t).body)["stdin"]; has {
		t.Fatal("--no-stdin must not read or send stdin")
	}
	edge := newSBServer(t, fixedSB(execOKJSON))
	if _, _, err := execRun(t, edge, strings.Repeat("a", 1<<20), testSandbox, "--", "cat"); err != nil {
		t.Fatalf("exactly 1 MiB is allowed: %v", err)
	}
	for name, in := range map[string]string{"over": strings.Repeat("a", 1<<20+1), "binary": "\xff\xfe"} {
		srv := newSBServer(t, fixedSB(execOKJSON))
		_, _, err := execRun(t, srv, in, testSandbox, "--", "cat")
		if err == nil || exitcode.From(err) != exitcode.Usage || len(srv.requests()) != 0 {
			t.Errorf("%s stdin: %v", name, err)
		}
	}
}

func TestExecFaults(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code, msg string
		want      string
		wantExit  int
	}{
		{"not running", 409, "sandbox_not_running", "x", "x", 1},
		{"quarantined", 409, "sandbox_quarantined", "", "quarantined", 1},
		{"unreachable", 502, "sandbox_agent_unreachable", "", "did not answer", 1},
		{"exec unavailable", 503, "sandbox_exec_unavailable", "", "not available yet", 1},
		{"busy", 429, "sandbox_busy", "Too many commands.", "Too many commands.", exitcode.RateLimited},
		{"bad timeout", 400, "invalid_timeout", "timeoutSeconds is between 1 and 600.", "between 1 and 600", 1},
		{"bad cwd", 400, "bad_cwd", "The working directory does not exist or is outside /workspace.", "working directory", 1},
		{"paused", 503, "SERVICE_PAUSED", "Paused for maintenance.", "operator decision", 1},
	}
	for _, tc := range cases {
		api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, tc.status, tc.code, tc.msg) })
		_, _, err := execRun(t, api, "", testSandbox, "--", "x")
		if err == nil || !strings.Contains(err.Error(), tc.want) || exitcode.From(err) != tc.wantExit {
			t.Errorf("%s: exit %d, %v", tc.name, exitcode.From(err), err)
		}
		if len(api.requests()) != 1 {
			t.Errorf("%s: a refused command must not be repeated (%d requests)", tc.name, len(api.requests()))
		}
	}
}

func TestExecBusyIsRetriedOnceAfterRetryAfter(t *testing.T) {
	waits := recordWaits(t)
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			sbFail(w, 429, "sandbox_busy", "Busy.")
			return
		}
		sbOK(w, execOKJSON)
	})
	out, _, err := execRun(t, api, "", testSandbox, "--", "x")
	if err != nil || out != "hi\n" || len(api.requests()) != 2 || len(waits.all()) != 1 {
		t.Fatalf("%q %v %d %v", out, err, len(api.requests()), waits.all())
	}
}

func TestFileCopyUploadChunks(t *testing.T) {
	const chunk = 3 << 20
	for _, size := range []int{0, 1, chunk - 1, chunk, chunk + 1, 2 * chunk, 7 * chunk / 2} {
		dir := t.TempDir()
		data := bytes.Repeat([]byte("0123456789abcdef"), (size+15)/16)[:size]
		local := filepath.Join(dir, "f.bin")
		if err := os.WriteFile(local, data, 0o600); err != nil {
			t.Fatal(err)
		}
		api := newSBServer(t, okAll)
		out, _, err := sbRun(t, api, "sandbox", "cp", local, testSandbox+":/workspace/dir/f.bin")
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		reqs := api.requests()
		wantChunks := 1
		if size > chunk {
			wantChunks = (size + chunk - 1) / chunk
		}
		if len(reqs) != wantChunks {
			t.Fatalf("size %d: %d requests, want %d", size, len(reqs), wantChunks)
		}
		var joined []byte
		offset := 0
		for i, r := range reqs {
			if r.method != "PUT" || r.path != "/sandbox/sandboxes/"+testSandbox+"/files" || r.contentType != "application/octet-stream" || r.auth != "Bearer btk_test" {
				t.Fatalf("size %d chunk %d: %+v", size, i, r)
			}
			q, _ := url.ParseQuery(r.query)
			if q.Get("path") != "/workspace/dir/f.bin" || q.Get("parents") != "true" {
				t.Fatalf("query %s", r.query)
			}
			if wantChunks == 1 {
				if q.Has("offset") {
					t.Fatalf("a single request replaces the file atomically and carries no offset: %s", r.query)
				}
			} else if q.Get("offset") != strconv.Itoa(offset) {
				t.Fatalf("size %d chunk %d: offset %s, want %d", size, i, q.Get("offset"), offset)
			}
			if len(r.body) > chunk {
				t.Fatalf("a chunk of %d bytes exceeds 3 MiB", len(r.body))
			}
			offset += len(r.body)
			joined = append(joined, r.body...)
		}
		if !bytes.Equal(joined, data) {
			t.Fatalf("size %d: the chunks do not add up to the file", size)
		}
		if res := decodeBody(t, out); res["bytes"] != float64(size) || res["direction"] != "upload" {
			t.Fatalf("result %s", out)
		}
	}
}

func TestFileCopyUploadRefusals(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	big := filepath.Join(dir, "big")
	f, _ := os.Create(big)
	_ = f.Truncate(100<<20 + 1)
	_ = f.Close()
	api := newSBServer(t, okAll)
	for name, args := range map[string][]string{
		"directory":      {"sandbox", "cp", dir, testSandbox + ":/workspace/d"},
		"missing":        {"sandbox", "cp", filepath.Join(dir, "nope"), testSandbox + ":/workspace/d"},
		"too big":        {"sandbox", "cp", big, testSandbox + ":/workspace/big"},
		"both remote":    {"sandbox", "cp", testSandbox + ":/workspace/a", testSandbox + ":/workspace/b"},
		"neither remote": {"sandbox", "cp", file, filepath.Join(dir, "b")},
		"relative path":  {"sandbox", "cp", file, testSandbox + ":workspace/a"},
	} {
		if _, _, err := sbRun(t, api, args...); err == nil {
			t.Errorf("%s: must fail", name)
		}
	}
	if len(api.requests()) != 0 {
		t.Fatalf("refused copies reached the network: %d", len(api.requests()))
	}
	if _, _, err := sbRun(t, api, "sandbox", "cp", file, testSandbox+":/workspace/dir/"); err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(api.only(t).query)
	if q.Get("path") != "/workspace/dir/a.txt" {
		t.Fatalf("a trailing slash names the directory, path was %q", q.Get("path"))
	}
}

func TestFileCopyUploadStopsAtTheFirstFailedChunk(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "big.bin")
	_ = os.WriteFile(local, bytes.Repeat([]byte("z"), 7<<20), 0o600)
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 2 {
			sbFail(w, 502, "sandbox_agent_unreachable", "")
			return
		}
		sbOK(w, `{"bytes":1}`)
	})
	_, errText, err := sbRun(t, api, "sandbox", "cp", local, testSandbox+":/workspace/big.bin")
	if err == nil || len(api.requests()) != 2 {
		t.Fatalf("got %d requests, %v", len(api.requests()), err)
	}
	if !strings.Contains(errText, "Upload stopped after 3145728 of 7340032 bytes") || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("%q / %v", errText, err)
	}
	tooBig := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sbFail(w, 413, "file_too_large", "Files are limited to 32 MiB.")
	})
	if _, _, err := sbRun(t, tooBig, "sandbox", "cp", local, testSandbox+":/workspace/big.bin"); err == nil || !strings.Contains(err.Error(), "limited to 32 MiB") {
		t.Fatalf("the server's own limit message must be shown: %v", err)
	}
}

func TestFileCopyDownload(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 2<<20)
	api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	})
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	out, _, err := sbRun(t, api, "sandbox", "cp", testSandbox+":/workspace/a/out.bin", dest)
	if err != nil {
		t.Fatal(err)
	}
	r := api.only(t)
	q, _ := url.ParseQuery(r.query)
	if r.method != "GET" || r.path != "/sandbox/sandboxes/"+testSandbox+"/files" || q.Get("path") != "/workspace/a/out.bin" || q.Has("format") {
		t.Fatalf("request %+v", r)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, payload) {
		t.Fatal("the downloaded bytes differ")
	}
	if decodeBody(t, out)["bytes"] != float64(len(payload)) {
		t.Fatalf("result %s", out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}

	if _, _, err := sbRun(t, api, "sandbox", "cp", testSandbox+":/workspace/a/out.bin", dest); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("an existing file must not be overwritten: %v", err)
	}
	if n := len(api.requests()); n != 1 {
		t.Fatalf("the refusal must come before any download, saw %d requests", n)
	}
	small := newSBServer(t, okAll)
	if _, _, err := sbRun(t, small, "sandbox", "cp", testSandbox+":/workspace/a/out.bin", dest, "--force"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "hello" {
		t.Fatalf("--force did not replace: %q", got)
	}
	if _, _, err := sbRun(t, small, "sandbox", "cp", testSandbox+":/workspace/dir/name.txt", dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "name.txt")); string(got) != "hello" {
		t.Fatalf("a directory target takes the remote name: %q", got)
	}
}

func TestFileCopyDownloadFaults(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "x")
	cut := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	if _, _, err := sbRun(t, cut, "sandbox", "cp", testSandbox+":/workspace/x", dest); err == nil {
		t.Fatal("a broken stream must fail")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a partial file was left: %v", entries)
	}
	for name, status := range map[string]int{"missing": 404, "not running": 409, "server error": 500} {
		code := map[int]string{404: "file_not_found", 409: "sandbox_not_running", 500: ""}[status]
		srv := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, status, code, "") })
		target := filepath.Join(dir, "never-"+strconv.Itoa(status))
		if _, _, err := sbRun(t, srv, "sandbox", "cp", testSandbox+":/workspace/x", target); err == nil {
			t.Errorf("%s: must fail", name)
		}
		if _, err := os.Stat(target); err == nil {
			t.Errorf("%s: a file was created", name)
		}
	}
	nf := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sbFail(w, 404, "file_not_found", "No such file or directory.")
	})
	_, _, err := sbRun(t, nf, "sandbox", "cp", testSandbox+":/workspace/x", filepath.Join(dir, "nf"))
	if err == nil || strings.Contains(err.Error(), "not available on this server") || exitcode.From(err) != exitcode.NotFound {
		t.Fatalf("a missing file is not a missing API: %v", err)
	}
	ok := newSBServer(t, okAll)
	if _, _, err := sbRun(t, ok, "sandbox", "cp", testSandbox+":/workspace/x", filepath.Join(dir, "no", "such", "dir", "f")); err == nil {
		t.Fatal("an unwritable destination must fail")
	}
}

func TestLsAndMkdir(t *testing.T) {
	api := newSBServer(t, okAll)
	out, _, err := sbRun(t, api, "sandbox", "ls", testSandbox+":/workspace/src", "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	r := api.requests()[0]
	if r.method != "GET" || r.path != "/sandbox/sandboxes/"+testSandbox+"/files/list" || r.query != "path=%2Fworkspace%2Fsrc" {
		t.Fatalf("request %+v", r)
	}
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "file") {
		t.Fatalf("table %q", out)
	}
	if _, _, err := sbRun(t, api, "sandbox", "ls", testSandbox); err != nil {
		t.Fatal(err)
	}
	if q := api.requests()[1].query; q != "path=%2Fworkspace" {
		t.Fatalf("a bare id lists /workspace, got %q", q)
	}
	out, _, _ = sbRun(t, api, "sandbox", "ls", testSandbox+":/workspace")
	if decodeBody(t, out)["truncated"] != false {
		t.Fatalf("json %s", out)
	}
	if _, _, err := sbRun(t, api, "sandbox", "mkdir", testSandbox+":/workspace/a/b"); err != nil {
		t.Fatal(err)
	}
	m := api.requests()[len(api.requests())-1]
	if m.method != "POST" || m.path != "/sandbox/sandboxes/"+testSandbox+"/files/mkdir" || m.query != "path=%2Fworkspace%2Fa%2Fb" {
		t.Fatalf("mkdir %+v", m)
	}
	n := len(api.requests())
	for _, args := range [][]string{{"sandbox", "ls", "nope"}, {"sandbox", "mkdir", testSandbox}, {"sandbox", "mkdir", "x:/a"}} {
		if _, _, err := sbRun(t, api, args...); err == nil || exitcode.From(err) != exitcode.Usage {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(api.requests()) != n {
		t.Fatal("a bad argument reached the network")
	}
	cut := newSBServer(t, fixedSB(`{"path":"/workspace","entries":[],"truncated":true}`))
	_, errText, err := sbRun(t, cut, "sandbox", "ls", testSandbox, "--format", "table")
	if err != nil || !strings.Contains(errText, "cut off") {
		t.Fatalf("%q %v", errText, err)
	}
	empty := newSBServer(t, fixedSB(`{"path":"/workspace","entries":null,"truncated":false}`))
	if out, _, err := sbRun(t, empty, "sandbox", "ls", testSandbox, "--format", "table"); err != nil || !strings.Contains(out, "No results.") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestEverySandboxSubcommandMapsInsufficientScopeToTheGrantMessage(t *testing.T) {
	for name, args := range sandboxInvocations(t) {
		t.Run(name, func(t *testing.T) {
			api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				sbFailWith(w, 403, "insufficient_scope", "This token was not granted sandbox:write.", map[string]any{
					"code": "insufficient_scope", "missingScope": []string{"sandbox:write", "sandbox:files"},
				})
			})
			_, _, err := sbRun(t, api, args...)
			if err == nil {
				t.Fatal("must fail")
			}
			msg := err.Error()
			for _, want := range []string{"sandbox:write and sandbox:files", "agent connection", "--scope sandbox:write,sandbox:files"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("message %q lacks %q", msg, want)
				}
			}
			if exitcode.From(err) != exitcode.Unauthenticated || strings.Contains(msg, "\n") {
				t.Fatalf("exit %d, message %q", exitcode.From(err), msg)
			}
		})
	}
}

func TestEverySandboxSubcommandMapsTheNewConflictCodes(t *testing.T) {
	codes := map[string]struct {
		status int
		want   string
	}{
		"no_free_slot": {409, "Every slot is in use"}, "sandbox_not_running": {409, "not running"}, "sandbox_quarantined": {409, "quarantined"},
		"sandbox_agent_unreachable": {502, "did not answer"}, "sandbox_exec_unavailable": {503, "not available yet"},
		"sandbox_busy": {429, "Wait a moment"}, "sandbox_create_rate_limited": {429, "Wait a moment"},
	}
	fastBackoff(t)
	for code, c := range codes {
		for name, args := range sandboxInvocations(t) {
			api := newSBServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, c.status, code, "") })
			_, _, err := sbRun(t, api, args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s / %s: %v", code, name, err)
			}
		}
	}
}

func TestCommandOutputIsNeverEchoedToDiagnostics(t *testing.T) {
	secret := "hunter2-from-the-sandbox"
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			sbOK(w, `{"exitCode":0,"stdout":"`+secret+`","stderr":""}`)
			return
		}
		okAll(w, r, 0)
	})
	env, out, errBuf := newTestEnv(t, api.URL)
	env.In = strings.NewReader("")
	if err := run(t, env, "sandbox", "exec", testSandbox, "--debug", "--", "cat", "/workspace/.env"); err != nil {
		t.Fatal(err)
	}
	if out.String() != secret {
		t.Fatalf("stdout %q", out.String())
	}
	if strings.Contains(errBuf.String(), secret) {
		t.Fatalf("command output reached stderr diagnostics: %q", errBuf.String())
	}
}
