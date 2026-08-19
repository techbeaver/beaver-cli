package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
)

// Turning an API failure into something a model can act on.
//
// This is the whole reason the response envelope grew a machine-readable code.
// A model cannot branch on prose, and "record not found" and "the account owner
// paused new deployments" call for completely different next steps: one is a
// bad identifier the agent should re-look-up, the other is an operator decision
// the agent must report and stop.
//
// Every message below ends with what to do next, in the second person, because
// the text lands in a conversation a customer is reading.

// describeAPIError rewrites an API failure as an instruction.
func describeAPIError(err error) error {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return err
	}

	switch normaliseCode(apiErr.Code) {
	case "service_paused":
		// An admin's circuit breaker. Retrying into it is how a maintenance
		// window becomes a thundering herd.
		return fmt.Errorf("TechBeaver has paused this action: %s. This is a deliberate operator decision, not a temporary error. Tell the customer what it says and stop; do not retry", apiErr.Message)

	case "confirmation_required", "confirmation_pending":
		return fmt.Errorf("this action destroys something, so the account owner has to approve it in their browser first. Call the same tool without a confirmation token to get a preview and an approval link, show both to the customer, and wait for them")

	case "confirmation_expired", "confirmation_used":
		return fmt.Errorf("%s Ask for the action again to get a fresh preview and approval link", apiErr.Message)

	case "confirmation_mismatch":
		return fmt.Errorf("that approval was for a different action or a different resource. Ask for a new one for exactly what you are trying to do")

	case "insufficient_scope":
		return fmt.Errorf("%s The account owner has to reconnect this client and grant that permission; you cannot grant it to yourself", apiErr.Message)

	case "route_not_permitted":
		return fmt.Errorf("that operation is not available to AI agents at all. It can only be done by a person signed in to the TechBeaver console. Tell the customer that rather than looking for another way")

	case "admin_unreachable":
		return fmt.Errorf("administrative operations are never available to AI agents. Tell the customer they need to do this from the admin console")

	case "token_expired":
		return fmt.Errorf("this connection's access token has expired. The client should refresh it and try again")

	case "token_revoked", "grant_revoked", "invalid_token":
		return fmt.Errorf("this connection is no longer authorised. Ask the account owner to reconnect TechBeaver")

	case "account_inactive":
		return fmt.Errorf("this TechBeaver account is not active, so nothing can be done on it. The customer should contact support")

	case "idempotency_in_progress":
		return fmt.Errorf("an identical request is still running. Wait a few seconds and check the result rather than sending it again")

	case "idempotency_key_reused":
		return fmt.Errorf("that retry did not match the original request. Treat this as a bug in the agent rather than something to work around, and start the operation again from the beginning")

	case "destructive_rate_limited":
		return fmt.Errorf("too many destructive actions have been requested on this account in the last hour, so this one was refused. Stop and tell the customer")
	}

	switch apiErr.StatusCode {
	case http.StatusNotFound:
		// Ownership failures return 404 rather than 403 throughout this platform,
		// deliberately, so that an identifier cannot be probed for existence. The
		// agent should be told both readings.
		return fmt.Errorf("%s Either that identifier is wrong or it belongs to another account. List the resources on this account and use an identifier from that list", apiErr.Message)

	case http.StatusPaymentRequired:
		return fmt.Errorf("%s Use pay_invoice or create_checkout_link to produce a payment link and give it to the customer", apiErr.Message)

	case http.StatusTooManyRequests:
		return fmt.Errorf("TechBeaver is rate limiting this connection. Slow down, and space out repeated polling")

	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s", apiErr.Message)

	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return fmt.Errorf("TechBeaver rejected the request: %s Fix the arguments rather than retrying the same call", apiErr.Message)
	}

	if apiErr.Retryable() {
		return fmt.Errorf("TechBeaver returned a temporary error: %s You may retry once, then report it to the customer", apiErr.Message)
	}
	return fmt.Errorf("%s", apiErr.Message)
}

// normaliseCode folds the two spellings this platform uses. The service-gate
// 503 carries SERVICE_PAUSED in its data payload because both consoles read it
// that way, while the top-level vocabulary is snake_case.
func normaliseCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}
