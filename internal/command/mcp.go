package command

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/mcp"
	"github.com/techbeaver/beaver-cli/scopes"
)

func newMCPCommand(env *Env) *cobra.Command {
	var (
		readOnly     bool
		allowSecrets bool
		only         []string
	)
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the Model Context Protocol over stdio",
		Long: "Exposes your TechBeaver infrastructure as tools for a local AI client,\n" +
			"using the credential already on this machine.\n\n" +
			"Configure your client to run: beaver mcp\n\n" +
			"What the client can reach is decided here, not by the model:\n\n" +
			"  --read-only       offer only tools that change nothing\n" +
			"  --scope LIST      offer only tools whose required scopes are all in LIST\n" +
			"  --allow-secrets   offer the tools that put secret values (environment variables,\n" +
			"                    database passwords) into the conversation. Off by default.\n\n" +
			"For a hosted connection instead, point the client at\n" +
			"https://mcp.techbeaver.io/mcp and approve it in your browser.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var limit []string
			if len(only) > 0 {
				known, unknown := scopes.Parse(strings.Join(only, " "))
				if len(unknown) > 0 {
					return usageErr("unknown scope %s", strings.Join(unknown, ", "))
				}
				limit = known
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			return mcp.ServeStdio(cmd.Context(), mcp.StdioOptions{
				APIBaseURL:   s.Resolved.Host,
				Token:        s.Token,
				UserAgent:    "beaver-cli-mcp/" + Version,
				ReadOnly:     readOnly,
				Scopes:       limit,
				AllowSecrets: allowSecrets,
			})
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "offer only tools that change nothing")
	cmd.Flags().StringSliceVar(&only, "scope", nil, "offer only tools whose required scopes are all in this list")
	cmd.Flags().BoolVar(&allowSecrets, "allow-secrets", false, "offer the tools that reveal secret values to the model")
	return cmd
}
