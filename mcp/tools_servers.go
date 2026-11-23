package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
)

var serverBillingCycles = map[string]bool{"monthly": true, "quarterly": true, "semiannual": true, "annual": true}

const (
	currencyNaira  = "NGN"
	currencyDollar = "USD"
)

type serverCatalogRegion struct {
	Id   string `json:"id"`
	City string `json:"city"`
	Code string `json:"code"`
}

type serverCatalogPlan struct {
	Id        string `json:"id"`
	Name      string `json:"name"`
	VCPU      int    `json:"vcpu"`
	RAMMB     int    `json:"ramMb"`
	StorageGB int    `json:"storageGb"`
}

type serverCatalogOS struct {
	Id      string `json:"id"`
	PlanID  string `json:"planId"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type serverCatalog struct {
	Regions []serverCatalogRegion `json:"regions"`
	Plans   []serverCatalogPlan   `json:"plans"`
	OSes    []serverCatalogOS     `json:"operatingSystems"`
}

func serverBillingCurrency(ctx context.Context, deps *Deps, call *Call) (string, error) {
	status, err := apiGet(ctx, deps, call, "/billing/wallet/status", nil)
	if err != nil {
		return "", err
	}
	if status["available"] == true {
		return currencyDollar, nil
	}
	return currencyNaira, nil
}

func serverCatalogFor(ctx context.Context, deps *Deps, call *Call, currency string) (map[string]any, *serverCatalog, error) {
	path := "/servers/catalog"
	if currency == currencyDollar {
		path = "/servers/catalog/usd"
	}
	raw, err := apiGet(ctx, deps, call, path, nil)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("the server catalog was not readable: %w", err)
	}
	var typed serverCatalog
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return nil, nil, fmt.Errorf("the server catalog had an unexpected shape: %w", err)
	}
	return raw, &typed, nil
}

type serverChoice struct {
	Plan   serverCatalogPlan
	Region serverCatalogRegion
	OS     serverCatalogOS
}

func resolveServerChoice(cat *serverCatalog, planId, regionCode, osId string) (*serverChoice, error) {
	var choice serverChoice
	found := false
	for _, p := range cat.Plans {
		if p.Id == planId {
			choice.Plan, found = p, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("there is no plan with id %q in the catalog for this account. Call list_server_plans and use an id from it", planId)
	}
	if regionCode != "" {
		found = false
		for _, r := range cat.Regions {
			if strings.EqualFold(r.Code, regionCode) {
				choice.Region, found = r, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("there is no region with code %q in the catalog. Call list_server_plans and use a region code from it", regionCode)
		}
	}
	found = false
	for _, o := range cat.OSes {
		if o.Id == osId && (o.PlanID == "" || o.PlanID == choice.Plan.Id) {
			choice.OS, found = o, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("operating system %q is not offered with that plan. Call list_server_plans and use an osId whose planId matches the plan", osId)
	}
	return &choice, nil
}

func checkBillingCycle(cycle string) (string, error) {
	cycle = strings.ToLower(strings.TrimSpace(cycle))
	if cycle == "" {
		return "monthly", nil
	}
	if !serverBillingCycles[cycle] {
		return "", fmt.Errorf("billingCycle has to be monthly, quarterly, semiannual or annual, not %q", cycle)
	}
	return cycle, nil
}

func serverQuoteBody(c *serverChoice, cycle, currency string) map[string]any {
	return map[string]any{
		"planId": c.Plan.Id, "osId": c.OS.Id, "osName": c.OS.Name, "vcpu": c.Plan.VCPU,
		"billingCycle": cycle, "currency": currency,
	}
}

func registerServerTools(r *Registry) {
	register(r, toolSpec{
		Name:     "list_server_plans",
		Title:    "List server plans",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "The servers (VPS) that can be ordered: plans with their hardware and monthly price, the regions, and the operating systems each plan offers. Prices come back in the currency this account is billed in, which the result states as billedIn. Prices are before tax and before any cycle discount, and they are live data: never quote one from memory, and use quote_server_order for what an order will actually cost. The ids here are what quote_server_order and order_server take.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ObjectResult, error) {
		currency, err := serverBillingCurrency(ctx, deps, call)
		if err != nil {
			return nil, err
		}
		raw, _, err := serverCatalogFor(ctx, deps, call, currency)
		if err != nil {
			return nil, err
		}
		item := omitSecrets(raw)
		if currency != currencyDollar {
			stripUsdFields(item)
		}
		item["billedIn"] = currency
		plans, _ := item["plans"].([]any)
		return &ObjectResult{
			Summary: fmt.Sprintf("%d server plan(s), priced in %s.", len(plans), currency),
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:     "quote_server_order",
		Title:    "Price a server order",
		Scopes:   []string{scopes.BillingRead},
		ReadOnly: true, Idempotent: true,
		Description: "What a server order would cost, worked out live, tax and cycle included. It creates nothing and charges nothing. This is the ONLY correct answer to what a server will cost: never do the arithmetic yourself. For an account billed in dollars it also says whether the wallet balance covers the order and, if not, how much is missing.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		PlanID       string `json:"planId" jsonschema:"the plan, an id from list_server_plans"`
		OSID         string `json:"osId" jsonschema:"the operating system, an id from list_server_plans whose planId is this plan"`
		BillingCycle string `json:"billingCycle,omitempty" jsonschema:"monthly, quarterly, semiannual or annual; monthly when left out"`
	}) (*ObjectResult, error) {
		cycle, err := checkBillingCycle(in.BillingCycle)
		if err != nil {
			return nil, err
		}
		currency, err := serverBillingCurrency(ctx, deps, call)
		if err != nil {
			return nil, err
		}
		_, cat, err := serverCatalogFor(ctx, deps, call, currency)
		if err != nil {
			return nil, err
		}
		choice, err := resolveServerChoice(cat, in.PlanID, "", in.OSID)
		if err != nil {
			return nil, err
		}
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/servers/orders/quote", serverQuoteBody(choice, cycle, currency), nil)
		if err != nil {
			return nil, err
		}
		item, err := client.DecodeObject(env)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{
			Summary: "Quoted live. Give the customer these figures exactly; do not round them or recompute anything.",
			Item:    omitSecrets(item),
		}, nil
	})

	register(r, toolSpec{
		Name:        "order_server",
		Title:       "Order a server",
		Scopes:      []string{scopes.BillingRead, scopes.BillingCheckout},
		Description: "Creates a DRAFT server order and prepares how the customer pays for it. Nothing is provisioned and nothing is charged until the customer pays, and you can never pay for them. For an account billed in naira the result has paymentUrl, a hosted payment page: hand it over. For an account billed in dollars the server is paid from the customer's wallet, which only they can charge, so the result has portalCheckoutUrl instead: hand that over and say they confirm the payment there. For a dollar order it also says whether the wallet covers it; if it does not, call create_wallet_topup_link with suggestedTopUpMinor and give the customer both links. Take planId, osId and regionCode from list_server_plans, and say the price from quote_server_order, never from memory. Repeating a call with the same idempotencyKey returns the first order rather than a second one.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		Name           string `json:"name" jsonschema:"a label for the server, 2 to 255 characters"`
		PlanID         string `json:"planId" jsonschema:"the plan, an id from list_server_plans"`
		RegionCode     string `json:"regionCode" jsonschema:"the region, a code from list_server_plans"`
		OSID           string `json:"osId" jsonschema:"the operating system, an id from list_server_plans whose planId is this plan"`
		BillingCycle   string `json:"billingCycle,omitempty" jsonschema:"monthly, quarterly, semiannual or annual; monthly when left out"`
		IdempotencyKey string `json:"idempotencyKey" jsonschema:"a unique string you generate once, such as a UUID, with no colon in it; reuse exactly the same value if you retry, or the customer may be charged for two servers"`
	}) (*ServerOrderResult, error) {
		return orderServer(ctx, deps, call, in.Name, in.PlanID, in.RegionCode, in.OSID, in.BillingCycle, in.IdempotencyKey)
	})
}

func stripUsdFields(item map[string]any) {
	plans, _ := item["plans"].([]any)
	for _, p := range plans {
		if plan, ok := p.(map[string]any); ok {
			delete(plan, "usdMonthlyPriceMinor")
			delete(plan, "stripeEligible")
		}
	}
}

func orderServer(ctx context.Context, deps *Deps, call *Call, name, planId, regionCode, osId, billingCycle, idempotencyKey string) (*ServerOrderResult, error) {
	if err := requireIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(idempotencyKey)
	if strings.Contains(key, ":") {
		return nil, fmt.Errorf("idempotencyKey must not contain a colon. Use something like a UUID")
	}
	checkoutKey := key + ":checkout"
	if len(checkoutKey) > 255 {
		return nil, fmt.Errorf("idempotencyKey is too long. Use something short and unique, such as a UUID")
	}
	name = strings.TrimSpace(name)
	if n := len([]rune(name)); n < 2 || n > 255 {
		return nil, fmt.Errorf("name has to be 2 to 255 characters")
	}
	if strings.TrimSpace(regionCode) == "" {
		return nil, fmt.Errorf("regionCode is required for an order. Call list_server_plans and use a region code from it")
	}
	cycle, err := checkBillingCycle(billingCycle)
	if err != nil {
		return nil, err
	}
	currency, err := serverBillingCurrency(ctx, deps, call)
	if err != nil {
		return nil, err
	}
	_, cat, err := serverCatalogFor(ctx, deps, call, currency)
	if err != nil {
		return nil, err
	}
	choice, err := resolveServerChoice(cat, planId, regionCode, osId)
	if err != nil {
		return nil, err
	}

	env, err := apiSend(ctx, deps, call, http.MethodPost, "/servers/orders", map[string]any{
		"name": name, "regionCode": choice.Region.Code, "planId": choice.Plan.Id, "planName": choice.Plan.Name,
		"osId": choice.OS.Id, "osName": choice.OS.Name, "osVersion": choice.OS.Version,
		"vcpu": choice.Plan.VCPU, "ramMb": choice.Plan.RAMMB, "storageGb": choice.Plan.StorageGB,
		"billingCycle": cycle, "currency": currency,
	}, idempotencyHeaders(key))
	if err != nil {
		return nil, err
	}
	order, err := client.DecodeObject(env)
	if err != nil {
		return nil, err
	}
	order = omitSecrets(order)
	orderId := pick(order, "orderId")
	if orderId == "" {
		return nil, fmt.Errorf("the API accepted the order but returned no order id, so there is nothing to pay yet. Do not retry with a new idempotencyKey. Tell the customer and ask them to check their servers page")
	}
	orderCurrency := strings.ToUpper(pick(order, "currency"))
	if orderCurrency == currencyDollar {
		return dollarOrderResult(ctx, deps, call, order, orderId, choice, cycle), nil
	}
	return nairaOrderResult(ctx, deps, call, order, orderId, checkoutKey)
}

func nairaOrderResult(ctx context.Context, deps *Deps, call *Call, order map[string]any, orderId, checkoutKey string) (*ServerOrderResult, error) {
	env, err := apiSend(ctx, deps, call, http.MethodPost, "/servers/orders/"+url.PathEscape(orderId)+"/checkout",
		map[string]any{}, idempotencyHeaders(checkoutKey))
	if err != nil {
		return nil, fmt.Errorf("the draft order %s was created, but its payment page could not be prepared: %w Retry with the same idempotencyKey: that returns this order and tries the payment page again", orderId, err)
	}
	checkout, err := client.DecodeObject(env)
	if err != nil {
		return nil, err
	}
	link := pick(checkout, "authorizationUrl", "paymentUrl", "checkoutUrl")
	if link == "" {
		return nil, fmt.Errorf("the draft order %s was created but the API returned no payment page for it. Tell the customer, and do not say a page is waiting. Retry with the same idempotencyKey", orderId)
	}
	return &ServerOrderResult{
		Summary:    "Draft server order created and its payment page is ready.",
		OrderID:    orderId,
		Currency:   currencyNaira,
		PaymentURL: link,
		Order:      order,
		NextStep:   "Give the customer this link and ask them to complete the payment themselves. You cannot pay it for them. The server is set up after they pay; check get_invoice or the servers page.",
	}, nil
}

func dollarOrderResult(ctx context.Context, deps *Deps, call *Call, order map[string]any, orderId string, choice *serverChoice, cycle string) *ServerOrderResult {
	out := &ServerOrderResult{
		Summary:           "Draft server order created. The customer confirms the payment from their wallet on the portal.",
		OrderID:           orderId,
		Currency:          currencyDollar,
		PortalCheckoutURL: pick(order, "portalCheckoutUrl"),
		Order:             order,
	}
	if out.PortalCheckoutURL == "" {
		out.Summary = "Draft server order created, but the API gave no portal link for it."
		out.NextStep = "Tell the customer plainly that the order exists (give the orderId) and that you could not produce the link where they confirm the payment. Do not invent a link. They can start the order again on the portal."
		return out
	}

	if env, err := apiSend(ctx, deps, call, http.MethodPost, "/servers/orders/quote", serverQuoteBody(choice, cycle, currencyDollar), nil); err == nil {
		if quote, derr := client.DecodeObject(env); derr == nil {
			applyWalletQuote(out, quote)
		}
	}

	switch {
	case out.WalletCoversOrder != nil && *out.WalletCoversOrder:
		out.NextStep = "Give the customer the portalCheckoutUrl and tell them the wallet covers this order and they confirm the payment there themselves. You cannot pay it for them."
	case out.WalletCoversOrder != nil:
		out.NextStep = "The wallet balance does not cover this order. Say by how much from shortfallMinor (cents). " + topUpAdvice(out) +
			" Then give the customer the portalCheckoutUrl, where they confirm the payment themselves. You cannot pay it for them."
	default:
		out.NextStep = "Give the customer the portalCheckoutUrl and tell them they confirm the payment there themselves; the portal shows whether their wallet covers it. You could not read the balance, so do not say whether it does."
	}
	return out
}

func topUpAdvice(out *ServerOrderResult) string {
	if out.SuggestedTopUpMinor != nil {
		return "Offer create_wallet_topup_link with amountMinor set to suggestedTopUpMinor, and give them that link first."
	}
	return "One top-up cannot cover it, so tell the customer they need to add funds more than once, with create_wallet_topup_link."
}

func applyWalletQuote(out *ServerOrderResult, quote map[string]any) {
	if _, hasBalance := quote["walletAvailableMinor"].(float64); !hasBalance {
		return
	}
	shortfall, _ := quote["shortfallMinor"].(float64)
	covers := shortfall <= 0
	out.WalletCoversOrder = &covers
	if !covers {
		s := int64(shortfall)
		out.ShortfallMinor = &s
		if suggested, ok := quote["suggestedTopUpMinor"].(float64); ok {
			v := int64(suggested)
			out.SuggestedTopUpMinor = &v
		}
	}
}
