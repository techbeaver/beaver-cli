package mcp

import (
	"context"
	"fmt"
	"net/http"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
)

func registerWalletTools(r *Registry) {
	register(r, toolSpec{
		Name:        "create_wallet_topup_link",
		Title:       "Prepare a dollar wallet top-up",
		Scopes:      []string{scopes.BillingCheckout},
		Description: "Produces a hosted card payment page that adds money to the dollar wallet, for accounts billed in dollars. It does NOT pay: hand the link over and tell the customer only they can complete it, and that nothing is added until they do. Amount in cents. The minimum, maximum and step are live settings: if the amount is refused, the error states the current limits. Accounts billed in naira use create_topup_link instead. It cannot save a card or turn on automatic top-ups, which only the customer can do on the portal.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		AmountMinor    int64  `json:"amountMinor" jsonschema:"how much to add, in cents"`
		IdempotencyKey string `json:"idempotencyKey" jsonschema:"a unique string you generate once; reuse exactly the same value if you retry, or the customer may be charged twice"`
	}) (*CheckoutResult, error) {
		if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
			return nil, err
		}
		if in.AmountMinor <= 0 {
			return nil, fmt.Errorf("amountMinor has to be a positive number of cents, not %d", in.AmountMinor)
		}
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/billing/wallet/top-ups",
			map[string]any{"amountMinor": in.AmountMinor}, idempotencyHeaders(in.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		item, err := client.DecodeObject(env)
		if err != nil {
			return nil, err
		}
		return walletTopUpResult(omitSecrets(item)), nil
	})
}

func walletTopUpResult(item map[string]any) *CheckoutResult {
	link := pick(item, "checkoutUrl")
	out := &CheckoutResult{Summary: "Wallet top-up payment page ready.", Result: item, PaymentURL: link}
	if link == "" {
		out.Summary = "The API returned a top-up with no payment page."
		out.NextStep = "Do not tell the customer anything was added or that a page is waiting. Read the status in the result and tell the customer plainly what it says. If it is expired or failed, start a new top-up with a new idempotencyKey."
		return out
	}
	out.NextStep = "Give the customer this link and ask them to complete the payment themselves. You cannot pay it for them, and the money reaches the wallet only after they do. State the amount from the result (amountMinor, in cents), not from the request: an open top-up for the account may have been returned."
	if item["resumed"] == true {
		out.Summary = "An open wallet top-up already existed and its payment page is returned."
	}
	return out
}
