package command

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
	"github.com/techbeaver/beaver-cli/internal/sandboxerr"
)

func newSandboxAddSlotsCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "add-slots", Short: "Price adding slots to a subscription"}
	var slots int
	quote := &cobra.Command{
		Use:   "quote SUBSCRIPTION_ID",
		Short: "Price adding slots to a subscription for the rest of its period",
		Long:  "Works out the prorated cost of adding slots now. It creates nothing and charges nothing.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuidArg("subscription id", args[0])
			if err != nil {
				return err
			}
			if slots < 1 {
				return usageErr("--slots must be a whole number of at least 1, got %d", slots)
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{
				Method: http.MethodPost, Path: "/sandbox/subscriptions/" + url.PathEscape(id) + "/add-slots/quote",
				Body: map[string]any{"slots": slots},
			})
			if err != nil {
				return err
			}
			obj, err := sandboxObject(envelope)
			if err != nil {
				return err
			}
			spec, human, err := env.outputSpec()
			if err != nil {
				return err
			}
			if !human {
				return output.New(env.Out, spec).Object(obj)
			}
			cur := text(obj["currency"])
			return printPairs(env.Out, []pair{
				{"Plan", text(obj["planName"])},
				{"Slots added", text(obj["slots"])},
				{"Days charged", text(obj["daysCharged"])},
				{"Subtotal", formatMinor(obj["subtotalMinor"], cur)},
				{"Tax", formatMinor(obj["taxMinor"], cur)},
				{"Total", formatMinor(obj["totalMinor"], cur)},
				{"Period", text(obj["periodStart"]) + " to " + text(obj["periodEnd"])},
				{"Wallet available", formatMinor(obj["walletAvailableMinor"], cur)},
			})
		},
	}
	quote.Flags().IntVar(&slots, "slots", 0, "how many slots to add, at least 1")
	cmd.AddCommand(quote)
	return cmd
}

type purchaseFlags struct {
	plan  string
	slots int
}

func (f *purchaseFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.plan, "plan", "", "sandbox plan id (see beaver sandbox plans)")
	cmd.Flags().IntVar(&f.slots, "slots", 0, "how many slots, at least 1")
}

func (f *purchaseFlags) body(project string) (map[string]any, error) {
	if f.plan == "" {
		return nil, usageErr("--plan is required (see beaver sandbox plans)")
	}
	if !uuidPattern.MatchString(f.plan) {
		return nil, usageErr("--plan must be a plan id from beaver sandbox plans, got %q", f.plan)
	}
	if f.slots < 1 {
		return nil, usageErr("--slots must be a whole number of at least 1, got %d", f.slots)
	}
	return map[string]any{"projectId": project, "planId": f.plan, "slots": f.slots}, nil
}

func newSandboxQuoteCommand(env *Env) *cobra.Command {
	var flags purchaseFlags
	cmd := &cobra.Command{
		Use:   "quote",
		Short: "Price sandbox slots before buying them",
		Long:  "Works out what a month of slots costs your account, tax included. It creates nothing and charges nothing.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			project, err := sandboxProject(s)
			if err != nil {
				return err
			}
			body, err := flags.body(project)
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodPost, Path: "/sandbox/billing/quote", Body: body})
			if err != nil {
				return err
			}
			obj, err := sandboxObject(envelope)
			if err != nil {
				return err
			}
			spec, human, err := env.outputSpec()
			if err != nil {
				return err
			}
			if !human {
				return output.New(env.Out, spec).Object(obj)
			}
			cur := text(obj["currency"])
			return printPairs(env.Out, []pair{
				{"Plan", text(obj["planName"])},
				{"Slots", text(obj["slots"])},
				{"Per slot, monthly", formatMinor(obj["unitMonthlyMinor"], cur)},
				{"Per slot, hourly", formatMinor(obj["hourlyMinor"], cur)},
				{"Subtotal", formatMinor(obj["subtotalMinor"], cur)},
				{"Tax", formatMinor(obj["taxMinor"], cur)},
				{"Total", formatMinor(obj["totalMinor"], cur)},
				{"Period", text(obj["periodStart"]) + " to " + text(obj["periodEnd"])},
				{"Wallet available", formatMinor(obj["walletAvailableMinor"], cur)},
			})
		},
	}
	flags.bind(cmd)
	return cmd
}

func firstText(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func newSandboxBuyCommand(env *Env) *cobra.Command {
	var (
		flags      purchaseFlags
		successURL string
		key        string
	)
	cmd := &cobra.Command{
		Use:   "buy",
		Short: "Get a payment link for a month of sandbox slots",
		Long: "Prepares the purchase and prints a payment link. beaver cannot pay: you open the link and\n" +
			"pay yourself, and nothing is bought until you do. Run beaver sandbox quote first to see the price.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			project, err := sandboxProject(s)
			if err != nil {
				return err
			}
			body, err := flags.body(project)
			if err != nil {
				return err
			}
			body["paymentSource"] = "checkout"
			if successURL != "" {
				body["successUrl"] = successURL
			}
			envelope, err := sendCharge(cmd.Context(), env, s, client.Request{
				Method: http.MethodPost, Path: "/sandbox/billing/purchase", Body: body,
			}, key)
			if err != nil {
				return sandboxerr.Describe(err)
			}
			obj, err := sandboxObject(envelope)
			if err != nil {
				return err
			}
			announceCheckout(env, obj)
			spec, human, err := env.outputSpec()
			if err != nil {
				return err
			}
			if !human {
				return output.New(env.Out, spec).Object(obj)
			}
			cur := text(obj["currency"])
			return printPairs(env.Out, []pair{
				{"Status", text(obj["status"])},
				{"Total", formatMinor(obj["totalMinor"], cur)},
				{"Invoice", text(obj["invoiceNumber"])},
				{"Short by", formatMinor(obj["shortfallMinor"], cur)},
				{"Wallet available", formatMinor(obj["walletAvailableMinor"], cur)},
				{"Message", text(obj["message"])},
			})
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&successURL, "success-url", "", "where the payment page sends you after paying")
	addIdempotencyKeyFlag(cmd, &key)
	return cmd
}

// announceCheckout tells the customer what the API returned. A dollar customer with a short wallet gets
// status needs_topup with the top-up page in paymentUrl: the purchase is paid from the wallet by itself
// once the top-up lands, so there is nothing to run again.
func announceCheckout(env *Env, obj map[string]any) {
	const cannotPay = "beaver cannot pay for you. Nothing is bought until you complete the payment yourself.\n"
	link := firstText(obj, "paymentUrl")
	switch {
	case link != "" && text(obj["status"]) == "needs_topup":
		fmt.Fprintf(env.Err, "\nYour wallet is short. Open this to top it up; the purchase is paid from the wallet when the top-up lands:\n\n  %s\n\n%s", link, cannotPay)
	case link != "":
		fmt.Fprintf(env.Err, "\nOpen this to pay:\n\n  %s\n\n%s", link, cannotPay)
	case text(obj["status"]) == "paid":
		fmt.Fprintf(env.Err, "\nThe server reports this purchase as paid, and no payment link is needed. beaver did not pay it. Check beaver sandbox usage.\n")
	default:
		fmt.Fprintf(env.Err, "\nThe server returned no payment link, so nothing is waiting to be paid. Read the message in the result.\n%s", cannotPay)
	}
}
