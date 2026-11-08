package command

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
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
				return "/paas/projects/" + url.PathEscape(project) + "/apps", nil
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
		return "/paas/apps/" + url.PathEscape(id) + suffix, nil
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
		Short: "Replace every instance with a fresh one, without going offline",
		Long:  "New instances start before old ones stop. A stopped app is refused: use beaver apps start.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/restart")(s, args)
			if err != nil {
				return err
			}
			if _, err := s.Client.Do(cmd.Context(), s.Token, client.Request{Method: http.MethodPost, Path: path}); err != nil {
				var apiErr *client.APIError
				if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && (apiErr.Code == "" || apiErr.Code == "upstream_not_json") {
					return fmt.Errorf("this TechBeaver API has no rolling restart yet; use beaver apps deploy to roll the app instead of stopping it: %w", err)
				}
				return err
			}
			fmt.Fprintln(env.Err, "Restarting: new instances replace the old ones one at a time.")
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
	cmd.AddCommand(newAppEnvListCommand(env), newAppEnvSetCommand(env), newAppEnvUnsetCommand(env), newAppEnvCheckCommand(env))
	return cmd
}

func newAppEnvListCommand(env *Env) *cobra.Command {
	var reveal, showValues bool
	cmd := &cobra.Command{
		Use:   "list [ID]",
		Short: "Show the environment variables (values are masked)",
		Long: "Shows the names of the variables. Values are masked unless you pass --reveal, which needs a\n" +
			"credential granted secrets:read. To test a value without revealing anything, use\n" +
			"beaver apps env check NAME.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/env")(s, args)
			if err != nil {
				return err
			}
			reveal = reveal || showValues
			vars, err := readEnv(cmd.Context(), s, path, reveal)
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
				value := maskedValue
				if reveal {
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
	cmd.Flags().BoolVar(&reveal, "reveal", false, "print the values, which are secrets (needs the secrets:read scope)")
	cmd.Flags().BoolVar(&showValues, "show-values", false, "same as --reveal")
	_ = cmd.Flags().MarkDeprecated("show-values", "use --reveal")
	return cmd
}

// EnvWithheldMarker sent as a value on a write means "keep the stored value". It lets a client
// that can only read names rewrite the set without ever holding a secret.
const EnvWithheldMarker = "[withheld]"

// maskedValue is what a value is shown as. It is printed whatever the API sent,
// so a masked or an unmasked reply can never reach the terminal by accident.
const maskedValue = "********"

func newAppEnvSetCommand(env *Env) *cobra.Command {
	var (
		replace, assumeYes bool
		wait               time.Duration
	)
	cmd := &cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE ...]",
		Short: "Set environment variables, leaving the others as they are",
		Long: "Adds or updates the named variables and keeps every other one, with no need to read them.\n" +
			"--replace makes the named variables the whole set, which needs your approval in a browser.",
		Args: cobra.MinimumNArgs(1),
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
			if !replace {
				if _, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
					Method: http.MethodPost, Path: path + "/upsert", Body: map[string]any{"envVars": given},
				}); err != nil {
					return explainEnvRouteUnsupported(err, "setting named variables (POST /env/upsert)")
				}
				fmt.Fprintf(env.Err, "Set %d environment variable(s); the others are untouched. A running app rolls out with them on its own.\n", len(given))
				return nil
			}
			return writeEnv(cmd.Context(), env, s, path, given, sortedKeys(given), assumeYes, wait)
		},
	}
	cmd.Flags().BoolVar(&replace, "replace", false, "make these the only variables, removing every other one")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip this CLI's prompt (the browser approval still applies)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "how long to wait for approval before exiting 6")
	return cmd
}

func newAppEnvUnsetCommand(env *Env) *cobra.Command {
	var (
		assumeYes bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "unset KEY [KEY ...]",
		Short: "Remove environment variables, leaving the others as they are",
		Long: "Removes each named variable with its own call, so a variable added meanwhile is never lost.\n" +
			"Each removal needs your approval in a browser. Names that are not set are skipped.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/env")(s, nil)
			if err != nil {
				return err
			}
			current, err := readEnv(cmd.Context(), s, path, false)
			if err != nil {
				return fmt.Errorf("nothing was changed, because the current variables could not be read: %w", err)
			}
			id, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/paas/apps/"), "/env"))
			var existing []string
			seen := map[string]bool{}
			for _, k := range args {
				if seen[k] {
					continue
				}
				seen[k] = true
				if _, ok := current[k]; !ok {
					fmt.Fprintf(env.Err, "%s was not set\n", k)
					continue
				}
				existing = append(existing, k)
			}
			if len(existing) == 0 {
				fmt.Fprintln(env.Err, "Nothing to remove.")
				return nil
			}
			for _, k := range existing {
				if _, err := RunDestructive(cmd.Context(), env, s, DestructiveRequest{
					Method: http.MethodDelete, Path: path + "/" + url.PathEscape(k),
					Action: "remove the environment variable", ResourceType: "app", ResourceID: id, ResourceName: id,
					Preview:      map[string]any{"variable": k},
					RecoveryNote: "The variable's value is not kept. Set it again to restore it.",
				}, assumeYes, wait); err != nil {
					return explainEnvRouteUnsupported(err, "removing one variable (DELETE /env/KEY)")
				}
				fmt.Fprintf(env.Err, "Removed %s.\n", k)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip this CLI's prompt (the browser approval still applies)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "how long to wait for approval before exiting 6")
	return cmd
}

// explainEnvRouteUnsupported turns what an older API answers for a route it does not have into a
// clear failure. The caller never falls back to PATCH or PUT: on an old API those replace the
// whole set.
func explainEnvRouteUnsupported(err error, what string) error {
	if !envRouteMissing(err) {
		return err
	}
	return fmt.Errorf("this server does not support %s yet, so nothing was changed. Update the platform, or use beaver apps env set --replace to write the whole set: %w", what, err)
}

func envRouteMissing(err error) bool {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	msg := strings.ToLower(apiErr.Message)
	switch apiErr.Code {
	case "route_not_permitted", "upstream_not_json", "confirmation_not_applicable":
		return true
	}
	if strings.Contains(msg, "allowlist") {
		return true
	}
	return apiErr.StatusCode == http.StatusNotFound && apiErr.Code == "" && !strings.Contains(msg, "not found")
}

// newAppEnvCheckCommand answers "is this the value?" without revealing one.
// The value is read from stdin or a no-echo prompt and never from an argument,
// which other users on the machine can see.
func newAppEnvCheckCommand(env *Env) *cobra.Command {
	var build bool
	cmd := &cobra.Command{
		Use:   "check NAME [ID]",
		Short: "Check whether a variable is set and whether it holds a value, without revealing it",
		Long: "Reads the value from stdin, or from a prompt that does not echo, and asks the platform whether\n" +
			"NAME is set and equal to it. The stored value is never sent back.\n\n" +
			"  printf %s \"$VALUE\" | beaver apps env check DATABASE_URL",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if name == "" || strings.Contains(name, "=") {
				return usageErr("%q is not a variable name. The value is read from stdin, not given as an argument", name)
			}
			value, err := readSecret(cmd.Context(), env, "Value to check (not echoed): ", true)
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := appPath("/env/check")(s, args[1:])
			if err != nil {
				return err
			}
			kind := "runtime"
			if build {
				kind = "build"
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodPost, Path: path,
				Body: map[string]any{"name": name, "value": value, "kind": kind},
			})
			if err != nil {
				return err
			}
			obj, err := client.DecodeObject(envelope)
			if err != nil {
				return err
			}
			w, err := env.Writer()
			if err != nil {
				return err
			}
			return w.Object(map[string]any{"name": name, "kind": kind, "set": obj["set"], "matches": obj["matches"]})
		},
	}
	cmd.Flags().BoolVar(&build, "build", false, "check a build-time variable instead of a runtime one")
	return cmd
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
// With reveal the values are real and the call needs secrets:read; without it
// they are masked and only the names can be relied on.
func readEnv(ctx context.Context, s *Session, path string, reveal bool) (map[string]string, error) {
	req := client.Request{Method: http.MethodGet, Path: path}
	if reveal {
		req.Query = url.Values{"reveal": {"true"}}
	}
	envelope, err := s.Client.Do(ctx, s.Token, req)
	if err != nil {
		return nil, explainSecretsScope(err)
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

// explainSecretsScope turns the API's secrets_scope_required into the command
// that fixes it. Any other error passes through.
func explainSecretsScope(err error) error {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != SecretsScopeRequired {
		return err
	}
	granted := append(append([]string{}, scopes.Default...), scopes.SecretsRead)
	return fmt.Errorf("%w\nThis credential was not granted %s. Sign in again with it: beaver auth login --scope %s",
		err, scopes.SecretsRead, strings.Join(granted, ","))
}

// SecretsScopeRequired is the API's code for a reveal without secrets:read.
const SecretsScopeRequired = "secrets_scope_required"

// writeEnv sends the complete set, which is what the endpoint stores. The
// platform requires a browser approval for it, bound to exactly this body.
// Only the names that change are shown to the approver, never values.
func writeEnv(ctx context.Context, env *Env, s *Session, path string, vars map[string]string, changed []string, assumeYes bool, wait time.Duration) error {
	id, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/paas/apps/"), "/env"))
	if _, err := RunDestructive(ctx, env, s, DestructiveRequest{
		Method: http.MethodPut, Path: path, Body: map[string]any{"envVars": vars},
		Action: "change the environment of", ResourceType: "app", ResourceID: id, ResourceName: id,
		Preview:      map[string]any{"variables changed": strings.Join(changed, ", "), "variables after": len(vars)},
		RecoveryNote: "The previous environment variables are not kept. A running app rolls out with the new ones.",
	}, assumeYes, wait); err != nil {
		return err
	}
	fmt.Fprintf(env.Err, "The app now has %d environment variable(s). A running app rolls out with them on its own; there is nothing to restart.\n", len(vars))
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
			path, err := appPath("/custom-domains/"+url.PathEscape(args[0])+"/verify")(s, nil)
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
			path, err := appPath("/custom-domains/"+url.PathEscape(args[0]))(s, nil)
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
			id, _ := url.PathUnescape(strings.TrimPrefix(path, "/paas/apps/"))
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
