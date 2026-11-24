package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
	"github.com/techbeaver/beaver-cli/internal/sandboxerr"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func newSandboxCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sandbox",
		Aliases: []string{"sandboxes"},
		Short:   "Sandbox plans, usage, quotes and buying slots",
		Long: "Run and manage sandboxes, and get a payment link for slots.\n\n" +
			"beaver cannot pay: buy prints a link that you open and pay yourself.\n\n" +
			"Most of these need sandbox scopes that are not in the default sign in. Grant them in the agent\n" +
			"connection's permissions, or run beaver auth login --scope sandbox:read,sandbox:write.\n" +
			"sandbox:exec runs commands, sandbox:files moves files, sandbox:destroy asks for deletion.\n" +
			"Destroying always needs your approval in a browser.",
	}
	cmd.AddCommand(
		newSandboxPlansCommand(env),
		newSandboxUsageCommand(env),
		newSandboxSubscriptionsCommand(env),
		newSandboxAddSlotsCommand(env),
		newSandboxQuoteCommand(env),
		newSandboxBuyCommand(env),
		newSandboxListCommand(env),
		newSandboxGetCommand(env),
		newSandboxCreateCommand(env),
		newSandboxTransitionCommand(env, "start", "Start a stopped sandbox"),
		newSandboxTransitionCommand(env, "stop", "Stop a sandbox and keep its disk"),
		newSandboxDestroyCommand(env),
		newSandboxExecCommand(env),
		newSandboxCopyCommand(env),
		newSandboxLsCommand(env),
		newSandboxMkdirCommand(env),
	)
	return cmd
}

func sandboxProject(s *Session) (string, error) {
	project, err := s.Resolved.RequireProject()
	if err != nil {
		return "", err
	}
	if !uuidPattern.MatchString(project) {
		return "", usageErr("sandbox commands need the project id, not its name (see beaver projects list), got %q", project)
	}
	return project, nil
}

// retryWait is how a rate limit pause is slept, replaceable in tests.
var retryWait = sleepCtx

// maxAutoRetryWait is the longest Retry-After worth waiting out inside one command.
const maxAutoRetryWait = 10 * time.Second

// retryOnce429 repeats a call one time when the server asked for a short wait.
func retryOnce429[T any](ctx context.Context, env *Env, do func() (T, error)) (T, error) {
	out, err := do()
	var apiErr *client.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests ||
		apiErr.RetryAfter <= 0 || apiErr.RetryAfter > maxAutoRetryWait {
		return out, err
	}
	fmt.Fprintf(env.Err, "Rate limited. Trying once more in %d seconds.\n", int(apiErr.RetryAfter.Seconds()))
	if !retryWait(ctx, apiErr.RetryAfter) {
		return out, err
	}
	return do()
}

func sandboxDo(env *Env, s *Session, cmd *cobra.Command, req client.Request) (*client.Envelope, error) {
	envelope, err := retryOnce429(cmd.Context(), env, func() (*client.Envelope, error) {
		return s.Client.Do(cmd.Context(), s.Token, req)
	})
	if err != nil {
		return nil, sandboxerr.Describe(err)
	}
	return envelope, nil
}

func decodeSandbox(envelope *client.Envelope) (any, error) {
	if envelope == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(envelope.Data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, sandboxerr.Describe(fmt.Errorf("%w: %v", sandboxerr.ErrMalformed, err))
	}
	return v, nil
}

func sandboxObject(envelope *client.Envelope) (map[string]any, error) {
	v, err := decodeSandbox(envelope)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, sandboxerr.Describe(fmt.Errorf("%w: expected an object", sandboxerr.ErrMalformed))
	}
	return obj, nil
}

func sandboxList(envelope *client.Envelope) ([]map[string]any, error) {
	v, err := decodeSandbox(envelope)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	if obj, isObj := v.(map[string]any); isObj {
		for _, key := range []string{"items", "data", "results", "rows", "records"} {
			if nested, ok := obj[key].([]any); ok {
				v = nested
				break
			}
		}
		if _, still := v.(map[string]any); still {
			return []map[string]any{obj}, nil
		}
	}
	items, ok := v.([]any)
	if !ok {
		return nil, sandboxerr.Describe(fmt.Errorf("%w: expected a list", sandboxerr.ErrMalformed))
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		obj, isObj := item.(map[string]any)
		if !isObj {
			return nil, sandboxerr.Describe(fmt.Errorf("%w: a list entry was not an object", sandboxerr.ErrMalformed))
		}
		out = append(out, obj)
	}
	return out, nil
}

// outputSpec reports the format for this run and whether it is the human one.
func (e *Env) outputSpec() (output.Spec, bool, error) {
	spec, err := output.ParseFormat(pick(e.flags.format, e.Format), e.IsTTY)
	if err != nil {
		return spec, false, usageErr("%s", err)
	}
	return spec, spec.Format == output.Table, nil
}

type pair struct{ key, value string }

func printPairs(w io.Writer, pairs []pair) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range pairs {
		if p.value == "" {
			continue
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", p.key, p.value); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func text(v any) string {
	if v == nil {
		return ""
	}
	return scalarText(v)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func newSandboxPlansCommand(env *Env) *cobra.Command {
	var currency string
	cmd := &cobra.Command{
		Use:   "plans",
		Short: "List the sandbox plans on sale, with prices",
		Long: "Lists the sandbox plans on sale with a price per currency. Prices are read live.\n" +
			"For what a purchase will cost your account, use beaver sandbox quote.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodGet, Path: "/sandbox/plans"})
			if err != nil {
				return err
			}
			plans, err := sandboxList(envelope)
			if err != nil {
				return err
			}
			plans = filterPlanCurrency(plans, currency)
			spec, human, err := env.outputSpec()
			if err != nil {
				return err
			}
			w := output.New(env.Out, spec)
			if !human {
				return w.List(plans)
			}
			return w.List(planRows(plans), "id", "name", "code", "vcpu", "memoryMb", "diskGb", "currency", "monthly", "hourly")
		},
	}
	cmd.Flags().StringVar(&currency, "currency", "", "show only prices in this currency, such as NGN or USD")
	return withListAlias(cmd)
}

func filterPlanCurrency(plans []map[string]any, currency string) []map[string]any {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		return plans
	}
	out := make([]map[string]any, 0, len(plans))
	for _, plan := range plans {
		var kept []any
		for _, price := range asList(plan["prices"]) {
			if strings.EqualFold(text(price["currency"]), currency) {
				kept = append(kept, price)
			}
		}
		if len(kept) == 0 {
			continue
		}
		clone := make(map[string]any, len(plan))
		for k, v := range plan {
			clone[k] = v
		}
		clone["prices"] = kept
		out = append(out, clone)
	}
	return out
}

func planRows(plans []map[string]any) []map[string]any {
	rows := make([]map[string]any, 0, len(plans))
	for _, plan := range plans {
		base := map[string]any{
			"id": plan["id"], "name": plan["name"], "code": plan["code"],
			"vcpu": plan["vcpu"], "memoryMb": plan["memoryMb"], "diskGb": plan["diskGb"],
		}
		prices := asList(plan["prices"])
		if len(prices) == 0 {
			rows = append(rows, base)
			continue
		}
		for _, price := range prices {
			row := make(map[string]any, len(base)+3)
			for k, v := range base {
				row[k] = v
			}
			cur := text(price["currency"])
			row["currency"] = cur
			row["monthly"] = formatMinor(price["monthlyPriceMinor"], cur)
			row["hourly"] = formatMinor(price["hourlyPriceMinor"], cur)
			rows = append(rows, row)
		}
	}
	return rows
}

func fetchUsage(env *Env, cmd *cobra.Command) (map[string]any, error) {
	s, err := env.Authenticate(cmd.Context())
	if err != nil {
		return nil, err
	}
	project, err := sandboxProject(s)
	if err != nil {
		return nil, err
	}
	envelope, err := sandboxDo(env, s, cmd, client.Request{
		Method: http.MethodGet, Path: "/sandbox/usage", Query: url.Values{"projectId": {project}},
	})
	if err != nil {
		return nil, err
	}
	return sandboxObject(envelope)
}

func newSandboxUsageCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "usage",
		Short: "Show a project's sandbox balance, hourly burn and hours remaining",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			obj, err := fetchUsage(env, cmd)
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
			return renderUsage(env, spec, obj)
		},
	}
}

func renderUsage(env *Env, spec output.Spec, obj map[string]any) error {
	currency := ""
	pairs := []pair{{"Project", text(obj["projectId"])}}
	if obj["balanceHidden"] == true {
		pairs = append(pairs, pair{"Balance", "hidden for your role on this project (only the owner and billing admins see it)"})
	} else {
		bal := asMap(obj["balance"])
		currency = text(bal["currency"])
		switch {
		case bal == nil:
			pairs = append(pairs, pair{"Balance", "not reported"})
		case bal["supported"] == true:
			pairs = append(pairs, pair{"Balance", formatMinor(bal["availableMinor"], currency)})
		default:
			pairs = append(pairs, pair{"Balance", "not available: " + orText(text(bal["reason"]), "no reason given")})
		}
		if burn, ok := obj["hourlyBurnMinor"]; ok {
			pairs = append(pairs, pair{"Hourly burn", formatMinor(burn, currency) + " per hour"})
		}
		if remaining, present := obj["hoursRemaining"]; present {
			line := "none, nothing is running by the hour"
			if remaining != nil {
				line = text(remaining)
				if obj["lowBalance"] == true {
					line += " (low balance)"
				}
			}
			pairs = append(pairs, pair{"Hours remaining", line})
		}
	}
	pairs = append(pairs,
		pair{"Period", text(obj["periodStart"]) + " to " + text(obj["periodEnd"])},
		pair{"Billing rule", text(obj["billingRule"])},
	)
	if err := printPairs(env.Out, pairs); err != nil {
		return err
	}
	w := output.New(env.Out, spec)
	subs := asList(obj["subscriptions"])
	fmt.Fprintln(env.Out, "\nSubscriptions")
	if len(subs) == 0 {
		fmt.Fprintln(env.Out, "None yet. See beaver sandbox plans and beaver sandbox quote.")
	} else if err := w.List(subscriptionRows(subs), subscriptionColumns...); err != nil {
		return err
	}
	fmt.Fprintln(env.Out, "\nSandboxes")
	used := asList(obj["usage"])
	if len(used) == 0 {
		fmt.Fprintln(env.Out, "None used this period.")
		return nil
	}
	rows := make([]map[string]any, 0, len(used))
	for _, u := range used {
		row := map[string]any{
			"name": u["name"], "status": u["status"], "billingMode": u["billingMode"], "minutes": u["minutes"],
		}
		if est, ok := u["estimatedAmountMinor"]; ok && est != nil {
			row["estimated"] = formatMinor(est, currency)
		}
		rows = append(rows, row)
	}
	return w.List(rows, "name", "status", "billingMode", "minutes", "estimated")
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

var subscriptionColumns = []string{"id", "plan", "mode", "status", "slots", "inUse", "perSlotMonthly", "nextCharge", "periodEnd"}

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
	return row
}

func newSandboxSubscriptionsCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:     "subscriptions [ID]",
		Aliases: []string{"subscription"},
		Short:   "List a project's sandbox subscriptions, or show one",
		Long:    "Shows slots bought and in use, price and next charge. Listing needs a project; showing one needs only its id.",
		Args:    cobra.MaximumNArgs(1),
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

func uuidArg(what, raw string) (string, error) {
	if !uuidPattern.MatchString(raw) {
		return "", usageErr("%s must be an id like 11111111-2222-3333-4444-555555555555, got %q", what, raw)
	}
	return raw, nil
}

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

func announceCheckout(env *Env, obj map[string]any) {
	const cannotPay = "beaver cannot pay for you. Nothing is bought until you complete the payment yourself.\n"
	link := firstText(obj, "authorizationUrl", "paymentUrl", "checkoutUrl")
	topUp := firstText(obj, "topUpUrl", "topUpCheckoutUrl", "walletTopUpUrl")
	switch {
	case link != "":
		fmt.Fprintf(env.Err, "\nOpen this to pay:\n\n  %s\n\n%s", link, cannotPay)
	case topUp != "":
		fmt.Fprintf(env.Err, "\nThe wallet has to be topped up before these slots can be bought. Open this to top up:\n\n  %s\n\n%s", topUp, cannotPay)
	default:
		fmt.Fprintf(env.Err, "\nThe server returned no payment link, so nothing is waiting to be paid. Read the message in the result.\n%s", cannotPay)
	}
}
