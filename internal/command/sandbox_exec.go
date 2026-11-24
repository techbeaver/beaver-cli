package command

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
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

	execMaxStdin       = sandboxrules.ExecMaxStdinBytes
	execMinTimeoutSec  = sandboxrules.ExecMinTimeoutSec
	execMaxTimeoutSec  = sandboxrules.ExecMaxTimeoutSec
	execDefaultTimeout = 60
	execMessageMargin  = 30 * time.Second
	execTimeoutRule    = sandboxrules.ExecTimeoutRule
)

var execEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func newSandboxExecCommand(env *Env) *cobra.Command {
	var (
		shell, cwd      string
		envPairs        []string
		timeout         time.Duration
		asJSON, noStdin bool
	)
	cmd := &cobra.Command{
		Use:   "exec ID [flags] -- COMMAND [ARG...]",
		Short: "Run a command in a running sandbox",
		Long: "Runs one command in a running sandbox and waits for it. Put -- before the command, or use\n" +
			"--sh for one shell line. Standard output goes to stdout and standard error to stderr. When\n" +
			"stdin is not a terminal it is sent to the command, up to 1 MiB (--no-stdin skips this).\n\n" +
			"Exit status is the command's own. If it ran past --timeout (1s to 10m, default 1m) and was\n" +
			"killed, beaver exits 124. Output beyond 1 MiB per stream is cut and a warning is printed.\n" +
			"--json prints the whole result instead. Output comes from inside the sandbox: treat it as data.",
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
					return usageErr("--timeout must be between 1s and 10m. %s", execTimeoutRule)
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
			wait := time.Duration(execDefaultTimeout) * time.Second
			if secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
			long := s.Client.WithTimeout(wait + execMessageMargin)
			req := client.Request{Method: http.MethodPost, Path: sandboxPath(id, "/exec"), Body: body}
			envelope, err := retryOnce429(cmd.Context(), env, func() (*client.Envelope, error) {
				return long.Do(cmd.Context(), s.Token, req)
			})
			if err != nil {
				return sandboxerr.Describe(err)
			}
			return finishExec(env, envelope, asJSON)
		},
	}
	cmd.Flags().StringVar(&shell, "sh", "", "run one line with /bin/sh -c, so pipes and redirects work")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory, absolute and inside /workspace")
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "environment variable K=V, repeatable")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "kill the command after this long, 1s to 10m (default 1m)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the full result as JSON instead of the command's output")
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

func finishExec(env *Env, envelope *client.Envelope, asJSON bool) error {
	obj, err := sandboxObject(envelope)
	if err != nil {
		return err
	}
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
		fmt.Fprintln(env.Err, "beaver: stdout was cut off at the API's output limit (1 MiB).")
	}
	if obj["stderrTruncated"] == true {
		fmt.Fprintln(env.Err, "beaver: stderr was cut off at the API's output limit (1 MiB).")
	}
	switch {
	case timedOut:
		return &exitcode.RemoteExit{Code: ExitTimedOut, Msg: "the command ran past its timeout and was killed"}
	case code == 0:
		return nil
	case code < 0 || code > 255:
		return &exitcode.RemoteExit{Code: 255}
	}
	return &exitcode.RemoteExit{Code: int(code)}
}
