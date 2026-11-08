package credential

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockPollInterval is how often a waiting process retries the lock.
const lockPollInterval = 25 * time.Millisecond

// Lock takes an exclusive advisory lock on one profile's credentials, so two
// CLI processes cannot refresh the same token at once. It waits until ctx is
// done. The returned function releases the lock and is safe to call twice.
//
// The lock lives in a sibling file, not the credential file, because that file
// is replaced by rename and a lock on a replaced inode protects nothing.
func Lock(ctx context.Context, dir, profile string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, ".credentials-"+sanitise(profile)+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- built from a sanitised profile name
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	for {
		ok, err := tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for another beaver process to finish refreshing the credential: %w", ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		unlock(f)
		_ = f.Close()
	}, nil
}
