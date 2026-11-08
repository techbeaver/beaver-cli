package command

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// maxSecretBytes bounds what is read from stdin as a secret value.
const maxSecretBytes = 1 << 20

// Seams for tests: a real terminal cannot be attached to a unit test.
var (
	isTerminal   = term.IsTerminal
	readPassword = term.ReadPassword
	getState     = term.GetState
	restoreState = term.Restore
)

// readSecret reads a secret from the user without echoing it when stdin is a
// terminal, and from stdin otherwise. There is no argument form on purpose:
// arguments are visible to other users. multiline keeps interior newlines (a
// PEM, say) and strips one trailing newline; otherwise only the first line is
// used.
func readSecret(ctx context.Context, env *Env, prompt string, multiline bool) (string, error) {
	if f, ok := env.In.(*os.File); ok && isTerminal(int(f.Fd())) { // #nosec G115 -- a file descriptor fits an int
		fd := int(f.Fd()) // #nosec G115 -- as above
		fmt.Fprint(env.Err, prompt)
		saved, _ := getState(fd)
		type result struct {
			raw []byte
			err error
		}
		done := make(chan result, 1)
		read := readPassword
		go func() {
			raw, err := read(fd)
			done <- result{raw, err}
		}()
		select {
		case <-ctx.Done():
			// The read cannot be interrupted, so put the terminal back by hand.
			if saved != nil {
				_ = restoreState(fd, saved)
			}
			fmt.Fprintln(env.Err)
			return "", ctx.Err()
		case res := <-done:
			fmt.Fprintln(env.Err)
			if res.err != nil {
				return "", fmt.Errorf("reading the value: %w", res.err)
			}
			return finishSecret(string(res.raw), multiline)
		}
	}

	if multiline {
		raw, err := io.ReadAll(io.LimitReader(env.In, maxSecretBytes+1))
		if err != nil {
			return "", fmt.Errorf("reading the value: %w", err)
		}
		if len(raw) > maxSecretBytes {
			return "", usageErr("the value is larger than %d bytes", maxSecretBytes)
		}
		return finishSecret(string(raw), true)
	}
	line, err := bufio.NewReader(io.LimitReader(env.In, maxSecretBytes)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the value: %w", err)
	}
	return finishSecret(line, false)
}

func finishSecret(raw string, multiline bool) (string, error) {
	if multiline {
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
	} else {
		raw = strings.TrimSpace(raw)
	}
	if raw == "" {
		return "", usageErr("no value was supplied")
	}
	return raw, nil
}
