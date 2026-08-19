// Package client is the HTTP client for the TechBeaver API.
//
// Every call goes over HTTPS to /api/v1 carrying the caller's own token, so it
// passes the same ownership check, service gate, rate limiter, audit trail and
// human-approval gate as the console. Nothing here re-implements a rule.
//
// Rationale is in docs/adr/0014-http-only-api-client.md.
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
	"strings"
	"time"
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
}

// New builds a client for the API at baseURL. userAgent identifies the caller
// in the platform's request log, which is what lets an agent's call be told
// apart from a browser's.
func New(baseURL, userAgent string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

// Request is one API call.
type Request struct {
	Method string
	// Path is relative to the API root, such as "/paas/apps".
	Path    string
	Query   url.Values
	Body    any
	Headers map[string]string
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

// Do performs a call as the customer whose token this is.
func (c *Client) Do(ctx context.Context, token string, req Request) (*Envelope, error) {
	var body io.Reader
	if req.Body != nil {
		encoded, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, c.endpoint(req), body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/json")
	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
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

	resp, err := c.HTTP.Do(httpReq)
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
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, envelopeError(resp.StatusCode, &env)
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

	streamer := &http.Client{Timeout: timeout}
	resp, err := streamer.Do(httpReq)
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
