package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/techbeaver/beaver-cli/client"
)

// StdioOptions configures a locally run MCP server.
type StdioOptions struct {
	// APIBaseURL is the platform this server calls.
	APIBaseURL string
	// Token is the customer's own credential. It never leaves this process
	// except as an Authorization header to the platform.
	Token string
	// UserAgent identifies the caller in the platform's request log.
	UserAgent string
}

// ServeStdio runs the tool surface over stdio, for an AI client that launches
// this binary as a subprocess.
//
// The difference from the hosted server is who holds the credential: here it is
// the customer's own, already on their machine, so there is no OAuth, no
// session and no caller address to forward. The tool surface is identical.
func ServeStdio(ctx context.Context, opts StdioOptions) error {
	if opts.Token == "" {
		return fmt.Errorf("no credential: sign in first")
	}
	if opts.UserAgent == "" {
		opts.UserAgent = UserAgent
	}

	apiClient := client.New(opts.APIBaseURL, opts.UserAgent)
	deps := &Deps{
		Client: apiClient,
		Auth:   NewAuthenticator(apiClient),
		Config: Config{APIBaseURL: opts.APIBaseURL},
	}

	identity, err := deps.Auth.Resolve(ctx, opts.Token)
	if err != nil {
		return err
	}

	server := AllTools().Build(deps, identity)
	return server.Run(ctx, &mcpsdk.StdioTransport{})
}

// SessionFor builds the session a locally run tool call acts under.
func SessionFor(token string, identity *Identity) *Session {
	return &Session{Token: token, Identity: identity}
}
