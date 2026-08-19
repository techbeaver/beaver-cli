package exitcode

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/techbeaver/beaver-cli/client"
)

func TestTheDocumentedContract(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, OK},
		{"a plain error", errors.New("boom"), Error},
		{"a usage mistake", fmt.Errorf("bad flag: %w", ErrUsage), Usage},
		{"no credential", ErrUnauthenticated, Unauthenticated},
		{"awaiting a person", ErrAwaitingApproval, AwaitingHuman},
		{"a 401", &client.APIError{StatusCode: http.StatusUnauthorized}, Unauthenticated},
		{"a 403", &client.APIError{StatusCode: http.StatusForbidden}, Unauthenticated},
		{"a 404", &client.APIError{StatusCode: http.StatusNotFound}, NotFound},
		{"a 402", &client.APIError{StatusCode: http.StatusPaymentRequired}, PaymentRequired},
		{"a 429", &client.APIError{StatusCode: http.StatusTooManyRequests}, RateLimited},
		{"an expired token", &client.APIError{StatusCode: 400, Code: "token_expired"}, Unauthenticated},
		{"a missing scope", &client.APIError{StatusCode: 403, Code: "insufficient_scope"}, Unauthenticated},
		{"a pending confirmation", &client.APIError{StatusCode: 409, Code: "confirmation_required"}, AwaitingHuman},
		{"a paused service", &client.APIError{StatusCode: 503, Code: "service_paused"}, Error},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := From(tc.err); got != tc.want {
				t.Fatalf("From() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAWrappedErrorStillMaps(t *testing.T) {
	wrapped := fmt.Errorf("deleting the app: %w", &client.APIError{StatusCode: http.StatusNotFound})
	if got := From(wrapped); got != NotFound {
		t.Fatalf("a wrapped API error must still map, got %d", got)
	}
}

func TestAwaitingApprovalIsNotAFailure(t *testing.T) {
	if AwaitingHuman == Error {
		t.Fatal("a script must be able to tell 'waiting for a person' from a failure")
	}
	if RateLimited == Error {
		t.Fatal("a script must be able to tell 'slow down' from a failure")
	}
}
