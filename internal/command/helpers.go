package command

import (
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
)

type listSpec struct {
	use     string
	short   string
	aliases []string
	args    cobra.PositionalArgs
	path    func(*Session, []string) (string, error)
	query   func([]string) url.Values
	columns []string
}

func listCommand(env *Env, spec listSpec) *cobra.Command {
	args := spec.args
	if args == nil {
		args = cobra.NoArgs
	}
	return &cobra.Command{
		Use:     spec.use,
		Short:   spec.short,
		Aliases: spec.aliases,
		Args:    args,
		RunE: func(cmd *cobra.Command, argv []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := spec.path(s, argv)
			if err != nil {
				return err
			}
			req := client.Request{Method: http.MethodGet, Path: path}
			if spec.query != nil {
				req.Query = spec.query(argv)
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, req)
			if err != nil {
				return err
			}
			rows, err := client.DecodeList(envelope)
			if err != nil {
				return err
			}
			w, err := env.Writer()
			if err != nil {
				return err
			}
			return w.List(rows, spec.columns...)
		},
	}
}

type getSpec struct {
	use     string
	short   string
	aliases []string
	args    cobra.PositionalArgs
	path    func(*Session, []string) (string, error)
}

func getCommand(env *Env, spec getSpec) *cobra.Command {
	args := spec.args
	if args == nil {
		args = cobra.ExactArgs(1)
	}
	return &cobra.Command{
		Use:     spec.use,
		Short:   spec.short,
		Aliases: spec.aliases,
		Args:    args,
		RunE: func(cmd *cobra.Command, argv []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := spec.path(s, argv)
			if err != nil {
				return err
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, client.Request{
				Method: http.MethodGet, Path: path,
			})
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
}

// actionSpec is a POST or PATCH that changes something without destroying it.
type actionSpec struct {
	use     string
	short   string
	method  string
	args    cobra.PositionalArgs
	path    func(*Session, []string) (string, error)
	body    func([]string) any
	flags   func(*cobra.Command)
	message string
}

func actionCommand(env *Env, spec actionSpec) *cobra.Command {
	args := spec.args
	if args == nil {
		args = cobra.NoArgs
	}
	cmd := &cobra.Command{
		Use:   spec.use,
		Short: spec.short,
		Args:  args,
		RunE: func(cmd *cobra.Command, argv []string) error {
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			path, err := spec.path(s, argv)
			if err != nil {
				return err
			}
			method := spec.method
			if method == "" {
				method = http.MethodPost
			}
			req := client.Request{Method: method, Path: path}
			if spec.body != nil {
				req.Body = spec.body(argv)
			}
			envelope, err := s.Client.Do(cmd.Context(), s.Token, req)
			if err != nil {
				return err
			}
			return renderObject(env, envelope)
		},
	}
	if spec.flags != nil {
		spec.flags(cmd)
	}
	return cmd
}

func renderObject(env *Env, envelope *client.Envelope) error {
	obj, err := client.DecodeObject(envelope)
	if err != nil {
		return err
	}
	w, err := env.Writer()
	if err != nil {
		return err
	}
	return w.Object(obj)
}

// withListAlias lets a bare listing command also answer the "noun list" form,
// because that is the shape gcloud and gh trained people to type.
func withListAlias(cmd *cobra.Command) *cobra.Command {
	alias := *cmd
	alias.Use = "list"
	alias.Aliases = nil
	alias.Flags().AddFlagSet(cmd.Flags())
	cmd.AddCommand(&alias)
	return cmd
}
