# 0008. Exit codes and --format are a public contract

## Status

Accepted.

## Context

The moment `beaver apps deploy` appears in somebody's CI pipeline, its exit code and its output
shape are as load-bearing as any HTTP endpoint. Changing either breaks a build at a customer who
never agreed to the change.

Most CLIs discover this after the fact and are then stuck with whatever they happened to do.

## Decision

Exit codes are defined, and part of the public interface:

| code | meaning |
|---|---|
| 0 | success |
| 1 | error |
| 2 | usage error |
| 3 | not authenticated, or the credential expired |
| 4 | not found |
| 5 | payment required |
| 6 | awaiting human approval ([0007](0007-server-side-destructive-gate.md)) |
| 7 | rate limited |

Codes 6 and 7 are not conventional. They exist because both are retryable in a specific way and a
script needs to distinguish them from a hard failure. 6 says "a person must click something", 7 says
"wait and try again".

`--format` accepts `json`, `yaml`, `table` and `value(...)`. **When stdout is not a terminal the
default is `json`**, so a pipe never receives a table somebody has to parse with `awk`.

Both are covered by the version number. Changing the meaning of a code, or the shape of a documented
JSON field, is a major version.

## Consequences

Adding a code later is safe, since nothing was relying on it. Repurposing one is not, and cannot be
done inside a major version.

Every command maps its errors through one place, so a new command cannot invent its own convention.

The TTY-dependent default means a command's output differs between a terminal and a pipe. That is
deliberate and matches `gh`, but it surprises people the first time, so it is documented on every
command rather than only in a general page.
