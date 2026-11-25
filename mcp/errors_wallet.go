package mcp

import (
	"fmt"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
)

func describeWalletCode(apiErr *client.APIError) (error, bool) {
	switch normaliseCode(apiErr.Code) {
	case "wallet_not_available":
		return fmt.Errorf("the dollar wallet is not open for this account. Either the account is billed in naira, or the wallet has not been switched on for it. Tell the customer and stop. For a naira account use create_topup_link instead"), true

	case "wallet_wrong_billing_track":
		return fmt.Errorf("this account is billed in naira, so the dollar wallet is not for it. Use create_topup_link to add to the naira prepaid balance"), true

	case "amount_out_of_range", "amount_not_on_step":
		return fmt.Errorf("that amount is not allowed for a top-up (%s). %s Ask the customer for an amount inside those limits and call again with a new idempotencyKey", apiErr.Message, walletLimits(apiErr.Data)), true

	case "wallet_org_role_required":
		return fmt.Errorf("only an organisation owner or admin may add money to the organisation's wallet. Tell the customer to ask one of them, or to do it themselves on the portal. Do not retry"), true

	case "wallet_consent_requires_human":
		return fmt.Errorf("saving a card for automatic top-ups, and tying a top-up to an order, can only be done by the account owner on the portal. Tell the customer and stop"), true

	case "wallet_spend_requires_human":
		return fmt.Errorf("paying from the dollar wallet can only be done by the account owner on the portal. Give them the order's portal checkout link and stop. You cannot pay it for them"), true

	case "wallet_frozen":
		return fmt.Errorf("this wallet is frozen. Only support can lift that. Tell the customer to contact support and stop"), true

	case "wallet_topup_rate_limited":
		return fmt.Errorf("too many top-ups have been started for this account in a short time. Wait a few minutes before trying again"), true

	case "topup_in_progress":
		return fmt.Errorf("a top-up for this account is already being prepared. Wait a few seconds and call again with the same idempotencyKey"), true

	case "card_payments_unavailable", "stripe_unavailable", "wallet_not_ready", "portal_url_not_configured", "order_check_unavailable":
		return fmt.Errorf("card payments are not available right now (%s). This is on TechBeaver's side. Tell the customer to try again later; do not retry in a loop", apiErr.Message), true

	case "plan_not_available_in_usd", "windows_not_available_in_usd":
		return fmt.Errorf("that plan or operating system cannot be bought in dollars. Call list_server_plans again and choose one it lists"), true

	case "billing_country_changed", "order_not_payable", "order_not_restartable":
		return fmt.Errorf("%s Start the order again with list_server_plans and order_server, using a new idempotencyKey", apiErr.Message), true

	case "currency_mismatch":
		return fmt.Errorf("the account's billing currency changed or was misread (%s). Call list_server_plans again to see the currency it is billed in and start the order again with a new idempotencyKey", apiErr.Message), true

	case "spec_mismatch":
		return fmt.Errorf("the plan's hardware differs from what was sent (%s). Call list_server_plans again and order from what it returns", apiErr.Message), true
	}
	return nil, false
}

func walletLimits(data map[string]any) string {
	min, okMin := data["minMinor"].(float64)
	max, okMax := data["maxMinor"].(float64)
	step, okStep := data["stepMinor"].(float64)
	if !okMin || !okMax {
		return "Read the limits from the customer or retry with an amount inside the account's range."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The limits right now are a minimum of %.0f and a maximum of %.0f", min, max)
	if okStep && step > 0 {
		fmt.Fprintf(&b, ", in multiples of %.0f", step)
	}
	b.WriteString(", all in cents.")
	return b.String()
}
