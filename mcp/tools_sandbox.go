package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/sandboxerr"
	"github.com/techbeaver/beaver-cli/scopes"
)

// Sandbox tools: read the catalogue and a project's usage, price a purchase, and get a payment link.
// The platform lets machine credentials use only these routes, and nothing here can pay.

const sandboxPaymentSourceCheckout = "checkout"

// describeSandboxError keeps the model-facing guidance for the codes that have it and otherwise gives one calm line.
func describeSandboxError(err error) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		if sandboxerr.Unavailable(apiErr) {
			return fmt.Errorf("%s", sandboxerr.NotAvailable)
		}
		switch normaliseCode(apiErr.Code) {
		case "idempotency_key_reused", "idempotency_in_progress", "service_paused",
			"token_expired", "token_revoked", "grant_revoked", "invalid_token", "insufficient_scope":
			return describeAPIError(err)
		}
	}
	if d := sandboxerr.Describe(err); d != err {
		return errors.New(d.Error())
	}
	return describeAPIError(err)
}

func sandboxGet(ctx context.Context, deps *Deps, call *Call, path string, query url.Values) (map[string]any, error) {
	env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{Method: http.MethodGet, Path: path, Query: query})
	if err != nil {
		return nil, describeSandboxError(err)
	}
	return client.DecodeObject(env)
}

func sandboxSend(ctx context.Context, deps *Deps, call *Call, path string, body any, headers map[string]string) (map[string]any, error) {
	env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{Method: http.MethodPost, Path: path, Body: body, Headers: headers})
	if err != nil {
		return nil, describeSandboxError(err)
	}
	return client.DecodeObject(env)
}

type sandboxPurchaseInput struct {
	ProjectID string `json:"projectId" jsonschema:"the project the slots are for, from list_projects"`
	PlanID    string `json:"planId" jsonschema:"the sandbox plan, from list_sandbox_plans"`
	Slots     int    `json:"slots" jsonschema:"how many sandbox slots, at least 1"`
}

func registerSandboxTools(r *Registry) {
	register(r, toolSpec{
		Name:     "list_sandbox_plans",
		Title:    "List sandbox plans",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "The sandbox plans that can be bought right now, with their live monthly and hourly prices per currency. If this comes back empty, no sandbox plan has been activated and priced, so a sandbox cannot currently be bought: say that rather than guessing at a plan id. Never quote a price from memory: use quote_sandbox_slots for what a purchase will actually cost.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ListResult, error) {
		env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{Method: http.MethodGet, Path: "/sandbox/plans"})
		if err != nil {
			return nil, describeSandboxError(err)
		}
		items, err := client.DecodeList(env)
		if err != nil {
			return nil, err
		}
		summary := fmt.Sprintf("%d sandbox plan(s) available. Prices here are live; do not recall them later.", len(items))
		if len(items) == 0 {
			summary = "No sandbox plans are available to buy at the moment."
		}
		return &ListResult{Summary: summary, Count: len(items), Items: items}, nil
	})

	register(r, toolSpec{
		Name:     "get_sandbox_usage",
		Title:    "Get sandbox usage for a project",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "A project's sandbox position: the slots bought, how many are in use, when the period ends, the balance available to run them, the hourly burn, the estimated hours remaining and each sandbox's usage this period. If balanceHidden is true the caller may not see the balance: say it is hidden, never that it is zero. Read every figure from the result rather than working it out.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		ProjectID string `json:"projectId" jsonschema:"the project's id, from list_projects"`
	}) (*ObjectResult, error) {
		projectID, err := sandboxProjectID(in.ProjectID)
		if err != nil {
			return nil, err
		}
		item, err := sandboxGet(ctx, deps, call, "/sandbox/usage", url.Values{"projectId": {projectID}})
		if err != nil {
			return nil, err
		}
		out := &ObjectResult{Summary: "Sandbox usage for the project, read live. Give the customer these figures as returned.", Item: item}
		if item["balanceHidden"] == true {
			out.Summary = "The balance is hidden for this member, so report it as hidden and not as zero."
		}
		return out, nil
	})

	register(r, toolSpec{
		Name:     "quote_sandbox_slots",
		Title:    "Price sandbox slots before buying them",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "What buying a number of sandbox slots on a plan would cost, worked out live by TechBeaver in the customer's own currency, tax and any proration included. This is the ONLY correct answer to what will this cost. It takes no amount, price or currency: you choose the plan and the number of slots and the server returns the figures. It creates nothing and charges nothing.",
	}, func(ctx context.Context, deps *Deps, call *Call, in sandboxPurchaseInput) (*ObjectResult, error) {
		body, err := sandboxPurchaseBody(in.ProjectID, in.PlanID, in.Slots)
		if err != nil {
			return nil, err
		}
		item, err := sandboxSend(ctx, deps, call, "/sandbox/billing/quote", body, nil)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{
			Summary: "Quoted live. Give the customer these figures exactly; do not round them or recompute anything.",
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:        "create_sandbox_checkout_link",
		Title:       "Prepare payment for sandbox slots",
		Scopes:      []string{scopes.BillingCheckout},
		Description: "Prepares the purchase of sandbox slots and returns a hosted payment page for the customer. It does NOT pay: hand the link over and tell the customer only they can complete it, and that nothing is bought until they do. It cannot pay from the balance, a saved card or credit, for any connection. For a customer billed in dollars there may be no card page: the answer then says the wallet has to be topped up first and carries the top-up link, which you hand over the same way. Price it first with quote_sandbox_slots. Take planId from list_sandbox_plans.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		ProjectID      string `json:"projectId" jsonschema:"the project to buy the slots for, from list_projects"`
		PlanID         string `json:"planId" jsonschema:"the sandbox plan, from list_sandbox_plans"`
		Slots          int    `json:"slots" jsonschema:"how many sandbox slots to buy, at least 1"`
		SuccessURL     string `json:"successUrl,omitempty" jsonschema:"where the customer is sent after paying; leave empty unless they gave you one"`
		PaymentSource  string `json:"paymentSource,omitempty" jsonschema:"leave empty. Only a payment link is ever produced here; any other value is refused"`
		IdempotencyKey string `json:"idempotencyKey" jsonschema:"a unique string you generate once; reuse exactly the same value if you retry, or the customer may be charged twice"`
	}) (*CheckoutResult, error) {
		if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
			return nil, err
		}
		if err := requireSandboxCheckoutSource(in.PaymentSource); err != nil {
			return nil, err
		}
		body, err := sandboxPurchaseBody(in.ProjectID, in.PlanID, in.Slots)
		if err != nil {
			return nil, err
		}
		body["paymentSource"] = sandboxPaymentSourceCheckout
		putIfSet(body, "successUrl", strings.TrimSpace(in.SuccessURL))
		item, err := sandboxSend(ctx, deps, call, "/sandbox/billing/purchase", body, idempotencyHeaders(in.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		return sandboxCheckoutResult(item), nil
	})
}

func sandboxProjectID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if !isUUID(id) {
		return "", fmt.Errorf("projectId has to be a project id (a UUID) from list_projects, not %q", raw)
	}
	return id, nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case !strings.ContainsRune("0123456789abcdefABCDEF", r):
			return false
		}
	}
	return true
}

func sandboxPurchaseBody(projectID, planID string, slots int) (map[string]any, error) {
	project, err := sandboxProjectID(projectID)
	if err != nil {
		return nil, err
	}
	plan := strings.TrimSpace(planID)
	if !isUUID(plan) {
		return nil, fmt.Errorf("planId has to be a sandbox plan id from list_sandbox_plans, not %q", planID)
	}
	if slots < 1 {
		return nil, fmt.Errorf("slots has to be a whole number of at least 1, not %d", slots)
	}
	return map[string]any{"projectId": project, "planId": plan, "slots": slots}, nil
}

func requireSandboxCheckoutSource(source string) error {
	s := strings.ToLower(strings.TrimSpace(source))
	if s == "" || s == sandboxPaymentSourceCheckout {
		return nil
	}
	return fmt.Errorf("paymentSource %q is refused: this tool only produces a payment link the customer opens themselves. Paying a sandbox purchase from the balance, a saved card or credit is a person's act, for every connection. Leave paymentSource empty", source)
}

// sandboxCheckoutResult never promises a page the API did not return.
func sandboxCheckoutResult(item map[string]any) *CheckoutResult {
	link := pick(item, "authorizationUrl", "paymentUrl", "checkoutUrl")
	topUp := pick(item, "topUpUrl", "topUpCheckoutUrl", "walletTopUpUrl")
	out := &CheckoutResult{Summary: "Sandbox slots payment page ready.", Result: item, PaymentURL: link}
	switch {
	case link != "":
		out.NextStep = "Give the customer this link and ask them to complete the payment themselves. You cannot pay it for them, and the slots are added only after they do. Poll get_sandbox_usage to see when they appear."
	case topUp != "":
		out.Summary = "The wallet has to be topped up before these slots can be bought."
		out.PaymentURL = topUp
		out.NextStep = "Give the customer this top-up link and say the wallet must be topped up first and they must complete it themselves. Nothing is bought yet. Once they have funded it, call this tool again with a new idempotencyKey."
	default:
		out.Summary = "The API returned no payment page."
		out.NextStep = "Do not tell the customer a page is waiting or that anything was bought. Read the result and the message in it and tell the customer plainly what it says."
	}
	return out
}
