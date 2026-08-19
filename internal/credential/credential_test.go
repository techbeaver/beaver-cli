package credential

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveCreatesA0600FileInA0700Directory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "beaver")
	s := &FileStore{Dir: dir}
	if err := s.Save("default", &Token{AccessToken: "btk_x"}); err != nil {
		t.Fatal(err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode is %04o, want 0700", dirInfo.Mode().Perm())
	}

	fileInfo, err := os.Stat(s.path("default"))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode is %04o, want 0600", fileInfo.Mode().Perm())
	}
}

func TestLoadRefusesAWorldReadableFileRatherThanWarning(t *testing.T) {
	dir := t.TempDir()
	s := &FileStore{Dir: dir}
	if err := s.Save("default", &Token{AccessToken: "btk_x"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.path("default"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := s.Load("default")
	if err == nil {
		t.Fatal("a world-readable credential must be refused, not loaded with a warning")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("the error must say how to fix it, got: %v", err)
	}
}

func TestLoadRefusesAGroupReadableFile(t *testing.T) {
	dir := t.TempDir()
	s := &FileStore{Dir: dir}
	if err := s.Save("default", &Token{AccessToken: "btk_x"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.path("default"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("default"); err == nil {
		t.Fatal("a group-readable credential must be refused")
	}
}

func TestRoundTrip(t *testing.T) {
	s := &FileStore{Dir: t.TempDir()}
	want := &Token{
		AccessToken:  "btk_access",
		RefreshToken: "btk_refresh",
		Scopes:       []string{"paas:read"},
		ExpiresAt:    time.Now().Add(time.Hour).Truncate(time.Second),
	}
	if err := s.Save("staging", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("staging")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("expiry did not survive: %v vs %v", got.ExpiresAt, want.ExpiresAt)
	}
}

func TestProfilesAreIsolated(t *testing.T) {
	s := &FileStore{Dir: t.TempDir()}
	if err := s.Save("prod", &Token{AccessToken: "prod-token"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("staging", &Token{AccessToken: "staging-token"}); err != nil {
		t.Fatal(err)
	}
	prod, err := s.Load("prod")
	if err != nil {
		t.Fatal(err)
	}
	if prod.AccessToken != "prod-token" {
		t.Fatalf("profiles bled into each other: %q", prod.AccessToken)
	}
}

func TestMissingCredentialIsARecognisableError(t *testing.T) {
	s := &FileStore{Dir: t.TempDir()}
	if _, err := s.Load("nothing-here"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Delete("nothing-here"); err != nil {
		t.Fatalf("deleting an absent credential must not be an error, got %v", err)
	}
}

func TestProfileNamesCannotEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	s := &FileStore{Dir: dir}
	if err := s.Save("../../etc/passwd", &Token{AccessToken: "x"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one file inside the directory, got %d", len(entries))
	}
	if strings.Contains(entries[0].Name(), "/") || strings.Contains(entries[0].Name(), "..") {
		t.Fatalf("a profile name escaped: %q", entries[0].Name())
	}
}

func TestExpiredLeavesMarginBeforeExpiry(t *testing.T) {
	if (&Token{}).Expired() {
		t.Fatal("a token with no expiry is not expired")
	}
	if !(&Token{ExpiresAt: time.Now().Add(5 * time.Second)}).Expired() {
		t.Fatal("a token expiring within the margin must count as expired")
	}
	if (&Token{ExpiresAt: time.Now().Add(time.Hour)}).Expired() {
		t.Fatal("a token valid for an hour is not expired")
	}
}
