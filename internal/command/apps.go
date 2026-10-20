package command

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
)

func newAppsCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "apps", Aliases: []string{"app"}, Short: "Work with applications"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use:     "list",
			short:   "List the applications in a project",
			columns: []string{"id", "name", "status", "url", "region"},
			path: func(s *Session, _ []string) (string, error) {
				project, err := s.Resolved.RequireProject()
				if err != nil {
					return "", err
				}
				return "/paas/projects/" + project + "/apps", nil
			},
		}),
		getCommand(env, getSpec{
			use:   "describe [ID]",
			short: "Show one application",
			args:  cobra.MaximumNArgs(1),
			path:  appPath(""),
		}),
		newAppCreateCommand(env),
		newAppDeployCommand(env),
		actionCommand(env, actionSpec{use: "start [ID]", short: "Start an application", args: cobra.MaximumNArgs(1), path: appPath("/start")}),
		actionCommand(env, actionSpec{use: "stop [ID]", short: "Stop an application", args: cobra.MaximumNArgs(1), path: appPath("/stop")}),
		actionCommand(env, actionSpec{use: "rollback [ID]", short: "Roll back to the previous deployment", args: cobra.MaximumNArgs(1), path: appPath("/rollback")}),
		newAppRestartCommand(env),
		newAppLogsCommand(env),
		newAppEnvCommand(env),
		newAppDomainsCommand(env),
		newAppScaleCommand(env),
		newAppPortCommand(env),
		listCommand(env, listSpec{
			use: "instances [ID]", short: "List the running instances of an application",
			args: cobra.MaximumNArgs(1), path: appPath("/instances"),
			columns: []string{"name", "status", "restarts", "startedAt"},
		}),
		newAppDeleteCommand(env),
	)
	return cmd
}

// appPath resolves the app from an argument or the configured default.
func appPath(suffix string) func(*Session, []string) (string, error) {
	return func(s *Session, args []string) (string, error) {
		id := s.Resolved.App
		if len(args) > 0 && args[0] != "" {
			id = args[0]
		}
		if id == "" {
			if _, err := s.Resolved.RequireApp(); err != nil {
				return "", err
			}
		}
		return "/paas/apps/" + id + suffix, nil
	}
}

func newAppDeployCommand(env *Env) *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "deploy [ID]",
		Short: "Deploy an application",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/deploy")(s, args)
			if err != nil {
				return err
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodPost, Path: path,
			})
			if err != nil {
				return err
			}
			if !wait {
				return renderObject(env, envelope)
			}
			logPath, err := appPath("/logs/build")(s, args)
			if err != nil {
				return err
			}
			fmt.Fprintln(env.Err, "Deploying. Following the build log; Ctrl-C stops watching, not the build.")
			return s.Client.Stream(cmd.Context(), s.Token, client.Request{Path: logPath}, func(line string) {
				fmt.Fprintln(env.Out, line)
			})
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "follow the build log until it finishes")
	return cmd
}

// newAppRestartCommand exists because the API has no restart route: it is a
// stop then a start, and doing it here saves every customer discovering that.
func newAppRestartCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "restart [ID]",
		Short: "Stop and start an application",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			for _, suffix := range []string{"/stop", "/start"} {
				path, err := appPath(suffix)(s, args)
				if err != nil {
					return err
				}
				if _, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
					Method: http.MethodPost, Path: path,
				}); err != nil {
					return err
				}
			}
			fmt.Fprintln(env.Err, "Restarted.")
			return nil
		},
	}
}

func newAppLogsCommand(env *Env) *cobra.Command {
	var (
		follow bool
		build  bool
		lines  int
	)
	cmd := &cobra.Command{
		Use:   "logs [ID]",
		Short: "Read an application's logs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			suffix := "/logs/runtime"
			if build {
				suffix = "/logs/build"
			}
			path, err := appPath(suffix)(s, args)
			if err != nil {
				return err
			}
			req := client.Request{Path: path}
			if follow {
				return s.Client.Stream(cmd.Context(), s.Token, req, func(line string) {
					fmt.Fprintln(env.Out, line)
				})
			}
			collected, truncated, err := s.Client.Tail(cmd.Context(), s.Token, req, lines, 10*time.Second)
			if err != nil {
				return err
			}
			if truncated {
				fmt.Fprintf(env.Err, "(showing the last %d lines)\n", lines)
			}
			for _, line := range collected {
				fmt.Fprintln(env.Out, line)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep the stream open")
	cmd.Flags().BoolVar(&build, "build", false, "read the build log instead of the runtime log")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "how many lines to show when not following")
	return cmd
}

func newAppEnvCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "env", Short: "Read and change environment variables"}
	cmd.AddCommand(newAppEnvListCommand(env), newAppEnvSetCommand(env), newAppEnvUnsetCommand(env))
	return cmd
}

func newAppEnvListCommand(env *Env) *cobra.Command {
	var showValues bool
	cmd := &cobra.Command{
		Use:   "list [ID]",
		Short: "Show the environment variables",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/env")(s, args)
			if err != nil {
				return err
			}
			vars, err := readEnv(cmd.Context(), s, path)
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(vars))
			for k := range vars {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			rows := make([]map[string]any, 0, len(keys))
			for _, k := range keys {
				value := "********"
				if showValues {
					value = vars[k]
				}
				rows = append(rows, map[string]any{"key": k, "value": value})
			}
			w, err := env.Writer()
			if err != nil {
				return err
			}
			return w.List(rows, "key", "value")
		},
	}
	cmd.Flags().BoolVar(&showValues, "show-values", false, "print the values, which are secrets")
	return cmd
}

func newAppEnvSetCommand(env *Env) *cobra.Command {
	var replace bool
	cmd := &cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE ...]",
		Short: "Set environment variables, leaving the others as they are",
		Long:  "Adds or updates the named variables and keeps every other one. --replace makes the named variables the whole set.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			given, err := parseKeyValues(args)
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/env")(s, nil)
			if err != nil {
				return err
			}
			vars := given
			if !replace {
				if vars, err = readEnv(cmd.Context(), s, path); err != nil {
					return fmt.Errorf("nothing was changed, because the current variables could not be read and setting replaces the whole set: %w", err)
				}
				for k, v := range given {
					vars[k] = v
				}
			}
			return writeEnv(cmd.Context(), env, s, path, vars)
		},
	}
	cmd.Flags().BoolVar(&replace, "replace", false, "make these the only variables, removing every other one")
	return cmd
}

func newAppEnvUnsetCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "unset KEY [KEY ...]",
		Short: "Remove environment variables, leaving the others as they are",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/env")(s, nil)
			if err != nil {
				return err
			}
			vars, err := readEnv(cmd.Context(), s, path)
			if err != nil {
				return fmt.Errorf("nothing was changed, because the current variables could not be read: %w", err)
			}
			for _, k := range args {
				if _, ok := vars[k]; !ok {
					fmt.Fprintf(env.Err, "%s was not set\n", k)
				}
				delete(vars, k)
			}
			return writeEnv(cmd.Context(), env, s, path, vars)
		},
	}
}

func parseKeyValues(pairs []string) (map[string]string, error) {
	vars := map[string]string{}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, usageErr("%q is not KEY=VALUE", pair)
		}
		vars[key] = value
	}
	return vars, nil
}

// readEnv returns the app's variables. The API answers a list of {key, value}.
func readEnv(ctx context.Context, s *Session, path string) (map[string]string, error) {
	envelope, err := s.Client.Do(ctx, s.Token, client.Request{Method: http.MethodGet, Path: path})
	if err != nil {
		return nil, err
	}
	rows, err := client.DecodeList(envelope)
	if err != nil {
		return nil, err
	}
	vars := make(map[string]string, len(rows))
	for _, row := range rows {
		k, _ := row["key"].(string)
		if k == "" {
			continue
		}
		v, _ := row["value"].(string)
		vars[k] = v
	}
	return vars, nil
}

// writeEnv sends the complete set, which is what the endpoint stores.
func writeEnv(ctx context.Context, env *Env, s *Session, path string, vars map[string]string) error {
	if _, err := s.Client.Do(ctx, s.Token, client.Request{
		Method: http.MethodPut, Path: path, Body: map[string]any{"envVars": vars},
	}); err != nil {
		return err
	}
	fmt.Fprintf(env.Err, "The app now has %d environment variable(s). Redeploy for a running app to see them.\n", len(vars))
	return nil
}

func newAppCreateCommand(env *Env) *cobra.Command {
	var (
		repo, branch, rootDir, subdomain, framework, plan, cycle, key string
		port                                                          int
		envPairs, buildPairs                                          []string
	)
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an application and queue its first build",
		Long:  "On a priced plan the app waits for payment; the response carries the invoice to pay.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			envVars, err := parseKeyValues(envPairs)
			if err != nil {
				return err
			}
			buildEnv, err := parseKeyValues(buildPairs)
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			project, err := s.Resolved.RequireProject()
			if err != nil {
				return err
			}
			body := map[string]any{"projectId": project, "name": args[0]}
			putIfSet(body, "gitRepo", repo)
			putIfSet(body, "branch", branch)
			putIfSet(body, "rootDirectory", rootDir)
			putIfSet(body, "subdomain", subdomain)
			putIfSet(body, "framework", framework)
			putIfSet(body, "paasPlanId", plan)
			putIfSet(body, "billingCycle", cycle)
			if port > 0 {
				body["port"] = port
			}
			if len(envVars) > 0 {
				body["envVars"] = envVars
			}
			if len(buildEnv) > 0 {
				body["buildEnv"] = buildEnv
			}
			envelope, err := sendCharge(cmd.Context(), env, s, client.Request{
				Method: http.MethodPost, Path: "/paas/apps", Body: body,
			}, key)
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "https URL of the git repository to deploy")
	cmd.Flags().StringVar(&branch, "branch", "", "branch to deploy")
	cmd.Flags().StringVar(&rootDir, "root-dir", "", "subdirectory to build, for a monorepo")
	cmd.Flags().StringVar(&subdomain, "subdomain", "", "subdomain to publish on")
	cmd.Flags().StringVar(&framework, "framework", "", "nixpacks or dockerfile")
	cmd.Flags().IntVar(&port, "port", 0, "port the app listens on")
	cmd.Flags().StringVar(&plan, "plan", "", "plan id (see beaver plans list)")
	cmd.Flags().StringVar(&cycle, "billing-cycle", "", "monthly or yearly")
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "KEY=VALUE for the running app; repeat for more")
	cmd.Flags().StringArrayVar(&buildPairs, "build-env", nil, "KEY=VALUE available only while building; repeat for more")
	addIdempotencyKeyFlag(cmd, &key)
	return cmd
}

func newAppDomainsCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "domains", Short: "Work with custom domains"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use: "list [ID]", short: "List custom domains", args: cobra.MaximumNArgs(1),
			path: appPath("/custom-domains"), columns: []string{"id", "domain", "status", "verified"},
		}),
		newAppDomainAddCommand(env),
		newAppDomainVerifyCommand(env),
		newAppDomainRemoveCommand(env),
	)
	return cmd
}

func newAppDomainAddCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "add DOMAIN",
		Short: "Add a custom domain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/custom-domains")(s, nil)
			if err != nil {
				return err
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodPost, Path: path, Body: map[string]any{"domain": args[0]},
			})
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
}

func newAppDomainVerifyCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "verify DOMAIN_ID",
		Short: "Check whether a custom domain's DNS is in place",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/custom-domains/"+args[0]+"/verify")(s, nil)
			if err != nil {
				return err
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodPost, Path: path,
			})
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
}

func newAppDomainRemoveCommand(env *Env) *cobra.Command {
	var (
		assumeYes bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "remove DOMAIN_ID",
		Short: "Remove a custom domain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/custom-domains/"+args[0])(s, nil)
			if err != nil {
				return err
			}
			envelope, err := RunDestructive(cmd.Context(), env, s, DestructiveRequest{
				Method: http.MethodDelete, Path: path,
				Action: "remove", ResourceType: "custom domain",
				ResourceID: args[0], ResourceName: args[0],
				RecoveryNote: "The domain stops serving as soon as this is approved.",
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

func newAppScaleCommand(env *Env) *cobra.Command {
	var (
		replicas int
		quote    bool
		key      string
	)
	cmd := &cobra.Command{
		Use:   "scale [ID]",
		Short: "Change how many instances run",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if replicas < 0 {
				return usageErr("--replicas must be zero or more")
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			if quote {
				path, err := appPath("/replicas/quote")(s, args)
				if err != nil {
					return err
				}
				envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
					Method: http.MethodGet, Path: path,
					Query: map[string][]string{"replicas": {fmt.Sprint(replicas)}},
				})
				if err != nil {
					return err
				}
				return renderObject(env, envelope)
			}
			path, err := appPath("/replicas")(s, args)
			if err != nil {
				return err
			}
			envelope, err := sendCharge(cmd.Context(), env, s, client.Request{
				Method: http.MethodPut, Path: path, Body: map[string]any{"replicas": replicas},
			}, key)
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().IntVar(&replicas, "replicas", 1, "how many instances to run")
	cmd.Flags().BoolVar(&quote, "quote", false, "show what the change would cost without making it")
	addIdempotencyKeyFlag(cmd, &key)
	return cmd
}

func newAppPortCommand(env *Env) *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "set-port [ID]",
		Short: "Change the port your application listens on",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if port <= 0 || port > 65535 {
				return usageErr("--port must be between 1 and 65535")
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/port")(s, args)
			if err != nil {
				return err
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodPatch, Path: path, Body: map[string]any{"port": port},
			})
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "the port your application listens on")
	return cmd
}

func newAppDeleteCommand(env *Env) *cobra.Command {
	var (
		assumeYes bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "delete [ID]",
		Short: "Delete an application",
		Long:  "Deleting needs your approval in a browser. --yes skips this CLI's prompt, not that approval.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("")(s, args)
			if err != nil {
				return err
			}
			id := strings.TrimPrefix(path, "/paas/apps/")
			envelope, err := RunDestructive(cmd.Context(), env, s, DestructiveRequest{
				Method: http.MethodDelete, Path: path,
				Action: "delete", ResourceType: "app", ResourceID: id, ResourceName: id,
				RecoveryNote: "The application and its deployments go away. Its custom domains stop serving.",
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
