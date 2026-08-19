// Package credential stores and loads the customer's tokens.
//
// Rationale is in docs/adr/0010-credential-storage.md. The rules it states are
// requirements, not preferences.
package credential

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNotFound means no credential is stored for that profile.
var ErrNotFound = errors.New("no stored credential")

// Token is what a successful login produces.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Host         string    `json:"host,omitempty"`
}

// Expired reports whether the access token has passed its expiry, with a small
// margin so a call is not made with a token about to lapse mid-flight.
func (t *Token) Expired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(30 * time.Second).After(t.ExpiresAt)
}

// Store persists tokens for a profile.
type Store interface {
	Save(profile string, tok *Token) error
	Load(profile string) (*Token, error)
	Delete(profile string) error
	Describe() string
}

// Open returns the best available store: the platform keychain where there is
// one, and a file otherwise. A machine with no keychain is the normal case in
// CI, not an exotic one.
//
// BEAVER_CREDENTIAL_STORE=file forces the file store, which is what a container
// with a mounted credential and an incidental keyring needs.
func Open(dir string) Store {
	if os.Getenv("BEAVER_CREDENTIAL_STORE") == "file" {
		return &FileStore{Dir: dir}
	}
	if ks := openKeyring(); ks != nil {
		return ks
	}
	return &FileStore{Dir: dir}
}

// DefaultDir is where the file store lives when nothing overrides it.
func DefaultDir() (string, error) {
	if custom := os.Getenv("BEAVER_CONFIG_DIR"); custom != "" {
		return custom, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating your home directory: %w", err)
	}
	return filepath.Join(home, ".config", "beaver"), nil
}

// FileStore keeps credentials in a 0600 file inside a 0700 directory.
type FileStore struct {
	Dir string
}

// Describe names the store, for `beaver auth whoami`.
func (s *FileStore) Describe() string { return "file " + s.path("<profile>") }

func (s *FileStore) path(profile string) string {
	return filepath.Join(s.Dir, "credentials-"+sanitise(profile)+".json")
}

// Save writes the token, creating the directory 0700 and the file 0600.
func (s *FileStore) Save(profile string, tok *Token) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", s.Dir, err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return fmt.Errorf("securing %s: %w", s.Dir, err)
	}
	encoded, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	path := s.path(profile)
	// Write via a 0600 temporary file and rename, so the credential is never
	// briefly readable and a crash cannot leave a truncated one.
	tmp, err := os.CreateTemp(s.Dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Load reads the token, refusing a file that others can read.
func (s *FileStore) Load(profile string) (*Token, error) {
	path := s.path(profile)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := checkMode(path, info.Mode()); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is built from a sanitised profile name
	if err != nil {
		return nil, err
	}
	var tok Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("%s is not readable as a credential; run beaver auth login again", path)
	}
	return &tok, nil
}

// Delete removes the stored token. A profile with none is not an error.
func (s *FileStore) Delete(profile string) error {
	err := os.Remove(s.path(profile))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// checkMode refuses a credential file that group or others can read. Refusing
// rather than warning is the decision in ADR 0010: a warning on a credential
// nobody is watching is not a control.
func checkMode(path string, mode os.FileMode) error {
	if mode.Perm()&0o077 != 0 {
		return fmt.Errorf(
			"%s is readable by other users (mode %04o). Run: chmod 600 %s",
			path, mode.Perm(), path)
	}
	return nil
}

func sanitise(profile string) string {
	if profile == "" {
		return "default"
	}
	out := make([]rune, 0, len(profile))
	for _, r := range profile {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
