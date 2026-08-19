package command

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"

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
		wanted    []string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to TechBeaver",
		Long: "Opens a browser to approve access.\n\n" +
			"On a machine with no browser use --no-browser, which prints a code to enter\n" +
			"on another device. --token reads a personal token from a prompt or stdin.",
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
			case useToken:
				tok, err = readPastedToken(env, resolved.Host)
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
	cmd.Flags().StringSliceVar(&wanted, "scope", nil, "scopes to request (defaults to the standard set)")
	return cmd
}

// readPastedToken accepts a token from stdin or a prompt. There is deliberately
// no --token=VALUE flag: arguments are visible to other users. See ADR 0010.
func readPastedToken(env *Env, host string) (*credential.Token, error) {
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
	return &credential.Token{AccessToken: token, Host: strings.TrimRight(host, "/")}, nil
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
