package command

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
)

const (
	statementsMaxLimit = 100
	openStatementNote  = "An open statement lists the windows settled so far. Hours still running are not on it: see beaver sandbox usage."
)

func newSandboxStatementsCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "statements",
		Aliases: []string{"statement"},
		Short:   "Monthly statements of by-the-hour sandbox use",
		Long: "Read only. A statement summarises one month of by-the-hour use for the owner of the selected project,\n" +
			"one per currency. It is a summary, never an invoice: each charge on it was already invoiced and settled\n" +
			"as it happened. Needs the billing:read scope and a project.",
	}
	cmd.AddCommand(newStatementsListCommand(env), newStatementsCurrentCommand(env), newStatementsShowCommand(env))
	return cmd
}

func statementsGet(env *Env, cmd *cobra.Command, path string, extra url.Values) (map[string]any, error) {
	s, err := env.Authenticate(cmd.Context())
	if err != nil {
		return nil, err
	}
	project, err := sandboxProject(s)
	if err != nil {
		return nil, err
	}
	q := url.Values{"projectId": {project}}
	for k, v := range extra {
		q[k] = v
	}
	envelope, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodGet, Path: path, Query: q})
	if err != nil {
		return nil, err
	}
	return sandboxObject(envelope)
}

func newStatementsListCommand(env *Env) *cobra.Command {
	var page, limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the statements, newest month first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if cmd.Flags().Changed("page") {
				if page < 1 {
					return usageErr("--page counts from 1, got %d", page)
				}
				q.Set("page", strconv.Itoa(page))
			}
			if cmd.Flags().Changed("limit") {
				if limit < 1 || limit > statementsMaxLimit {
					return usageErr("--limit is between 1 and %d, got %d", statementsMaxLimit, limit)
				}
				q.Set("limit", strconv.Itoa(limit))
			}
			obj, err := statementsGet(env, cmd, "/sandbox/statements", q)
			if err != nil {
				return err
			}
			items := asList(obj["items"])
			spec, human, err := env.outputSpec()
			if err != nil {
				return err
			}
			w := output.New(env.Out, spec)
			if shown, total := len(items), obj["total"]; total != nil {
				if n, ok := minorInt(total); ok && n > int64(shown) {
					fmt.Fprintf(env.Err, "Showing %d of %d statements (page %s). Use --page and --limit for the rest.\n", shown, n, orText(text(obj["page"]), "1"))
				}
			}
			if !human {
				return w.List(items)
			}
			if len(items) == 0 {
				fmt.Fprintln(env.Out, "No statements yet. They appear once by-the-hour use has been charged.")
				return nil
			}
			return w.List(statementRows(items), statementColumns...)
		},
	}
	cmd.Flags().IntVar(&page, "page", 0, "page number, from 1")
	cmd.Flags().IntVar(&limit, "limit", 0, "page size, 1 to 100 (the API's default is 20)")
	return cmd
}

var statementColumns = []string{"id", "number", "month", "status", "sandboxes", "total", "paymentState", "unpaid", "net"}

func statementRows(items []map[string]any) []map[string]any {
	rows := make([]map[string]any, 0, len(items))
	for _, st := range items {
		cur := text(st["currency"])
		rows = append(rows, map[string]any{
			"id": st["id"], "number": st["number"], "month": st["month"], "status": st["status"],
			"sandboxes": st["chargeCount"], "total": formatMinor(st["totalMinor"], cur),
			"paymentState": st["paymentState"], "unpaid": formatMinor(st["unpaidMinor"], cur),
			"net": formatMinor(st["netTotalMinor"], cur),
		})
	}
	return rows
}

func newStatementsCurrentCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Show the month so far, with line items",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			obj, err := statementsGet(env, cmd, "/sandbox/statements/current", nil)
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
			if err := printPairs(env.Out, []pair{{"Month", text(obj["month"])}, {"Period", text(obj["periodStart"]) + " to " + text(obj["periodEnd"])}}); err != nil {
				return err
			}
			details := asList(obj["statements"])
			if len(details) == 0 {
				fmt.Fprintln(env.Out, "\nNothing has been settled this month yet.")
				return nil
			}
			for _, d := range details {
				fmt.Fprintln(env.Out)
				if err := renderStatement(env, spec, d); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newStatementsShowCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:     "show ID",
		Aliases: []string{"get"},
		Short:   "Show one statement with its line items",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuidArg("statement id", args[0])
			if err != nil {
				return err
			}
			obj, err := statementsGet(env, cmd, "/sandbox/statements/"+url.PathEscape(id), nil)
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
			return renderStatement(env, spec, obj)
		},
	}
}

func renderStatement(env *Env, spec output.Spec, st map[string]any) error {
	cur := text(st["currency"])
	pairs := []pair{
		{"Statement", text(st["number"])}, {"Month", text(st["month"])}, {"Status", text(st["status"])},
		{"Subtotal", formatMinor(st["subtotalMinor"], cur)}, {"Discount", formatMinor(st["discountMinor"], cur)},
		{"Tax", formatMinor(st["taxMinor"], cur)}, {"Total", formatMinor(st["totalMinor"], cur)},
		{"Paid", formatMinor(st["paidMinor"], cur)}, {"Unpaid", formatMinor(st["unpaidMinor"], cur)},
		{"Adjustments", formatMinor(st["adjustmentsMinor"], cur)}, {"Net total", formatMinor(st["netTotalMinor"], cur)},
		{"Payment", text(st["paymentState"])}, {"As of", text(st["asOf"])},
	}
	if err := printPairs(env.Out, pairs); err != nil {
		return err
	}
	w := output.New(env.Out, spec)
	if lines := asList(st["sandboxes"]); len(lines) > 0 {
		rows := make([]map[string]any, 0, len(lines))
		for _, l := range lines {
			rows = append(rows, map[string]any{
				"name": l["name"], "plan": l["planName"], "hourly": formatMinor(l["hourlyMinor"], cur),
				"hours": l["hours"], "amount": formatMinor(l["amountMinor"], cur),
			})
		}
		fmt.Fprintln(env.Out, "\nSandboxes")
		if err := w.List(rows, "name", "plan", "hourly", "hours", "amount"); err != nil {
			return err
		}
	}
	if err := renderStatementCharges(env, w, st, cur); err != nil {
		return err
	}
	if st["status"] == "open" {
		fmt.Fprintln(env.Out, "\n"+openStatementNote)
	}
	return nil
}

func renderStatementCharges(env *Env, w *output.Writer, st map[string]any, cur string) error {
	if charges := asList(st["charges"]); len(charges) > 0 {
		rows := make([]map[string]any, 0, len(charges))
		for _, c := range charges {
			rows = append(rows, map[string]any{
				"invoice": c["invoiceNumber"], "state": c["invoiceStatus"], "from": c["periodStart"], "to": c["periodEnd"],
				"hours": c["hours"], "total": formatMinor(c["totalMinor"], cur),
			})
		}
		fmt.Fprintln(env.Out, "\nCharges")
		if err := w.List(rows, "invoice", "state", "from", "to", "hours", "total"); err != nil {
			return err
		}
	}
	if adj := asList(st["adjustments"]); len(adj) > 0 {
		rows := make([]map[string]any, 0, len(adj))
		for _, a := range adj {
			rows = append(rows, map[string]any{"invoice": a["invoiceNumber"], "reason": a["reason"], "amount": formatMinor(a["amountMinor"], cur)})
		}
		fmt.Fprintln(env.Out, "\nAdjustments")
		return w.List(rows, "invoice", "reason", "amount")
	}
	return nil
}
