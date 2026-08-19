package mcp

import (
	"strings"
)

// Config is everything this service needs to run, and it is deliberately all
// addresses and switches.
//
// There is no database URL, no cluster kubeconfig and no payment key here, and
// there is nowhere to put one. That is the property the whole design rests on.
type Config struct {
	// Addr is the listen address, e.g. ":4600".
	Addr string

	// APIBaseURL is the TechBeaver API this service calls, e.g.
	// https://cloudapi.techbeaver.io. Every tool call goes through it, carrying
	// the caller's own token.
	APIBaseURL string

	// PublicURL is the origin customers and AI clients reach this service at,
	// e.g. https://mcp.techbeaver.io.
	//
	// It is load-bearing in a way that is easy to miss: it forms the OAuth
	// resource identifier tokens are bound to, so changing it invalidates every
	// issued token, and it is what the 401 challenge points a client at to
	// discover how to authenticate. Pick it before anything ships; moving it
	// afterwards is a breaking change inflicted on customers' configs and CI.
	PublicURL string

	// AuthorizationServer is the issuer clients are sent to, which is the API's
	// own origin: the authorization server and the resource server are the same
	// system here.
	AuthorizationServer string

	// AllowedOrigins are the browser origins permitted to call /mcp.
	//
	// Browsers are not the expected client. The list exists so that permitting
	// one is a deliberate act rather than the result of leaving cross-origin
	// checks off, which is how DNS rebinding gets in.
	AllowedOrigins []string

	// Disabled takes the whole surface down without a deploy. An environment
	// variable rather than a service gate, because it has to work when this
	// service cannot reach the API at all.
	Disabled bool

	// DocsURL is published in the discovery documents.
	DocsURL string

	// TrustedProxies are the peers allowed to state a caller's real address in
	// a CF-Connecting-IP header. Empty means the private ranges, which is the
	// right answer behind a ClusterIP Service: see clientip.go for why that
	// default is safe rather than merely convenient.
	TrustedProxies []string

	// RateLimit bounds what one caller may do at this edge, before anything
	// reaches the API.
	RateLimit RateLimitConfig
}

// ResourceIndicator is the RFC 8707 resource identifier tokens for this service
// are bound to. Empty when the service has no public identity yet, which
// disables the audience check and is the right degrade for a local run.
func (c Config) ResourceIndicator() string {
	base := strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	if base == "" {
		return ""
	}
	return base + MCPPath
}

// MCPPath is where the streamable HTTP transport is mounted.
const MCPPath = "/mcp"

// ProtectedResourceMetadataPath is the RFC 9728 discovery document a client
// fetches after being challenged.
const ProtectedResourceMetadataPath = "/.well-known/oauth-protected-resource"
