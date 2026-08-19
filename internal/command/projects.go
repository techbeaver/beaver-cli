package command

import (
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
)

func newProjectsCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "projects", Aliases: []string{"project"}, Short: "Work with projects"}
	cmd.AddCommand(
		listCommand(env, listSpec{
			use:     "list",
			short:   "List your projects",
			path:    func(*Session, []string) (string, error) { return "/paas/projects", nil },
			columns: []string{"id", "name", "region", "createdAt"},
		}),
		getCommand(env, getSpec{
			use:   "describe ID",
			short: "Show one project",
			path:  func(_ *Session, args []string) (string, error) { return "/paas/projects/" + args[0], nil },
		}),
		newProjectCreateCommand(env),
		newProjectDeleteCommand(env),
	)
	return cmd
}

func newProjectCreateCommand(env *Env) *cobra.Command {
	var region string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			body := map[string]any{"name": args[0]}
			if region != "" {
				body["region"] = region
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodPost, Path: "/paas/projects", Body: body,
			})
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	cmd.Flags().StringVar(&region, "region", "", "region to create it in (see beaver regions list)")
	return cmd
}

func newProjectDeleteCommand(env *Env) *cobra.Command {
	var (
		assumeYes bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "delete ID",
		Short: "Delete a project",
		Long:  "Deleting needs your approval in a browser. --yes skips this CLI's prompt, not that approval.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := RunDestructive(cmd.Context(), env, s, DestructiveRequest{
				Method:       http.MethodDelete,
				Path:         "/paas/projects/" + args[0],
				Action:       "delete",
				ResourceType: "project",
				ResourceID:   args[0],
				ResourceName: args[0],
				Preview:      map[string]any{"project": args[0]},
				RecoveryNote: "Deleting a project removes everything inside it.",
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
