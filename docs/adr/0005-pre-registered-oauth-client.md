# 0005. A pre-registered public client, not dynamic registration

## Status

Accepted.

## Decision context

The authorization server supports RFC 7591 dynamic client registration, which the MCP surface uses:
an AI client nobody has heard of registers itself and gets a `client_id`. That is right there,
because the set of clients is open and unknowable in advance.

The CLI is the opposite case. There is exactly one of it, we build it, and we ship it.

Using dynamic registration here would mean every installation creates a new client row on first
login. Those rows are indistinguishable from a stranger's, so the registration cap that protects the
endpoint cannot tell our own users from an attacker, and the client table grows by one row per
customer machine forever.

## Decision

One `client_id` is created by an operator in the platform, marked as not dynamically registered, and
compiled into the binary as a constant. Its registered redirect is the loopback literal, and the
RFC 8252 port carve-out ([0004](0004-loopback-pkce-first.md)) covers the port the CLI actually binds.

There is no client secret. A distributed binary cannot hold one: extracting it is a `strings` call.
PKCE is what proves the token request came from the process that started the flow, and it is what
makes a public client safe.

## Consequences

A `client_id` in a public repository looks like a leaked credential to anyone scanning, and will be
reported as one. It is not. It is an identifier, it is public by design in every OAuth public client,
and it grants nothing without a customer completing a consent screen. This is written down so the
answer exists before the report does.

Revoking or rotating it invalidates every installed copy at once, so it cannot be rotated casually.
If it ever must be, the version endpoint is the channel for warning people first.

Registrations from the CLI do not consume the dynamic registration cap, so a busy launch day cannot
lock strangers out of the MCP registration endpoint or the reverse.
