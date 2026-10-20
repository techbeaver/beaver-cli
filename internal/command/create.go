package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
)

// IdempotencyHeader makes a retried charge return the first answer instead of a second charge.
const IdempotencyHeader = "Idempotency-Key"

const chargeAttempts = 3

var chargeBackoff = []time.Duration{time.Second, 3 * time.Second}

func newIdempotencyKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cli-" + hex.EncodeToString(b), nil
}

func addIdempotencyKeyFlag(cmd *cobra.Command, key *string) {
	cmd.Flags().StringVar(key, "idempotency-key", "", "reuse the key a failed attempt printed, so a retry cannot charge twice")
}

// sendCharge sends a request that can create a charge, retrying transient failures under one key.
func sendCharge(ctx context.Context, env *Env, s *Session, req client.Request, key string) (*client.Envelope, error) {
	if key == "" {
		generated, err := newIdempotencyKey()
		if err != nil {
			return nil, err
		}
		key = generated
	}
	headers := map[string]string{IdempotencyHeader: key}
	for k, v := range req.Headers {
		headers[k] = v
	}
	req.Headers = headers

	var last error
	for attempt := 1; ; attempt++ {
		envelope, err := s.Client.Do(ctx, s.Token, req)
		if err == nil {
			return envelope, nil
		}
		last = err
		if !ambiguousOutcome(ctx, err) {
			return nil, err
		}
		if attempt >= chargeAttempts {
			break
		}
		fmt.Fprintf(env.Err, "Retrying with the same idempotency key: %v\n", err)
		if !sleepCtx(ctx, chargeBackoff[min(attempt-1, len(chargeBackoff)-1)]) {
			break
		}
	}
	return nil, fmt.Errorf("%w\nIt may or may not have gone through. To retry without risking a second charge, run the same command with --idempotency-key %s", last, key)
}

// ambiguousOutcome reports whether the request may have been applied and is safe to repeat under its key.
func ambiguousOutcome(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable() || apiErr.Code == "idempotency_in_progress"
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
