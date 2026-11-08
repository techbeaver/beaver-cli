package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Token type hints for [Revoke], from RFC 7009.
const (
	HintRefreshToken = "refresh_token"
	HintAccessToken  = "access_token"
)

// Revoke asks the authorization server to invalidate a token (RFC 7009). The
// server answers 200 whether or not the token was known, so any other outcome
// is a failure to tell it.
func Revoke(ctx context.Context, host, token, hint string) error {
	target, err := apiURL(host, "/api/v1/oauth/revoke")
	if err != nil {
		return err
	}
	form := url.Values{
		"token":           {token},
		"token_type_hint": {hint},
		"client_id":       {ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := authClient().Do(req)
	if err != nil {
		return fmt.Errorf("reaching the authorization server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("the authorization server answered %d to the revocation of the %s", resp.StatusCode, strings.ReplaceAll(hint, "_", " "))
	}
	return nil
}
