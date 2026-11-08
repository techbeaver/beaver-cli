package auth

import (
	"net/http"
	"time"

	"github.com/techbeaver/beaver-cli/internal/endpoint"
)

// apiURL joins a validated origin and a path. Every request this package makes
// goes through it, so a credential or identity token only ever goes to a host
// that passed [endpoint.Normalise].
func apiURL(host, path string) (string, error) {
	origin, err := endpoint.Normalise(host)
	if err != nil {
		return "", err
	}
	return origin + path, nil
}

// authClient is the HTTP client for the authorization endpoints. It follows no
// redirects: a token endpoint that redirects is not one to trust with a code,
// a refresh token or an identity token.
func authClient() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// originOf returns the normalised origin recorded on a credential. It is only
// called after apiURL accepted the same host, so the error cannot occur.
func originOf(host string) string {
	origin, _ := endpoint.Normalise(host)
	return origin
}
