package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests run the real install.sh with a stand-in curl that serves a local
// directory and a stand-in cosign that records its arguments. Nothing touches
// the network.

type fixture struct {
	t       *testing.T
	release string // the directory the fake curl serves
	bin     string // PATH for the installer
	dest    string
	archive string
	version string
}

func platform(t *testing.T) string {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh supports linux and darwin")
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		t.Skip("unsupported arch")
	}
	return runtime.GOOS + "_" + runtime.GOARCH
}

func tarball(t *testing.T) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho beaver\n")
	if err := tw.WriteHeader(&tar.Header{Name: "beaver", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, version: "v1.2.3"}
	root := t.TempDir()
	f.release = filepath.Join(root, "release")
	f.bin = filepath.Join(root, "bin")
	f.dest = filepath.Join(root, "dest")
	for _, d := range []string{f.release, f.bin, f.dest} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.archive = fmt.Sprintf("beaver_1.2.3_%s.tar.gz", platform(t))
	data := tarball(t)
	f.put(f.archive, data)
	sum := sha256.Sum256(data)
	f.put("checksums.txt", []byte(hex.EncodeToString(sum[:])+"  "+f.archive+"\n"+strings.Repeat("0", 64)+"  other_file.zip\n"))
	f.put("checksums.txt.sig", []byte("sig"))
	f.put("checksums.txt.pem", []byte("pem"))

	for _, tool := range []string{"sh", "tar", "mktemp", "uname", "tr", "awk", "sed", "grep", "head", "sha256sum", "mv", "chmod", "mkdir", "rm", "cat", "dirname", "printf", "basename", "cp", "gzip"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(f.bin, tool))
		}
	}
	curl := `#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\n' "$url" >> "$FAKE_LOG"
name=$(basename "$url")
[ -f "$FAKE_RELEASE/$name" ] || exit 22
if [ -n "$out" ]; then cp "$FAKE_RELEASE/$name" "$out"; else cat "$FAKE_RELEASE/$name"; fi
`
	f.script("curl", curl)
	return f
}

func (f *fixture) put(name string, data []byte) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.release, name), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) script(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.bin, name), []byte(body), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) withCosign(exit int) string {
	log := filepath.Join(f.t.TempDir(), "cosign.args")
	f.script("cosign", fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\"; done > %q\nexit %d\n", log, exit))
	return log
}

func (f *fixture) run(env ...string) (string, error) {
	f.t.Helper()
	script, err := filepath.Abs("install.sh")
	if err != nil {
		f.t.Fatal(err)
	}
	sh, _ := exec.LookPath("sh")
	cmd := exec.Command(sh, script)
	cmd.Env = append([]string{
		"PATH=" + f.bin, "HOME=" + f.dest, "FAKE_RELEASE=" + f.release, "FAKE_LOG=" + f.urlLog(),
		"BEAVER_VERSION=v1.2.3", "BEAVER_INSTALL_DIR=" + f.dest,
	}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *fixture) urlLog() string { return filepath.Join(filepath.Dir(f.release), "urls.log") }

func (f *fixture) requested() string {
	raw, _ := os.ReadFile(f.urlLog())
	return string(raw)
}

func (f *fixture) installed() bool {
	_, err := os.Stat(filepath.Join(f.dest, "beaver"))
	return err == nil
}

func TestHappyPathWithoutCosignInstallsAndShoutsAboutTheMissingSignatureCheck(t *testing.T) {
	f := newFixture(t)
	out, err := f.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !f.installed() {
		t.Fatal("nothing was installed")
	}
	for _, want := range []string{"Checksum verified", "WARNING", "NOT verified", "BEAVER_REQUIRE_SIGNATURE=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRequireSignatureMakesAMissingCosignFatal(t *testing.T) {
	f := newFixture(t)
	out, err := f.run("BEAVER_REQUIRE_SIGNATURE=1")
	if err == nil || f.installed() {
		t.Fatalf("installed without a signature check:\n%s", out)
	}
	if !strings.Contains(out, "cosign") {
		t.Errorf("the error should name cosign:\n%s", out)
	}
}

func TestWithCosignTheSignatureIsVerifiedAgainstAnAnchoredIdentityAndAPinnedIssuer(t *testing.T) {
	f := newFixture(t)
	log := f.withCosign(0)
	out, err := f.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Signature verified") {
		t.Errorf("no confirmation:\n%s", out)
	}
	raw, _ := os.ReadFile(log)
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	value := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if args[0] != "verify-blob" {
		t.Fatalf("args %v", args)
	}
	if value("--certificate-oidc-issuer") != "https://token.actions.githubusercontent.com" {
		t.Errorf("issuer %q", value("--certificate-oidc-issuer"))
	}
	if value("--certificate-identity-regexp") != "" {
		t.Error("a regexp identity is back; the exact identity for the tag is required")
	}
	const real = "https://github.com/techbeaver/beaver-cli/.github/workflows/release.yml@refs/tags/"
	if got := value("--certificate-identity"); got != real+"v1.2.3" {
		t.Errorf("identity %q, want the exact workflow on the requested tag", got)
	}
	if !strings.HasSuffix(value("--signature"), "checksums.txt.sig") || !strings.HasSuffix(value("--certificate"), "checksums.txt.pem") {
		t.Errorf("args %v", args)
	}
	if !strings.HasSuffix(args[len(args)-1], "checksums.txt") {
		t.Errorf("the blob verified was %q", args[len(args)-1])
	}
}

func TestAFailedSignatureCheckIsFatalAndInstallsNothing(t *testing.T) {
	f := newFixture(t)
	f.withCosign(1)
	out, err := f.run()
	if err == nil || f.installed() {
		t.Fatalf("installed after a failed signature check:\n%s", out)
	}
	if !strings.Contains(out, "did not verify") {
		t.Errorf("%s", out)
	}
}

func TestMissingSignatureFilesWithCosignPresentIsFatal(t *testing.T) {
	for _, name := range []string{"checksums.txt.sig", "checksums.txt.pem"} {
		f := newFixture(t)
		f.withCosign(0)
		_ = os.Remove(filepath.Join(f.release, name))
		if out, err := f.run(); err == nil || f.installed() {
			t.Errorf("installed without %s:\n%s", name, out)
		}
	}
}

func TestChecksumProblemsAreAllFatal(t *testing.T) {
	good := func(f *fixture) string {
		raw, _ := os.ReadFile(filepath.Join(f.release, "checksums.txt"))
		return strings.Fields(string(raw))[0]
	}
	cases := map[string]func(f *fixture){
		"checksums.txt missing": func(f *fixture) { _ = os.Remove(filepath.Join(f.release, "checksums.txt")) },
		"checksums.txt empty":   func(f *fixture) { f.put("checksums.txt", nil) },
		"archive not listed":    func(f *fixture) { f.put("checksums.txt", []byte(strings.Repeat("a", 64)+"  something_else.tar.gz\n")) },
		"wrong hash":            func(f *fixture) { f.put("checksums.txt", []byte(strings.Repeat("a", 64)+"  "+f.archive+"\n")) },
		"short hash":            func(f *fixture) { f.put("checksums.txt", []byte(good(f)[:63]+"  "+f.archive+"\n")) },
		"long hash":             func(f *fixture) { f.put("checksums.txt", []byte(good(f)+"0  "+f.archive+"\n")) },
		"uppercase hash":        func(f *fixture) { f.put("checksums.txt", []byte(strings.ToUpper(good(f))+"  "+f.archive+"\n")) },
		"non hex hash":          func(f *fixture) { f.put("checksums.txt", []byte(strings.Repeat("g", 64)+"  "+f.archive+"\n")) },
		"listed twice": func(f *fixture) {
			f.put("checksums.txt", []byte(good(f)+"  "+f.archive+"\n"+good(f)+"  "+f.archive+"\n"))
		},
		"listed twice, one bad": func(f *fixture) {
			f.put("checksums.txt", []byte(good(f)+"  "+f.archive+"\n"+strings.Repeat("b", 64)+"  "+f.archive+"\n"))
		},
		"only a substring name": func(f *fixture) { f.put("checksums.txt", []byte(good(f)+"  x"+f.archive+"\n")) },
		"archive tampered": func(f *fixture) {
			f.put(f.archive, append(tarball(f.t), 'x'))
		},
		"archive missing": func(f *fixture) { _ = os.Remove(filepath.Join(f.release, f.archive)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(f)
			out, err := f.run()
			if err == nil {
				t.Fatalf("the install succeeded:\n%s", out)
			}
			if f.installed() {
				t.Fatal("a binary was installed")
			}
		})
	}
}

func TestChecksumProblemsAreFatalEvenWhenTheSignatureIsFine(t *testing.T) {
	f := newFixture(t)
	f.withCosign(0)
	f.put("checksums.txt", []byte(strings.Repeat("a", 64)+"  "+f.archive+"\n"))
	if out, err := f.run(); err == nil || f.installed() {
		t.Fatalf("a valid signature excused a bad checksum:\n%s", out)
	}
}

func TestNoChecksumToolMeansNoInstall(t *testing.T) {
	f := newFixture(t)
	_ = os.Remove(filepath.Join(f.bin, "sha256sum"))
	out, err := f.run()
	if err == nil || f.installed() {
		t.Fatalf("installed with no way to verify:\n%s", out)
	}
	if !strings.Contains(out, "sha256sum") {
		t.Errorf("%s", out)
	}
}

func TestShasumIsAcceptedWhenSha256sumIsAbsent(t *testing.T) {
	f := newFixture(t)
	real, _ := exec.LookPath("sha256sum")
	_ = os.Remove(filepath.Join(f.bin, "sha256sum"))
	f.script("shasum", fmt.Sprintf("#!/bin/sh\nshift 2\nexec %q \"$@\"\n", real)) // shasum -a 256 FILE
	if out, err := f.run(); err != nil || !f.installed() {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestAHostileVersionNeverReachesAURL(t *testing.T) {
	for _, v := range []string{"v1.2.3/../../x", "v1.2.3;rm -rf /", "$(id)", "1.2", "latest", "v1.2.3 ", "../v1.2.3", "v1.2.3?x=1", "v1.2.3#f"} {
		f := newFixture(t)
		out, err := f.run("BEAVER_VERSION=" + v)
		if err == nil || f.installed() {
			t.Errorf("version %q was accepted:\n%s", v, out)
		}
		if got := f.requested(); got != "" {
			t.Errorf("version %q reached the network: %s", v, got)
		}
	}
	f := newFixture(t)
	if out, err := f.run("BEAVER_VERSION=1.2.3"); err != nil {
		t.Errorf("a version without the v prefix should be accepted: %v\n%s", err, out)
	}
}

func TestNothingIsLeftBehindOnFailure(t *testing.T) {
	f := newFixture(t)
	f.put("checksums.txt", []byte(strings.Repeat("a", 64)+"  "+f.archive+"\n"))
	tmpdir := t.TempDir()
	_, _ = f.run("TMPDIR=" + tmpdir)
	left, _ := os.ReadDir(tmpdir)
	if len(left) != 0 {
		t.Errorf("temporary files were left in %s: %v", tmpdir, left)
	}
	if entries, _ := os.ReadDir(f.dest); len(entries) != 0 {
		t.Errorf("the destination was touched: %v", entries)
	}
}
