// Package client is the HTTP client for the TechBeaver API.
//
// Every call goes over HTTPS to /api/v1 carrying the caller's own token, so it
// passes the same ownership check, service gate, rate limiter, audit trail and
// human-approval gate as the console. Nothing here re-implements a rule.
//
// Rationale is in ADR 0014.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/techbeaver/beaver-cli/internal/endpoint"
)

const (
	maxResponseBytes = 2 << 20
	maxLogBytes      = 1 << 20
)

// Client reaches the TechBeaver API. The zero value is not usable; build one
// with [New].
type Client struct {
	BaseURL   string
	HTTP      *http.Client
	UserAgent string
	// Debug, when set, receives one line per request: method, path, status,
	// duration and request id. Never a credential, query value or body.
	Debug   io.Writer
	debugMu sync.Mutex
}

// New builds a client for the API at baseURL. userAgent identifies the caller
// in the platform's request log, which is what lets an agent's call be told
// apart from a browser's.
func New(baseURL, userAgent string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: 60 * time.Second, CheckRedirect: endpoint.CheckRedirect},
	}
}

// ErrBadPath means a request path could not be sent safely.
var ErrBadPath = errors.New("refusing a request path with a query, fragment, backslash, empty or dot segment")

// checkPath refuses a path that could address something other than what the
// caller built it for: callers must escape each segment with [url.PathEscape].
func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\\") {
		return ErrBadPath
	}
	for _, seg := range strings.Split(path[1:], "/") {
		if seg == "" {
			return ErrBadPath
		}
		low := strings.ToLower(seg)
		if low == "." || low == ".." || low == "%2e" || low == "%2e%2e" || low == ".%2e" || low == "%2e." {
			return ErrBadPath
		}
	}
	return nil
}

// Request is one API call.
type Request struct {
	Method string
	// Path is relative to the API root, such as "/paas/apps".
	Path    string
	Query   url.Values
	Body    any
	Headers map[string]string
	// RawBody, when non-nil, is sent as application/octet-stream instead of Body.
	RawBody []byte
}

func (c *Client) endpoint(req Request) string {
	out := c.BaseURL + "/api/v1" + req.Path
	if len(req.Query) > 0 {
		out += "?" + req.Query.Encode()
	}
	return out
}

func (c *Client) userAgent() string {
	if c.UserAgent == "" {
		return "beaver-client"
	}
	return c.UserAgent
}

// WithTimeout returns a copy of the client whose requests give up after d.
func (c *Client) WithTimeout(d time.Duration) *Client {
	return &Client{
		BaseURL:   c.BaseURL,
		UserAgent: c.UserAgent,
		Debug:     c.Debug,
		HTTP:      &http.Client{Timeout: d, CheckRedirect: endpoint.CheckRedirect},
	}
}

func (c *Client) build(ctx context.Context, token string, req Request, accept string) (*http.Request, error) {
	if err := checkPath(req.Path); err != nil {
		return nil, fmt.Errorf("%w: %q", err, req.Path)
	}
	var body io.Reader
	contentType := ""
	switch {
	case req.RawBody != nil:
		body, contentType = bytes.NewReader(req.RawBody), "application/octet-stream"
	case req.Body != nil:
		encoded, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		body, contentType = bytes.NewReader(encoded), "application/json"
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, c.endpoint(req), body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", accept)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	httpReq.Header.Set("User-Agent", c.userAgent())
	if ip := CallerIPFrom(ctx); ip != "" {
		httpReq.Header.Set(HeaderCFConnectingIP, ip)
	}
	for k, v := range req.Headers {
		if v != "" {
			httpReq.Header.Set(k, v)
		}
	}
	return httpReq, nil
}

// Open performs a call and returns the response with its body unread, so a large body can be
// streamed. The caller closes it. A non-2xx answer is read and returned as an error.
func (c *Client) Open(ctx context.Context, token string, req Request) (*http.Response, error) {
	httpReq, err := c.build(ctx, token, req, "*/*")
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling the TechBeaver API: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	return nil, errorFromResponse(resp)
}

func errorFromResponse(resp *http.Response) *APIError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	var env Envelope
	if json.Unmarshal(raw, &env) != nil {
		return &APIError{
			StatusCode: resp.StatusCode,
			Code:       "upstream_not_json",
			Message: fmt.Sprintf("The TechBeaver API returned a %d that was not JSON. Something between you and the API answered instead.",
				resp.StatusCode),
			RetryAfter: retryAfter(resp),
		}
	}
	apiErr := envelopeError(resp.StatusCode, &env)
	apiErr.RetryAfter = retryAfter(resp)
	return apiErr
}

// retryAfter reads a Retry-After given in whole seconds.
func retryAfter(resp *http.Response) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// Do performs a call as the customer whose token this is.
func (c *Client) Do(ctx context.Context, token string, req Request) (*Envelope, error) {
	httpReq, err := c.build(ctx, token, req, "application/json")
	if err != nil {
		return nil, err
	}

	started := time.Now()
	resp, err := c.HTTP.Do(httpReq)
	c.logCall(req.Method, req.Path, resp, started, err)
	if err != nil {
		return nil, fmt.Errorf("calling the TechBeaver API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the response: %w", err)
	}

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Code:       "upstream_not_json",
			Message: fmt.Sprintf("The TechBeaver API returned a %d that was not JSON. Something between you and the API answered instead.",
				resp.StatusCode),
			RetryAfter: retryAfter(resp),
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := envelopeError(resp.StatusCode, &env)
		apiErr.RetryAfter = retryAfter(resp)
		return nil, apiErr
	}
	return &env, nil
}

// Tail reads a bounded amount from one of the platform's server-sent event
// endpoints and returns the last lines of it. Use [Client.Stream] to follow one
// live instead.
func (c *Client) Tail(ctx context.Context, token string, req Request, maxLines int, wait time.Duration) (lines []string, truncated bool, err error) {
	if maxLines <= 0 {
		maxLines = 200
	}
	if wait <= 0 {
		wait = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	resp, err := c.openStream(ctx, token, req, wait+2*time.Second)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxLogBytes))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var collected []string
	for scanner.Scan() {
		line, ok := eventPayload(scanner.Text())
		if !ok {
			continue
		}
		collected = append(collected, line)
		if len(collected) > maxLines*4 {
			collected = collected[len(collected)-maxLines:]
			truncated = true
		}
	}
	if scanErr := scanner.Err(); scanErr != nil &&
		!errors.Is(scanErr, context.DeadlineExceeded) && !errors.Is(scanErr, context.Canceled) {
		if len(collected) == 0 {
			return nil, false, fmt.Errorf("reading the log stream: %w", scanErr)
		}
	}
	if len(collected) > maxLines {
		collected = collected[len(collected)-maxLines:]
		truncated = true
	}
	return collected, truncated, nil
}

// Stream follows a server-sent event endpoint, calling onLine for each payload
// line until the context is cancelled or the server closes the stream. This is
// what `--follow` uses; [Client.Tail] is the bounded form for an agent.
func (c *Client) Stream(ctx context.Context, token string, req Request, onLine func(string)) error {
	resp, err := c.openStream(ctx, token, req, 0)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		if line, ok := eventPayload(scanner.Text()); ok {
			onLine(line)
		}
	}
	if err := scanner.Err(); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("reading the log stream: %w", err)
	}
	return nil
}

func (c *Client) openStream(ctx context.Context, token string, req Request, timeout time.Duration) (*http.Response, error) {
	if err := checkPath(req.Path); err != nil {
		return nil, fmt.Errorf("%w: %q", err, req.Path)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(req), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", c.userAgent())
	if ip := CallerIPFrom(ctx); ip != "" {
		httpReq.Header.Set(HeaderCFConnectingIP, ip)
	}

	streamer := &http.Client{Timeout: timeout, CheckRedirect: endpoint.CheckRedirect}
	started := time.Now()
	resp, err := streamer.Do(httpReq)
	c.logCall(http.MethodGet, req.Path, resp, started, err)
	if err != nil {
		return nil, fmt.Errorf("reading the log stream: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		var env Envelope
		if json.Unmarshal(raw, &env) == nil {
			return nil, envelopeError(resp.StatusCode, &env)
		}
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("the log endpoint returned %d", resp.StatusCode),
		}
	}
	return resp, nil
}

// eventPayload extracts the payload of a server-sent event line. A line that is
// not a "data:" line is a comment or a field this client ignores.
func eventPayload(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}
