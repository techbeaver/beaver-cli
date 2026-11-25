package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
	"github.com/techbeaver/beaver-cli/internal/sandboxerr"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func newSandboxCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sandbox",
		Aliases: []string{"sandboxes"},
		Short:   "Sandbox plans, usage, quotes and buying slots",
		Long: "Run and manage sandboxes, and get a payment link for slots.\n\n" +
			"beaver cannot pay: buy prints a link that you open and pay yourself.\n\n" +
			"Most of these need sandbox scopes that are not in the default sign in. Grant them in the agent\n" +
			"connection's permissions, or run beaver auth login --scope sandbox:read,sandbox:write.\n" +
			"sandbox:exec runs commands, sandbox:files moves files, sandbox:destroy asks for deletion.\n" +
			"Creating or starting on a by-the-hour subscription needs billing:spend, which no sign in can hold.\n" +
			"Destroying always needs your approval in a browser.",
	}
	cmd.AddCommand(
		newSandboxPlansCommand(env),
		newSandboxUsageCommand(env),
		newSandboxSubscriptionsCommand(env),
		newSandboxStatementsCommand(env),
		newSandboxAddSlotsCommand(env),
		newSandboxQuoteCommand(env),
		newSandboxBuyCommand(env),
		newSandboxListCommand(env),
		newSandboxGetCommand(env),
		newSandboxCreateCommand(env),
		newSandboxTransitionCommand(env, "start", "Start a stopped sandbox"),
		newSandboxTransitionCommand(env, "stop", "Stop a sandbox and keep its disk"),
		newSandboxDestroyCommand(env),
		newSandboxExecCommand(env),
		newSandboxExecStatusCommand(env),
		newSandboxExecCancelCommand(env),
		newSandboxCopyCommand(env),
		newSandboxLsCommand(env),
		newSandboxMkdirCommand(env),
	)
	return cmd
}

func sandboxProject(s *Session) (string, error) {
	project, err := s.Resolved.RequireProject()
	if err != nil {
		return "", err
	}
	if !uuidPattern.MatchString(project) {
		return "", usageErr("sandbox commands need the project id, not its name (see beaver projects list), got %q", project)
	}
	return project, nil
}

// retryWait is how a rate limit pause is slept, replaceable in tests.
var retryWait = sleepCtx

// maxAutoRetryWait is the longest Retry-After worth waiting out inside one command.
const maxAutoRetryWait = 10 * time.Second

// retryOnce429 repeats a call one time when the server asked for a short wait.
func retryOnce429[T any](ctx context.Context, env *Env, do func() (T, error)) (T, error) {
	out, err := do()
	var apiErr *client.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests ||
		apiErr.RetryAfter <= 0 || apiErr.RetryAfter > maxAutoRetryWait {
		return out, err
	}
	fmt.Fprintf(env.Err, "Rate limited. Trying once more in %d seconds.\n", int(apiErr.RetryAfter.Seconds()))
	if !retryWait(ctx, apiErr.RetryAfter) {
		return out, err
	}
	return do()
}

func sandboxDo(env *Env, s *Session, cmd *cobra.Command, req client.Request) (*client.Envelope, error) {
	envelope, err := retryOnce429(cmd.Context(), env, func() (*client.Envelope, error) {
		return s.Client.Do(cmd.Context(), s.Token, req)
	})
	if err != nil {
		return nil, sandboxerr.Describe(err)
	}
	return envelope, nil
}

func decodeSandbox(envelope *client.Envelope) (any, error) {
	if envelope == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(envelope.Data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, sandboxerr.Describe(fmt.Errorf("%w: %v", sandboxerr.ErrMalformed, err))
	}
	return v, nil
}

func sandboxObject(envelope *client.Envelope) (map[string]any, error) {
	v, err := decodeSandbox(envelope)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, sandboxerr.Describe(fmt.Errorf("%w: expected an object", sandboxerr.ErrMalformed))
	}
	return obj, nil
}

func sandboxList(envelope *client.Envelope) ([]map[string]any, error) {
	v, err := decodeSandbox(envelope)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	if obj, isObj := v.(map[string]any); isObj {
		for _, key := range []string{"items", "data", "results", "rows", "records"} {
			if nested, ok := obj[key].([]any); ok {
				v = nested
				break
			}
		}
		if _, still := v.(map[string]any); still {
			return []map[string]any{obj}, nil
		}
	}
	items, ok := v.([]any)
	if !ok {
		return nil, sandboxerr.Describe(fmt.Errorf("%w: expected a list", sandboxerr.ErrMalformed))
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		obj, isObj := item.(map[string]any)
		if !isObj {
			return nil, sandboxerr.Describe(fmt.Errorf("%w: a list entry was not an object", sandboxerr.ErrMalformed))
		}
		out = append(out, obj)
	}
	return out, nil
}

// outputSpec reports the format for this run and whether it is the human one.
func (e *Env) outputSpec() (output.Spec, bool, error) {
	spec, err := output.ParseFormat(pick(e.flags.format, e.Format), e.IsTTY)
	if err != nil {
		return spec, false, usageErr("%s", err)
	}
	return spec, spec.Format == output.Table, nil
}

type pair struct{ key, value string }

func printPairs(w io.Writer, pairs []pair) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range pairs {
		if p.value == "" {
			continue
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", p.key, p.value); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func text(v any) string {
	if v == nil {
		return ""
	}
	return scalarText(v)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func uuidArg(what, raw string) (string, error) {
	if !uuidPattern.MatchString(raw) {
		return "", usageErr("%s must be an id like 11111111-2222-3333-4444-555555555555, got %q", what, raw)
	}
	return raw, nil
}
