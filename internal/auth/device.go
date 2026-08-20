package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/credential"
)

// DeviceOptions configures a login on a machine with no browser.
type DeviceOptions struct {
	Host   string
	Scopes []string
	Prompt func(string)
}

type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// LoginDevice runs the RFC 8628 device authorization grant.
func LoginDevice(ctx context.Context, opts DeviceOptions) (*credential.Token, error) {
	if opts.Prompt == nil {
		opts.Prompt = func(string) {}
	}
	start, err := requestDeviceCode(ctx, opts)
	if err != nil {
		return nil, err
	}

	where := start.VerificationURIComplete
	if where == "" {
		where = start.VerificationURI
	}
	opts.Prompt(fmt.Sprintf("On any device with a browser, open:\n\n  %s\n\nand enter the code:\n\n  %s\n",
		where, start.UserCode))

	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(max(start.ExpiresIn, 300)) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("that code expired before it was approved. Run beaver auth login --no-browser again")
		}

		tok, retry, err := pollDeviceToken(ctx, opts.Host, start.DeviceCode)
		switch {
		case err != nil:
			return nil, err
		case tok != nil:
			return tok, nil
		case retry > 0:
			// Ignoring slow_down is indistinguishable from guessing the user code.
			interval += retry
		}
	}
}

func requestDeviceCode(ctx context.Context, opts DeviceOptions) (*deviceAuthResponse, error) {
	form := url.Values{"client_id": {ClientID}}
	if len(opts.Scopes) > 0 {
		form.Set("scope", strings.Join(opts.Scopes, " "))
	}
	endpoint := strings.TrimRight(opts.Host, "/") + "/api/v1/oauth/device_authorization"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the authorization server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("this TechBeaver deployment does not offer device login yet. Use beaver auth login --token with a personal token")
	}
	var body deviceAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.DeviceCode == "" {
		return nil, &client.APIError{
			StatusCode: resp.StatusCode,
			Code:       "device_authorization_failed",
			Message:    "the authorization server would not start a device login",
		}
	}
	return &body, nil
}

// pollDeviceToken returns a token, or a back-off to add, or an error.
func pollDeviceToken(ctx context.Context, host, deviceCode string) (*credential.Token, time.Duration, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {ClientID},
	}
	endpoint := strings.TrimRight(host, "/") + "/api/v1/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("reaching the authorization server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		tokenResponse
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, 0, fmt.Errorf("the authorization server returned something unreadable")
	}

	switch body.Error {
	case "authorization_pending":
		return nil, 0, nil
	case "slow_down":
		return nil, 5 * time.Second, nil
	case "expired_token":
		return nil, 0, fmt.Errorf("that code expired before it was approved. Run beaver auth login --no-browser again")
	case "access_denied":
		return nil, 0, ErrDenied
	}

	if body.AccessToken == "" {
		return nil, 0, &client.APIError{
			StatusCode: resp.StatusCode,
			Code:       "device_token_failed",
			Message:    "the authorization server refused this device login",
		}
	}

	tok := &credential.Token{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		TokenType:    body.TokenType,
		Host:         strings.TrimRight(host, "/"),
	}
	if body.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	if body.Scope != "" {
		tok.Scopes = strings.Fields(body.Scope)
	}
	return tok, 0, nil
}
