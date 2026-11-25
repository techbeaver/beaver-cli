package command

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
)

var subscriptionColumns = []string{"id", "plan", "mode", "status", "slots", "inUse", "perSlotMonthly", "nextCharge", "autoRenew", "hourlyAutoTopUp", "periodEnd"}

func subscriptionRows(subs []map[string]any) []map[string]any {
	rows := make([]map[string]any, 0, len(subs))
	for _, sub := range subs {
		rows = append(rows, subscriptionDisplay(sub))
	}
	return rows
}

func subscriptionDisplay(sub map[string]any) map[string]any {
	cur := text(sub["currency"])
	row := map[string]any{
		"id": sub["id"], "plan": sub["planName"], "mode": sub["billingMode"], "status": sub["status"],
		"slots": sub["slots"], "inUse": sub["slotsInUse"],
		"perSlotMonthly": formatMinor(sub["unitMonthlyMinor"], cur),
		"hourlyPrice":    formatMinor(sub["hourlyMinor"], cur),
		"nextCharge":     formatMinor(sub["nextChargeMinor"], cur),
		"periodEnd":      sub["currentPeriodEnd"],
		"periodStart":    sub["periodStart"],
	}
	if v := sub["pendingSlots"]; v != nil {
		row["pendingSlots"] = v
	}
	if sub["cancelAtPeriodEnd"] == true {
		row["cancelAtPeriodEnd"] = "yes"
	}
	for _, key := range []string{"renewalInvoiceId", "suspendedAt", "destroyAfter", "nextChargeSlots"} {
		if v := sub[key]; v != nil {
			row[key] = v
		}
	}
	addAutoPayFields(row, sub)
	return row
}

// addAutoPayFields shows the two switches and the card top-up rule only when the API sent them:
// autoRenew belongs to a monthly subscription, hourlyAutoTopUp and hourlyTopUp to an hourly one. An
// absent field stays absent, so a missing switch is never shown as off.
func addAutoPayFields(row, sub map[string]any) {
	if on, ok := sub["autoRenew"].(bool); ok {
		row["autoRenew"] = onOff(on)
	}
	if on, ok := sub["hourlyAutoTopUp"].(bool); ok {
		row["hourlyAutoTopUp"] = onOff(on)
	}
	if rule := asMap(sub["hourlyTopUp"]); rule != nil {
		row["hourlyTopUp"] = topUpRuleText(rule)
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// topUpRuleText states the rule exactly as the API gave it, so the CLI holds no amount of its own.
func topUpRuleText(rule map[string]any) string {
	amount := formatMinor(rule["amountMinor"], text(rule["currency"]))
	if amount == "" {
		return ""
	}
	out := amount + " per top-up"
	if max, ok := minorInt(rule["max"]); ok {
		out += ", at most " + strconv.FormatInt(max, 10)
		if hours, ok := minorInt(rule["windowHours"]); ok {
			out += " in " + strconv.FormatInt(hours, 10) + " hours"
		}
	}
	return out
}

func newSandboxSubscriptionsCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:     "subscriptions [ID]",
		Aliases: []string{"subscription"},
		Short:   "List a project's sandbox subscriptions, or show one",
		Long: "Shows slots bought and in use, price and next charge, and whether auto-renew (monthly) or the card top-up\n" +
			"for by-the-hour use is on. Listing needs a project; showing one needs only its id.\n" +
			"Turning auto-renew or the top-up on or off is not possible from beaver: use the portal.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			spec, human, err := env.outputSpec()
			if err != nil {
				return err
			}
			w := output.New(env.Out, spec)
			if len(args) == 1 {
				id, err := uuidArg("subscription id", args[0])
				if err != nil {
					return err
				}
				envelope, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodGet, Path: "/sandbox/subscriptions/" + url.PathEscape(id)})
				if err != nil {
					return err
				}
				sub, err := sandboxObject(envelope)
				if err != nil {
					return err
				}
				if !human {
					return w.Object(sub)
				}
				return w.Object(subscriptionDisplay(sub))
			}
			project, err := sandboxProject(s)
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{
				Method: http.MethodGet, Path: "/sandbox/subscriptions", Query: url.Values{"projectId": {project}},
			})
			if err != nil {
				return err
			}
			subs, err := sandboxList(envelope)
			if err != nil {
				return err
			}
			if !human {
				return w.List(subs)
			}
			return w.List(subscriptionRows(subs), subscriptionColumns...)
		},
	}
}
