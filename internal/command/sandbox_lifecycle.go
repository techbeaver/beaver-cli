package command

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
	"github.com/techbeaver/beaver-cli/internal/sandboxerr"
	"github.com/techbeaver/beaver-cli/internal/sandboxrules"
)

// checkSandboxName applies the platform's naming rule before anything is sent. The server checks again.
func checkSandboxName(raw string) (string, error) {
	name, ok := sandboxrules.CheckName(raw)
	if !ok {
		return "", usageErr("%s", sandboxrules.NameRule)
	}
	return name, nil
}

func sandboxPath(id string, suffix string) string {
	return "/sandbox/sandboxes/" + url.PathEscape(id) + suffix
}

func newSandboxListCommand(env *Env) *cobra.Command {
	var (
		all, includeDestroyed bool
		page, limit           int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your sandboxes",
		Long:  "Lists your sandboxes. With a project selected, only that project's. Use --all to ignore the selection.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			q := url.Values{}
			if !all && s.Resolved.Project != "" {
				project, err := sandboxProject(s)
				if err != nil {
					return err
				}
				q.Set("projectId", project)
			}
			if includeDestroyed {
				q.Set("includeDestroyed", "true")
			}
			if page > 0 {
				q.Set("page", strconv.Itoa(page))
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodGet, Path: "/sandbox/sandboxes", Query: q})
			if err != nil {
				return err
			}
			rows, err := sandboxList(envelope)
			if err != nil {
				return err
			}
			spec, _, err := env.outputSpec()
			if err != nil {
				return err
			}
			return output.New(env.Out, spec).List(rows, "id", "name", "status", "vcpu", "memoryMb", "diskGb", "startedAt", "expiresAt")
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list across every project, ignoring the selected one")
	cmd.Flags().BoolVar(&includeDestroyed, "include-destroyed", false, "also list destroyed sandboxes")
	cmd.Flags().IntVar(&page, "page", 0, "page number, from 1")
	cmd.Flags().IntVar(&limit, "limit", 0, "page size")
	return cmd
}

func newSandboxGetCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:     "get ID",
		Aliases: []string{"describe"},
		Short:   "Show one sandbox",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuidArg("sandbox id", args[0])
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodGet, Path: sandboxPath(id, "")})
			if err != nil {
				return err
			}
			return renderSandboxObject(env, envelope)
		},
	}
}

func renderSandboxObject(env *Env, envelope *client.Envelope) error {
	obj, err := sandboxObject(envelope)
	if err != nil {
		return err
	}
	spec, _, err := env.outputSpec()
	if err != nil {
		return err
	}
	return output.New(env.Out, spec).Object(obj)
}

func newSandboxCreateCommand(env *Env) *cobra.Command {
	var subscription, key string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create and start a sandbox on a slot you already hold",
		Long: "Uses a free slot of the project's subscription. It buys nothing. A running sandbox uses your\n" +
			"hourly allowance, so stop it when you are done. On a by-the-hour subscription this spends your balance and\n" +
			"needs billing:spend, which a sign in cannot hold: it is refused and nothing is charged. A monthly slot is fine.\n" +
			sandboxrules.NameRule,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := checkSandboxName(args[0])
			if err != nil {
				return err
			}
			if subscription != "" {
				if _, err := uuidArg("--subscription", subscription); err != nil {
					return err
				}
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			project, err := sandboxProject(s)
			if err != nil {
				return err
			}
			body := map[string]any{"projectId": project, "name": name}
			if subscription != "" {
				body["subscriptionId"] = subscription
			}
			if key == "" {
				if key, err = newIdempotencyKey(); err != nil {
					return err
				}
			}
			req := client.Request{Method: http.MethodPost, Path: "/sandbox/sandboxes", Body: body}
			envelope, err := retryOnce429(cmd.Context(), env, func() (*client.Envelope, error) {
				return sendKeyed(cmd.Context(), env, s, req, key, "a second sandbox", is429)
			})
			if err != nil {
				return sandboxerr.Describe(err)
			}
			return renderSandboxObject(env, envelope)
		},
	}
	cmd.Flags().StringVar(&subscription, "subscription", "", "which subscription's slot to use, when the project has more than one")
	addIdempotencyKeyFlag(cmd, &key)
	return cmd
}

func is429(e *client.APIError) bool { return e.StatusCode == http.StatusTooManyRequests }

func newSandboxTransitionCommand(env *Env, action, short string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " ID",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuidArg("sandbox id", args[0])
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodPost, Path: sandboxPath(id, "/"+action)})
			if err != nil {
				return err
			}
			return renderSandboxObject(env, envelope)
		},
	}
}

func newSandboxDestroyCommand(env *Env) *cobra.Command {
	var (
		assumeYes bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "destroy ID",
		Short: "Destroy a sandbox and everything on its disk",
		Long: "Destroying needs your approval in a browser. --yes skips this CLI's prompt, not that approval.\n" +
			"To stop paying without losing the disk, use beaver sandbox stop.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuidArg("sandbox id", args[0])
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			current, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodGet, Path: sandboxPath(id, "")})
			if err != nil {
				return err
			}
			sb, err := sandboxObject(current)
			if err != nil {
				return err
			}
			envelope, err := RunDestructive(cmd.Context(), env, s, DestructiveRequest{
				Method: http.MethodDelete, Path: sandboxPath(id, ""),
				Action: "destroy", ResourceType: "sandbox", ResourceID: id, ResourceName: id,
				Preview: map[string]any{
					"name":         text(sb["name"]),
					"status":       text(sb["status"]),
					"whatGoes":     "the sandbox and everything on its disk",
					"whatSurvives": "nothing of the sandbox; its slot is free again",
				},
				RecoveryNote: "The sandbox and everything on its disk are deleted and cannot be recovered. The slot it used becomes free again.",
			}, assumeYes, wait)
			if err != nil {
				return sandboxerr.Describe(err)
			}
			return renderSandboxObject(env, envelope)
		},
	}
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip this CLI's prompt (the browser approval still applies)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "how long to wait for approval before exiting 6")
	return cmd
}
