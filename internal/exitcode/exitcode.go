// Package exitcode maps errors to the process exit codes documented in
// docs/adr/0008-exit-codes-and-format-are-contract.md.
//
// The mapping lives in one place so a new command cannot invent its own
// convention.
package exitcode

import (
	"errors"
	"net/http"

	"github.com/techbeaver/beaver-cli/client"
)

// The exit codes. These are a public contract: adding one is safe, changing
// what one means is a major version.
const (
	OK              = 0
	Error           = 1
	Usage           = 2
	Unauthenticated = 3
	NotFound        = 4
	PaymentRequired = 5
	AwaitingHuman   = 6
	RateLimited     = 7
)

// ErrUsage marks an error as the caller's mistake rather than a failure.
var ErrUsage = errors.New("usage")

// ErrUnauthenticated means there is no usable credential.
var ErrUnauthenticated = errors.New("not authenticated")

// ErrAwaitingApproval means the platform created a pending confirmation and a
// person has to approve it. It is a retryable state, not a failure.
var ErrAwaitingApproval = errors.New("awaiting approval")

// From maps an error to its exit code.
func From(err error) int {
	switch {
	case err == nil:
		return OK
	case errors.Is(err, ErrUsage):
		return Usage
	case errors.Is(err, ErrAwaitingApproval):
		return AwaitingHuman
	case errors.Is(err, ErrUnauthenticated):
		return Unauthenticated
	}

	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return fromAPI(apiErr)
	}
	return Error
}

func fromAPI(e *client.APIError) int {
	switch e.Code {
	case "confirmation_required", "confirmation_pending":
		return AwaitingHuman
	case "token_expired", "token_revoked", "grant_revoked", "invalid_token", "insufficient_scope":
		return Unauthenticated
	}

	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return Unauthenticated
	case http.StatusNotFound:
		return NotFound
	case http.StatusPaymentRequired:
		return PaymentRequired
	case http.StatusTooManyRequests:
		return RateLimited
	}
	return Error
}
