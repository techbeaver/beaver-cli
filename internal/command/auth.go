package command

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/auth"
	"github.com/techbeaver/beaver-cli/internal/credential"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
	"github.com/techbeaver/beaver-cli/scopes"
)

func newAuthCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Sign in and out"}
	cmd.AddCommand(
		newAuthLoginCommand(env),
		newAuthLogoutCommand(env),
		newAuthWhoamiCommand(env),
		newAuthPrintTokenCommand(env),
	)
	return cmd
}

func newAuthLoginCommand(env *Env) *cobra.Command {
	var (
		noBrowser bool
		useToken  bool
		useCI     bool
		wanted    []string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to TechBeaver",
		Long: "Opens a browser to approve access.\n\n" +
			"On a machine with no browser use --no-browser, which prints a code to enter\n" +
			"on another device. --token reads a personal token from a prompt or stdin.\n\n" +
			"In a CI pipeline no flag is needed: the runner's own identity is used, and\n" +
			"there is nothing to store as a secret. Bind the repository first under\n" +
			"Portal, AI and CLI.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := env.Resolve()
			if err != nil {
				return err
			}
			granted := scopes.Default
			if len(wanted) > 0 {
				known, unknown := scopes.Parse(strings.Join(wanted, " "))
				if len(unknown) > 0 {
					return usageErr("unknown scope %s", strings.Join(unknown, ", "))
				}
				granted = known
			}

			var tok *credential.Token
			switch {
			case useCI || (!useToken && !noBrowser && auth.WorkloadName() != ""):
				// Auto-selected so a workflow author need not know of it; --ci forces it.
				fmt.Fprintf(env.Err, "Signing in with this %s job's own identity.\n", auth.WorkloadName())
				tok, err = auth.LoginWorkload(cmd.Context(), auth.WorkloadOptions{
					Host:   resolved.Host,
					Scopes: granted,
				})
			case useToken:
				tok, err = readPastedToken(cmd.Context(), env, resolved.Host)
			case noBrowser:
				tok, err = auth.LoginDevice(cmd.Context(), auth.DeviceOptions{
					Host:   resolved.Host,
					Scopes: granted,
					Prompt: func(msg string) { fmt.Fprintln(env.Err, msg) },
				})
			default:
				tok, err = auth.Login(cmd.Context(), auth.LoginOptions{
					Host:   resolved.Host,
					Scopes: granted,
					Prompt: func(url string) {
						fmt.Fprintf(env.Err, "Opening your browser to approve access.\nIf it does not open, use this link:\n\n  %s\n\n", url)
					},
				})
			}
			if err != nil {
				return err
			}

			if err := env.Store.Save(resolved.Profile, tok); err != nil {
				return err
			}
			fmt.Fprintf(env.Err, "Signed in. Credential stored in %s.\n", env.Store.Describe())
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "sign in on a machine with no browser")
	cmd.Flags().BoolVar(&useToken, "token", false, "sign in with a personal token read from a prompt or stdin")
	cmd.Flags().BoolVar(&useCI, "ci", false, "sign in with this CI job's own identity, failing if it has none")
	cmd.Flags().StringSliceVar(&wanted, "scope", nil, "scopes to request (defaults to the standard set)")
	return cmd
}

// readPastedToken accepts a token from stdin or a prompt. There is deliberately
// no --token=VALUE flag: arguments are visible to other users. See ADR 0010.
//
// The token is presented to the platform before it is stored, for two reasons.
// A typo is reported here rather than at the first real command. And the reply
// carries the token's expiry, which is the only way this client can ever warn
// before a pasted credential lapses: the string itself says nothing.
func readPastedToken(ctx context.Context, env *Env, host string) (*credential.Token, error) {
	fmt.Fprint(env.Err, "Paste your personal token (it will not be echoed by your shell): ")
	reader := bufio.NewReader(env.In)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return nil, fmt.Errorf("no token was supplied")
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return nil, usageErr("no token was supplied")
	}
	fmt.Fprintln(env.Err)

	tok := &credential.Token{AccessToken: token, Host: strings.TrimRight(host, "/")}
	if expiry, scopeList, err := describeCredential(ctx, host, token); err == nil {
		tok.ExpiresAt = expiry
		tok.Scopes = scopeList
	} else {
		return nil, err
	}
	return tok, nil
}

// describeCredential asks the platform what a token is and when it lapses.
func describeCredential(ctx context.Context, host, token string) (time.Time, []string, error) {
	api := client.New(host, "beaver/"+Version)
	env, err := api.Do(ctx, token, client.Request{Method: http.MethodGet, Path: "/mcp/whoami"})
	if err != nil {
		return time.Time{}, nil, err
	}
	obj, err := client.DecodeObject(env)
	if err != nil {
		return time.Time{}, nil, err
	}

	var expiry time.Time
	if raw, ok := obj["expiresAt"].(string); ok && raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			expiry = parsed
		}
	}
	var scopeList []string
	if raw, ok := obj["scopes"].([]any); ok {
		for _, s := range raw {
			if str, ok := s.(string); ok {
				scopeList = append(scopeList, str)
			}
		}
	}
	return expiry, scopeList, nil
}

func newAuthLogoutCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored credential",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			resolved, err := env.Resolve()
			if err != nil {
				return err
			}
			if err := env.Store.Delete(resolved.Profile); err != nil {
				return err
			}
			fmt.Fprintf(env.Err, "Signed out of profile %q.\n", resolved.Profile)
			return nil
		},
	}
}

func newAuthWhoamiCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who this credential acts for",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			env2, err := session.Client.Do(cmd.Context(), session.Token, client.Request{
				Method: http.MethodGet, Path: "/mcp/whoami",
			})
			if err != nil {
				return err
			}
			obj, err := client.DecodeObject(env2)
			if err != nil {
				return err
			}
			obj["profile"] = session.Profile
			obj["host"] = session.Resolved.Host
			w, err := env.Writer()
			if err != nil {
				return err
			}
			return w.Object(obj)
		},
	}
}

func newAuthPrintTokenCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "print-access-token",
		Short: "Print the current access token",
		Long: "Prints the access token so another tool can use it.\n\n" +
			"It is a bearer credential: anything holding it can act as you until it expires.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintln(env.Out, session.Token)
			return nil
		},
	}
}

var _ = exitcode.OK
