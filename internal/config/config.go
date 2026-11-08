// Package config resolves settings from the profile file, a project file and
// the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/techbeaver/beaver-cli/internal/endpoint"
)

// DefaultHost is the platform this CLI talks to.
const DefaultHost = "https://cloudapi.techbeaver.io"

// ProjectFile is the per-repository file that pins a project and app.
const ProjectFile = "beaver.toml"

// Profile is one named set of settings.
type Profile struct {
	Host    string `toml:"host,omitempty"`
	Project string `toml:"project,omitempty"`
	App     string `toml:"app,omitempty"`
}

// File is the on-disk profile configuration.
type File struct {
	Current  string             `toml:"current_profile,omitempty"`
	Profiles map[string]Profile `toml:"profiles"`
}

// Project is the contents of beaver.toml. Host is read only so it can be
// reported as ignored: a cloned repository must not choose where credentials go.
type Project struct {
	Project string `toml:"project"`
	App     string `toml:"app"`
	Host    string `toml:"host,omitempty"`
}

// idPattern is what a project or app identifier from beaver.toml may look
// like: a UUID or a slug. It has no slash, dot, question mark or hash.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// ValidID reports whether s is a safe project or app identifier.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// Resolved is the effective configuration for one invocation.
type Resolved struct {
	Profile string
	Host    string
	Project string
	App     string
	// IgnoredProjectHost is a host found in beaver.toml and not used.
	IgnoredProjectHost string
	// ProjectFile is the beaver.toml that was read, if any.
	ProjectFile string
	// HostErr is set only by ResolveLenient, when the configured host is unusable. Host then
	// holds the raw value for display and must never be contacted.
	HostErr error
}

// Loader reads configuration. Dir is where the profile file lives; WorkDir is
// where the search for a project file starts.
type Loader struct {
	Dir     string
	WorkDir string
}

func (l *Loader) configPath() string { return filepath.Join(l.Dir, "config.toml") }

// Load reads the profile file, returning an empty one if it does not exist.
func (l *Loader) Load() (*File, error) {
	f := &File{Profiles: map[string]Profile{}}
	raw, err := os.ReadFile(l.configPath()) // #nosec G304 -- a path this process owns
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	if err := toml.Unmarshal(raw, f); err != nil {
		return nil, fmt.Errorf("%s is not valid TOML: %w", l.configPath(), err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	return f, nil
}

// Save writes the profile file, creating the directory 0700.
func (l *Loader) Save(f *File) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(f); err != nil {
		return err
	}
	return os.WriteFile(l.configPath(), []byte(buf.String()), 0o600)
}

// FindProjectFile walks up from WorkDir looking for beaver.toml, so a command
// works anywhere inside a repository rather than only at its root.
func (l *Loader) FindProjectFile() (*Project, string, error) {
	dir := l.WorkDir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return nil, "", err
		}
	}
	for {
		candidate := filepath.Join(dir, ProjectFile)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			raw, err := os.ReadFile(candidate) // #nosec G304 -- discovered by walking up from the working directory
			if err != nil {
				return nil, "", err
			}
			var p Project
			if err := toml.Unmarshal(raw, &p); err != nil {
				return nil, "", fmt.Errorf("%s is not valid TOML: %w", candidate, err)
			}
			return &p, candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, "", nil
		}
		dir = parent
	}
}

// Overrides are the values a command line supplied. Empty means absent.
type Overrides struct {
	Profile string
	Host    string
	Project string
	App     string
}

// Resolve applies the precedence: a flag, then the environment, then
// beaver.toml, then the profile, then the default. The host is the exception:
// it never comes from beaver.toml, only from a flag, the environment, the
// profile or the default.
func (l *Loader) Resolve(o Overrides) (*Resolved, error) {
	r, err := l.resolve(o)
	if err != nil {
		return nil, err
	}
	if r.HostErr != nil {
		return nil, fmt.Errorf("%w. Fix it with: beaver config set host https://... or pass --host", r.HostErr)
	}
	return r, nil
}

// ResolveLenient is Resolve for the commands that repair a bad host (config, logout). A host
// that fails validation is reported in HostErr instead of failing, and callers must not send
// anything to it.
func (l *Loader) ResolveLenient(o Overrides) (*Resolved, error) {
	return l.resolve(o)
}

func (l *Loader) resolve(o Overrides) (*Resolved, error) {
	f, err := l.Load()
	if err != nil {
		return nil, err
	}
	project, projectPath, err := l.FindProjectFile()
	if err != nil {
		return nil, err
	}
	if project == nil {
		project = &Project{}
	}
	for field, v := range map[string]string{"project": project.Project, "app": project.App} {
		if v != "" && !ValidID(v) {
			return nil, fmt.Errorf("%s: %s %q is not a valid identifier (letters, digits, - and _ only)", projectPath, field, v)
		}
	}

	name := firstNonEmpty(o.Profile, os.Getenv("BEAVER_PROFILE"), f.Current, "default")
	profile := f.Profiles[name]
	if o.Profile != "" && o.Profile != "default" {
		if _, ok := f.Profiles[o.Profile]; !ok {
			return nil, fmt.Errorf("no profile named %q. Run: beaver config list", o.Profile)
		}
	}

	rawHost := firstNonEmpty(o.Host, os.Getenv("BEAVER_HOST"), profile.Host, DefaultHost)
	host, hostErr := endpoint.Normalise(rawHost)
	if hostErr != nil {
		host = rawHost
	}
	return &Resolved{
		Profile:            name,
		Host:               host,
		Project:            firstNonEmpty(o.Project, os.Getenv("BEAVER_PROJECT"), project.Project, profile.Project),
		App:                firstNonEmpty(o.App, os.Getenv("BEAVER_APP"), project.App, profile.App),
		IgnoredProjectHost: strings.TrimSpace(project.Host),
		ProjectFile:        projectPath,
		HostErr:            hostErr,
	}, nil
}

// ErrNoProject means a command needed a project and none was resolved.
var ErrNoProject = errors.New("no project selected")

// RequireProject returns the resolved project or an error naming every way to
// supply one.
func (r *Resolved) RequireProject() (string, error) {
	if r.Project == "" {
		return "", fmt.Errorf("%w: pass --project, set one in %s, or run beaver config set project NAME",
			ErrNoProject, ProjectFile)
	}
	return r.Project, nil
}

// RequireApp returns the resolved app or an error naming every way to supply
// one.
func (r *Resolved) RequireApp() (string, error) {
	if r.App == "" {
		return "", fmt.Errorf("no app selected: pass --app, set one in %s, or run beaver config set app NAME",
			ProjectFile)
	}
	return r.App, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
