// Package command builds the command tree. One file per noun; this file holds
// what they share.
package command

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/auth"
	"github.com/techbeaver/beaver-cli/internal/config"
	"github.com/techbeaver/beaver-cli/internal/credential"
	"github.com/techbeaver/beaver-cli/internal/endpoint"
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

	flags             flagValues
	warnedProjectHost bool
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
	r, err := e.Loader.Resolve(config.Overrides{
		Profile: pick(e.flags.profile, e.Profile),
		Host:    pick(e.flags.host, e.Host),
		Project: pick(e.flags.project, e.Project),
		App:     pick(e.flags.app, e.App),
	})
	if err != nil {
		return nil, err
	}
	if r.IgnoredProjectHost != "" && !e.warnedProjectHost {
		e.warnedProjectHost = true
		fmt.Fprintf(e.Err, "Warning: %s sets host = %q, which is ignored. A project file cannot choose where your credentials are sent. Use --host, BEAVER_HOST or beaver config set host.\n",
			r.ProjectFile, r.IgnoredProjectHost)
	}
	return r, nil
}

// ResolveLenient is Resolve for config and logout, which must work when the host is bad.
func (e *Env) ResolveLenient() (*config.Resolved, error) {
	return e.Loader.ResolveLenient(config.Overrides{
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
		if err != credential.ErrNotFound {
			return nil, err
		}
		// On a runner this is a login to perform, not a problem to report. ADR 0015.
		if auth.WorkloadName() == "" {
			return nil, fmt.Errorf("%w: run beaver auth login", exitcode.ErrUnauthenticated)
		}
		if tok, err = e.workloadSignIn(ctx, resolved); err != nil {
			return nil, err
		}
	}

	if err := endpoint.Require(tok.Host, resolved.Host, config.DefaultHost); err != nil {
		return nil, fmt.Errorf("%w: %s", exitcode.ErrUnauthenticated, err)
	}

	if tok.Expired() {
		switch {
		case tok.RefreshToken != "":
			if tok, err = e.refresh(ctx, resolved, tok); err != nil {
				return nil, err
			}
		case auth.WorkloadName() != "":
			// A workload token has no refresh token by design, so exchange again. ADR 0015.
			if tok, err = e.workloadSignIn(ctx, resolved); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: your session has expired, run beaver auth login", exitcode.ErrUnauthenticated)
		}
	}

	e.warnIfExpiringSoon(tok)

	return &Session{
		Client:   client.New(resolved.Host, "beaver/"+Version),
		Token:    tok.AccessToken,
		Profile:  resolved.Profile,
		Resolved: resolved,
	}, nil
}

// withCredentialLock runs fn while holding the profile's credential lock, so a login or logout
// cannot interleave with a refresh that is in flight.
func (e *Env) withCredentialLock(ctx context.Context, profile string, fn func() error) error {
	lockCtx, cancel := context.WithTimeout(ctx, refreshLockWait)
	defer cancel()
	release, err := credential.Lock(lockCtx, e.Loader.Dir, profile)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// refreshLockWait bounds how long one process waits for another to finish
// refreshing the same credential.
const refreshLockWait = 30 * time.Second

// refresh renews an expired credential while holding the profile's lock.
//
// Refresh tokens rotate, so two processes refreshing at once would spend the
// same one twice and leave one of them signed out. The credential is read again
// after the lock is taken, in case the other process already renewed it.
func (e *Env) refresh(ctx context.Context, resolved *config.Resolved, stale *credential.Token) (*credential.Token, error) {
	lockCtx, cancel := context.WithTimeout(ctx, refreshLockWait)
	defer cancel()
	release, err := credential.Lock(lockCtx, e.Loader.Dir, resolved.Profile)
	if err != nil {
		return nil, err
	}
	defer release()

	current := stale
	if latest, err := e.Store.Load(resolved.Profile); err == nil {
		if err := endpoint.Require(latest.Host, resolved.Host, config.DefaultHost); err != nil {
			return nil, fmt.Errorf("%w: %s", exitcode.ErrUnauthenticated, err)
		}
		if !latest.Expired() {
			return latest, nil
		}
		if latest.RefreshToken != "" {
			current = latest
		}
	}

	refreshed, err := auth.Refresh(ctx, resolved.Host, current.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("%w: your session could not be renewed, run beaver auth login", exitcode.ErrUnauthenticated)
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	if len(refreshed.Scopes) == 0 {
		refreshed.Scopes = current.Scopes
	}
	if err := e.Store.Save(resolved.Profile, refreshed); err != nil {
		return nil, err
	}
	return refreshed, nil
}

// credentialExpiryWarning is how long before a credential lapses this client
// starts saying so.
//
// Two weeks is chosen against the failure it exists to prevent: a personal
// token capped at 90 days that expires overnight, in a pipeline, on a day
// nobody touched anything. Two weeks is long enough to cover a holiday and
// short enough that the warning still means something when it appears.
const credentialExpiryWarning = 14 * 24 * time.Hour

// warnIfExpiringSoon says something before a credential that cannot renew
// itself runs out.
//
// Deliberately silent for the two credential types that renew: an OAuth login
// has a refresh token, and a CI job can exchange again whenever it likes.
// Warning about those would be noise, and noise is how a real warning gets
// ignored.
func (e *Env) warnIfExpiringSoon(tok *credential.Token) {
	if tok == nil || tok.ExpiresAt.IsZero() || tok.RefreshToken != "" {
		return
	}
	if auth.WorkloadName() != "" {
		return
	}
	left := time.Until(tok.ExpiresAt)
	if left <= 0 || left > credentialExpiryWarning {
		return
	}
	// Rounded up: 2.9 days reported as 2 reads a whole day sooner than it is.
	days := int(math.Ceil(left.Hours() / 24))
	when := fmt.Sprintf("in %d days", days)
	if days <= 1 {
		when = "within a day"
	}
	fmt.Fprintf(e.Err, "Warning: this credential expires %s (%s). Mint a new one under Portal, AI and CLI, or move this pipeline to a CI identity so it needs no token at all.\n",
		when, tok.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
}

// workloadSignIn exchanges the CI runner's identity token for a machine token.
//
// The result is stored when the store will take it, because a job runs several
// commands and each one exchanging again is waste. A store that refuses is not
// an error: the token in hand still works for this command, and a runner is
// discarded at the end of the job anyway.
func (e *Env) workloadSignIn(ctx context.Context, resolved *config.Resolved) (*credential.Token, error) {
	name := auth.WorkloadName()
	fmt.Fprintf(e.Err, "Signing in with this %s job's own identity.\n", name)

	tok, err := auth.LoginWorkload(ctx, auth.WorkloadOptions{Host: resolved.Host})
	if err != nil {
		return nil, fmt.Errorf("%w: %s", exitcode.ErrUnauthenticated, err)
	}
	_ = e.withCredentialLock(ctx, resolved.Profile, func() error { return e.Store.Save(resolved.Profile, tok) })
	return tok, nil
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
