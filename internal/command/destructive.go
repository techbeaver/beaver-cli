package command

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

// ConfirmationHeader carries an approved confirmation on the request it
// authorises.
const ConfirmationHeader = "X-Confirmation-Token"

type pendingConfirmation struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	ApprovalURL  string `json:"approvalUrl"`
	Token        string `json:"confirmationToken"`
	ExpiresAt    string `json:"expiresAt"`
	RecoveryNote string `json:"recoveryNote"`
}

// DestructiveRequest is an action the platform requires a human to approve.
type DestructiveRequest struct {
	Method       string
	Path         string
	Body         any
	Action       string
	ResourceType string
	ResourceID   string
	ResourceName string
	Preview      map[string]any
	RecoveryNote string
}

// RunDestructive registers the action, prints the approval link, and waits for
// a person to approve it in a browser.
//
// --yes suppresses only this CLI's local prompt. It cannot suppress the human
// approval, which the server enforces. See ADR 0007.
func RunDestructive(ctx context.Context, env *Env, s *Session, req DestructiveRequest, assumeYes bool, wait time.Duration) (*client.Envelope, error) {
	// The body is encoded once: the API hashes it, so both calls must send these exact bytes.
	body, err := fixedBody(req.Body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Body = body
	}

	if !assumeYes && env.IsTTY {
		if err := confirmLocally(env, req); err != nil {
			return nil, err
		}
	}

	pending, err := createConfirmation(ctx, s, req)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(env.Err, "\nThis destroys something, so it needs your approval in a browser.\n\n  %s\n\n", pending.ApprovalURL)
	if req.RecoveryNote != "" {
		fmt.Fprintf(env.Err, "%s\n\n", req.RecoveryNote)
	}

	if !env.IsTTY && wait <= 0 {
		return nil, fmt.Errorf("%w: approve it at %s, then run this again",
			exitcode.ErrAwaitingApproval, pending.ApprovalURL)
	}

	token, err := waitForApproval(ctx, env, s, pending, wait)
	if err != nil {
		return nil, err
	}

	return s.Client.Do(ctx, s.Token, client.Request{
		Method:  req.Method,
		Path:    req.Path,
		Body:    req.Body,
		Headers: map[string]string{ConfirmationHeader: token},
	})
}

// fixedBody encodes a request body once. A nil body stays nil so the replay has
// no body at all, exactly as the registration declared.
func fixedBody(body any) (json.RawMessage, error) {
	if body == nil {
		return nil, nil
	}
	if raw, ok := body.(json.RawMessage); ok {
		return raw, nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding the request body: %w", err)
	}
	return encoded, nil
}

func confirmLocally(env *Env, req DestructiveRequest) error {
	name := req.ResourceName
	if name == "" {
		name = req.ResourceID
	}
	fmt.Fprintf(env.Err, "About to %s %s %q.\n", req.Action, req.ResourceType, name)
	for k, v := range req.Preview {
		fmt.Fprintf(env.Err, "  %s: %v\n", k, v)
	}
	fmt.Fprintf(env.Err, "\nType the name to continue: ")

	var typed string
	if _, err := fmt.Fscanln(env.In, &typed); err != nil {
		return fmt.Errorf("cancelled")
	}
	if strings.TrimSpace(typed) != name {
		return fmt.Errorf("that did not match %q, so nothing was done", name)
	}
	return nil
}

func createConfirmation(ctx context.Context, s *Session, req DestructiveRequest) (*pendingConfirmation, error) {
	env, err := s.Client.Do(ctx, s.Token, client.Request{
		Method: http.MethodPost,
		Path:   "/mcp/confirmations",
		Body:   confirmationBody(req),
	})
	if err != nil {
		return nil, err
	}
	obj, err := client.DecodeObject(env)
	if err != nil {
		return nil, err
	}
	return &pendingConfirmation{
		ID:           str(obj["id"]),
		Status:       str(obj["status"]),
		ApprovalURL:  str(obj["approvalUrl"]),
		Token:        str(obj["confirmationToken"]),
		RecoveryNote: str(obj["recoveryNote"]),
	}, nil
}

// confirmationBody is what registers a confirmation. "body" carries the exact
// JSON the replayed call will send, so the API can bind the approval to it.
func confirmationBody(req DestructiveRequest) map[string]any {
	out := map[string]any{
		"method":       req.Method,
		"path":         req.Path,
		"action":       req.Action,
		"resourceType": req.ResourceType,
		"resourceId":   req.ResourceID,
		"resourceName": req.ResourceName,
		"summary":      req.Preview,
		"recoveryNote": req.RecoveryNote,
	}
	if raw, ok := req.Body.(json.RawMessage); ok && len(raw) > 0 {
		out["body"] = raw
	}
	return out
}

// approvalPoll is how often a pending confirmation is checked.
var approvalPoll = 3 * time.Second

func waitForApproval(ctx context.Context, env *Env, s *Session, pending *pendingConfirmation, wait time.Duration) (string, error) {
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	deadline := time.Now().Add(wait)
	fmt.Fprint(env.Err, "Waiting for approval. Press Ctrl-C to stop; the link stays valid.\n")

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(approvalPoll):
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: not approved yet. Approve it at %s and run this again",
				exitcode.ErrAwaitingApproval, pending.ApprovalURL)
		}

		envelope, err := s.Client.Do(ctx, s.Token, client.Request{
			Method: http.MethodGet,
			Path:   "/mcp/confirmations/" + url.PathEscape(pending.ID),
		})
		if err != nil {
			return "", err
		}
		obj, err := client.DecodeObject(envelope)
		if err != nil {
			return "", err
		}
		done, token, err := interpretStatus(str(obj["status"]), str(obj["confirmationToken"]), pending)
		if err != nil || done {
			return token, err
		}
	}
}

// interpretStatus maps every status the API reports for a confirmation to an
// outcome. done with no error means approved. pending means keep waiting. A
// status this client does not know is a failure: acting on it would mean
// guessing whether a human said yes.
func interpretStatus(status, token string, pending *pendingConfirmation) (done bool, tok string, err error) {
	switch status {
	case "approved":
		if token == "" {
			token = pending.Token
		}
		if token == "" {
			return false, "", fmt.Errorf("the approval came back without a confirmation token, so nothing was done. Run the command again")
		}
		return true, token, nil
	case "pending":
		return false, "", nil
	case "rejected", "declined":
		return false, "", fmt.Errorf("the approval was rejected, so nothing was done")
	case "consumed":
		return false, "", fmt.Errorf("that approval was already used for an earlier call. Nothing was done this time; run the command again for a fresh approval")
	case "expired":
		return false, "", fmt.Errorf("that approval request expired. Run the command again for a fresh one")
	default:
		return false, "", fmt.Errorf("the platform reported the approval as %q, which this version of beaver does not understand, so nothing was done. Update beaver", status)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
