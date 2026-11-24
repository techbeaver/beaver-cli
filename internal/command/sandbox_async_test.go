package command

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

const runningJSON = `{"execId":"` + testExec + `","status":"running","elapsedMs":31000,"stdoutTail":"tail so far","stderrTail":""}`

const finishedJSON = `{"execId":"` + testExec + `","status":"exited","exitCode":0,"stdout":"final out\n","stderr":"final err\n","stdoutTruncated":false,"stderrTruncated":false,"timedOut":false,"canceled":false,"durationMs":95000}`

func quickPolls(t *testing.T) {
	t.Helper()
	saved := pollBackoff
	pollBackoff = time.Millisecond
	t.Cleanup(func() { pollBackoff = saved })
}

func noSignals(t *testing.T) {
	t.Helper()
	saved := interruptSignals
	interruptSignals = func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() {} }
	t.Cleanup(func() { interruptSignals = saved })
}

func asyncServer(t *testing.T, polls int, final string) *sbServer {
	t.Helper()
	var mu sync.Mutex
	seen := 0
	return newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == http.MethodPost {
			w.WriteHeader(202)
			sbOK(w, runningJSON)
			return
		}
		mu.Lock()
		seen++
		n := seen
		mu.Unlock()
		if n <= polls {
			sbOK(w, runningJSON)
			return
		}
		sbOK(w, final)
	})
}

func TestExecFinishingInTheFirstCallPollsNothing(t *testing.T) {
	noSignals(t)
	api := newSBServer(t, fixedSB(finishedJSON))
	out, errText, err := execRun(t, api, "", testSandbox, "--", "x")
	if err != nil || out != "final out\n" || errText != "final err\n" || len(api.requests()) != 1 {
		t.Fatalf("%q %q %v %d", out, errText, err, len(api.requests()))
	}
	if b := decodeBody(t, api.only(t).body); b["waitSeconds"] != float64(30) {
		t.Fatalf("the first call waits 30 seconds, body %v", b)
	}
	old := newSBServer(t, fixedSB(`{"exitCode":0,"stdout":"old api"}`))
	if out, _, err := execRun(t, old, "", testSandbox, "--", "x"); err != nil || out != "old api" {
		t.Fatalf("an answer with no status is a finished result: %q %v", out, err)
	}
}

func TestExecNeedingThreePollsPrintsOnlyTheFinalOutputOnce(t *testing.T) {
	noSignals(t)
	api := asyncServer(t, 2, finishedJSON)
	out, errText, err := execRun(t, api, "", testSandbox, "--", "sleep", "95")
	if err != nil {
		t.Fatal(err)
	}
	if out != "final out\n" || errText != "final err\n" {
		t.Fatalf("only the final output is printed, once: %q / %q", out, errText)
	}
	reqs := api.requests()
	if len(reqs) != 4 || reqs[0].method != "POST" {
		t.Fatalf("one start and three polls expected: %+v", reqs)
	}
	for _, r := range reqs[1:] {
		if r.method != "GET" || r.path != "/sandbox/sandboxes/"+testSandbox+"/exec/"+testExec || r.query != "waitSeconds=60" || r.auth != "Bearer btk_test" {
			t.Fatalf("poll %+v", r)
		}
	}
	for _, r := range reqs {
		if strings.Contains(r.body, "tail so far") {
			t.Fatal("tails are never sent back")
		}
	}
}

func TestExecPollExitCodesFollowTheFinalResult(t *testing.T) {
	noSignals(t)
	for name, tc := range map[string]struct {
		final string
		code  int
	}{
		"exit 3":    {`{"status":"exited","exitCode":3,"stdout":"","stderr":""}`, 3},
		"timed out": {`{"status":"exited","exitCode":-1,"timedOut":true,"stdout":"p"}`, ExitTimedOut},
		"canceled":  {`{"status":"exited","exitCode":-1,"canceled":true}`, 130},
	} {
		api := asyncServer(t, 1, tc.final)
		_, _, err := execRun(t, api, "", testSandbox, "--", "x")
		if exitcode.From(err) != tc.code {
			t.Errorf("%s: exit %d, %v", name, exitcode.From(err), err)
		}
	}
}

func TestExecPollErrorsAreReportedAndNeverRetried(t *testing.T) {
	noSignals(t)
	quickPolls(t)
	for _, tc := range []struct {
		status    int
		code, msg string
		want      string
	}{
		{404, "sandbox_exec_not_found", "", "No such command"},
		{410, "sandbox_exec_expired", "", "kept for 10 minutes"},
		{410, "sandbox_exec_lost", "", "agent restarted"},
		{410, "sandbox_exec_killed", "", "not running"},
		{409, "sandbox_not_running", "", "not running"},
		{403, "insufficient_scope", "x", "agent connection"},
	} {
		var polls int
		api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			if r.Method == "POST" {
				w.WriteHeader(202)
				sbOK(w, runningJSON)
				return
			}
			polls++
			sbFail(w, tc.status, tc.code, tc.msg)
		})
		_, _, err := execRun(t, api, "", testSandbox, "--", "x")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.code, err)
		}
		if polls != 1 {
			t.Errorf("%s: a real answer must not be polled again (%d polls)", tc.code, polls)
		}
	}
}

func TestExecResumesTheSamePollAfterANetworkError(t *testing.T) {
	noSignals(t)
	quickPolls(t)
	var mu sync.Mutex
	polls := 0
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			sbOK(w, runningJSON)
			return
		}
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		switch n {
		case 1:
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		case 2:
			sbFail(w, 502, "", "")
		default:
			sbOK(w, finishedJSON)
		}
	})
	out, errText, err := execRun(t, api, "", testSandbox, "--", "x")
	if err != nil || out != "final out\n" {
		t.Fatalf("%q %v", out, err)
	}
	if !strings.Contains(errText, "Lost contact") {
		t.Fatalf("the user is told: %q", errText)
	}
	posts := 0
	for _, r := range api.requests() {
		if r.method == "POST" {
			posts++
		} else if r.path != "/sandbox/sandboxes/"+testSandbox+"/exec/"+testExec {
			t.Fatalf("a poll named another id: %+v", r)
		}
	}
	if posts != 1 || len(api.requests()) != 4 {
		t.Fatalf("the start is sent once and the poll resumed: %d posts, %d requests", posts, len(api.requests()))
	}
}

func TestExecGivesUpPollingAfterRepeatedFailuresAndSaysHowToCheck(t *testing.T) {
	noSignals(t)
	quickPolls(t)
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			sbOK(w, runningJSON)
			return
		}
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	_, _, err := execRun(t, api, "", testSandbox, "--", "x")
	if err == nil || !strings.Contains(err.Error(), "beaver sandbox exec-status "+testSandbox+" "+testExec) {
		t.Fatalf("%v", err)
	}
	posts := 0
	for _, r := range api.requests() {
		if r.method == "POST" {
			posts++
		}
	}
	if n := len(api.requests()); posts != 1 || n < 1+execPollAttempts {
		t.Fatalf("%d requests, %d starts", n, posts)
	}
}

func TestExecStartIsNeverRetriedAutomatically(t *testing.T) {
	noSignals(t)
	fastBackoff(t)
	for name, handler := range map[string]func(http.ResponseWriter, *http.Request, int){
		"reset": func(w http.ResponseWriter, _ *http.Request, _ int) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		},
		"502":         func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, 502, "sandbox_agent_unreachable", "") },
		"503":         func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, 503, "billing_unavailable", "") },
		"429 no wait": func(w http.ResponseWriter, _ *http.Request, _ int) { sbFail(w, 429, "sandbox_busy", "") },
	} {
		api := newSBServer(t, handler)
		_, _, err := execRun(t, api, "", testSandbox, "--", "x")
		if err == nil || len(api.requests()) != 1 {
			t.Errorf("%s: %d requests, %v", name, len(api.requests()), err)
		}
		if name == "reset" && !strings.Contains(err.Error(), "may or may not have started") {
			t.Errorf("an unknown outcome must be said: %v", err)
		}
	}
}

func TestExecDetachPrintsTheIDAndReturns(t *testing.T) {
	noSignals(t)
	api := asyncServer(t, 99, finishedJSON)
	out, _, err := execRun(t, api, "", testSandbox, "--detach", "--", "sleep", "500")
	if err != nil || strings.TrimSpace(out) != testExec || len(api.requests()) != 1 {
		t.Fatalf("%q %v %d", out, err, len(api.requests()))
	}
	out, _, err = execRun(t, api, "", testSandbox, "--detach", "--json", "--", "x")
	if err != nil || decodeBody(t, out)["execId"] != testExec {
		t.Fatalf("%q %v", out, err)
	}
	quick := newSBServer(t, fixedSB(finishedJSON))
	out, _, err = execRun(t, quick, "", testSandbox, "--detach", "--", "x")
	if err != nil || out != "final out\n" {
		t.Fatalf("a command that already finished is just printed: %q %v", out, err)
	}
}

func TestCtrlCCancelsOnceAndExits130(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	saved := interruptSignals
	interruptSignals = func() (<-chan os.Signal, func()) { return sigs, func() {} }
	t.Cleanup(func() { interruptSignals = saved })

	polling := make(chan struct{}, 4)
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cancel"):
			sbOK(w, `{"execId":"`+testExec+`","status":"exited","exitCode":-1,"stdout":"partial","stderr":"","canceled":true}`)
		case r.Method == "POST":
			w.WriteHeader(202)
			sbOK(w, runningJSON)
		default:
			polling <- struct{}{}
			<-r.Context().Done()
		}
	})
	done := make(chan error, 1)
	var out, errText string
	go func() {
		var err error
		out, errText, err = execRun(t, api, "", testSandbox, "--", "sleep", "500")
		done <- err
	}()
	select {
	case <-polling:
	case <-time.After(5 * time.Second):
		t.Fatal("never polled")
	}
	sigs <- os.Interrupt
	select {
	case err := <-done:
		if exitcode.From(err) != 130 {
			t.Fatalf("exit %d, %v", exitcode.From(err), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit after Ctrl-C")
	}
	cancels := 0
	for _, r := range api.requests() {
		if strings.HasSuffix(r.path, "/cancel") {
			cancels++
			if r.method != "POST" || r.path != "/sandbox/sandboxes/"+testSandbox+"/exec/"+testExec+"/cancel" {
				t.Fatalf("cancel %+v", r)
			}
		}
		if r.method == "DELETE" {
			t.Fatal("tokens cannot DELETE: cancel is a POST")
		}
	}
	if cancels != 1 {
		t.Fatalf("exactly one cancel, got %d", cancels)
	}
	if out != "partial" || !strings.Contains(errText, "Interrupted") {
		t.Fatalf("what it wrote before the cancel is printed: %q / %q", out, errText)
	}
}

func TestASecondCtrlCLeavesAtOnceEvenIfTheCancelHangs(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	saved := interruptSignals
	interruptSignals = func() (<-chan os.Signal, func()) { return sigs, func() {} }
	t.Cleanup(func() { interruptSignals = saved })

	hanging := make(chan struct{}, 4)
	polling := make(chan struct{}, 4)
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cancel"):
			hanging <- struct{}{}
			<-r.Context().Done()
		case r.Method == "POST":
			w.WriteHeader(202)
			sbOK(w, runningJSON)
		default:
			polling <- struct{}{}
			<-r.Context().Done()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := execRun(t, api, "", testSandbox, "--", "sleep", "500")
		done <- err
	}()
	<-polling
	sigs <- os.Interrupt
	select {
	case <-hanging:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel was never sent")
	}
	sigs <- os.Interrupt
	select {
	case err := <-done:
		if exitcode.From(err) != 130 {
			t.Fatalf("exit %d", exitcode.From(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second Ctrl-C must leave at once")
	}
}

func TestCtrlCBeforeTheCommandIsAcceptedDropsTheCallAndExits130(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	saved := interruptSignals
	interruptSignals = func() (<-chan os.Signal, func()) { return sigs, func() {} }
	t.Cleanup(func() { interruptSignals = saved })
	started := make(chan struct{}, 1)
	api := newSBServer(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		started <- struct{}{}
		<-r.Context().Done()
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := execRun(t, api, "", testSandbox, "--", "x")
		done <- err
	}()
	<-started
	sigs <- os.Interrupt
	select {
	case err := <-done:
		if exitcode.From(err) != 130 {
			t.Fatalf("exit %d, %v", exitcode.From(err), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit")
	}
	if len(api.requests()) != 1 {
		t.Fatalf("nothing else may be sent: %d", len(api.requests()))
	}
}

func TestExecStatusAndCancelCommands(t *testing.T) {
	api := newSBServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == "POST" {
			sbOK(w, `{"execId":"`+testExec+`","status":"exited","exitCode":-1,"canceled":true,"stdout":"x"}`)
			return
		}
		sbOK(w, runningJSON)
	})
	out, _, err := sbRun(t, api, "sandbox", "exec-status", testSandbox, testExec, "--wait", "60")
	if err != nil || decodeBody(t, out)["status"] != "running" {
		t.Fatalf("%q %v", out, err)
	}
	r := api.requests()[0]
	if r.method != "GET" || r.path != "/sandbox/sandboxes/"+testSandbox+"/exec/"+testExec || r.query != "waitSeconds=60" {
		t.Fatalf("status %+v", r)
	}
	if _, _, err := sbRun(t, api, "sandbox", "exec-status", testSandbox, testExec); err != nil {
		t.Fatal(err)
	}
	if api.requests()[1].query != "waitSeconds=0" {
		t.Fatalf("default wait is 0: %q", api.requests()[1].query)
	}
	out, _, err = sbRun(t, api, "sandbox", "exec-cancel", testSandbox, testExec)
	if err != nil || decodeBody(t, out)["canceled"] != true {
		t.Fatalf("%q %v", out, err)
	}
	if c := api.requests()[2]; c.method != "POST" || c.path != "/sandbox/sandboxes/"+testSandbox+"/exec/"+testExec+"/cancel" || c.body != "" {
		t.Fatalf("cancel %+v", c)
	}
	n := len(api.requests())
	for _, args := range [][]string{
		{"sandbox", "exec-status", testSandbox, "short"},
		{"sandbox", "exec-status", testSandbox, strings.ToUpper(testExec)},
		{"sandbox", "exec-status", testSandbox, testExec, "--wait", "61"},
		{"sandbox", "exec-status", testSandbox, testExec, "--wait", "-1"},
		{"sandbox", "exec-cancel", "nope", testExec},
		{"sandbox", "exec-cancel", testSandbox, testExec + "0"},
	} {
		if _, _, err := sbRun(t, api, args...); err == nil || exitcode.From(err) != exitcode.Usage {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(api.requests()) != n {
		t.Fatal("a bad id reached the network")
	}
}
