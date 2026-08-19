package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The HTTP front door.
//
// Three things happen here that are not the MCP protocol and that the protocol
// does not do for us:
//
//  1. The 401 challenge. Without a WWW-Authenticate header naming the
//     protected-resource metadata document, a client has no way to discover
//     where to authenticate, and a connector simply fails with nothing to act
//     on. It is a handful of lines and among the most important in the project.
//  2. Origin validation. A streamable HTTP server that answers any origin is a
//     DNS rebinding target: a page in a customer's browser could drive their
//     agent's session.
//  3. Per-caller server construction, so the tool list a client is shown is
//     the tool list that caller was actually granted.

// NewHandler builds the complete HTTP surface for the MCP service.
func NewHandler(cfg Config, logger *slog.Logger) http.Handler {
	apiClient := client.New(cfg.APIBaseURL, UserAgent)
	deps := &Deps{Client: apiClient, Auth: NewAuthenticator(apiClient), Config: cfg}
	registry := AllTools()

	trusted, invalidProxies := parseTrustedProxies(cfg.TrustedProxies)
	if len(invalidProxies) > 0 {
		// A typo here silently narrows the trust list, which shows up much later
		// as every customer sharing one rate-limit bucket. Say it out loud.
		logger.Error("MCP_TRUSTED_PROXIES has entries that are neither an IP nor a CIDR; they are ignored",
			"entries", invalidProxies)
	}
	if len(trusted) == 0 {
		trusted, _ = parseTrustedProxies(defaultTrustedProxyCIDRs)
	}
	limiter := newRateLimiter(cfg.RateLimit)

	mux := http.NewServeMux()

	// Liveness. Deliberately says nothing about the API behind us: a health
	// check that fails when a dependency is briefly unreachable gets the
	// container killed and turns a blip into a restart loop.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "beaver-mcp", "version": Version})
	})

	// RFC 9728. Served at both the bare path and the resource-suffixed one,
	// because clients differ on which they request and a discovery miss is
	// invisible from our side: the customer just sees a connector that will not
	// connect.
	metadata := protectedResourceMetadata(cfg)
	for _, path := range []string{
		ProtectedResourceMetadataPath,
		ProtectedResourceMetadataPath + MCPPath,
	} {
		// Discovery is static JSON, but it is also the one thing an
		// uncredentialed caller may legitimately fetch, so it gets the same
		// address-keyed allowance rather than none at all. /healthz deliberately
		// does not: the probes all arrive from the node and share a bucket, and
		// a 429 there is read by the platform as a dead container and answered
		// by killing it. That turns a busy minute into a restart loop.
		mux.Handle(path, withEdgeLimit(limiter, trusted, metadataHandler(metadata)))
	}

	streamable := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		identity := identityFromRequest(r)
		if identity == nil {
			return nil
		}
		return registry.Build(deps, identity)
	}, &mcpsdk.StreamableHTTPOptions{
		Logger: logger,
		// The default is 4 MiB. A tool call on this surface is a handful of
		// identifiers and some environment variables; nothing legitimate here is
		// megabytes.
		MaxRequestBodyBytes: 512 << 10,
	})

	guarded := withGuards(cfg, deps, logger, limiter, trusted, streamable)
	mux.Handle(MCPPath, guarded)
	mux.Handle(MCPPath+"/", guarded)

	return withRecovery(logger, mux)
}

// withEdgeLimit applies the address-keyed allowance to a handler that has no
// credential to key on.
func withEdgeLimit(limiter *rateLimiter, trusted []*net.IPNet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limiter.cfg.Disabled {
			next.ServeHTTP(w, r)
			return
		}
		if ok, retry := limiter.allow(ipKey(callerIP(r, trusted)), limiter.cfg.PerIPPerMinute); !ok {
			tooMany(w, retry, "Too many requests from this address. Slow down and try again shortly.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withGuards is the chain in front of the protocol: kill switch, rate limit,
// origin check, authentication.
//
// The rate limit sits ahead of the origin and authentication checks on purpose.
// Both of those refuse a request in this process without calling the API, which
// is what made an unauthenticated flood free before this existed: every limiter
// on the API was blind to it.
func withGuards(cfg Config, deps *Deps, logger *slog.Logger, limiter *rateLimiter, trusted []*net.IPNet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.Disabled {
			// The standing kill switch. An environment variable rather than a
			// service gate because it has to work when this service cannot reach
			// the API at all, which is exactly the situation where somebody
			// wants to turn it off.
			writeJSONError(w, http.StatusServiceUnavailable, "service_disabled",
				"The TechBeaver MCP service is temporarily switched off. Existing apps and databases are unaffected.")
			return
		}
		caller := callerIP(r, trusted)
		token := bearerFrom(r)
		// Attached before anything downstream runs, because the very first call
		// this makes is the authentication call, and that one needs forwarding
		// too: it is the request the API's failed-auth limiter counts.
		r = r.WithContext(client.WithCallerIP(r.Context(), caller))

		if !limiter.cfg.Disabled {
			// Keyed on the credential when there is one, because under a hosted
			// MCP the address is the same for every customer and one address
			// bucket would have them throttling each other.
			key, limit := ipKey(caller), limiter.cfg.PerIPPerMinute
			if token != "" {
				key, limit = tokenKey(token), limiter.cfg.PerTokenPerMinute
			}
			if ok, retry := limiter.allow(key, limit); !ok {
				tooMany(w, retry, "Too many requests. Slow down, and space out repeated polling.")
				return
			}
		}

		// Checked before authenticating, not after. A caller guessing tokens
		// that is only told off once the API has already answered still costs us
		// a round trip and an indexed lookup for every guess.
		if !limiter.cfg.Disabled && token != "" {
			if over, retry := limiter.exceeded(failKey(caller), limiter.cfg.FailedAuthPerMinute); over {
				tooMany(w, retry, "Too many rejected credentials from this address. Reconnect this client in the TechBeaver console.")
				return
			}
		}

		if !originAllowed(cfg, r) {
			// DNS rebinding. Refusing an unrecognised Origin is what stops a page
			// in a customer's browser driving their agent's session.
			writeJSONError(w, http.StatusForbidden, "origin_not_allowed",
				"This origin is not allowed to reach the TechBeaver MCP service.")
			return
		}

		if token == "" {
			challenge(w, cfg)
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated",
				"Connect this client to TechBeaver first.")
			return
		}

		identity, err := deps.Auth.Resolve(r.Context(), token)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				// Counted by address rather than by token, because a caller
				// guessing tokens gets a fresh token bucket per guess. The
				// budget is checked above, before the API is called at all.
				if !limiter.cfg.Disabled {
					limiter.record(failKey(caller))
				}
				challenge(w, cfg)
				writeJSONError(w, http.StatusUnauthorized, "invalid_token",
					"This connection is no longer authorised. Reconnect it in the TechBeaver console.")
				return
			}
			logger.Error("could not reach the TechBeaver API to authenticate a request", "error", err)
			writeJSONError(w, http.StatusBadGateway, "upstream_unavailable",
				"TechBeaver could not be reached to check this credential. Try again shortly.")
			return
		}

		ctx := WithSession(r.Context(), &Session{Token: token, Identity: identity})
		ctx = withIdentity(ctx, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type identityKey struct{}

func withIdentity(ctx context.Context, identity *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

func identityFromRequest(r *http.Request) *Identity {
	identity, _ := r.Context().Value(identityKey{}).(*Identity)
	return identity
}

// challenge writes the header that starts an OAuth flow.
//
// A client that gets a bare 401 has nowhere to go. A client that gets this
// knows which document to fetch, which authorization server to use, and can
// register itself and open a browser without anybody writing an integration.
func challenge(w http.ResponseWriter, cfg Config) {
	resource := cfg.ResourceIndicator()
	if resource == "" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		return
	}
	base := strings.TrimRight(cfg.PublicURL, "/")
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer resource_metadata=%q, error="invalid_token"`,
		base+ProtectedResourceMetadataPath+MCPPath))
}

// originAllowed decides whether a cross-origin caller may proceed.
//
// A request with no Origin header is not a browser and is allowed: that is
// every real MCP client, and a curl. A request WITH one has to name an origin
// we listed, or be same-origin with the service itself.
func originAllowed(cfg Config, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	if strings.EqualFold(origin, strings.TrimRight(cfg.PublicURL, "/")) {
		return true
	}
	for _, allowed := range cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(allowed), "/"), origin) {
			return true
		}
	}
	return false
}

// bearerFrom extracts the credential.
//
// Header only. This service deliberately does NOT accept a token in the query
// string, even though the API does for its event-stream endpoints: a URL ends
// up in proxy logs, browser history and referrer headers, and there is no
// browser here that needs the concession.
func bearerFrom(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return ""
	}
	return fields[1]
}

func protectedResourceMetadata(cfg Config) map[string]any {
	resource := cfg.ResourceIndicator()
	if resource == "" {
		resource = strings.TrimRight(cfg.PublicURL, "/")
	}
	return map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{strings.TrimRight(cfg.AuthorizationServer, "/")},
		"scopes_supported":         scopes.Issuables(),
		"bearer_methods_supported": []string{"header"},
		"resource_documentation":   cfg.DocsURL,
	}
}

func metadataHandler(metadata map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public discovery data, fetched cross-origin by clients that have no
		// relationship with us yet, so it is readable from anywhere. It contains
		// nothing but configuration.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metadata)
	})
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": code, "error_description": message})
}

// withRecovery keeps one bad request from taking the process down.
func withRecovery(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic serving an MCP request", "error", rec, "path", r.URL.Path)
				writeJSONError(w, http.StatusInternalServerError, "internal_error", "Something went wrong handling that request.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ParseOrigins splits a comma separated origin list, dropping blanks and
// anything that is not a usable scheme and host. A typo must not silently
// become an allowed origin, and it must not silently become nothing either, so
// the invalid entries are returned for the caller to complain about.
func ParseOrigins(raw string) (allowed, invalid []string) {
	for _, field := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(field)
		if entry == "" {
			continue
		}
		parsed, err := url.Parse(entry)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			invalid = append(invalid, entry)
			continue
		}
		allowed = append(allowed, parsed.Scheme+"://"+parsed.Host)
	}
	return allowed, invalid
}
