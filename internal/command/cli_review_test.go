package command

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

func ok200(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
	writeEnvelope(w, 200, map[string]any{"ok": true})
}

func TestRollbackSendsTheNamedRevision(t *testing.T) {
	api := newFakeAPI(t, ok200)
	env, _, _ := newTestEnv(t, api.URL)

	if err := run(t, env, "apps", "rollback", "app-1", "--revision", "  rev-7 "); err != nil {
		t.Fatal(err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("want one request, got %d", len(api.requests))
	}
	got := api.requests[0]
	if got.method != http.MethodPost || got.path != "/paas/apps/app-1/rollback" {
		t.Fatalf("sent %s %s", got.method, got.path)
	}
	if got.body["revisionName"] != "rev-7" || len(got.body) != 1 {
		t.Fatalf("body was %s, want exactly {revisionName: rev-7}", got.raw)
	}
}

func TestRollbackWithoutARevisionIsAUsageErrorAndSendsNothing(t *testing.T) {
	for name, args := range map[string][]string{
		"flag absent":   {"apps", "rollback", "app-1"},
		"flag empty":    {"apps", "rollback", "app-1", "--revision", ""},
		"flag blank":    {"apps", "rollback", "app-1", "--revision", "   "},
		"no app either": {"apps", "rollback"},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t, ok200)
			env, _, _ := newTestEnv(t, api.URL)
			err := run(t, env, args...)
			if err == nil {
				t.Fatal("a rollback with no revision must fail")
			}
			if code := exitcode.From(err); code != exitcode.Usage {
				t.Fatalf("exit code %d, want usage (%d): %v", code, exitcode.Usage, err)
			}
			if len(api.requests) != 0 {
				t.Fatalf("nothing may be sent, got %d request(s)", len(api.requests))
			}
		})
	}
}

func TestRollbackSurfacesAServerFailure(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 500, map[string]any{})
	})
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "rollback", "app-1", "--revision", "r1"); err == nil {
		t.Fatal("a 500 must not look like success")
	}
	if len(api.requests) != 1 {
		t.Fatalf("a rollback must not be retried blindly, got %d requests", len(api.requests))
	}
}

func TestDeploymentsListsRevisionsToRollBackTo(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, []map[string]any{{"revision": "rev-2", "status": "active", "isActive": true}})
	})
	env, out, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "apps", "deployments", "app-1"); err != nil {
		t.Fatal(err)
	}
	if api.requests[0].method != http.MethodGet || api.requests[0].path != "/paas/apps/app-1/deployments" {
		t.Fatalf("sent %+v", api.requests[0])
	}
	if !strings.Contains(out.String(), "rev-2") {
		t.Fatalf("the revision was not shown: %q", out.String())
	}
}

func payKeys(api *fakeAPI) []string {
	var keys []string
	for _, r := range api.requests {
		keys = append(keys, r.idempotencyKey)
	}
	return keys
}

func TestPayInvoiceSendsAFreshIdempotencyKeyPerInvocation(t *testing.T) {
	api := newFakeAPI(t, ok200)
	env, _, _ := newTestEnv(t, api.URL)
	for i := 0; i < 2; i++ {
		if err := run(t, env, "billing", "invoices", "pay", "inv-1"); err != nil {
			t.Fatal(err)
		}
	}
	keys := payKeys(api)
	if len(keys) != 2 || api.requests[0].path != "/billing/invoices/inv-1/pay" || api.requests[0].method != http.MethodPost {
		t.Fatalf("unexpected requests: %+v", api.requests)
	}
	for _, k := range keys {
		if len(k) < 20 || !strings.HasPrefix(k, "cli-") {
			t.Fatalf("key %q is not a generated key", k)
		}
	}
	if keys[0] == keys[1] {
		t.Fatal("two invocations shared a key, so the second payment would be swallowed as a replay")
	}
}

func TestPayInvoiceHonoursAnExplicitKey(t *testing.T) {
	api := newFakeAPI(t, ok200)
	env, _, _ := newTestEnv(t, api.URL)
	for i := 0; i < 2; i++ {
		if err := run(t, env, "billing", "invoices", "pay", "inv-1", "--idempotency-key", "my-retry-key"); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range payKeys(api) {
		if k != "my-retry-key" {
			t.Fatalf("explicit key not sent, got %q", k)
		}
	}
}

func TestPayInvoiceRetriesReuseOneKeyAndReportIt(t *testing.T) {
	old := chargeBackoff
	chargeBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { chargeBackoff = old })

	t.Run("recovers on the second attempt", func(t *testing.T) {
		api := newFakeAPI(t, func(a *fakeAPI, w http.ResponseWriter, _ *http.Request) {
			if len(a.requests) == 1 {
				writeEnvelope(w, 503, map[string]any{})
				return
			}
			writeEnvelope(w, 200, map[string]any{"ok": true})
		})
		env, _, _ := newTestEnv(t, api.URL)
		if err := run(t, env, "billing", "invoices", "pay", "inv-1"); err != nil {
			t.Fatal(err)
		}
		keys := payKeys(api)
		if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
			t.Fatalf("a retry must reuse the key, got %v", keys)
		}
	})

	t.Run("gives up and prints the key", func(t *testing.T) {
		api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
			writeEnvelope(w, 502, map[string]any{})
		})
		env, _, _ := newTestEnv(t, api.URL)
		err := run(t, env, "billing", "invoices", "pay", "inv-1")
		if err == nil {
			t.Fatal("expected failure")
		}
		keys := payKeys(api)
		if len(keys) != chargeAttempts {
			t.Fatalf("want %d attempts, got %d", chargeAttempts, len(keys))
		}
		for _, k := range keys {
			if k != keys[0] {
				t.Fatalf("keys differ across retries: %v", keys)
			}
		}
		if !strings.Contains(err.Error(), "--idempotency-key "+keys[0]) {
			t.Fatalf("the error must name the key to reuse, got %v", err)
		}
	})
}

func TestPayInvoiceMalformedResponseIsAnError(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json"))
	})
	env, _, _ := newTestEnv(t, api.URL)
	if err := run(t, env, "billing", "invoices", "pay", "inv-1", "--idempotency-key", "k"); err == nil {
		t.Fatal("a non-JSON reply must not look like success")
	}
}

func TestGeneratedIdempotencyKeysAreUniqueUnderConcurrency(t *testing.T) {
	const n = 200
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := newIdempotencyKey()
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			seen[k] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("%d keys collided out of %d", n-len(seen), n)
	}
}

func TestDebugFlagLogsRequestsWithoutSecrets(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "req-xyz")
		writeEnvelope(w, 200, map[string]any{"id": "app-1", "name": "very-secret-name"})
	})

	t.Run("on", func(t *testing.T) {
		env, _, errBuf := newTestEnv(t, api.URL)
		if err := run(t, env, "--debug", "apps", "describe", "app-1"); err != nil {
			t.Fatal(err)
		}
		got := errBuf.String()
		for _, want := range []string{"GET", "/api/v1/paas/apps/app-1", "200", "request-id=req-xyz"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in %q", want, got)
			}
		}
		for _, secret := range []string{"btk_test", "Bearer", "Authorization", "very-secret-name"} {
			if strings.Contains(got, secret) {
				t.Errorf("debug output leaked %q: %q", secret, got)
			}
		}
	})

	t.Run("off", func(t *testing.T) {
		env, _, errBuf := newTestEnv(t, api.URL)
		if err := run(t, env, "apps", "describe", "app-1"); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(errBuf.String(), "debug") {
			t.Fatalf("diagnostics appeared without --debug: %q", errBuf.String())
		}
	})
}

func TestDebugFlagLogsAFailedRequest(t *testing.T) {
	api := newFakeAPI(t, func(_ *fakeAPI, w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 500, map[string]any{})
	})
	env, _, errBuf := newTestEnv(t, api.URL)
	if err := run(t, env, "--debug", "apps", "describe", "app-1"); err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(errBuf.String(), "-> 500") {
		t.Fatalf("the failed request was not logged: %q", errBuf.String())
	}
}
