package command

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
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
				return "/dbaas/projects/" + url.PathEscape(project) + "/instances", nil
			},
		}),
		getCommand(env, getSpec{use: "describe ID", short: "Show one database", path: instancePath("")}),
		newDBCreateCommand(env),
		getCommand(env, getSpec{use: "usage ID", short: "Show CPU, memory and disk usage", path: instancePath("/usage")}),
		getCommand(env, getSpec{use: "stats ID", short: "Show live statistics", path: instancePath("/stats")}),
		newDBLogsCommand(env),
		newDBRolesCommand(env),
		newDBDatabasesCommand(env),
		newDBExtensionsCommand(env),
		newDBBackupsCommand(env),
		newDBConnectionCommand(env),
		newDBAllowlistCommand(env),
		newDBDeleteCommand(env),
	)
	return cmd
}

func instancePath(suffix string) func(*Session, []string) (string, error) {
	return func(_ *Session, args []string) (string, error) {
		return "/dbaas/instances/" + url.PathEscape(args[0]) + suffix, nil
	}
}

func newDBCreateCommand(env *Env) *cobra.Command {
	var (
		plan, name, version, cycle, dbName, username, key string
	)
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a managed database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if plan == "" {
				return usageErr("--plan is required (see beaver plans list --db)")
			}
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
			body := map[string]any{"name": name, "dbaasPlanId": plan}
			putIfSet(body, "engineVersion", version)
			putIfSet(body, "billingCycle", cycle)
			putIfSet(body, "databaseName", dbName)
			putIfSet(body, "username", username)
			envelope, err := sendCharge(cmd.Context(), env, s, client.Request{
				Method: http.MethodPost, Path: "/dbaas/projects/" + url.PathEscape(project) + "/instances", Body: body,
			}, key)
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().StringVar(&plan, "plan", "", "plan id to create it on (see beaver plans list --db)")
	cmd.Flags().StringVar(&name, "name", "", "name, if it should differ from the argument")
	cmd.Flags().StringVar(&version, "version", "", "Postgres major version")
	cmd.Flags().StringVar(&cycle, "billing-cycle", "", "monthly or yearly")
	cmd.Flags().StringVar(&dbName, "database-name", "", "name of the first database inside the instance")
	cmd.Flags().StringVar(&username, "username", "", "login role that owns the first database")
	addIdempotencyKeyFlag(cmd, &key)
	return cmd
}

func putIfSet(body map[string]any, key, value string) {
	if value != "" {
		body[key] = value
	}
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
			req := client.Request{Path: "/dbaas/instances/" + url.PathEscape(args[0]) + "/logs/provision"}
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
		newDBResetPasswordCommand(env),
	)
	return cmd
}

// newDBResetPasswordCommand rotates a role's password. The platform requires a browser approval
// for it, because it locks out whatever still uses the old one.
func newDBResetPasswordCommand(env *Env) *cobra.Command {
	var (
		assumeYes bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "reset-password ID ROLE_ID",
		Short: "Set a new password on a role",
		Long: "Rotating a password needs your approval in a browser, because every client using the old one\n" +
			"stops connecting. --yes skips this CLI's prompt, not that approval. The new password is\n" +
			"masked in the reply unless this credential was granted secrets:read.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := instancePath("/roles/"+url.PathEscape(args[1])+"/password")(s, args)
			if err != nil {
				return err
			}
			envelope, err := RunDestructive(cmd.Context(), env, s, DestructiveRequest{
				Method: http.MethodPost, Path: path,
				Action: "reset the password of", ResourceType: "database role", ResourceID: args[1], ResourceName: args[1],
				Preview:      map[string]any{"database": args[0]},
				RecoveryNote: "Anything connecting with the old password stops working until it is given the new one.",
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

// newDBExtensionsCommand covers the one thing the platform does that a customer's own database
// role cannot: install an untrusted extension. See ADR 0017.
func newDBExtensionsCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "extensions", Aliases: []string{"ext"}, Short: "Work with database extensions"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use: "list ID DATABASE_ID", short: "List extensions and whether each is installed",
			args: cobra.ExactArgs(2), path: extensionsPath(""),
			columns: []string{"name", "label", "available", "installed", "version"},
		}),
		actionCommand(env, actionSpec{
			use: "enable ID DATABASE_ID NAME", short: "Turn an extension on", args: cobra.ExactArgs(3),
			path: extensionsPath(""),
			body: func(args []string) any { return map[string]any{"name": args[2]} },
		}),
		newDBExtensionDisableCommand(env),
	)
	return cmd
}

func extensionsPath(suffix string) func(*Session, []string) (string, error) {
	return func(_ *Session, args []string) (string, error) {
		return "/dbaas/instances/" + url.PathEscape(args[0]) + "/databases/" + url.PathEscape(args[1]) + "/extensions" + suffix, nil
	}
}

// newDBExtensionDisableCommand prompts locally and does not use RunDestructive.
//
// The platform marks this destructive but does not require a browser approval for it, and asking
// for one would be refused: a confirmation may only cover a call that needs one. The drop runs
// without CASCADE, so PostgreSQL refuses it outright if anything depends on the extension. What it
// can do is stop a running application, which is what the prompt is for.
func newDBExtensionDisableCommand(env *Env) *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "disable ID DATABASE_ID NAME",
		Short: "Turn an extension off",
		Long:  "Refused, harmlessly, if anything in the schema still depends on the extension. Nothing is dropped with it.",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			if !assumeYes && env.IsTTY {
				fmt.Fprintf(env.Err, "About to disable the %q extension. Anything using it will break.\n", args[2])
				fmt.Fprintf(env.Err, "\nType the extension name to continue: ")
				var typed string
				if _, err := fmt.Fscanln(env.In, &typed); err != nil {
					return fmt.Errorf("cancelled")
				}
				if strings.TrimSpace(typed) != args[2] {
					return fmt.Errorf("that did not match %q, so nothing was done", args[2])
				}
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodDelete,
				Path: "/dbaas/instances/" + url.PathEscape(args[0]) + "/databases/" + url.PathEscape(args[1]) +
					"/extensions/" + url.PathEscape(args[2]),
			})
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip the prompt")
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
				Method: http.MethodPost, Path: "/dbaas/instances/" + url.PathEscape(args[0]) + "/restore", Body: body,
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
	var reveal bool
	cmd := &cobra.Command{
		Use:   "connection-string ID DATABASE_ID",
		Short: "Show how to connect to a database (the password is masked)",
		Long:  "The password is masked unless you pass --reveal, which needs a credential granted secrets:read.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			req := client.Request{
				Method: http.MethodGet,
				Path:   "/dbaas/instances/" + url.PathEscape(args[0]) + "/databases/" + url.PathEscape(args[1]) + "/connection-info",
			}
			if reveal {
				req.Query = url.Values{"reveal": {"true"}}
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, req)
			if err != nil {
				return explainSecretsScope(err)
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().BoolVar(&reveal, "reveal", false, "include the password, which is a secret (needs the secrets:read scope)")
	return cmd
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
				Method: http.MethodDelete, Path: "/dbaas/instances/" + url.PathEscape(args[0]),
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
