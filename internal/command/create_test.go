package command

import (
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
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/env"):
			rows := []map[string]any{}
			for k, v := range s.env {
				rows = append(rows, map[string]any{"key": k, "value": v})
			}
			writeEnvelope(w, http.StatusOK, rows)
		case strings.HasSuffix(path, "/env"):
			s.env = map[string]string{}
			for k, v := range body["envVars"].(map[string]any) {
				s.env[k] = v.(string)
			}
			writeEnvelope(w, http.StatusOK, map[string]any{})
		default:
			writeEnvelope(w, http.StatusCreated, map[string]any{"id": "new-1", "status": "pending_payment"})
		}
	})
	return s
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

func TestSettingAVariableKeepsTheOthers(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "set", "--app", "app-1", "B=3", "C=4"); err != nil {
		t.Fatal(err)
	}
	if got := envKeys(api.env); got != "A=1,B=3,C=4" {
		t.Errorf("env = %s", got)
	}
}

func TestReplaceIsTheOnlyWayToDropVariablesBySetting(t *testing.T) {
	api := newStrictAPI(t)
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "set", "--app", "app-1", "--replace", "C=4"); err != nil {
		t.Fatal(err)
	}
	if got := envKeys(api.env); got != "C=4" {
		t.Errorf("env = %s", got)
	}
}

func TestUnsetRemovesOnlyTheNamedVariables(t *testing.T) {
	api := newStrictAPI(t)
	env, _, errOut := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "env", "unset", "--app", "app-1", "A", "NOPE"); err != nil {
		t.Fatal(err)
	}
	if got := envKeys(api.env); got != "B=2" {
		t.Errorf("env = %s", got)
	}
	if !strings.Contains(errOut.String(), "NOPE was not set") {
		t.Errorf("stderr = %q", errOut.String())
	}

	if err := run(t, env, "apps", "env", "unset", "--app", "app-1", "B"); err != nil {
		t.Fatal(err)
	}
	if len(api.env) != 0 {
		t.Errorf("removing the last variable left %v", api.env)
	}
}

func TestNothingIsWrittenWhenTheCurrentSetCannotBeRead(t *testing.T) {
	api := newFakeAPI(t, func(a *fakeAPI, w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusInternalServerError, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)
	for _, args := range [][]string{
		{"apps", "env", "set", "--app", "app-1", "A=1"},
		{"apps", "env", "unset", "--app", "app-1", "A"},
	} {
		if err := run(t, env, args...); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if api.calls(http.MethodPut, "/paas/apps/app-1/env")+api.calls(http.MethodPatch, "/paas/apps/app-1/env") != 0 {
		t.Error("wrote after a failed read")
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
