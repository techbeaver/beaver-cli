package sandboxerr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/client"
)

func TestUnavailableOnlyForAMissingAPI(t *testing.T) {
	cases := []struct {
		name string
		err  *client.APIError
		want bool
	}{
		{"nil", nil, false},
		{"not json 404", &client.APIError{StatusCode: 404, Code: "upstream_not_json"}, true},
		{"no code, router message", &client.APIError{StatusCode: 404, Message: "Cannot GET /x"}, true},
		{"real not found", &client.APIError{StatusCode: 404, Message: "Project not found"}, false},
		{"real not found, any case", &client.APIError{StatusCode: 404, Message: "Sandbox Not Found"}, false},
		{"coded 404", &client.APIError{StatusCode: 404, Code: "sandbox_plan_not_on_sale", Message: "That plan does not exist."}, false},
		{"not json 502", &client.APIError{StatusCode: 502, Code: "upstream_not_json"}, false},
		{"403", &client.APIError{StatusCode: 403, Message: "no"}, false},
	}
	for _, tc := range cases {
		if got := Unavailable(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDescribeKeepsTheOriginalErrorOnTheChain(t *testing.T) {
	orig := &client.APIError{StatusCode: 402, Code: "usage_owed", Message: "You owe\n  money."}
	got := Describe(fmt.Errorf("wrapped: %w", orig))
	var apiErr *client.APIError
	if !errors.As(got, &apiErr) || apiErr != orig {
		t.Fatal("the APIError must stay reachable so exit codes keep working")
	}
	if got.Error() != "You owe money." {
		t.Fatalf("the message must be one line, got %q", got.Error())
	}
	if again := Describe(got); again != got {
		t.Fatal("describing twice must not wrap twice")
	}
}

func TestDescribeKeepsARetryHintAfterTheFirstLine(t *testing.T) {
	inner := &client.APIError{StatusCode: 503, Message: "Starting up."}
	err := fmt.Errorf("%w\nIt may or may not have gone through. Run with --idempotency-key k1", inner)
	got := Describe(err).Error()
	if !strings.HasPrefix(got, "Starting up.\n") || !strings.Contains(got, "--idempotency-key k1") {
		t.Fatalf("got %q", got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestDescribeTransportAndMalformedFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", fmt.Errorf("calling the TechBeaver API: %w", context.DeadlineExceeded), "did not answer in time"},
		{"net timeout", fmt.Errorf("calling the TechBeaver API: %w", timeoutErr{}), "did not answer in time"},
		{"reset", errors.New("calling the TechBeaver API: Post x: read: connection reset by peer"), "Could not reach TechBeaver"},
		{"malformed", fmt.Errorf("%w: bad", ErrMalformed), "could not read"},
		{"html in front", &client.APIError{StatusCode: 502, Code: "upstream_not_json"}, "HTTP 502"},
		{"paused keeps the gate message", &client.APIError{StatusCode: 503, Code: "service_paused", Message: "Paused for maintenance."}, "Paused for maintenance"},
	}
	for _, tc := range cases {
		got := Describe(tc.err)
		if got == nil || !strings.Contains(got.Error(), tc.want) {
			t.Errorf("%s: got %v, want text %q", tc.name, got, tc.want)
		}
	}
}

func TestDescribeLeavesWhatItDoesNotKnow(t *testing.T) {
	if Describe(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	plain := errors.New("usage: --slots is required")
	if Describe(plain) != plain {
		t.Fatal("a local error must pass through untouched")
	}
	if Describe(context.Canceled) != context.Canceled {
		t.Fatal("a cancelled run must not be reworded")
	}
}

func TestEveryStatusGetsAMessageAndNeverAnEmptyOne(t *testing.T) {
	for status := 400; status < 600; status++ {
		for _, code := range []string{"", "x_unknown", "validation_failed"} {
			got := Describe(&client.APIError{StatusCode: status, Code: code})
			if got == nil || strings.TrimSpace(got.Error()) == "" {
				t.Fatalf("status %d code %q produced an empty message", status, code)
			}
			if strings.Contains(got.Error(), "\n") {
				t.Fatalf("status %d code %q is not one line: %q", status, code, got)
			}
			if status != http.StatusNotFound && got.Error() == NotAvailable {
				t.Fatalf("status %d must not read as a missing API", status)
			}
		}
	}
}

func TestInsufficientScopeNamesTheScopeAndWhereToGrantIt(t *testing.T) {
	with := Describe(&client.APIError{StatusCode: 403, Code: "insufficient_scope", Message: "x",
		Data: map[string]any{"missingScope": []any{"sandbox:exec", "sandbox:files"}}}).Error()
	for _, want := range []string{"sandbox:exec and sandbox:files", "agent connection", "--scope sandbox:exec,sandbox:files"} {
		if !strings.Contains(with, want) {
			t.Errorf("%q lacks %q", with, want)
		}
	}
	without := Describe(&client.APIError{StatusCode: 403, Code: "insufficient_scope"}).Error()
	if !strings.Contains(without, "agent connection") || strings.Contains(without, "--scope") {
		t.Errorf("no scope named: %q", without)
	}
	odd := Describe(&client.APIError{StatusCode: 403, Code: "insufficient_scope", Data: map[string]any{"missingScope": "sandbox:read"}}).Error()
	if !strings.Contains(odd, "agent connection") {
		t.Errorf("a malformed data field must still give the grant message: %q", odd)
	}
}

func TestRateLimitTellsTheWaitAndNeverOverstates(t *testing.T) {
	got := Describe(&client.APIError{StatusCode: 429, Code: "sandbox_busy", Message: "Busy.", RetryAfter: 2 * time.Second}).Error()
	if got != "Busy. Try again in 2 seconds." {
		t.Errorf("got %q", got)
	}
	got = Describe(&client.APIError{StatusCode: 429, Code: "sandbox_busy", Message: "sandbox_busy"}).Error()
	if strings.Contains(got, "sandbox_busy") {
		t.Errorf("the code must not be the message: %q", got)
	}
}

func TestSpendRequiredNamesThePermissionAndTheWayOut(t *testing.T) {
	for _, code := range []string{"billing_spend_required", "BILLING_SPEND_REQUIRED", " billing_spend_required "} {
		err := Describe(&client.APIError{StatusCode: 403, Code: code, Message: "server wording that must not be the only advice",
			Data: map[string]any{"code": "billing_spend_required", "requiredScope": "billing:spend"}})
		msg := err.Error()
		for _, want := range []string{"billing:spend", "by-the-hour", "monthly slot", "Nothing was created or charged"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q: message lacks %q: %s", code, want, msg)
			}
		}
		if strings.Contains(msg, "--scope billing:spend") {
			t.Errorf("a scope no sign in can hold must not be offered as a login flag: %s", msg)
		}
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
			t.Errorf("the original error must stay on the chain so exit codes follow it")
		}
	}
	other := Describe(&client.APIError{StatusCode: 403, Code: "wallet_spend_requires_human", Message: "Paying from the USD wallet needs the account owner."})
	if !strings.Contains(other.Error(), "needs the account owner") {
		t.Errorf("another 403 keeps the server's own words: %v", other)
	}
}
