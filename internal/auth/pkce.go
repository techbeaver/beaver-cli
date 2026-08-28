// Package auth performs the OAuth 2.1 flows this CLI uses.
//
// The requirements in ADR 0004 are enforced here
// and covered by tests, not left to review.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCE is one attempt's proof-of-possession pair.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a verifier and its S256 challenge. There is no plain
// method here and adding one is a change to ADR 0004.
func NewPKCE() (*PKCE, error) {
	verifier, err := randomURLSafe(64)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return &PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// NewState generates the per-attempt value compared on return.
func NewState() (string, error) { return randomURLSafe(32) }

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating random data: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
