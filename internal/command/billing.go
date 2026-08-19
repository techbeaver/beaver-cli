package command

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
)

func newBillingCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "billing", Short: "Invoices, payments and credit"}
	cmd.AddCommand(newInvoicesCommand(env), newReferralsCommand(env))
	return cmd
}

func newInvoicesCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "invoices", Aliases: []string{"invoice"}, Short: "Work with invoices"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use: "list", short: "List your invoices",
			path:    func(*Session, []string) (string, error) { return "/billing/invoices", nil },
			columns: []string{"id", "reference", "status", "total", "currency", "dueDate"},
		}),
		getCommand(env, getSpec{
			use: "describe ID", short: "Show one invoice",
			path: func(_ *Session, args []string) (string, error) { return "/billing/invoices/" + args[0], nil },
		}),
		newInvoicePayCommand(env),
	)
	return cmd
}

// newInvoicePayCommand asks the platform to prepare a payment. It cannot
// complete one: that happens on the payment page, by a person.
func newInvoicePayCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "pay ID",
		Short: "Prepare a payment for an invoice",
		Long: "Produces a payment link. It cannot complete a payment: only you can, on the\n" +
			"payment page. An invoice already covered by credit may settle with no link.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodPost, Path: "/billing/invoices/" + args[0] + "/pay",
			})
			if err != nil {
				return err
			}
			obj, err := client.DecodeObject(envelope)
			if err != nil {
				return err
			}
			if url := str(obj["authorizationUrl"]); url != "" && env.IsTTY {
				fmt.Fprintf(env.Err, "\nOpen this to pay:\n\n  %s\n\n", url)
			}
			w, err := env.Writer()
			if err != nil {
				return err
			}
			return w.Object(obj)
		},
	}
}

func newReferralsCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "credit", Short: "Referral credit"}
	cmd.AddCommand(
		getCommand(env, getSpec{
			use: "summary", short: "Show your credit balance", args: cobra.NoArgs,
			path: func(*Session, []string) (string, error) { return "/referrals/summary", nil },
		}),
		listCommand(env, listSpec{
			use: "ledger", short: "Show credit movements",
			path:    func(*Session, []string) (string, error) { return "/referrals/ledger", nil },
			columns: []string{"createdAt", "type", "amount", "description"},
		}),
	)
	return cmd
}
