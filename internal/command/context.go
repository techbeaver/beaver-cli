// Package command builds the command tree. One file per noun; this file holds
// what they share.
package command

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/auth"
	"github.com/techbeaver/beaver-cli/internal/config"
	"github.com/techbeaver/beaver-cli/internal/credential"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
	"github.com/techbeaver/beaver-cli/internal/output"
)

// Version is set at build time.
var Version = "dev"

// Env is everything a command needs from the outside world, so tests can
// replace all of it.
type Env struct {
	Out    io.Writer
	Err    io.Writer
	In     io.Reader
	Store  credential.Store
	Loader *config.Loader
	IsTTY  bool

	// Format and the fields below it are the programmatic defaults. Command
	// line flags are held separately in flags, because binding a flag directly
	// to these would overwrite them with the flag's empty default the moment
	// the flag is registered.
	Format  string
	Profile string
	Host    string
	Project string
	App     string

	flags flagValues
}

// flagValues holds what the command line supplied this invocation.
type flagValues struct {
	format  string
	profile string
	host    string
	project string
	app     string
	debug   bool
}

func pick(flag, programmatic string) string {
	if flag != "" {
		return flag
	}
	return programmatic
}

// Resolve applies the configuration precedence for this invocation.
func (e *Env) Resolve() (*config.Resolved, error) {
	return e.Loader.Resolve(config.Overrides{
		Profile: pick(e.flags.profile, e.Profile),
		Host:    pick(e.flags.host, e.Host),
		Project: pick(e.flags.project, e.Project),
		App:     pick(e.flags.app, e.App),
	})
}

// Writer builds the renderer for this invocation.
func (e *Env) Writer() (*output.Writer, error) {
	spec, err := output.ParseFormat(pick(e.flags.format, e.Format), e.IsTTY)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", exitcode.ErrUsage, err)
	}
	return output.New(e.Out, spec), nil
}

// Session is an authenticated invocation.
type Session struct {
	Client   *client.Client
	Token    string
	Profile  string
	Resolved *config.Resolved
}

// Authenticate loads the stored credential, refreshing it when the access
// token has lapsed, and returns a ready client.
func (e *Env) Authenticate(ctx context.Context) (*Session, error) {
	resolved, err := e.Resolve()
	if err != nil {
		return nil, err
	}

	tok, err := e.Store.Load(resolved.Profile)
	if err != nil {
		if err == credential.ErrNotFound {
			return nil, fmt.Errorf("%w: run beaver auth login", exitcode.ErrUnauthenticated)
		}
		return nil, err
	}

	if tok.Expired() {
		if tok.RefreshToken == "" {
			return nil, fmt.Errorf("%w: your session has expired, run beaver auth login", exitcode.ErrUnauthenticated)
		}
		refreshed, err := auth.Refresh(ctx, resolved.Host, tok.RefreshToken)
		if err != nil {
			return nil, fmt.Errorf("%w: your session could not be renewed, run beaver auth login", exitcode.ErrUnauthenticated)
		}
		if err := e.Store.Save(resolved.Profile, refreshed); err != nil {
			return nil, err
		}
		tok = refreshed
	}

	return &Session{
		Client:   client.New(resolved.Host, "beaver/"+Version),
		Token:    tok.AccessToken,
		Profile:  resolved.Profile,
		Resolved: resolved,
	}, nil
}

// NewEnv builds the default environment.
func NewEnv() (*Env, error) {
	dir, err := credential.DefaultDir()
	if err != nil {
		return nil, err
	}
	workDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return &Env{
		Out:    os.Stdout,
		Err:    os.Stderr,
		In:     os.Stdin,
		Store:  credential.Open(dir),
		Loader: &config.Loader{Dir: dir, WorkDir: workDir},
		IsTTY:  output.IsTTY(os.Stdout),
	}, nil
}
