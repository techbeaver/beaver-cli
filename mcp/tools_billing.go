package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
)

// Money.
//
// The single most important fact about this file is a negative: nothing in it
// can complete a payment. Every purchase path on this platform ends at a hosted
// payment page, so the strongest consent an AI's owner could ask for, a person
// typing a card into a page themselves, is enforced by the architecture rather
// than by a rule an agent could be talked out of.
//
// Three paths on the platform CAN move money without that page: spending
// referral credit, charging a saved card, and paying out to a bank account.
// None of them is a tool here, and none of them is on the API's allowlist for
// machine credentials either, so an agent cannot reach them by any route.

func registerBillingTools(r *Registry) {
	register(r, toolSpec{
		Name:     "quote_purchase",
		Title:    "Price something before buying it",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "What a plan change would actually cost, worked out live: proration, tax and any discount included. This is the ONLY correct answer to what will this cost. Prices in a plan listing are before tax and before proration, and the tax rate is a configurable record rather than a constant, so never do this arithmetic yourself.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		ResourceType string `json:"resourceType" jsonschema:"app or database"`
		ResourceID   string `json:"resourceId" jsonschema:"the app's or managed database's id"`
		PlanID       string `json:"planId" jsonschema:"the plan being priced, from list_plans or list_database_plans"`
		BillingCycle string `json:"billingCycle,omitempty" jsonschema:"monthly or yearly"`
		DiscountCode string `json:"discountCode,omitempty" jsonschema:"a promotion code to include in the quote"`
	}) (*ObjectResult, error) {
		var path string
		body := map[string]any{}
		switch strings.ToLower(in.ResourceType) {
		case "app", "paas", "application":
			path = "/paas/apps/" + url.PathEscape(in.ResourceID) + "/upgrade/quote"
			body["paasPlanId"] = in.PlanID
		case "database", "dbaas", "db":
			path = "/dbaas/instances/" + url.PathEscape(in.ResourceID) + "/upgrade/quote"
			body["dbaasPlanId"] = in.PlanID
		default:
			return nil, fmt.Errorf("resourceType has to be app or database, not %q", in.ResourceType)
		}
		putIfSet(body, "billingCycle", in.BillingCycle)
		putIfSet(body, "discountCode", in.DiscountCode)

		env, err := apiSend(ctx, deps, call, http.MethodPost, path, body, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ObjectResult{
			Summary: "Quoted live. Give the customer these figures exactly; do not round them or recompute anything.",
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:     "list_invoices",
		Title:    "List invoices",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "The account's invoices and their payment status.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		Status string `json:"status,omitempty" jsonschema:"filter by status, for example unpaid"`
	}) (*ListResult, error) {
		query := url.Values{}
		if in.Status != "" {
			query.Set("status", in.Status)
		}
		items, err := apiList(ctx, deps, call, "/billing/invoices", query)
		if err != nil {
			return nil, err
		}
		return &ListResult{Summary: fmt.Sprintf("%d invoice(s).", len(items)), Count: len(items), Items: items}, nil
	})

	register(r, toolSpec{
		Name:     "get_invoice",
		Title:    "Get an invoice",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "One invoice in full, including its line items and what is still owed. Also how to check whether a payment link has been paid: poll this rather than asking the customer repeatedly.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InvoiceID string `json:"invoiceId" jsonschema:"the invoice's id"`
	}) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/billing/invoices/"+url.PathEscape(in.InvoiceID), nil)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{
			Summary: fmt.Sprintf("Invoice %s is %s.", pick(item, "invoiceNumber", "id"), pick(item, "status")),
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:     "get_credit_balance",
		Title:    "Get account credit",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "How much referral credit the account holds. Read only: you cannot spend it, and no permission exists that would let you.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/referrals/summary", nil)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{
			Summary: "Credit balance. Spending it is something only the customer can do, in the console.",
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:        "pay_invoice",
		Title:       "Prepare payment for an invoice",
		Scopes:      []string{scopes.BillingCheckout},
		Description: "Produces a hosted payment page for an unpaid invoice. It does NOT pay it: hand the link to the customer and say plainly that only they can complete the payment. Poll get_invoice afterwards to see whether they did.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InvoiceID      string `json:"invoiceId" jsonschema:"the invoice to pay"`
		DiscountCode   string `json:"discountCode,omitempty" jsonschema:"a promotion code to apply"`
		IdempotencyKey string `json:"idempotencyKey" jsonschema:"a unique string you generate once; reuse exactly the same value if you retry, or the customer may be charged twice"`
	}) (*CheckoutResult, error) {
		if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
			return nil, err
		}
		body := map[string]any{}
		putIfSet(body, "discountCode", in.DiscountCode)
		env, err := apiSend(ctx, deps, call, http.MethodPost,
			"/billing/invoices/"+url.PathEscape(in.InvoiceID)+"/pay", body, idempotencyHeaders(in.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return checkoutResult(item, "Payment page ready for this invoice."), nil
	})

	register(r, toolSpec{
		Name:        "create_checkout_link",
		Title:       "Prepare payment for an app or database",
		Scopes:      []string{scopes.BillingCheckout},
		Description: "Produces a hosted payment page for an app or managed database that is waiting to be paid for. It does NOT pay: hand the link over and tell the customer only they can complete it. If the total comes to nothing, because of a discount or credit, the response says so and there is no link to give.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		ResourceType   string `json:"resourceType" jsonschema:"app or database"`
		ResourceID     string `json:"resourceId" jsonschema:"the app's or managed database's id"`
		DiscountCode   string `json:"discountCode,omitempty" jsonschema:"a promotion code to apply"`
		PaymentSource  string `json:"paymentSource,omitempty" jsonschema:"leave empty for a payment link. Only a partner key (whoami canSpendCredit true) may set balance or saved_card to pay with no payment page; any other connection is refused"`
		IdempotencyKey string `json:"idempotencyKey" jsonschema:"a unique string you generate once; reuse exactly the same value if you retry, or the customer may be charged twice"`
	}) (*CheckoutResult, error) {
		if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
			return nil, err
		}
		var path string
		switch strings.ToLower(in.ResourceType) {
		case "app", "paas", "application":
			path = "/paas/apps/" + url.PathEscape(in.ResourceID) + "/checkout"
		case "database", "dbaas", "db":
			path = "/dbaas/instances/" + url.PathEscape(in.ResourceID) + "/checkout"
		default:
			return nil, fmt.Errorf("resourceType has to be app or database, not %q", in.ResourceType)
		}
		body := map[string]any{}
		putIfSet(body, "discountCode", in.DiscountCode)
		putIfSet(body, "paymentSource", in.PaymentSource)
		env, err := apiSend(ctx, deps, call, http.MethodPost, path, body, idempotencyHeaders(in.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return checkoutResult(item, "Payment page ready."), nil
	})

	register(r, toolSpec{
		Name:     "get_prepaid_balance",
		Title:    "Get the prepaid balance",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "How much money the account has paid in advance, and its latest movements. Renewals are paid from it before any card. It can only be spent on TechBeaver: it is never withdrawn or refunded as cash.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/billing/prepaid-balance", nil)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{Summary: "Prepaid balance, in kobo.", Item: item}, nil
	})

	register(r, toolSpec{
		Name:        "create_topup_link",
		Title:       "Prepare a prepaid balance top-up",
		Scopes:      []string{scopes.BillingCheckout},
		Description: "Produces a hosted payment page that adds money to the prepaid balance. It does NOT pay: hand the link over and tell the customer only they can complete it. Amount in kobo, from 100000 (N1,000) to 1000000000 (N10,000,000). Tell them a top-up cannot be refunded as cash.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		AmountMinor    int64  `json:"amountMinor" jsonschema:"how much to add, in kobo"`
		IdempotencyKey string `json:"idempotencyKey" jsonschema:"a unique string you generate once; reuse exactly the same value if you retry, or the customer may be charged twice"`
	}) (*CheckoutResult, error) {
		if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
			return nil, err
		}
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/billing/prepaid-balance/top-ups",
			map[string]any{"amountMinor": in.AmountMinor}, idempotencyHeaders(in.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return checkoutResult(item, "Top-up payment page ready."), nil
	})
}

// paidWithoutLink says how a create or checkout was paid when no payment page was involved, or
// "" when it was not paid that way.
func paidWithoutLink(item map[string]any) string {
	switch {
	case item["paidFromBalance"] == true:
		return "from the prepaid balance"
	case item["paidWithSavedCard"] == true:
		return "with the saved card"
	}
	return ""
}

// checkoutResult reads the payment link out of a checkout response and says the
// right thing in both cases: link, or nothing left to pay.
//
// The zero-total case is not hypothetical. A full discount or enough credit
// settles an order outright, and a tool that promised a link and produced none
// would leave an agent telling a customer to pay something that is already
// paid.
func checkoutResult(item map[string]any, summary string) *CheckoutResult {
	link := pick(item, "authorizationUrl", "paymentUrl", "checkoutUrl")
	out := &CheckoutResult{Summary: summary, Result: item, PaymentURL: link}
	if paid := paidWithoutLink(item); paid != "" {
		out.Summary = "Paid " + paid + "."
		out.NextStep = "No payment link is needed: it is paid. Confirm with get_invoice if the customer asks."
		return out
	}
	if link == "" {
		out.Summary = "Nothing is left to pay on this."
		out.NextStep = "There is no payment link because the balance is already settled. Confirm with get_invoice and tell the customer no payment is needed."
		return out
	}
	out.NextStep = "Give the customer this link and ask them to complete the payment themselves. You cannot pay it for them. Poll get_invoice to see when it is settled."
	return out
}
