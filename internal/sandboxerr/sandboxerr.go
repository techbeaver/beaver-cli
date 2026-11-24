// Package sandboxerr turns a failed sandbox call into one calm line.
//
// The original error stays on the chain, so exit codes still follow it.
package sandboxerr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
)

// NotAvailable is the line shown when the server has no sandbox API.
const NotAvailable = "Sandboxes are not available on this server yet."

// ErrMalformed marks a response that arrived but could not be read.
var ErrMalformed = errors.New("unreadable response")

type described struct {
	msg string
	err error
}

func (d *described) Error() string { return d.msg }
func (d *described) Unwrap() error { return d.err }

// Describe rewrites err as one line. Text after the first newline of err, such
// as a retry hint, is kept. A nil or already described error is returned as is.
func Describe(err error) error {
	if err == nil {
		return nil
	}
	var d *described
	if errors.As(err, &d) {
		return err
	}
	head := headline(err)
	if head == "" {
		return err
	}
	if rest := hint(err); rest != "" {
		head += "\n" + rest
	}
	return &described{msg: head, err: err}
}

// hint is what a wrapper added after the typed error's own text, such as a retry instruction.
func hint(err error) string {
	full := err.Error()
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		inner := apiErr.Error()
		if i := strings.Index(full, inner); i >= 0 {
			return strings.TrimSpace(full[i+len(inner):])
		}
		return ""
	}
	if _, rest, found := strings.Cut(full, "\n"); found {
		return strings.TrimSpace(rest)
	}
	return ""
}

// Unavailable reports whether a 404 means the server has no sandbox API at all.
// A real "not found" from the API carries a message saying so.
func Unavailable(e *client.APIError) bool {
	if e == nil || e.StatusCode != http.StatusNotFound {
		return false
	}
	if e.Code == "upstream_not_json" {
		return true
	}
	return e.Code == "" && !strings.Contains(strings.ToLower(e.Message), "not found")
}

func headline(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiHeadline(apiErr)
	}
	switch {
	case errors.Is(err, ErrMalformed):
		return "TechBeaver sent a response that beaver could not read. Try again, and report it if it keeps happening."
	case errors.Is(err, context.Canceled):
		return ""
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "TechBeaver did not answer in time."
	}
	var netErr net.Error
	if errors.As(err, &netErr) || strings.Contains(err.Error(), "calling the TechBeaver API") {
		return "Could not reach TechBeaver. Check your connection and try again."
	}
	return ""
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func apiHeadline(e *client.APIError) string {
	msg := oneLine(e.Message)
	if msg == fmt.Sprintf("The TechBeaver API returned %d", e.StatusCode) || (e.Code != "" && strings.EqualFold(msg, e.Code)) {
		msg = ""
	}
	code := strings.ToLower(strings.TrimSpace(e.Code))

	if Unavailable(e) {
		return NotAvailable
	}
	switch code {
	case "service_paused":
		return "TechBeaver has paused this: " + trimStop(msg) + ". This is an operator decision, so beaver will not retry."
	case "route_not_permitted":
		return "This server does not allow the CLI to use that sandbox feature."
	case "idempotency_key_reused":
		return "That idempotency key was already used for a different request, so nothing was done. Run the command again without --idempotency-key for a fresh attempt."
	case "idempotency_in_progress":
		return "An identical request is still being processed. Wait a few seconds before trying again."
	case "agent_cannot_pay":
		return "beaver cannot pay for this. Open the payment link yourself."
	case "sandbox_check_unavailable", "billing_unavailable", "sandbox_billing_unavailable", "sandbox_service_unavailable":
		return "Sandboxes cannot be checked right now. Try again in a moment."
	case "token_expired", "token_revoked", "grant_revoked", "invalid_token":
		return "Your session is no longer valid. Run beaver auth login."
	case "insufficient_scope":
		return scopeMessage(e)
	case "confirmation_required", "confirmation_pending":
		return "This needs your approval in a browser first. Run the command again to get the approval link."
	}
	if code == "upstream_not_json" {
		return fmt.Sprintf("The server answered with HTTP %d, which is not a TechBeaver response. Something between you and the API replied instead.", e.StatusCode)
	}
	if fallback, ok := fallbacks[code]; ok {
		return orDefault(msg, fallback)
	}
	if e.StatusCode == http.StatusTooManyRequests {
		return rateLimited(e, msg)
	}
	switch {
	case e.StatusCode == http.StatusUnauthorized:
		return "You are not signed in, or your session has expired. Run beaver auth login."
	case e.StatusCode == http.StatusForbidden:
		return orDefault(msg, "Your account is not allowed to do that.")
	case e.StatusCode == http.StatusTooManyRequests:
		return "TechBeaver is rate limiting this connection. Wait a moment and try again."
	case e.StatusCode >= 500:
		return orDefault(msg, "TechBeaver had a problem on its side. Try again in a moment.")
	}
	return orDefault(msg, fmt.Sprintf("TechBeaver returned HTTP %d.", e.StatusCode))
}

var fallbacks = map[string]string{
	"usage_owed":                     "By-the-hour use is still owed for this project, so that cannot be done yet.",
	"sandboxes_exist":                "Sandboxes still exist, so that cannot be done.",
	"sandbox_subscription_live":      "A sandbox subscription is still live, so that cannot be done.",
	"hourly_cap":                     "The by-the-hour limit has been reached.",
	"hourly_subscription_cap":        "The by-the-hour subscription limit has been reached.",
	"sandbox_too_many_open_payments": "Too many sandbox payments are waiting to be completed. Finish or let them expire, then try again.",
	"sandbox_credit_not_supported":   "Sandboxes cannot be paid for with credit.",
	"no_free_slot":                   "Every slot is in use. Stop or destroy a sandbox, or buy more slots with beaver sandbox buy.",
	"sandbox_not_running":            "That sandbox is not running. Start it with beaver sandbox start.",
	"sandbox_quarantined":            "That sandbox is quarantined and cannot be used. Contact support.",
	"sandbox_agent_unreachable":      "The sandbox did not answer. Try again in a moment; if it keeps failing, stop and start it.",
	"sandbox_agent_malformed":        "The sandbox gave an answer TechBeaver could not use. Try again.",
	"sandbox_exec_not_found":         "No such command in this sandbox.",
	"sandbox_exec_expired":           "That command's result was dropped: results are kept for 10 minutes after the command ends.",
	"sandbox_exec_lost":              "The sandbox's agent restarted while that command ran, so the command and its output are gone.",
	"sandbox_exec_killed":            "The sandbox is not running, so any command in it was ended and its output is gone.",
	"sandbox_exec_unavailable":       "Running commands and moving files in sandboxes is not available yet.",
	"file_not_found":                 "No such file or directory.",
	"sandbox_runtime_failed":         "The sandbox runtime failed. Try again in a moment.",
	"sandbox_runtime_malformed":      "The sandbox runtime sent an answer TechBeaver could not use. Try again in a moment.",
	"sandbox_runtime_not_configured": "Sandboxes are not available yet. The sandbox runtime is still being set up.",
	"sandbox_plan_not_on_sale":       "That sandbox plan is not on sale right now.",
	"sandbox_plan_not_priced":        "That sandbox plan is not sold in your currency yet.",
	"sandbox_bad_slots":              "That slot count is not allowed.",
	"validation_failed":              "The request was not accepted. Check the project, plan and slot count.",
	"invalid_fields":                 "The request was not accepted. Check the project, plan and slot count.",
	"payment_provider_unavailable":   "The payment provider is not available right now. Try again in a moment.",
}

func orDefault(msg, fallback string) string {
	if msg == "" {
		return fallback
	}
	return msg
}

func trimStop(s string) string { return strings.TrimRight(s, ". ") }

// scopeMessage names the scope to grant and where to grant it.
func scopeMessage(e *client.APIError) string {
	var missing []string
	if list, ok := e.Data["missingScope"].([]any); ok {
		for _, v := range list {
			if name, ok := v.(string); ok && name != "" {
				missing = append(missing, oneLine(name))
			}
		}
	}
	if len(missing) == 0 {
		return "This credential was not granted the permission this needs. Add it in the agent connection's permissions in the portal (Portal, AI and CLI), then sign in again with beaver auth login."
	}
	list := strings.Join(missing, " and ")
	return "This credential was not granted " + list + ". Add it in the agent connection's permissions in the portal (Portal, AI and CLI), or sign in again with beaver auth login --scope " + strings.Join(missing, ",") + "."
}

func rateLimited(e *client.APIError, msg string) string {
	base := orDefault(msg, "TechBeaver is rate limiting this connection.")
	if e.Code == "" || strings.EqualFold(msg, e.Code) {
		base = "TechBeaver is rate limiting this connection."
	}
	if secs := int(e.RetryAfter.Seconds()); secs > 0 {
		return fmt.Sprintf("%s Try again in %d seconds.", strings.TrimRight(base, " "), secs)
	}
	return base + " Wait a moment and try again."
}
