package command

import (
	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/mcp"
)

func newMCPCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the Model Context Protocol over stdio",
		Long: "Exposes your TechBeaver infrastructure as tools for a local AI client,\n" +
			"using the credential already on this machine.\n\n" +
			"Configure your client to run: beaver mcp\n\n" +
			"For a hosted connection instead, point the client at\n" +
			"https://mcp.techbeaver.io/mcp and approve it in your browser.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			return mcp.ServeStdio(cmd.Context(), mcp.StdioOptions{
				APIBaseURL: s.Resolved.Host,
				Token:      s.Token,
				UserAgent:  "beaver-cli-mcp/" + Version,
			})
		},
	}
}
