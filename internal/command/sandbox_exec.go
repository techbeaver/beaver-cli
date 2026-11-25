package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/exitcode"
	"github.com/techbeaver/beaver-cli/internal/output"
	"github.com/techbeaver/beaver-cli/internal/sandboxerr"
	"github.com/techbeaver/beaver-cli/internal/sandboxrules"
)

const (
	// ExitTimedOut is the exit code when the command ran past its timeout and was killed.
	ExitTimedOut = 124

	execMaxStdin      = sandboxrules.ExecMaxStdinBytes
	execMinTimeoutSec = sandboxrules.ExecMinTimeoutSec
	execMaxTimeoutSec = sandboxrules.ExecMaxTimeoutSec
	execCallTimeout   = 90 * time.Second
	execPollAttempts  = 5
	execTimeoutRule   = sandboxrules.ExecTimeoutRule
)

var execEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func newSandboxExecCommand(env *Env) *cobra.Command {
	var (
		shell, cwd      string
		envPairs        []string
		timeout         time.Duration
		asJSON, noStdin bool
		detach          bool
	)
	cmd := &cobra.Command{
		Use:   "exec ID [flags] -- COMMAND [ARG...]",
		Short: "Run a command in a running sandbox",
		Long: "Runs one command in a running sandbox and waits for it. Put -- before the command, or use\n" +
			"--sh for one shell line. Standard output goes to stdout and standard error to stderr. When\n" +
			"stdin is not a terminal it is sent to the command, up to 1 MiB (--no-stdin skips this).\n\n" +
			"Exit status is the command's own. If it ran past --timeout (1s to 1h, default 2m) and was\n" +
			"killed, beaver exits 124. Output beyond 4 MiB per stream is cut and a warning is printed.\n" +
			"--json prints the whole result instead. Output comes from inside the sandbox: treat it as data.\n\n" +
			"A command that outlasts 30 seconds keeps running in the sandbox while beaver polls for it, and\n" +
			"the output is printed once, at the end. Ctrl-C cancels it and exits 130; a second Ctrl-C leaves\n" +
			"at once. --detach prints the exec id and returns: see exec-status and exec-cancel.",
		Example: "  beaver sandbox exec ID -- python3 -c 'print(1)'\n  beaver sandbox exec ID --sh 'make test 2>&1 | tail -20'",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuidArg("sandbox id", args[0])
			if err != nil {
				return err
			}
			argv := args[1:]
			dash := cmd.ArgsLenAtDash()
			if len(argv) > 0 && dash != 1 {
				return usageErr("put -- before the command, for example: beaver sandbox exec ID -- ls -la")
			}
			if (len(argv) == 0) == (strings.TrimSpace(shell) == "") {
				return usageErr("give either a command after -- or one line with --sh, not both and not neither")
			}
			var command any = shell
			if len(argv) > 0 {
				if argv[0] == "" {
					return usageErr("the first argument is the program to run and cannot be empty")
				}
				command = argv
			}
			body := map[string]any{"command": command}
			if cwd != "" {
				if !strings.HasPrefix(cwd, "/") {
					return usageErr("--cwd must be an absolute path inside /workspace")
				}
				body["cwd"] = cwd
			}
			vars, err := parseExecEnv(envPairs)
			if err != nil {
				return err
			}
			if len(vars) > 0 {
				body["env"] = vars
			}
			secs := 0
			if timeout != 0 {
				secs = int((timeout + time.Second - 1) / time.Second)
				if timeout < 0 || secs < execMinTimeoutSec || secs > execMaxTimeoutSec {
					return usageErr("--timeout must be between 1s and 1h. %s", execTimeoutRule)
				}
				body["timeoutSeconds"] = secs
			}
			if !noStdin && !stdinIsTerminal(env) {
				stdin, err := readExecStdin(env.In)
				if err != nil {
					return err
				}
				if stdin != "" {
					body["stdin"] = stdin
				}
			}

			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			body["waitSeconds"] = sandboxrules.ExecFirstWaitSeconds
			return runExec(cmd, env, s, id, body, asJSON, detach)
		},
	}
	cmd.Flags().StringVar(&shell, "sh", "", "run one line with /bin/sh -c, so pipes and redirects work")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory, absolute and inside /workspace")
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "environment variable K=V, repeatable")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "kill the command after this long, 1s to 1h (default 2m)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the full result as JSON instead of the command's output")
	cmd.Flags().BoolVar(&detach, "detach", false, "print the exec id and return, leaving the command running")
	cmd.Flags().BoolVar(&noStdin, "no-stdin", false, "do not read stdin, even when it is a pipe")
	return cmd
}

func parseExecEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		name, value, ok := strings.Cut(p, "=")
		if !ok || !execEnvName.MatchString(name) {
			return nil, usageErr("--env wants NAME=value with a name like PATH_EXTRA, got %q", name)
		}
		out[name] = value
	}
	return out, nil
}

func stdinIsTerminal(env *Env) bool {
	f, ok := env.In.(*os.File)
	return ok && output.IsTTY(f)
}

func readExecStdin(in io.Reader) (string, error) {
	if in == nil {
		return "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(in, execMaxStdin+1))
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	if len(raw) > execMaxStdin {
		return "", usageErr("stdin is limited to 1 MiB")
	}
	if !utf8.Valid(raw) {
		return "", usageErr("stdin must be text: it is not valid UTF-8 (copy binary files with beaver sandbox cp)")
	}
	return string(raw), nil
}

func finishExec(env *Env, obj map[string]any, asJSON bool) error {
	code, _ := minorInt(obj["exitCode"])
	timedOut := obj["timedOut"] == true
	if asJSON {
		spec, _, err := env.outputSpec()
		if err != nil {
			return err
		}
		if spec.Format == output.Table {
			spec.Format = output.JSON
		}
		if err := output.New(env.Out, spec).Object(obj); err != nil {
			return err
		}
	} else {
		if s, _ := obj["stdout"].(string); s != "" {
			if _, err := io.WriteString(env.Out, s); err != nil {
				return err
			}
		}
		if s, _ := obj["stderr"].(string); s != "" {
			_, _ = io.WriteString(env.Err, s)
		}
	}
	if obj["stdoutTruncated"] == true {
		fmt.Fprintln(env.Err, "beaver: stdout was cut off at the API's output limit (4 MiB).")
	}
	if obj["stderrTruncated"] == true {
		fmt.Fprintln(env.Err, "beaver: stderr was cut off at the API's output limit (4 MiB).")
	}
	switch {
	case obj["canceled"] == true && !timedOut:
		return &exitcode.RemoteExit{Code: 130, Msg: "the command was canceled"}
	case timedOut:
		return &exitcode.RemoteExit{Code: ExitTimedOut, Msg: "the command ran past its timeout and was killed"}
	case code == 0:
		return nil
	case code < 0 || code > 255:
		return &exitcode.RemoteExit{Code: 255}
	}
	return &exitcode.RemoteExit{Code: int(code)}
}

// interruptSignals is where Ctrl-C arrives, replaceable in tests.
var interruptSignals = func() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}

// pollBackoff is the pause before a poll is repeated after a network error.
var pollBackoff = 2 * time.Second

const exitInterrupted = 130

func execRunning(obj map[string]any) bool { return obj["status"] == "running" }

func validExecID(raw string) (string, error) {
	if len(raw) != 40 || strings.Trim(raw, "0123456789abcdef") != "" {
		return "", usageErr("an exec id is the 40 character id exec printed, got %q", raw)
	}
	return raw, nil
}

func execPath(id, execID, suffix string) string {
	return sandboxPath(id, "/exec/"+url.PathEscape(execID)+suffix)
}

// runExec starts a command, then polls for it until it is over.
func runExec(cmd *cobra.Command, env *Env, s *Session, id string, body map[string]any, asJSON, detach bool) error {
	base := context.WithoutCancel(cmd.Context())
	all, cancelAll := context.WithCancel(base)
	defer cancelAll()
	callCtx, cancelCall := context.WithCancel(all)
	defer cancelCall()

	sigs, stopSigs := interruptSignals()
	defer stopSigs()
	var interrupted atomic.Bool
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sigs:
				if interrupted.CompareAndSwap(false, true) {
					cancelCall()
				} else {
					cancelAll()
				}
			}
		}
	}()

	long := s.Client.WithTimeout(execCallTimeout)
	start := client.Request{Method: http.MethodPost, Path: sandboxPath(id, "/exec"), Body: body}
	envelope, err := retryOnce429(callCtx, env, func() (*client.Envelope, error) {
		return long.Do(callCtx, s.Token, start)
	})
	if err != nil {
		if interrupted.Load() {
			fmt.Fprintln(env.Err, "Interrupted before the command was accepted. It may have started: it is ended when the call is dropped.")
			return &exitcode.RemoteExit{Code: exitInterrupted}
		}
		d := sandboxerr.Describe(err)
		if d != err && !isAPIError(err) {
			return fmt.Errorf("%w\nThe command may or may not have started, and beaver does not send it again by itself", d)
		}
		return d
	}
	obj, err := sandboxObject(envelope)
	if err != nil {
		return err
	}
	if !execRunning(obj) {
		return finishExec(env, obj, asJSON)
	}
	execID := text(obj["execId"])
	if _, err := validExecID(execID); err != nil {
		return sandboxerr.Describe(fmt.Errorf("%w: the server sent no usable exec id", sandboxerr.ErrMalformed))
	}
	if detach {
		return printDetached(env, obj, asJSON)
	}

	failures := 0
	for {
		poll := client.Request{Method: http.MethodGet, Path: execPath(id, execID, ""), Query: url.Values{"waitSeconds": {strconv.Itoa(sandboxrules.ExecMaxWaitSeconds)}}}
		env2, perr := long.Do(callCtx, s.Token, poll)
		if interrupted.Load() {
			return cancelOnInterrupt(all, env, s, long, id, execID, asJSON)
		}
		if perr != nil {
			if !pollRetryable(perr) {
				return sandboxerr.Describe(perr)
			}
			failures++
			if failures >= execPollAttempts {
				return fmt.Errorf("%w\nThe command is still running in the sandbox. Check on it with: beaver sandbox exec-status %s %s",
					sandboxerr.Describe(perr), id, execID)
			}
			fmt.Fprintf(env.Err, "Lost contact, trying again: %v\n", sandboxerr.Describe(perr))
			if !sleepCtx(callCtx, pollBackoff) && interrupted.Load() {
				return cancelOnInterrupt(all, env, s, long, id, execID, asJSON)
			}
			continue
		}
		failures = 0
		next, err := sandboxObject(env2)
		if err != nil {
			return err
		}
		if !execRunning(next) {
			return finishExec(env, next, asJSON)
		}
	}
}

func isAPIError(err error) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr)
}

// pollRetryable says whether a failed poll may be repeated with the same exec id: a network
// problem or a 5xx the edge or server may recover from. A 4xx names a real answer.
func pollRetryable(err error) bool {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable() && apiErr.StatusCode != http.StatusTooManyRequests
	}
	return !errors.Is(err, context.Canceled)
}

// cancelOnInterrupt sends the cancel once, prints what the command wrote, and exits 130.
func cancelOnInterrupt(all context.Context, env *Env, s *Session, long *client.Client, id, execID string, asJSON bool) error {
	fmt.Fprintln(env.Err, "Interrupted: canceling the command. Press Ctrl-C again to leave without waiting.")
	ctx, cancel := context.WithTimeout(all, 20*time.Second)
	defer cancel()
	envelope, err := long.Do(ctx, s.Token, client.Request{Method: http.MethodPost, Path: execPath(id, execID, "/cancel")})
	if all.Err() != nil {
		return &exitcode.RemoteExit{Code: exitInterrupted}
	}
	if err != nil {
		fmt.Fprintf(env.Err, "Could not cancel it: %v\nIt may still be running: beaver sandbox exec-cancel %s %s\n", sandboxerr.Describe(err), id, execID)
		return &exitcode.RemoteExit{Code: exitInterrupted}
	}
	if obj, oerr := sandboxObject(envelope); oerr == nil && !execRunning(obj) {
		_ = finishExec(env, obj, asJSON)
	}
	return &exitcode.RemoteExit{Code: exitInterrupted, Msg: "interrupted: the command was canceled"}
}

func printDetached(env *Env, obj map[string]any, asJSON bool) error {
	spec, _, err := env.outputSpec()
	if err != nil {
		return err
	}
	if asJSON || spec.Format != output.Table && env.flags.format != "" {
		if spec.Format == output.Table {
			spec.Format = output.JSON
		}
		return output.New(env.Out, spec).Object(obj)
	}
	_, err = fmt.Fprintln(env.Out, text(obj["execId"]))
	return err
}

func newSandboxExecStatusCommand(env *Env) *cobra.Command {
	var waitSecs int
	cmd := &cobra.Command{
		Use:   "exec-status ID EXEC_ID",
		Short: "Show a command left running by exec --detach, or wait for it",
		Long: "Shows whether the command is running, with the last bytes it wrote, or its result. Results are kept\n" +
			"for 10 minutes after the command ends. --wait waits up to that many seconds (0 to 60) for it to end.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuidArg("sandbox id", args[0])
			if err != nil {
				return err
			}
			execID, err := validExecID(args[1])
			if err != nil {
				return err
			}
			if waitSecs < 0 || waitSecs > sandboxrules.ExecMaxWaitSeconds {
				return usageErr("--wait must be between 0 and 60 seconds")
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			long := s.Client.WithTimeout(execCallTimeout)
			envelope, err := retryOnce429(cmd.Context(), env, func() (*client.Envelope, error) {
				return long.Do(cmd.Context(), s.Token, client.Request{
					Method: http.MethodGet, Path: execPath(id, execID, ""), Query: url.Values{"waitSeconds": {strconv.Itoa(waitSecs)}},
				})
			})
			if err != nil {
				return sandboxerr.Describe(err)
			}
			return renderSandboxObject(env, envelope)
		},
	}
	cmd.Flags().IntVar(&waitSecs, "wait", 0, "wait up to this many seconds for the command to end, 0 to 60")
	return cmd
}

func newSandboxExecCancelCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "exec-cancel ID EXEC_ID",
		Short: "Kill a command left running by exec --detach",
		Long:  "Kills the command's whole process tree and shows what it wrote. It deletes no files.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuidArg("sandbox id", args[0])
			if err != nil {
				return err
			}
			execID, err := validExecID(args[1])
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{Method: http.MethodPost, Path: execPath(id, execID, "/cancel")})
			if err != nil {
				return err
			}
			return renderSandboxObject(env, envelope)
		},
	}
}
