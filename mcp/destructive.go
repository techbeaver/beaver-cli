package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/techbeaver/beaver-cli/client"
)

// How a destructive act gets a human's approval.
//
// The requirement is that a person approves anything that tears something down.
// The honest starting point is that the MCP annotation for this does not
// achieve it: destructiveHint is a hint, a client may ignore it, and a client
// configured to auto-approve everything will. A two-phase handshake where the
// agent asks for a preview and then calls again with the token it was handed is
// not a human gate either, because an autonomous loop can make both calls.
//
// So the layers are:
//
//	1. destructiveHint on the tool, which makes well-behaved clients prompt.
//	   Pleasant, not a control.
//	2. A preview, returned before anything happens, so the consequences land in
//	   the conversation where a person can see them. Also not a control.
//	3. Elicitation, where the client supports it: the server asks the client to
//	   put a question to the user, and this code then waits for the approval to
//	   actually land. A genuine prompt, but only where a client implements it.
//	4. A row in TechBeaver's own database that only becomes usable when somebody
//	   signed into the portal, in a browser, approves that exact action, checked
//	   by the API before the handler runs.
//
// Only the fourth is enforcement, and it is the one that holds regardless of
// how a client is configured. The first three exist to make the fourth pleasant
// rather than to substitute for it.

// destructiveSpec describes one act that needs approval.
type destructiveSpec struct {
	// Method and Path are the exact API call being authorised. The confirmation
	// is bound to both, so an approval for deleting one app cannot delete
	// another, and an approval for deleting an app cannot delete a database.
	Method string
	Path   string
	Body   any

	Action       string
	ResourceType string
	ResourceId   string
	ResourceName string

	// Preview is what the human is shown, assembled from live data rather than
	// from anything this service assumed.
	Preview map[string]any
	// RecoveryNote states the recovery path and its window in plain words.
	// Required: an agent that deletes something and cannot say how to get it
	// back has turned a recoverable mistake into a panic.
	RecoveryNote string

	// ElicitMessage is what a client that supports elicitation puts in front of
	// the user.
	ElicitMessage string
}

// elicitationWait is how long this tool call waits for an approval after
// prompting through the client.
//
// It exists so that, on a client that supports elicitation, a customer sees one
// question, opens a link, approves, and the work happens: no second tool call,
// no token to shuttle around. If they take longer, the call returns the link
// and the agent can come back to it, so nothing is lost by the wait ending.
const elicitationWait = 90 * time.Second

// performDestructive runs the whole flow for one destructive act.
func performDestructive(ctx context.Context, deps *Deps, call *Call, confirmationToken string, spec destructiveSpec) (*DestructiveResult, error) {
	if confirmationToken != "" {
		return executeDestructive(ctx, deps, call, confirmationToken, spec)
	}

	pending, err := createConfirmation(ctx, deps, call, spec)
	if err != nil {
		return nil, err
	}

	// Layer 3. Ask the user directly, then wait, so one tool call finishes it.
	if approved := askAndWait(ctx, deps, call, pending); approved {
		return executeDestructive(ctx, deps, call, pending.Token, spec)
	}

	return &DestructiveResult{
		Status:            "awaiting_approval",
		Summary:           fmt.Sprintf("Waiting for the account owner to approve: %s %s.", spec.Action, describeResource(spec)),
		Preview:           spec.Preview,
		RecoveryNote:      spec.RecoveryNote,
		ApprovalURL:       pending.ApprovalURL,
		ConfirmationToken: pending.Token,
		ExpiresAt:         pending.ExpiresAt,
		NextStep: "Show the customer the preview above and the approval link. They have to open it and approve in their browser. " +
			"Once they say they have, call this tool again with confirmationToken set to the value above. " +
			"Do not call it repeatedly in the meantime: nothing will happen until they approve, and repeated attempts are rate limited.",
	}, nil
}

type pendingConfirmation struct {
	Id           string `json:"id"`
	Status       string `json:"status"`
	ApprovalURL  string `json:"approvalUrl"`
	Token        string `json:"confirmationToken"`
	ExpiresAt    string `json:"expiresAt"`
	RecoveryNote string `json:"recoveryNote"`
}

// createConfirmation registers the pending act with the API.
func createConfirmation(ctx context.Context, deps *Deps, call *Call, spec destructiveSpec) (*pendingConfirmation, error) {
	env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{
		Method: http.MethodPost,
		Path:   "/mcp/confirmations",
		Body: map[string]any{
			"method":       spec.Method,
			"path":         spec.Path,
			"action":       spec.Action,
			"resourceType": spec.ResourceType,
			"resourceId":   spec.ResourceId,
			"resourceName": spec.ResourceName,
			"summary":      spec.Preview,
			"recoveryNote": spec.RecoveryNote,
		},
	})
	if err != nil {
		return nil, describeAPIError(err)
	}
	return decodeInto[pendingConfirmation](env)
}

// executeDestructive makes the call the human approved.
func executeDestructive(ctx context.Context, deps *Deps, call *Call, confirmationToken string, spec destructiveSpec) (*DestructiveResult, error) {
	env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{
		Method:  spec.Method,
		Path:    spec.Path,
		Body:    spec.Body,
		Headers: map[string]string{"X-Confirmation-Token": confirmationToken},
	})
	if err != nil {
		return nil, describeAPIError(err)
	}
	item, _ := client.DecodeObject(env)
	return &DestructiveResult{
		Status:       "done",
		Summary:      fmt.Sprintf("Done: %s %s.", spec.Action, describeResource(spec)),
		RecoveryNote: spec.RecoveryNote,
		Result:       item,
		// Restated at deletion: the preview has been scrolled past by the time it matters.
		NextStep: "Tell the customer plainly what was removed and how to recover it: " + spec.RecoveryNote,
	}, nil
}

// askAndWait puts the approval in front of the user through the client, then
// waits for it to actually land.
//
// Returns false for every failure mode, including a client that does not
// support elicitation at all, which is the common case. False is not an error:
// it means fall back to handing the link over, and the caller does.
func askAndWait(ctx context.Context, deps *Deps, call *Call, pending *pendingConfirmation) bool {
	if call.MCP == nil || call.MCP.Session == nil || pending.ApprovalURL == "" {
		return false
	}

	message := pending.RecoveryNote
	if message == "" {
		message = "This action cannot be undone from here."
	}
	prompt := "TechBeaver needs you to approve this in your browser. Open the link, approve it, then come back. " + message

	// URL-mode elicitation is this shape; form-mode clients fall through to a yes/no.
	_, err := call.MCP.Session.Elicit(ctx, &mcpsdk.ElicitParams{
		Mode:    "url",
		Message: prompt,
		URL:     pending.ApprovalURL,
	})
	if err != nil {
		_, err = call.MCP.Session.Elicit(ctx, &mcpsdk.ElicitParams{
			Mode:    "form",
			Message: prompt + " Approval link: " + pending.ApprovalURL,
			RequestedSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"approvedInBrowser": map[string]any{
						"type":        "boolean",
						"description": "Tick this once you have approved it in your browser",
					},
				},
				"required": []string{"approvedInBrowser"},
			},
		})
		if err != nil {
			return false
		}
	}

	// Only the approval landing counts: a client can answer "accept" on its own.
	return waitForApproval(ctx, deps, call, pending.Id)
}

// waitForApproval polls until the account owner has approved, or time runs out.
func waitForApproval(ctx context.Context, deps *Deps, call *Call, confirmationID string) bool {
	if confirmationID == "" {
		return false
	}
	deadline := time.Now().Add(elicitationWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
		env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{
			Method: http.MethodGet,
			Path:   "/mcp/confirmations/" + url.PathEscape(confirmationID),
		})
		if err != nil {
			return false
		}
		state, err := decodeInto[pendingConfirmation](env)
		if err != nil {
			return false
		}
		switch state.Status {
		case "approved":
			return true
		case "rejected":
			return false
		}
	}
	return false
}

func describeResource(spec destructiveSpec) string {
	name := spec.ResourceName
	if name == "" {
		name = spec.ResourceId
	}
	return fmt.Sprintf("%s %s", spec.ResourceType, name)
}
