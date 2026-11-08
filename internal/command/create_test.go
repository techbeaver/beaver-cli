package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

// apiContract is the request body each create route accepts. A field the API does not know is
// silently dropped there, so the stand-in refuses it here instead: that is how "region" for
// "regionCode" and "plan" for "dbaasPlanId" shipped in 0.2.0.
var apiContract = []struct {
	method, pattern string
	required        []string
	allowed         []string
	needsKey        bool
}{
	{http.MethodPost, `^/paas/projects$`, []string{"name", "regionCode"}, []string{"description"}, false},
	{http.MethodPost, `^/paas/apps$`, []string{"projectId", "name"}, []string{
		"gitRepo", "branch", "rootDirectory", "subdomain", "framework", "deployMethod", "registryCredentialId",
		"imageUrl", "port", "paasPlanId", "billingCycle", "envVars", "buildEnv", "discountCode", "applyCredit", "paymentSource",
	}, true},
	{http.MethodPost, `^/dbaas/projects/[^/]+/instances$`, []string{"name", "dbaasPlanId"}, []string{
		"engineVersion", "billingCycle", "haTier", "pooledEnabled", "databaseName", "username", "paymentSource",
	}, true},
	{http.MethodPut, `^/paas/apps/[^/]+/replicas$`, []string{"replicas"}, nil, true},
	{http.MethodPut, `^/paas/apps/[^/]+/env$`, []string{"envVars"}, nil, false},
	{http.MethodPatch, `^/paas/apps/[^/]+/env$`, []string{"envVars"}, nil, false},
}

// contractViolation answers what the API would refuse, or "" when the request is acceptable.
func contractViolation(r *http.Request, path string, body map[string]any) string {
	for _, c := range apiContract {
		if r.Method != c.method || !regexp.MustCompile(c.pattern).MatchString(path) {
			continue
		}
		if c.needsKey && strings.TrimSpace(r.Header.Get(IdempotencyHeader)) == "" {
			return "idempotency_key_required"
		}
		known := map[string]bool{}
		for _, k := range append(append([]string{}, c.required...), c.allowed...) {
			known[k] = true
		}
		for _, k := range c.required {
			if _, ok := body[k]; !ok {
				return "missing " + k
			}
		}
		for k := range body {
			if !known[k] {
				return "unknown field " + k
			}
		}
	}
	return ""
}

type strictAPI struct {
	*fakeAPI
	mu   sync.Mutex
	keys []string
	env  map[string]string
	// revealed is set if any read asked for reveal; writes lists each env write sent.
	revealed bool
	// leak makes a plain read return real values, as a misbehaving server might.
	leak   bool
	writes []string
	// confirmedBody is the body the registered confirmation was bound to.
	confirmedBody []byte
	// script answers the first calls to charge routes before the normal handler does.
	script []func(w http.ResponseWriter) bool
}

func newStrictAPI(t *testing.T) *strictAPI {
	t.Helper()
	s := &strictAPI{env: map[string]string{"A": "1", "B": "2"}}
	s.fakeAPI = newFakeAPI(t, func(a *fakeAPI, w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		body := a.requests[len(a.requests)-1].body
		if v := contractViolation(r, path, body); v != "" {
			writeEnvelope(w, http.StatusBadRequest, map[string]any{"error": v})
			return
		}
		s.mu.Lock()
		if k := r.Header.Get(IdempotencyHeader); k != "" {
			s.keys = append(s.keys, k)
			if len(s.script) > 0 {
				step := s.script[0]
				s.script = s.script[1:]
				if step(w) {
					s.mu.Unlock()
					return
				}
			}
		}
		s.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && path == "/mcp/confirmations":
			s.confirmedBody, _ = json.Marshal(body["body"])
			writeEnvelope(w, http.StatusCreated, map[string]any{"id": "conf-1", "status": "pending", "approvalUrl": "https://example/approve"})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/mcp/confirmations/"):
			writeEnvelope(w, http.StatusOK, map[string]any{"status": "approved", "confirmationToken": "ct-1"})
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/env") && r.Header.Get(ConfirmationHeader) != "ct-1":
			writeEnvelope(w, http.StatusPreconditionRequired, map[string]any{"code": "confirmation_required"})
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/env") && !bytes.Equal(compact(s.confirmedBody), compact(a.requests[len(a.requests)-1].raw)):
			writeEnvelope(w, http.StatusForbidden, map[string]any{"code": "confirmation_body_mismatch"})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/env/upsert"):
			sent := body["envVars"].(map[string]any)
			for k, v := range sent {
				if v.(string) == EnvWithheldMarker {
					writeEnvelope(w, http.StatusBadRequest, map[string]any{"code": "withheld_marker_for_unset_key"})
					return
				}
				s.env[k] = v.(string)
			}
			s.writes = append(s.writes, "UPSERT "+string(a.requests[len(a.requests)-1].raw))
			writeEnvelope(w, http.StatusOK, map[string]any{})
		case r.Method == http.MethodDelete && strings.Contains(path, "/env/") && r.Header.Get(ConfirmationHeader) != "ct-1":
			writeEnvelope(w, http.StatusPreconditionRequired, map[string]any{"code": "confirmation_required"})
		case r.Method == http.MethodDelete && strings.Contains(path, "/env/"):
			key := path[strings.LastIndex(path, "/")+1:]
			if _, ok := s.env[key]; !ok {
				writeEnvelope(w, http.StatusNotFound, map[string]any{"code": "env_key_not_found"})
				return
			}
			delete(s.env, key)
			s.writes = append(s.writes, "DELETE "+key+" body="+string(a.requests[len(a.requests)-1].raw))
			writeEnvelope(w, http.StatusOK, map[string]any{})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/env"):
			s.revealed = r.URL.Query().Get("reveal") == "true"
			rows := []map[string]any{}
			for k, v := range s.env {
				if s.revealed || s.leak {
					rows = append(rows, map[string]any{"key": k, "value": v})
				} else {
					rows = append(rows, map[string]any{"key": k, "value": EnvWithheldMarker})
				}
			}
			writeEnvelope(w, http.StatusOK, rows)
		case strings.HasSuffix(path, "/env"):
			sent := body["envVars"].(map[string]any)
			next := map[string]string{}
			if r.Method == http.MethodPatch {
				for k, v := range s.env {
					next[k] = v
				}
			}
			for k, v := range sent {
				if v.(string) == EnvWithheldMarker {
					old, ok := s.env[k]
					if !ok {
						writeEnvelope(w, http.StatusBadRequest, map[string]any{"code": "withheld_marker_for_unset_key"})
						return
					}
					next[k] = old
					continue
				}
				next[k] = v.(string)
			}
			s.env = next
			s.writes = append(s.writes, r.Method+" "+string(a.requests[len(a.requests)-1].raw))
			writeEnvelope(w, http.StatusOK, map[string]any{})
		default:
			writeEnvelope(w, http.StatusCreated, map[string]any{"id": "new-1", "status": "pending_payment"})
		}
	})
	return s
}

func compact(raw []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

func failWith(status int, code string) func(http.ResponseWriter) bool {
	return func(w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"status":%d,"success":false,"message":"no","code":%q}`, status, code)
		return true
	}
}

func dropConnection(w http.ResponseWriter) bool {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
	return true
}

func noBackoff(t *testing.T) {
	t.Helper()
	saved := chargeBackoff
	chargeBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { chargeBackoff = saved })
}

func TestEveryCreateSendsWhatTheAPIAccepts(t *testing.T) {
	cases := [][]string{
		{"projects", "create", "bothy", "--region", "EU"},
		{"apps", "create", "bothy", "--project", "p1", "--repo", "https://github.com/a/b", "--branch", "main",
			"--root-dir", "web", "--subdomain", "bothy", "--framework", "dockerfile", "--port", "8080",
			"--plan", "plan-1", "--billing-cycle", "monthly", "--env", "A=1", "--build-env", "B=2"},
		{"db", "create", "bothy-db", "--project", "p1", "--plan", "plan-2", "--version", "18",
			"--billing-cycle", "monthly", "--database-name", "bothy", "--username", "bothy"},
		{"apps", "scale", "app-1", "--replicas", "2"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			api := newStrictAPI(t)
			env, _, _ := newTestEnv(t, api.URL)
			if err := run(t, env, args...); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

func TestAProjectNeedsARegion(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	err := run(t, env, "projects", "create", "bothy")
	if !errors.Is(err, exitcode.ErrUsage) || !strings.Contains(err.Error(), "--region") {
		t.Fatalf("err = %v", err)
	}
	if len(api.requests) != 0 {
		t.Error("called the API without a region")
	}
}

func TestADatabaseNeedsAPlan(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "db", "create", "x", "--project", "p1"); !errors.Is(err, exitcode.ErrUsage) {
		t.Fatalf("err = %v", err)
	}
	if len(api.requests) != 0 {
		t.Error("called the API without a plan")
	}
}

func TestATransientFailureIsRetriedUnderTheSameKey(t *testing.T) {
	for name, first := range map[string]func(http.ResponseWriter) bool{
		"503":             failWith(http.StatusServiceUnavailable, "upstream"),
		"429":             failWith(http.StatusTooManyRequests, "rate_limited"),
		"still running":   failWith(http.StatusConflict, "idempotency_in_progress"),
		"connection lost": dropConnection,
	} {
		t.Run(name, func(t *testing.T) {
			noBackoff(t)
			api := newStrictAPI(t)
			api.script = []func(http.ResponseWriter) bool{first}
			env, _, _ := newTestEnv(t, api.URL)
			if err := run(t, env, "apps", "create", "bothy", "--project", "p1"); err != nil {
				t.Fatalf("did not recover: %v", err)
			}
			if len(api.keys) != 2 || api.keys[0] != api.keys[1] {
				t.Errorf("keys = %v, want the same key twice", api.keys)
			}
		})
	}
}

func TestARefusalIsNotRetriedAndNamesNoKey(t *testing.T) {
	for name, refusal := range map[string]func(http.ResponseWriter) bool{
		"400":    failWith(http.StatusBadRequest, "invalid"),
		"reused": failWith(http.StatusUnprocessableEntity, "idempotency_key_reused"),
		"paused": failWith(http.StatusServiceUnavailable, "service_paused"),
	} {
		t.Run(name, func(t *testing.T) {
			noBackoff(t)
			api := newStrictAPI(t)
			api.script = []func(http.ResponseWriter) bool{refusal}
			env, _, _ := newTestEnv(t, api.URL)
			err := run(t, env, "db", "create", "x", "--project", "p1", "--plan", "p")
			if err == nil {
				t.Fatal("a refusal succeeded")
			}
			if len(api.keys) != 1 {
				t.Errorf("sent %d times", len(api.keys))
			}
			if strings.Contains(err.Error(), "--idempotency-key") {
				t.Error("a definite refusal suggested retrying with the key")
			}
		})
	}
}

func TestGivingUpPrintsTheKeyToRetryWith(t *testing.T) {
	noBackoff(t)
	api := newStrictAPI(t)
	fail := failWith(http.StatusBadGateway, "upstream")
	api.script = []func(http.ResponseWriter) bool{fail, fail, fail, fail}
	env, _, _ := newTestEnv(t, api.URL)
	err := run(t, env, "apps", "create", "bothy", "--project", "p1")
	if err == nil {
		t.Fatal("succeeded")
	}
	if len(api.keys) != chargeAttempts {
		t.Errorf("attempts = %d, want %d", len(api.keys), chargeAttempts)
	}
	if !strings.Contains(err.Error(), "--idempotency-key "+api.keys[0]) {
		t.Errorf("error does not carry the key: %v", err)
	}

	api.script = nil
	if err := run(t, env, "apps", "create", "bothy", "--project", "p1", "--idempotency-key", api.keys[0]); err != nil {
		t.Fatal(err)
	}
	if last := api.keys[len(api.keys)-1]; last != api.keys[0] {
		t.Errorf("a supplied key was replaced: %q", last)
	}
}

func TestEachCreateGetsItsOwnKey(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	for i := 0; i < 2; i++ {
		if err := run(t, env, "apps", "create", "bothy", "--project", "p1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.keys) != 2 || api.keys[0] == api.keys[1] || !strings.HasPrefix(api.keys[0], "cli-") {
		t.Errorf("keys = %v", api.keys)
	}
}

func envKeys(m map[string]string) string {
	out := []string{}
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestSettingAVariableUpsertsOnlyTheNamedOnesWithNoReadAndNoApproval(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "set", "--app", "app-1", "B=3", "C=4"); err != nil {
		t.Fatal(err)
	}
	if got := envKeys(api.env); got != "A=1,B=3,C=4" {
		t.Errorf("env = %s", got)
	}
	if len(api.writes) != 1 || !strings.HasPrefix(api.writes[0], "UPSERT ") || strings.Contains(api.writes[0], `"A"`) {
		t.Errorf("writes = %v", api.writes)
	}
	for _, r := range api.requests {
		if r.method == http.MethodGet && strings.HasSuffix(r.path, "/env") {
			t.Error("set read the variables first")
		}
		if strings.Contains(r.path, "confirmations") || r.method == http.MethodPatch || r.method == http.MethodPut {
			t.Errorf("unexpected %s %s", r.method, r.path)
		}
	}
}

func TestReplaceIsTheOnlyWayToDropVariablesBySetting(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "set", "--app", "app-1", "--yes", "--wait", "1m", "--replace", "C=4"); err != nil {
		t.Fatal(err)
	}
	if got := envKeys(api.env); got != "C=4" {
		t.Errorf("env = %s", got)
	}
}

func TestUnsetDeletesEachExistingKeyOnItsOwnThroughTheApprovalFlow(t *testing.T) {
	api := newStrictAPI(t)
	env, _, errOut := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "unset", "--app", "app-1", "--yes", "--wait", "1m", "A", "NOPE", "A"); err != nil {
		t.Fatal(err)
	}
	if got := envKeys(api.env); got != "B=2" {
		t.Errorf("env = %s", got)
	}
	if len(api.writes) != 1 || api.writes[0] != "DELETE A body=" {
		t.Errorf("writes = %v", api.writes)
	}
	if !strings.Contains(errOut.String(), "NOPE was not set") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if api.calls(http.MethodPost, "/mcp/confirmations") != 1 {
		t.Errorf("want one approval for the one key that exists")
	}
	for _, r := range api.requests {
		if r.method == http.MethodPut || r.method == http.MethodPatch {
			t.Errorf("a whole-set write was sent: %s", r.method)
		}
	}

	if err := run(t, env, "apps", "env", "unset", "--app", "app-1", "--yes", "--wait", "1m", "B"); err != nil {
		t.Fatal(err)
	}
	if len(api.env) != 0 {
		t.Errorf("removing the last variable left %v", api.env)
	}
}

func TestUnsetOfOnlyMissingKeysAsksForNoApprovalAndSendsNothing(t *testing.T) {
	api := newStrictAPI(t)
	env, _, errOut := newTestEnv(t, api.URL)
	before := len(api.requests)
	if err := run(t, env, "apps", "env", "unset", "--app", "app-1", "X", "Y"); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.requests[before:] {
		if r.method != http.MethodGet {
			t.Errorf("sent %s %s", r.method, r.path)
		}
	}
	if !strings.Contains(errOut.String(), "X was not set") || !strings.Contains(errOut.String(), "Nothing to remove") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestUnsetNeverWritesTheWholeSetSoAKeyAddedMeanwhileSurvives(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "unset", "--app", "app-1", "--yes", "--wait", "1m", "A"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.env["ADDED_MEANWHILE"] = "v"
	api.mu.Unlock()
	if err := run(t, env, "apps", "env", "unset", "--app", "app-1", "--yes", "--wait", "1m", "B"); err != nil {
		t.Fatal(err)
	}
	if got := envKeys(api.env); got != "ADDED_MEANWHILE=v" {
		t.Errorf("env = %s", got)
	}
}

func TestAnOlderAPIWithoutTheEnvRoutesFailsClearlyAndNeverFallsBack(t *testing.T) {
	for name, answer := range map[string]func(w http.ResponseWriter){
		"plain 404": func(w http.ResponseWriter) { w.WriteHeader(404); _, _ = w.Write([]byte("Cannot POST")) },
		"json 404 no code": func(w http.ResponseWriter) {
			writeEnvelope(w, 404, map[string]any{})
		},
		"not allowlisted": func(w http.ResponseWriter) {
			writeEnvelope(w, 403, map[string]any{"code": "route_not_permitted"})
		},
		"confirmation not applicable": func(w http.ResponseWriter) {
			writeEnvelope(w, 400, map[string]any{"code": "confirmation_not_applicable"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t, func(a *fakeAPI, w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/env"):
					writeEnvelope(w, 200, []map[string]any{{"key": "A", "value": "[withheld]"}})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mcp/confirmations") && name != "confirmation not applicable":
					writeEnvelope(w, 201, map[string]any{"id": "c1", "status": "pending", "approvalUrl": "https://x/a"})
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/mcp/confirmations/"):
					writeEnvelope(w, 200, map[string]any{"status": "approved", "confirmationToken": "ct"})
				default:
					answer(w)
				}
			})
			env, _, _ := newTestEnv(t, api.URL)
			for _, args := range [][]string{
				{"apps", "env", "set", "--app", "app-1", "N=1"},
				{"apps", "env", "unset", "--app", "app-1", "--yes", "--wait", "1m", "A"},
			} {
				err := run(t, env, args...)
				if err == nil || !strings.Contains(err.Error(), "does not support") || !strings.Contains(err.Error(), "nothing was changed") {
					t.Errorf("%v: %v", args, err)
				}
			}
			for _, r := range api.requests {
				if r.method == http.MethodPatch || r.method == http.MethodPut {
					t.Errorf("fell back to %s", r.method)
				}
			}
		})
	}
}

func TestARealNotFoundIsNotMistakenForAMissingRoute(t *testing.T) {
	for _, err := range []error{
		&client.APIError{StatusCode: 404, Message: "Application not found"},
		&client.APIError{StatusCode: 404, Code: "env_key_not_found", Message: "That variable is not set"},
		&client.APIError{StatusCode: 500, Message: "boom"},
		&client.APIError{StatusCode: 403, Code: "secrets_scope_required"},
	} {
		if envRouteMissing(err) {
			t.Errorf("%v was treated as a missing route", err)
		}
	}
}

func TestNothingIsWrittenWhenTheCurrentSetCannotBeRead(t *testing.T) {
	api := newFakeAPI(t, func(a *fakeAPI, w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusInternalServerError, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)
	args := []string{"apps", "env", "unset", "--app", "app-1", "A"}
	if err := run(t, env, args...); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("%v: err = %v", args, err)
	}
	if api.calls(http.MethodPut, "/paas/apps/app-1/env")+api.calls(http.MethodPatch, "/paas/apps/app-1/env") != 0 {
		t.Error("wrote after a failed read")
	}
}

func TestNoWritePathEverAsksToReveal(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	for _, args := range [][]string{
		{"apps", "env", "set", "--app", "app-1", "N=1"},
		{"apps", "env", "set", "--app", "app-1", "--replace", "--yes", "--wait", "1m", "N=1"},
		{"apps", "env", "unset", "--app", "app-1", "--yes", "--wait", "1m", "N"},
	} {
		_ = api
		if err := run(t, env, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, r := range api.requests {
		if strings.Contains(r.query, "reveal") {
			t.Fatalf("reveal sent: %s?%s", r.path, r.query)
		}
	}
}

func TestEnvListShowsKeysAndHidesValuesUnlessAsked(t *testing.T) {
	api := newStrictAPI(t)
	env, out, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "list", "app-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"key":"A"`) && !strings.Contains(out.String(), `"key": "A"`) {
		t.Errorf("keys not shown: %s", out.String())
	}
	if strings.Contains(out.String(), `"1"`) {
		t.Errorf("a value was shown without --show-values: %s", out.String())
	}

	out.Reset()
	if err := run(t, env, "apps", "env", "list", "app-1", "--show-values"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"1"`) {
		t.Errorf("--show-values did not show values: %s", out.String())
	}

	empty := newStrictAPI(t)
	empty.env = map[string]string{}
	env, out, _ = newTestEnv(t, empty.URL)
	if err := run(t, env, "apps", "env", "list", "app-1"); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("no variables: %q", out.String())
	}
}
