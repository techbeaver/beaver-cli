// Command beaver is the TechBeaver command line interface.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/techbeaver/beaver-cli/internal/command"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
)

// version is set by the release build.
var version = "dev"

func main() {
	command.Version = version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env, err := command.NewEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "beaver:", err)
		os.Exit(exitcode.Error)
	}

	if err := command.NewRoot(env).ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(exitcode.Error)
		}
		fmt.Fprintln(os.Stderr, "beaver:", cleanMessage(err))
		os.Exit(exitcode.From(err))
	}
}

// cleanMessage strips the sentinel prefixes that carry an exit code, so a user
// reads "run beaver auth login" rather than "not authenticated: run beaver...".
func cleanMessage(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"usage: ", "not authenticated: ", "awaiting approval: "} {
		if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
			return msg[len(prefix):]
		}
	}
	return msg
}
