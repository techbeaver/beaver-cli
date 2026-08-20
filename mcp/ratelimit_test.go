package mcp

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/techbeaver/beaver-cli/client"
)

// The edge limiter exists to close a gap that every limiter on the API was
// blind to: a request carrying no credential is refused in this process, before
// any API call, so nothing counted it. These tests are mostly about that.

func TestUncredentialedRequestsAreCountedAndRefused(t *testing.T) {
	limiter := newRateLimiter(RateLimitConfig{PerIPPerMinute: 3})

	for i := 1; i <= 3; i++ {
		if ok, _ := limiter.allow(ipKey("1.2.3.4"), limiter.cfg.PerIPPerMinute); !ok {
			t.Fatalf("request %d was refused while still inside the limit", i)
		}
	}
	ok, retry := limiter.allow(ipKey("1.2.3.4"), limiter.cfg.PerIPPerMinute)
	if ok {
		t.Fatal("the fourth request was allowed past a limit of three")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("Retry-After would be nonsense: %v. A client told to back off without being told for how long will guess wrong", retry)
	}
}

func TestOneCallerCannotExhaustAnother(t *testing.T) {
	limiter := newRateLimiter(RateLimitConfig{PerIPPerMinute: 2})

	limiter.allow(ipKey("1.1.1.1"), 2)
	limiter.allow(ipKey("1.1.1.1"), 2)
	if ok, _ := limiter.allow(ipKey("1.1.1.1"), 2); ok {
		t.Fatal("the flooding caller was not cut off")
	}
	if ok, _ := limiter.allow(ipKey("2.2.2.2"), 2); !ok {
		t.Fatal("an unrelated caller was throttled by somebody else's flood")
	}
}

func TestTheWindowResets(t *testing.T) {
	limiter := newRateLimiter(RateLimitConfig{PerIPPerMinute: 1})
	now := time.Now()
	limiter.now = func() time.Time { return now }

	limiter.allow(ipKey("1.2.3.4"), 1)
	if ok, _ := limiter.allow(ipKey("1.2.3.4"), 1); ok {
		t.Fatal("a second request inside the window was allowed")
	}

	now = now.Add(rateLimitWindow + time.Second)
	if ok, _ := limiter.allow(ipKey("1.2.3.4"), 1); !ok {
		t.Fatal("the caller was still refused after the window had passed, so the limit is permanent rather than per minute")
	}
}

func TestTheTokenKeyDoesNotContainTheToken(t *testing.T) {
	key := tokenKey("btk_averysecretvalue")
	if strings.Contains(key, "averysecretvalue") {
		t.Fatalf("a live credential is sitting in a rate-limit key: %q", key)
	}
	if tokenKey("btk_a") == tokenKey("btk_b") {
		t.Fatal("two different tokens share a bucket")
	}
}

func TestTheBucketTableIsBounded(t *testing.T) {
	limiter := newRateLimiter(RateLimitConfig{PerIPPerMinute: 100})
	for i := 0; i < maxRateLimitKeys+2_000; i++ {
		limiter.allow(ipKey(string(rune(i%1000))+"-"+time.Duration(i).String()), 100)
	}
	if len(limiter.buckets) > maxRateLimitKeys+1 {
		t.Fatalf("the bucket table grew to %d entries, so a flood of distinct callers is a memory leak", len(limiter.buckets))
	}
}

// --- caller address ---------------------------------------------------------

func TestAForwardedAddressIsReadOnlyFromATrustedPeer(t *testing.T) {
	trusted, _ := parseTrustedProxies(defaultTrustedProxyCIDRs)

	fromIngress := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	fromIngress.RemoteAddr = "10.42.0.9:34567"
	fromIngress.Header.Set(client.HeaderCFConnectingIP, "203.0.113.7")
	if got := callerIP(fromIngress, trusted); got != "203.0.113.7" {
		t.Fatalf("a trusted peer's forwarded address was ignored: got %q", got)
	}

	// From an untrusted peer: honouring it lets a caller pick its own bucket.
	direct := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	direct.RemoteAddr = "198.51.100.4:34567"
	direct.Header.Set(client.HeaderCFConnectingIP, "203.0.113.7")
	if got := callerIP(direct, trusted); got != "198.51.100.4" {
		t.Fatalf("an untrusted peer was allowed to state its own address: got %q", got)
	}
}

func TestAForwardedAddressThatIsNotAnAddressIsRefused(t *testing.T) {
	trusted, _ := parseTrustedProxies(defaultTrustedProxyCIDRs)

	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.RemoteAddr = "10.42.0.9:34567"
	// This becomes a limiter key and reaches the API, so never arbitrary text.
	r.Header.Set(client.HeaderCFConnectingIP, "not-an-address, 1.2.3.4")
	if got := callerIP(r, trusted); got != "10.42.0.9" {
		t.Fatalf("arbitrary text was accepted as a caller address: got %q", got)
	}
}

func TestTrustedProxyParsingReportsWhatItCouldNotRead(t *testing.T) {
	nets, invalid := parseTrustedProxies([]string{"10.0.0.0/8", "  ", "192.168.1.5", "not-a-cidr"})
	if len(nets) != 2 {
		t.Fatalf("expected two usable entries, got %d", len(nets))
	}
	if len(invalid) != 1 || invalid[0] != "not-a-cidr" {
		t.Fatalf("a typo was swallowed silently: %v. It would show up much later as every customer sharing one bucket", invalid)
	}
}

// --- the guard chain --------------------------------------------------------

// stubAPI answers /mcp/whoami however the test asks it to, so the guard chain
// can be driven without a database.
func stubAPI(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAnUncredentialedFloodIsRefusedWithoutCallingTheAPI(t *testing.T) {
	var apiCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(api.Close)

	handler := NewHandler(Config{
		APIBaseURL: api.URL,
		PublicURL:  "https://mcp.example.com",
		RateLimit:  RateLimitConfig{PerIPPerMinute: 2},
	}, testLogger())

	statuses := make([]int, 0, 4)
	for i := 0; i < 4; i++ {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		r.RemoteAddr = "10.42.0.9:1234"
		r.Header.Set(client.HeaderCFConnectingIP, "203.0.113.7")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		statuses = append(statuses, w.Code)
	}

	// First two for having no credential, the rest for volume. Once free forever.
	if statuses[0] != http.StatusUnauthorized || statuses[1] != http.StatusUnauthorized {
		t.Fatalf("expected the first two to be 401, got %v", statuses)
	}
	if statuses[2] != http.StatusTooManyRequests || statuses[3] != http.StatusTooManyRequests {
		t.Fatalf("an uncredentialed flood was not rate limited: %v", statuses)
	}
	if apiCalls != 0 {
		t.Fatalf("a request with no credential reached the API %d times; it should never leave this process", apiCalls)
	}
}

func TestGuessedTokensAreCutOffByAddress(t *testing.T) {
	var apiCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"no"}`))
	}))
	t.Cleanup(api.Close)

	handler := NewHandler(Config{
		APIBaseURL: api.URL,
		PublicURL:  "https://mcp.example.com",
		// Generous per token, tight on failures. ADR 0016.
		RateLimit: RateLimitConfig{PerTokenPerMinute: 1000, PerIPPerMinute: 1000, FailedAuthPerMinute: 3},
	}, testLogger())

	var lastStatus int
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		r.RemoteAddr = "10.42.0.9:1234"
		r.Header.Set(client.HeaderCFConnectingIP, "203.0.113.7")
		// A different invented token every time.
		r.Header.Set("Authorization", "Bearer btk_guess"+strings.Repeat("x", i))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		lastStatus = w.Code
	}

	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("a caller guessing tokens was never cut off; last status %d", lastStatus)
	}
	if apiCalls > 3 {
		t.Fatalf("token guessing cost %d API round trips; the failure counter should have stopped it at 3", apiCalls)
	}
}

func TestHealthIsNeverRateLimited(t *testing.T) {
	handler := NewHandler(Config{
		APIBaseURL: "http://127.0.0.1:1",
		PublicURL:  "https://mcp.example.com",
		RateLimit:  RateLimitConfig{PerIPPerMinute: 1},
	}, testLogger())

	// Probes share the node's bucket, and a 429 here reads as a dead container.
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.RemoteAddr = "10.42.0.1:5678"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("/healthz answered %d on probe %d; a rate-limited health check gets the container killed", w.Code, i)
		}
	}
}

func TestDiscoveryIsRateLimited(t *testing.T) {
	handler := NewHandler(Config{
		APIBaseURL: "http://127.0.0.1:1",
		PublicURL:  "https://mcp.example.com",
		RateLimit:  RateLimitConfig{PerIPPerMinute: 2},
	}, testLogger())

	var last int
	for i := 0; i < 4; i++ {
		r := httptest.NewRequest(http.MethodGet, ProtectedResourceMetadataPath, nil)
		r.RemoteAddr = "10.42.0.9:1234"
		r.Header.Set(client.HeaderCFConnectingIP, "203.0.113.7")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("the discovery document was served unboundedly; last status %d", last)
	}
}

func TestARefusalTellsTheClientHowLongToWait(t *testing.T) {
	handler := NewHandler(Config{
		APIBaseURL: "http://127.0.0.1:1",
		PublicURL:  "https://mcp.example.com",
		RateLimit:  RateLimitConfig{PerIPPerMinute: 1},
	}, testLogger())

	var recorder *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		r.RemoteAddr = "10.42.0.9:1234"
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
	}

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected a 429, got %d", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("a 429 with no Retry-After leaves the client to guess, and it will guess wrong")
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal was not JSON, so a client cannot act on it: %v", err)
	}
}

func TestRateLimitingCanBeSwitchedOff(t *testing.T) {
	handler := NewHandler(Config{
		APIBaseURL: "http://127.0.0.1:1",
		PublicURL:  "https://mcp.example.com",
		RateLimit:  RateLimitConfig{Disabled: true},
	}, testLogger())

	for i := 0; i < 50; i++ {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		r.RemoteAddr = "10.42.0.9:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited even though limiting is off", i)
		}
	}
}

func TestTheCallerAddressIsForwardedToTheAPI(t *testing.T) {
	seen := make(chan string, 4)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(client.HeaderCFConnectingIP)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"userId":"u1","email":"a@b.c","scopes":["paas:read"]}}`))
	}))
	t.Cleanup(api.Close)

	handler := NewHandler(Config{
		APIBaseURL: api.URL,
		PublicURL:  "https://mcp.example.com",
	}, testLogger())

	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	r.RemoteAddr = "10.42.0.9:1234"
	r.Header.Set(client.HeaderCFConnectingIP, "203.0.113.7")
	r.Header.Set("Authorization", "Bearer btk_real")
	handler.ServeHTTP(httptest.NewRecorder(), r)

	select {
	case forwarded := <-seen:
		// Without this the API sees this pod for the whole customer base. ADR 0016.
		if forwarded != "203.0.113.7" {
			t.Fatalf("the API was told the caller was %q rather than the real address", forwarded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the API was never called")
	}
}

// testLogger keeps the guard chain's own logging out of the test output.
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
