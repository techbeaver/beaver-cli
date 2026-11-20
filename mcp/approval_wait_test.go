package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/client"
)

func approvalFixture(t *testing.T, handler http.HandlerFunc) (*Deps, *Call) {
	t.Helper()
	oldPoll, oldWait := approvalPoll, approvalWait
	approvalPoll, approvalWait = time.Millisecond, 3*time.Second
	t.Cleanup(func() { approvalPoll, approvalWait = oldPoll, oldWait })
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Deps{Client: client.New(srv.URL, "test")}, &Call{Session: &Session{Token: "t"}}
}

func statusAfter(polls int, final string, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		status := "pending"
		if int(hits.Add(1)) > polls {
			status = final
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 200, "success": true, "data": map[string]any{"id": "c1", "status": status},
		})
	}
}

func TestWaitForApprovalEndsOnEveryTerminalStatus(t *testing.T) {
	cases := map[string]bool{
		"approved": true,
		"rejected": false, "declined": false, "consumed": false, "expired": false,
		"cancelled": false, "": false, "APPROVED": false,
	}
	for status, want := range cases {
		t.Run("status="+status, func(t *testing.T) {
			var hits atomic.Int32
			deps, call := approvalFixture(t, statusAfter(2, status, &hits))
			start := time.Now()
			got := waitForApproval(context.Background(), deps, call, "c1")
			if got != want {
				t.Fatalf("waitForApproval = %v, want %v", got, want)
			}
			if time.Since(start) > time.Second {
				t.Fatalf("polled for %v: a settled status must end the wait", time.Since(start))
			}
			if n := hits.Load(); n != 3 {
				t.Fatalf("polled %d times, want 3 (two pending then the settled status)", n)
			}
		})
	}
}

func TestWaitForApprovalGivesUpWhenAlwaysPending(t *testing.T) {
	var hits atomic.Int32
	deps, call := approvalFixture(t, statusAfter(1<<30, "approved", &hits))
	approvalWait = 50 * time.Millisecond
	if waitForApproval(context.Background(), deps, call, "c1") {
		t.Fatal("a request that never settled was treated as approved")
	}
	if hits.Load() == 0 {
		t.Fatal("never polled")
	}
}

func TestWaitForApprovalBoundaries(t *testing.T) {
	var hits atomic.Int32
	deps, call := approvalFixture(t, statusAfter(0, "approved", &hits))
	if waitForApproval(context.Background(), deps, call, "") {
		t.Fatal("an empty confirmation id was treated as approved")
	}
	if hits.Load() != 0 {
		t.Fatal("an empty confirmation id must not reach the API")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForApproval(ctx, deps, call, "c1") {
		t.Fatal("a cancelled context was treated as approved")
	}
}

func TestWaitForApprovalFailsSafeOnFaults(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":500,"success":false,"message":"boom"}`))
		},
		"not json": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"wrong shape": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":200,"success":true,"data":"approved"}`))
		},
		"timeout": func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			deps, call := approvalFixture(t, h)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if waitForApproval(ctx, deps, call, "c1") {
				t.Fatal("a failed poll was treated as approved")
			}
		})
	}
}
