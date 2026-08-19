package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The rate limiter at this service's own front door.
//
// It exists because of a gap that is easy to miss: authentication happens here,
// against the API, but a request carrying no credential at all is refused in
// this process before any API call is made. So every limiter on the API was
// blind to exactly the cheapest flood to mount. A caller could hold a socket
// open sending unauthenticated MCP requests all day and be counted by nothing.
//
// Three buckets, for the same reason the API has three. They are not
// interchangeable:
//
//	by token   the normal case. A credential is the only stable identity we
//	           have for a caller, and under a hosted MCP the address is not:
//	           every customer's agent arrives through the same ingress.
//	by address for requests with no credential, which have no other identity.
//	failures   the backstop for the first bucket's one weakness. Anyone can
//	           invent a token, and each invention is its own bucket up there.
//	           Counting only the attempts that failed means a working agent
//	           never touches this and a caller guessing tokens hits it at once.
//
// The window is fixed rather than sliding. A fixed window lets a caller send
// two windows' worth across a boundary, which is a real and well known
// weakness; it is accepted here because the alternative costs per-caller state
// proportional to the limit, and because this is a backstop rather than a
// quota. The ingress limit in deploy/*/mcp.yaml is the layer that survives this
// process being too busy to run its own accounting.

// RateLimitConfig bounds one caller. Zero values mean the defaults below, so a
// zero Config is a working Config rather than an open door.
type RateLimitConfig struct {
	// PerTokenPerMinute is what one credential may send. Higher than a
	// browser's allowance for the same reason a deploy is not a click: one
	// deploy that polls status while reading build logs is a hundred requests
	// without doing anything unreasonable.
	PerTokenPerMinute int
	// PerIPPerMinute is what one address may send without a credential. Low on
	// purpose: there is nothing legitimate to do here without one beyond
	// discovery, and discovery is a handful of requests.
	PerIPPerMinute int
	// FailedAuthPerMinute is how many rejected credentials one address may
	// present before it is cut off.
	FailedAuthPerMinute int
	// Disabled turns the whole thing off. For a local run, and for the case
	// where it is wrong and somebody needs it out of the way now.
	Disabled bool
}

const (
	defaultPerTokenPerMinute   = 600
	defaultPerIPPerMinute      = 60
	defaultFailedAuthPerMinute = 20
	rateLimitWindow            = time.Minute
)

func (c RateLimitConfig) resolved() RateLimitConfig {
	if c.PerTokenPerMinute <= 0 {
		c.PerTokenPerMinute = defaultPerTokenPerMinute
	}
	if c.PerIPPerMinute <= 0 {
		c.PerIPPerMinute = defaultPerIPPerMinute
	}
	if c.FailedAuthPerMinute <= 0 {
		c.FailedAuthPerMinute = defaultFailedAuthPerMinute
	}
	return c
}

type counter struct {
	count      int
	windowEnds time.Time
}

// rateLimiter is a fixed-window counter, kept in this process.
//
// Per process, and therefore per pod: with two replicas the real ceiling is
// twice each number here, and it resets on every rollout. That is stated in the
// documentation rather than hidden, because a limit nobody can predict is worse
// than a limit set higher on purpose.
type rateLimiter struct {
	cfg RateLimitConfig

	mu      sync.Mutex
	buckets map[string]*counter
	// now is swappable so the tests can cross a window boundary without
	// sleeping through one.
	now func() time.Time
}

func newRateLimiter(cfg RateLimitConfig) *rateLimiter {
	return &rateLimiter{
		cfg:     cfg.resolved(),
		buckets: map[string]*counter{},
		now:     time.Now,
	}
}

// allow records one hit against a key and reports whether it is within the
// limit. It returns how long until the window resets, for the Retry-After
// header: a client told to back off without being told for how long will guess,
// and it will guess wrong.
func (l *rateLimiter) allow(key string, limit int) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	// Sweep here rather than from a goroutine. The map only holds keys seen in
	// the last window, so it is bounded by traffic rather than by time, and a
	// sweep on the path that creates entries cannot leak while the process is
	// idle. Bounded again below in case one window brings a great many keys.
	if len(l.buckets) > maxRateLimitKeys {
		for k, c := range l.buckets {
			if now.After(c.windowEnds) {
				delete(l.buckets, k)
			}
		}
		if len(l.buckets) > maxRateLimitKeys {
			// Still too many live keys: that is a flood of distinct callers, and
			// dropping the table costs one window of accounting rather than
			// unbounded memory.
			l.buckets = map[string]*counter{}
		}
	}

	entry, ok := l.buckets[key]
	if !ok || now.After(entry.windowEnds) {
		entry = &counter{windowEnds: now.Add(rateLimitWindow)}
		l.buckets[key] = entry
	}
	entry.count++

	if entry.count > limit {
		return false, time.Until(entry.windowEnds)
	}
	return true, 0
}

// exceeded reports whether a key is already over its limit, WITHOUT recording a
// hit.
//
// This is what makes the failed-authentication counter worth having. Recording
// first and checking afterwards still costs one API round trip per attempt, so a
// caller guessing tokens is billed to us at full price and merely told off at
// the end. Checking first means the fourth guess never leaves this process.
func (l *rateLimiter) exceeded(key string, limit int) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.buckets[key]
	if !ok || now.After(entry.windowEnds) {
		return false, 0
	}
	if entry.count >= limit {
		return true, time.Until(entry.windowEnds)
	}
	return false, 0
}

// record counts a hit without caring whether it is within the limit. Used for
// the failure counter, whose check happens separately and earlier.
func (l *rateLimiter) record(key string) {
	l.allow(key, int(^uint(0)>>1))
}

// maxRateLimitKeys bounds the table. Chosen to be far above the number of
// distinct callers a minute of real traffic produces and far below anything
// that would matter against the pod's 256Mi.
const maxRateLimitKeys = 65536

// tokenKey buckets by credential without putting one in the key space.
//
// Hashed rather than used directly so that nothing places a live token
// somewhere it would sit in memory beside data with none of a credential's
// handling rules.
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "tok:" + hex.EncodeToString(sum[:8])
}

func ipKey(ip string) string   { return "ip:" + ip }
func failKey(ip string) string { return "fail:" + ip }

// tooMany writes the refusal.
func tooMany(w http.ResponseWriter, retryAfter time.Duration, message string) {
	seconds := int(retryAfter.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSONError(w, http.StatusTooManyRequests, "rate_limited", message)
}
