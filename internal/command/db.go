package command

import (
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
)

func newDBCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "db", Aliases: []string{"databases"}, Short: "Work with managed databases"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use: "list", short: "List the managed databases in a project",
			columns: []string{"id", "name", "status", "plan", "version"},
			path: func(s *Session, _ []string) (string, error) {
				project, err := s.Resolved.RequireProject()
				if err != nil {
					return "", err
				}
				return "/dbaas/projects/" + project + "/instances", nil
			},
		}),
		getCommand(env, getSpec{use: "describe ID", short: "Show one database", path: instancePath("")}),
		newDBCreateCommand(env),
		getCommand(env, getSpec{use: "usage ID", short: "Show CPU, memory and disk usage", path: instancePath("/usage")}),
		getCommand(env, getSpec{use: "stats ID", short: "Show live statistics", path: instancePath("/stats")}),
		newDBLogsCommand(env),
		newDBRolesCommand(env),
		newDBDatabasesCommand(env),
		newDBBackupsCommand(env),
		newDBConnectionCommand(env),
		newDBAllowlistCommand(env),
		newDBDeleteCommand(env),
	)
	return cmd
}

func instancePath(suffix string) func(*Session, []string) (string, error) {
	return func(_ *Session, args []string) (string, error) {
		return "/dbaas/instances/" + args[0] + suffix, nil
	}
}

func newDBCreateCommand(env *Env) *cobra.Command {
	var (
		plan    string
		name    string
		version string
	)
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a managed database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			project, err := s.Resolved.RequireProject()
			if err != nil {
				return err
			}
			if name == "" {
				name = args[0]
			}
			body := map[string]any{"name": name}
			if plan != "" {
				body["plan"] = plan
			}
			if version != "" {
				body["version"] = version
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodPost, Path: "/dbaas/projects/" + project + "/instances", Body: body,
			})
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().StringVar(&plan, "plan", "", "plan to create it on (see beaver plans list --db)")
	cmd.Flags().StringVar(&name, "name", "", "name, if it should differ from the argument")
	cmd.Flags().StringVar(&version, "version", "", "Postgres major version")
	return cmd
}

func newDBLogsCommand(env *Env) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs ID",
		Short: "Read the provisioning log",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			req := client.Request{Path: "/dbaas/instances/" + args[0] + "/logs/provision"}
			if follow {
				return s.Client.Stream(cmd.Context(), s.Token, req, func(line string) {
					_, _ = env.Out.Write([]byte(line + "\n"))
				})
			}
			lines, _, err := s.Client.Tail(cmd.Context(), s.Token, req, 200, 10*time.Second)
			if err != nil {
				return err
			}
			for _, line := range lines {
				_, _ = env.Out.Write([]byte(line + "\n"))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep the stream open")
	return cmd
}

func newDBRolesCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "roles", Short: "Work with database roles"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use: "list ID", short: "List roles", args: cobra.ExactArgs(1),
			path: instancePath("/roles"), columns: []string{"id", "name", "canLogin"},
		}),
		actionCommand(env, actionSpec{
			use: "create ID NAME", short: "Create a role", args: cobra.ExactArgs(2),
			path: instancePath("/roles"),
			body: func(args []string) any { return map[string]any{"name": args[1]} },
		}),
		actionCommand(env, actionSpec{
			use: "reset-password ID ROLE_ID", short: "Set a new password on a role",
			args: cobra.ExactArgs(2),
			path: func(_ *Session, args []string) (string, error) {
				return "/dbaas/instances/" + args[0] + "/roles/" + args[1] + "/password", nil
			},
		}),
	)
	return cmd
}

func newDBDatabasesCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "databases", Aliases: []string{"dbs"}, Short: "Work with databases inside an instance"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use: "list ID", short: "List databases", args: cobra.ExactArgs(1),
			path: instancePath("/databases"), columns: []string{"id", "name", "owner"},
		}),
		actionCommand(env, actionSpec{
			use: "create ID NAME", short: "Create a database", args: cobra.ExactArgs(2),
			path: instancePath("/databases"),
			body: func(args []string) any { return map[string]any{"name": args[1]} },
		}),
	)
	return cmd
}

func newDBBackupsCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "backups", Short: "Work with backups"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use: "list ID", short: "List backups", args: cobra.ExactArgs(1),
			path: instancePath("/backups"), columns: []string{"id", "status", "sizeBytes", "createdAt"},
		}),
		actionCommand(env, actionSpec{
			use: "create ID", short: "Take a backup now", args: cobra.ExactArgs(1),
			path: instancePath("/backups"),
		}),
		getCommand(env, getSpec{
			use: "restore-preview ID", short: "Show what a restore would do",
			path: instancePath("/restore-preview"),
		}),
		newDBRestoreCommand(env),
	)
	return cmd
}

// newDBRestoreCommand is destructive: a restore writes over live data.
func newDBRestoreCommand(env *Env) *cobra.Command {
	var (
		backupID  string
		assumeYes bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "restore ID",
		Short: "Restore over this database's current data",
		Long:  "Restoring replaces live data, so it needs your approval in a browser.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			body := map[string]any{}
			if backupID != "" {
				body["backupId"] = backupID
			}
			envelope, err := RunDestructive(cmd.Context(), env, s, DestructiveRequest{
				Method: http.MethodPost, Path: "/dbaas/instances/" + args[0] + "/restore", Body: body,
				Action: "restore over", ResourceType: "database", ResourceID: args[0], ResourceName: args[0],
				Preview:      map[string]any{"backup": backupID},
				RecoveryNote: "Restoring writes over the data that is there now.",
			}, assumeYes, wait)
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().StringVar(&backupID, "backup", "", "which backup to restore")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip this CLI's prompt (the browser approval still applies)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "how long to wait for approval before exiting 6")
	return cmd
}

func newDBConnectionCommand(env *Env) *cobra.Command {
	return getCommand(env, getSpec{
		use:   "connection-string ID DATABASE_ID",
		short: "Show how to connect to a database",
		args:  cobra.ExactArgs(2),
		path: func(_ *Session, args []string) (string, error) {
			return "/dbaas/instances/" + args[0] + "/databases/" + args[1] + "/connection-info", nil
		},
	})
}

func newDBAllowlistCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "allowlist", Short: "Work with the IP allowlist"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use: "list ID", short: "List allowed addresses", args: cobra.ExactArgs(1),
			path: instancePath("/ip-allowlist"), columns: []string{"id", "cidr", "description"},
		}),
		actionCommand(env, actionSpec{
			use: "add ID CIDR", short: "Allow an address range", args: cobra.ExactArgs(2),
			path: instancePath("/ip-allowlist"),
			body: func(args []string) any { return map[string]any{"cidr": args[1]} },
		}),
	)
	return cmd
}

func newDBDeleteCommand(env *Env) *cobra.Command {
	var (
		assumeYes bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "delete ID",
		Short: "Delete a managed database",
		Long:  "Deleting needs your approval in a browser. --yes skips this CLI's prompt, not that approval.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := RunDestructive(cmd.Context(), env, s, DestructiveRequest{
				Method: http.MethodDelete, Path: "/dbaas/instances/" + args[0],
				Action: "delete", ResourceType: "database", ResourceID: args[0], ResourceName: args[0],
				RecoveryNote: "Deleting a managed database removes it and its data.",
			}, assumeYes, wait)
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip this CLI's prompt (the browser approval still applies)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "how long to wait for approval before exiting 6")
	return cmd
}
