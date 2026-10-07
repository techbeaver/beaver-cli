package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/techbeaver/beaver-cli/client"
)

// Who a request is acting for, and what it is allowed to do.
//
// This service validates nothing itself: it has no token store and no database,
// which is the whole point. It asks the API, which is the only thing that can
// answer, and caches the answer for seconds rather than minutes so that a
// customer revoking a client in the portal is obeyed almost immediately.

// Identity is what /mcp/whoami said about the presented token.
type Identity struct {
	UserID         string   `json:"userId"`
	Email          string   `json:"email"`
	FirstName      string   `json:"firstName,omitempty"`
	LastName       string   `json:"lastName,omitempty"`
	OrganisationID string   `json:"organisationId,omitempty"`
	Principal      string   `json:"principal"`
	ClientName     string   `json:"clientName,omitempty"`
	TokenPrefix    string   `json:"tokenPrefix,omitempty"`
	Scopes         []string `json:"scopes"`
	SpendCapMinor  int64    `json:"spendCapMinor"`
	// SpentThisMonthMinor is what a partner key has spent this calendar month without a payment
	// page. Zero for every other token.
	SpentThisMonthMinor int64 `json:"spentThisMonthMinor"`

	CanSpendWithoutCheckout bool `json:"canSpendWithoutCheckout"`
}

// Has reports whether the identity holds a scope.
func (i *Identity) Has(scope string) bool {
	if i == nil {
		return false
	}
	for _, s := range i.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// HasAll reports whether the identity holds every scope. An empty requirement
// is satisfied by any valid identity.
func (i *Identity) HasAll(required []string) bool {
	for _, s := range required {
		if !i.Has(s) {
			return false
		}
	}
	return true
}

// ErrUnauthenticated means the presented token is not usable. The HTTP layer
// turns it into a 401 with the challenge that starts an OAuth flow.
var ErrUnauthenticated = errors.New("the presented token is not valid")

// identityCacheTTL is how long a whoami answer is trusted.
//
// Seconds, not minutes, and the reason is revocation. A customer who
// disconnects an AI client expects it to stop working, and every second of
// cache is a second where it still does. Set against that, an uncached lookup
// per tool call would add a round trip to every call an agent makes. A few
// seconds is short enough that "revoked" and "stopped working" are the same
// event from a customer's point of view.
const identityCacheTTL = 10 * time.Second

type cachedIdentity struct {
	identity *Identity
	expires  time.Time
}

// Authenticator resolves tokens to identities, with a small cache.
type Authenticator struct {
	api *client.Client

	mu    sync.Mutex
	cache map[string]cachedIdentity
}

// NewAuthenticator builds an authenticator over an API client.
func NewAuthenticator(api *client.Client) *Authenticator {
	return &Authenticator{api: api, cache: map[string]cachedIdentity{}}
}

// Resolve returns who a token acts for.
func (a *Authenticator) Resolve(ctx context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	key := cacheKey(token)

	a.mu.Lock()
	if entry, ok := a.cache[key]; ok && time.Now().Before(entry.expires) {
		a.mu.Unlock()
		return entry.identity, nil
	}
	a.mu.Unlock()

	env, err := a.api.Do(ctx, token, client.Request{Method: http.MethodGet, Path: "/mcp/whoami"})
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
			// Negative answers are never cached. ADR 0016.
			return nil, ErrUnauthenticated
		}
		return nil, err
	}

	identity, err := decodeInto[Identity](env)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.cache[key] = cachedIdentity{identity: identity, expires: time.Now().Add(identityCacheTTL)}
	// Bounded by wiping wholesale; an obviously correct bound beats a clever one.
	if len(a.cache) > 4096 {
		a.cache = map[string]cachedIdentity{}
	}
	a.mu.Unlock()
	return identity, nil
}

// Forget drops a token's cached identity. Called after anything that could have
// changed what the token may do.
func (a *Authenticator) Forget(token string) {
	a.mu.Lock()
	delete(a.cache, cacheKey(token))
	a.mu.Unlock()
}

// cacheKey hashes the token so a live credential is never a map key sitting in
// memory beside data with none of a credential's handling rules.
func cacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Session is what a tool handler needs: the caller's token and who they are.
type Session struct {
	Token    string
	Identity *Identity
}

type sessionKey struct{}

// WithSession attaches a session to a context.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFrom returns the session for the current tool call.
//
// A tool that cannot find one must fail rather than proceed: there is no
// sensible default caller, and inventing one would mean acting for nobody.
func SessionFrom(ctx context.Context) (*Session, error) {
	s, _ := ctx.Value(sessionKey{}).(*Session)
	if s == nil || s.Token == "" {
		return nil, errors.New("this tool call carried no TechBeaver credential")
	}
	return s, nil
}
