package command

import (
	"strings"
	"testing"
)

// machineSandboxRoutes mirrors the sandbox entries of the platform's machine token policy
// (internal/shared/machineauth/policy.go in the private API repository, read on 2026-11-25). A route
// that is not here is closed to tokens, so no sandbox command may call it. The two PATCH switches
// (auto-renew and hourly-auto-topup), cancel, resume, reduce-slots, hourly activation, events, ports,
// file delete and every /admin route are absent on purpose.
var machineSandboxRoutes = []string{
	"GET /sandbox/plans", "GET /sandbox/usage",
	"GET /sandbox/statements", "GET /sandbox/statements/current", "GET /sandbox/statements/:id",
	"POST /sandbox/billing/quote", "POST /sandbox/subscriptions/:id/add-slots/quote",
	"POST /sandbox/billing/purchase", "POST /sandbox/subscriptions/:id/add-slots",
	"GET /sandbox/subscriptions", "GET /sandbox/subscriptions/:id",
	"GET /sandbox/sandboxes", "GET /sandbox/sandboxes/:id",
	"POST /sandbox/sandboxes", "POST /sandbox/sandboxes/:id/start", "POST /sandbox/sandboxes/:id/stop",
	"DELETE /sandbox/sandboxes/:id",
	"POST /sandbox/sandboxes/:id/exec", "GET /sandbox/sandboxes/:id/exec/:execId", "POST /sandbox/sandboxes/:id/exec/:execId/cancel",
	"GET /sandbox/sandboxes/:id/files", "PUT /sandbox/sandboxes/:id/files",
	"GET /sandbox/sandboxes/:id/files/list", "POST /sandbox/sandboxes/:id/files/mkdir",
}

func routeMatches(pattern, method, path string) bool {
	pm, pp, _ := strings.Cut(pattern, " ")
	if pm != method {
		return false
	}
	want, got := strings.Split(pp, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if !strings.HasPrefix(want[i], ":") && want[i] != got[i] {
			return false
		}
	}
	return true
}

func TestRouteMatcherBoundaries(t *testing.T) {
	cases := []struct {
		pattern, method, path string
		want                  bool
	}{
		{"GET /sandbox/statements/:id", "GET", "/sandbox/statements/abc", true},
		{"GET /sandbox/statements/:id", "GET", "/sandbox/statements", false},
		{"GET /sandbox/statements/:id", "GET", "/sandbox/statements/abc/extra", false},
		{"GET /sandbox/statements/:id", "POST", "/sandbox/statements/abc", false},
		{"GET /sandbox/statements/current", "GET", "/sandbox/statements/current", true},
		{"GET /sandbox/usage", "GET", "/sandbox/usage/", false},
	}
	for _, tc := range cases {
		if got := routeMatches(tc.pattern, tc.method, tc.path); got != tc.want {
			t.Errorf("%s vs %s %s: got %v", tc.pattern, tc.method, tc.path, got)
		}
	}
}

func TestEverySandboxCommandOnlyCallsRoutesTokensMayUse(t *testing.T) {
	for name, args := range sandboxInvocations(t) {
		if name == "destroy" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			api := newSBServer(t, okAll)
			if _, _, err := sbRun(t, api, args...); err != nil {
				t.Fatal(err)
			}
			reqs := api.requests()
			if len(reqs) == 0 {
				t.Fatal("the command made no request")
			}
			for _, r := range reqs {
				ok := false
				for _, pattern := range machineSandboxRoutes {
					if routeMatches(pattern, r.method, r.path) {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("%s %s is not open to machine tokens, so the CLI may not call it", r.method, r.path)
				}
			}
		})
	}
}
