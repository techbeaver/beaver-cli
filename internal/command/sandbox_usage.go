package command

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
)

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
			"billedHours": u["billedHours"],
		}
		if est, ok := u["estimatedAmountMinor"]; ok && est != nil {
			row["estimated"] = formatMinor(est, currency)
		}
		rows = append(rows, row)
	}
	return w.List(rows, "name", "status", "billingMode", "minutes", "billedHours", "estimated")
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
