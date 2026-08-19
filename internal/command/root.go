package command

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

// NewRoot builds the command tree.
func NewRoot(env *Env) *cobra.Command {
	root := &cobra.Command{
		Use:           "beaver",
		Short:         "Run your TechBeaver infrastructure from the terminal",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.SetOut(env.Out)
	root.SetErr(env.Err)
	root.SetIn(env.In)

	f := root.PersistentFlags()
	f.StringVar(&env.flags.format, "format", "", "output format: json, yaml, table or value(field,...)")
	f.StringVar(&env.flags.profile, "profile", "", "named profile to use")
	f.StringVar(&env.flags.host, "host", "", "API host to talk to")
	f.StringVar(&env.flags.project, "project", "", "project to act on")
	f.StringVar(&env.flags.app, "app", "", "app to act on")
	f.BoolVar(&env.flags.debug, "debug", false, "print request diagnostics, with credentials redacted")

	root.AddCommand(
		newAuthCommand(env),
		newConfigCommand(env),
		newProjectsCommand(env),
		newAppsCommand(env),
		newDBCommand(env),
		newBillingCommand(env),
		newMCPCommand(env),
		newVersionCommand(env),
	)
	root.AddCommand(catalogCommands(env)...)
	return root
}

// usageErr marks an error as the caller's mistake so it maps to exit code 2.
func usageErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{exitcode.ErrUsage}, args...)...)
}
