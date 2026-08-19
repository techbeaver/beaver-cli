package command

import (
	"github.com/spf13/cobra"
)

// catalogCommands are the read-only lookups that are not about one resource a
// customer owns. They sit at the top level because that is how they are reached
// in conversation: "what plans are there", not "what is in the catalog".
func catalogCommands(env *Env) []*cobra.Command {
	return []*cobra.Command{
		withListAlias(newPlansCommand(env)),
		withListAlias(listCommand(env, listSpec{
			use: "regions", short: "List the regions you can deploy to",
			path:    func(*Session, []string) (string, error) { return "/paas/regions", nil },
			columns: []string{"id", "name", "code", "isActive"},
		})),
		listCommand(env, listSpec{
			use: "status", short: "Show which services are currently accepting new work",
			path:    func(*Session, []string) (string, error) { return "/service-gates", nil },
			columns: []string{"service", "paused", "message"},
		}),
	}
}

func newPlansCommand(env *Env) *cobra.Command {
	var forDB bool
	cmd := listCommand(env, listSpec{
		use:     "plans",
		short:   "List the plans you can buy",
		columns: []string{"id", "name", "price", "currency", "cpu", "memory", "storage"},
		path: func(_ *Session, _ []string) (string, error) {
			if forDB {
				return "/dbaas/plans", nil
			}
			return "/paas/plans", nil
		},
	})
	cmd.Flags().BoolVar(&forDB, "db", false, "list managed database plans instead of application plans")
	return cmd
}
