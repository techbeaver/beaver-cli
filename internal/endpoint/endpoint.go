// Package endpoint decides which hosts this CLI will send a credential to.
package endpoint

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ErrHostMismatch means a credential was issued for a different host than the
// one a request is about to go to.
var ErrHostMismatch = errors.New("credential was issued for a different host")

// Normalise validates an API host and returns its canonical origin:
// lowercase scheme and host, no trailing dot, no default port, no path.
//
// https is required for every host except loopback. Userinfo, paths, queries,
// fragments and non-ASCII hosts are refused, so two spellings of one origin
// compare equal and a lookalike never does.
func Normalise(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("the API host is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the API host %q is not a valid URL", redact(raw))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("the API host %q must start with https://", redact(raw))
	}
	if u.User != nil {
		return "", fmt.Errorf("the API host %q must not carry a username or password", redact(raw))
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "?#") {
		return "", fmt.Errorf("the API host %q must be a bare origin with no path, query or fragment", redact(raw))
	}
	hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if hostname == "" {
		return "", fmt.Errorf("the API host %q has no host name", redact(raw))
	}
	if !validHostname(hostname) {
		return "", fmt.Errorf("the API host %q is not a plain ASCII host name", redact(raw))
	}
	loopback := IsLoopback(hostname)
	if scheme == "http" && !loopback {
		return "", fmt.Errorf("the API host %q must use https (plain http is only allowed for localhost)", redact(raw))
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" && !validPort(port) {
		return "", fmt.Errorf("the API host %q has an invalid port", redact(raw))
	}
	hostPart := hostname
	if strings.Contains(hostname, ":") {
		hostPart = "[" + hostname + "]"
	}
	if port != "" {
		hostPart += ":" + port
	}
	return scheme + "://" + hostPart, nil
}

// Same reports whether two hosts normalise to the same origin.
func Same(a, b string) bool {
	na, err := Normalise(a)
	if err != nil {
		return false
	}
	nb, err := Normalise(b)
	return err == nil && na == nb
}

// Require returns an error unless issuedFor and target are the same origin.
// An empty issuedFor is a credential with no recorded host: it is only
// accepted when legacyDefault names the same origin as target.
func Require(issuedFor, target, legacyDefault string) error {
	nt, err := Normalise(target)
	if err != nil {
		return err
	}
	if strings.TrimSpace(issuedFor) == "" {
		if Same(legacyDefault, nt) {
			return nil
		}
		return fmt.Errorf("%w: this stored credential records no host, so it will only be sent to %s. Run beaver auth login for %s",
			ErrHostMismatch, legacyDefault, nt)
	}
	ni, err := Normalise(issuedFor)
	if err != nil || ni != nt {
		return fmt.Errorf("%w: it was issued for %s and this command would send it to %s. Nothing was sent. Run beaver auth login for that host, or use the host it was issued for",
			ErrHostMismatch, redact(issuedFor), nt)
	}
	return nil
}

// IsLoopback reports whether a bare host name is localhost or a loopback address.
func IsLoopback(hostname string) bool {
	hostname = strings.Trim(strings.ToLower(hostname), "[]")
	if hostname == "localhost" {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

// CheckRedirect refuses any redirect that leaves the origin of the first
// request or downgrades it, so a bearer credential cannot follow a 3xx to
// another host.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	first := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, first.Scheme) || !strings.EqualFold(req.URL.Host, first.Host) {
		return fmt.Errorf("refusing a redirect from %s to %s: credentials stay with the host that was asked", first.Host, req.URL.Host)
	}
	return nil
}

func validHostname(h string) bool {
	if strings.Contains(h, ":") {
		return net.ParseIP(h) != nil
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return !strings.HasPrefix(h, ".") && !strings.HasSuffix(h, ".") && !strings.Contains(h, "..")
}

func validPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	n := 0
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
		n = n*10 + int(r-'0')
	}
	return n > 0 && n < 65536
}

func redact(raw string) string {
	if i := strings.Index(raw, "@"); i >= 0 {
		if j := strings.Index(raw, "://"); j >= 0 && j < i {
			return raw[:j+3] + "***@" + raw[i+1:]
		}
	}
	return raw
}
