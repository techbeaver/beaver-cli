# 0010. Credential storage: the platform keychain, with a 0600 file fallback

## Status

Accepted.

## Context

The CLI holds an access token and a refresh token. The refresh token is the valuable one: it is
long-lived and it mints access tokens.

Where a CLI puts that is one of the few decisions in a tool like this that can be straightforwardly
wrong. A file in the home directory with default permissions is readable by every process the user
runs, and on a shared machine sometimes by other users.

`CGO_ENABLED=0` ([0002](0002-go-single-static-binary.md)) rules out linking against the platform
keychain directly, so the mechanism must reach it over IPC or a subprocess.

## Decision

Store in the platform keychain where one exists: Keychain on macOS, Credential Manager on Windows,
Secret Service on Linux.

Fall back to a file only when there is none, which is the common case on a server. Then:

- the directory is `0700` and the file is `0600`;
- **permissions are checked on read, and a world-readable or group-readable credential file is
  refused, not warned about.** A warning on a credential nobody is watching is not a control;
- the token is never in `argv`, so `--token=VALUE` does not exist as a flag. Arguments are visible to
  other users via the process table. A token is accepted from a prompt, a file, or an environment
  variable.

`--debug` redacts `Authorization`, `Set-Cookie` and anything matching the platform's credential
prefixes. Redaction is by construction in the transport, not something each call site remembers.

## Consequences

Refusing to read a badly permissioned file will lock somebody out of their own credential after they
copy it around. The error says exactly which file and which mode, and `chmod 600` fixes it. Being
locked out is the correct outcome and is better than a warning nobody reads.

Headless Linux commonly has no Secret Service, so the file path is the normal path in CI, not an
exotic one, and gets equal test coverage.

No flag can set `InsecureSkipVerify`. If local development against a self-signed endpoint needs it,
that is an environment variable which logs a warning on every request.
