package endpoint

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormaliseAcceptsAndCanonicalises(t *testing.T) {
	cases := map[string]string{
		"https://cloudapi.techbeaver.io":      "https://cloudapi.techbeaver.io",
		"https://cloudapi.techbeaver.io/":     "https://cloudapi.techbeaver.io",
		"HTTPS://CloudAPI.TechBeaver.IO":      "https://cloudapi.techbeaver.io",
		"https://cloudapi.techbeaver.io.":     "https://cloudapi.techbeaver.io",
		"https://cloudapi.techbeaver.io:443":  "https://cloudapi.techbeaver.io",
		"https://cloudapi.techbeaver.io:8443": "https://cloudapi.techbeaver.io:8443",
		"  https://api.example.test  ":        "https://api.example.test",
		"http://localhost":                    "http://localhost",
		"http://localhost:8080/":              "http://localhost:8080",
		"http://127.0.0.1:9000":               "http://127.0.0.1:9000",
		"http://[::1]:9000":                   "http://[::1]:9000",
		"https://127.0.0.1:9000":              "https://127.0.0.1:9000",
		"http://127.0.0.1:80":                 "http://127.0.0.1",
		"https://xn--bcher-kva.example":       "https://xn--bcher-kva.example",
		"https://a.b.c.d.example.test":        "https://a.b.c.d.example.test",
		"http://LOCALHOST.":                   "http://localhost",
		"https://example.test:1":              "https://example.test:1",
		"https://example.test:65535":          "https://example.test:65535",
	}
	for in, want := range cases {
		got, err := Normalise(in)
		if err != nil {
			t.Errorf("Normalise(%q) refused: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormaliseRefusesWhatCouldRedirectACredential(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"cloudapi.techbeaver.io",
		"ftp://cloudapi.techbeaver.io",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"//cloudapi.techbeaver.io",
		"http://cloudapi.techbeaver.io",
		"http://localhost.evil.test",
		"http://127.0.0.1.evil.test",
		"http://10.0.0.1",
		"http://0.0.0.0",
		"http://[::]",
		"http://169.254.169.254",
		"https://user@cloudapi.techbeaver.io",
		"https://cloudapi.techbeaver.io@evil.test",
		"https://user:pass@cloudapi.techbeaver.io",
		"https://cloudapi.techbeaver.io/api",
		"https://cloudapi.techbeaver.io/../",
		"https://cloudapi.techbeaver.io?x=1",
		"https://cloudapi.techbeaver.io#frag",
		"https://cloudapi.techbeaver.io\\@evil.test",
		"https://cloudapi.techbeaver.io:0",
		"https://cloudapi.techbeaver.io:65536",
		"https://cloudapi.techbeaver.io:abc",
		"https://cloudapi.techbeaver.io:-1",
		"https://cloudapi..techbeaver.io",
		"https://.cloudapi.techbeaver.io",
		"https://cloudapi.techbeaver.io..",
		"https://cloudaрi.techbeaver.io",
		"https://cloud api.techbeaver.io",
		"https://cloudapi.techbeaver.io%2f@evil.test",
		"https://%63loudapi.techbeaver.io",
		"https://",
		"https://:443",
		"https://exa_mple.test",
	}
	for _, in := range bad {
		if got, err := Normalise(in); err == nil {
			t.Errorf("Normalise(%q) accepted and returned %q", in, got)
		}
	}
}

func TestRefusalNeverEchoesACredentialInTheHost(t *testing.T) {
	_, err := Normalise("https://user:hunter2@cloudapi.techbeaver.io")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaked or missing: %v", err)
	}
}

func TestSameTreatsSpellingsOfOneOriginAsEqualAndLookalikesAsDifferent(t *testing.T) {
	const real = "https://cloudapi.techbeaver.io"
	same := []string{
		real, real + "/", "HTTPS://CLOUDAPI.TECHBEAVER.IO", real + ".", real + ":443",
	}
	for _, s := range same {
		if !Same(real, s) {
			t.Errorf("%q should equal %q", s, real)
		}
	}
	different := []string{
		"https://cloudapi.techbeaver.io.evil.test",
		"https://cloudapi.techbeaver.io.evil.test.",
		"https://evil.test/cloudapi.techbeaver.io",
		"https://cloudapi-techbeaver.io",
		"https://cloudapi.techbeaver.com",
		"https://xcloudapi.techbeaver.io",
		"https://cloudapi.techbeaver.io:8443",
		"https://api.techbeaver.io",
		"https://techbeaver.io",
		"https://cloudapi.techbeaver.io@evil.test",
		"http://cloudapi.techbeaver.io",
		"",
		"not a url",
	}
	for _, d := range different {
		if Same(real, d) {
			t.Errorf("%q must not equal %q", d, real)
		}
		if Same(d, real) {
			t.Errorf("%q must not equal %q (reversed)", real, d)
		}
	}
	if Same("", "") {
		t.Error("two invalid hosts must not be considered the same")
	}
}

func TestRequire(t *testing.T) {
	const def = "https://cloudapi.techbeaver.io"
	if err := Require(def, def+"/", def); err != nil {
		t.Errorf("same host refused: %v", err)
	}
	if err := Require("", def, def); err != nil {
		t.Errorf("a legacy credential with no host should go to the default host: %v", err)
	}
	if err := Require("", "https://other.example.test", def); !errors.Is(err, ErrHostMismatch) {
		t.Errorf("a legacy credential must not go to a non-default host: %v", err)
	}
	if err := Require("   ", "https://other.example.test", def); !errors.Is(err, ErrHostMismatch) {
		t.Errorf("a blank host must count as no host: %v", err)
	}
	for _, target := range []string{"https://evil.test", "https://cloudapi.techbeaver.io.evil.test", "http://cloudapi.techbeaver.io", "garbage", ""} {
		if err := Require(def, target, def); err == nil {
			t.Errorf("a credential for %s was allowed to go to %q", def, target)
		}
	}
	if err := Require("garbage", def, def); err == nil {
		t.Error("a credential with an unparseable recorded host was trusted")
	}
	// A lenient legacyDefault must not rescue an invalid target.
	if err := Require("", "http://cloudapi.techbeaver.io", "http://cloudapi.techbeaver.io"); err == nil {
		t.Error("plain http to a non-loopback host was allowed because it matched the default")
	}
}

func TestIsLoopback(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST", "127.0.0.1", "127.1.2.3", "::1", "[::1]"} {
		if !IsLoopback(h) {
			t.Errorf("%q should be loopback", h)
		}
	}
	for _, h := range []string{"", "localhost.evil.test", "evil-localhost", "10.0.0.1", "0.0.0.0", "192.168.1.1", "example.test", "127.0.0.1.nip.io"} {
		if IsLoopback(h) {
			t.Errorf("%q must not be loopback", h)
		}
	}
}

func TestCheckRedirectKeepsCredentialsWithTheirHost(t *testing.T) {
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits++
		w.WriteHeader(200)
	}))
	defer elsewhere.Close()

	cases := map[string]struct {
		location string
		followed bool
	}{
		"other host":  {elsewhere.URL + "/steal", false},
		"same origin": {"/ok", true},
		"downgrade":   {"", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var origin *httptest.Server
			origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ok" {
					w.WriteHeader(200)
					return
				}
				loc := c.location
				if name == "downgrade" {
					loc = strings.Replace(origin.URL, "http://", "https://", 1)
				}
				http.Redirect(w, r, loc, http.StatusFound)
			}))
			defer origin.Close()
			hc := &http.Client{CheckRedirect: CheckRedirect}
			req, _ := http.NewRequest(http.MethodGet, origin.URL+"/start", nil)
			req.Header.Set("Authorization", "Bearer secret")
			resp, err := hc.Do(req)
			if c.followed {
				if err != nil || resp.StatusCode != 200 {
					t.Fatalf("a same-origin redirect should be followed: %v", err)
				}
				_ = resp.Body.Close()
				return
			}
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("a redirect that leaves the origin was followed")
			}
		})
	}
	if elsewhereHits != 0 {
		t.Fatalf("the other host received %d request(s)", elsewhereHits)
	}
}

func TestCheckRedirectStopsLoops(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/again", http.StatusFound)
	}))
	defer srv.Close()
	hc := &http.Client{CheckRedirect: CheckRedirect}
	if resp, err := hc.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a redirect loop was followed forever")
	}
}
