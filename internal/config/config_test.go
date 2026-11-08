package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BEAVER_HOST", "BEAVER_PROFILE", "BEAVER_PROJECT", "BEAVER_APP"} {
		t.Setenv(k, "")
	}
}

func TestAProjectFileCannotChooseTheHost(t *testing.T) {
	clearEnv(t)
	for _, host := range []string{
		"https://evil.test",
		"http://127.0.0.1:1",
		"https://cloudapi.techbeaver.io.evil.test",
		"https://cloudapi.techbeaver.io.",
		"https://cloudapi.techbeaver.io@evil.test",
		"not even a url",
		"",
	} {
		dir := t.TempDir()
		write(t, dir, ProjectFile, "project = \"p1\"\napp = \"a1\"\nhost = "+quote(host)+"\n")
		l := &Loader{Dir: t.TempDir(), WorkDir: dir}
		r, err := l.Resolve(Overrides{})
		if err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
		if r.Host != DefaultHost {
			t.Errorf("beaver.toml host %q redirected the CLI to %q", host, r.Host)
		}
		if r.IgnoredProjectHost != strings.TrimSpace(host) {
			t.Errorf("the ignored host %q was not reported (got %q)", host, r.IgnoredProjectHost)
		}
		if r.Project != "p1" || r.App != "a1" {
			t.Errorf("project and app should still come from the file: %+v", r)
		}
	}
}

func quote(s string) string {
	return "\"" + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + "\""
}

func TestAProjectFileFoundInAParentDirectoryIsAlsoIgnoredForHost(t *testing.T) {
	clearEnv(t)
	root := t.TempDir()
	child := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, ProjectFile, "host = \"https://evil.test\"\n")
	r, err := (&Loader{Dir: t.TempDir(), WorkDir: child}).Resolve(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Host != DefaultHost {
		t.Fatalf("host = %q", r.Host)
	}
}

func TestHostPrecedenceIsFlagThenEnvThenProfileThenDefault(t *testing.T) {
	clearEnv(t)
	cfgDir := t.TempDir()
	write(t, cfgDir, "config.toml", "[profiles.default]\nhost = \"https://profile.example.test\"\n")
	l := &Loader{Dir: cfgDir, WorkDir: t.TempDir()}

	r, _ := l.Resolve(Overrides{})
	if r.Host != "https://profile.example.test" {
		t.Errorf("profile: %q", r.Host)
	}
	t.Setenv("BEAVER_HOST", "https://env.example.test/")
	r, _ = l.Resolve(Overrides{})
	if r.Host != "https://env.example.test" {
		t.Errorf("env: %q", r.Host)
	}
	r, _ = l.Resolve(Overrides{Host: "http://localhost:4000"})
	if r.Host != "http://localhost:4000" {
		t.Errorf("flag: %q", r.Host)
	}
	t.Setenv("BEAVER_HOST", "")
	empty, _ := (&Loader{Dir: t.TempDir(), WorkDir: t.TempDir()}).Resolve(Overrides{})
	if empty.Host != DefaultHost {
		t.Errorf("default: %q", empty.Host)
	}
}

func TestAnUnsafeHostFromAnyTrustedSourceIsRefused(t *testing.T) {
	clearEnv(t)
	cfgDir := t.TempDir()
	write(t, cfgDir, "config.toml", "[profiles.default]\nhost = \"http://api.example.test\"\n")
	if _, err := (&Loader{Dir: cfgDir, WorkDir: t.TempDir()}).Resolve(Overrides{}); err == nil {
		t.Error("a plain http profile host was accepted")
	}
	l := &Loader{Dir: t.TempDir(), WorkDir: t.TempDir()}
	if _, err := l.Resolve(Overrides{Host: "http://api.example.test"}); err == nil {
		t.Error("a plain http --host was accepted")
	}
	t.Setenv("BEAVER_HOST", "https://user:pw@api.example.test")
	_, err := l.Resolve(Overrides{})
	if err == nil {
		t.Fatal("userinfo in BEAVER_HOST was accepted")
	}
	if strings.Contains(err.Error(), "pw@") && !strings.Contains(err.Error(), "***") {
		t.Errorf("the password was echoed: %v", err)
	}
}

func TestIdentifiersFromAProjectFileAreValidated(t *testing.T) {
	clearEnv(t)
	good := []string{"my-project", "a", "A1_b-2", "3f2b8c1e-7a4d-4c1e-9b0a-123456789abc", strings.Repeat("a", 128)}
	bad := []string{
		"../x", "a/b", "a\\b", "..", ".", "a?b", "a#b", "a b", "-lead", "_lead", "a%2fb", "a%00", "a\nb",
		"a;b", "a.b", strings.Repeat("a", 129), "é", "a:b", "a@b",
	}
	for _, field := range []string{"project", "app"} {
		for _, v := range good {
			dir := t.TempDir()
			write(t, dir, ProjectFile, field+" = "+quote(v)+"\n")
			if _, err := (&Loader{Dir: t.TempDir(), WorkDir: dir}).Resolve(Overrides{}); err != nil {
				t.Errorf("%s %q refused: %v", field, v, err)
			}
		}
		for _, v := range bad {
			dir := t.TempDir()
			write(t, dir, ProjectFile, field+" = "+quote(v)+"\n")
			_, err := (&Loader{Dir: t.TempDir(), WorkDir: dir}).Resolve(Overrides{})
			if err == nil {
				t.Errorf("%s %q accepted", field, v)
			} else if !strings.Contains(err.Error(), ProjectFile) {
				t.Errorf("the error should name the file: %v", err)
			}
		}
	}
}

func TestValidID(t *testing.T) {
	if ValidID("") {
		t.Error("an empty id is not valid")
	}
	if !ValidID("x") || ValidID("x/y") {
		t.Error("ValidID is wrong")
	}
}
