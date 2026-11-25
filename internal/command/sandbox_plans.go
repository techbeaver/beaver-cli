package command

import (
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
)

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
