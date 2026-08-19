# 0004. Loopback PKCE is the primary login

## Status

Accepted.

## Context

A native application cannot keep a client secret, so it needs a browser-based flow that does not
depend on one. Two apply: the RFC 8252 loopback redirect, and the RFC 8628 device grant.

The planning work assumed the device grant would have to come first, because a loopback redirect
needs the authorization server to accept `http` on a variable port. Checking rather than assuming
showed the server already does: it accepts `http` on a loopback literal, refuses `localhost`
deliberately because that name can be made to resolve elsewhere on a machine somebody has touched,
and compares loopback redirects ignoring the port exactly as RFC 8252 requires.

So loopback works today with no server change. The device grant needs a new endpoint, a new grant
type, a new page and its own rate limiting on a short guessable code.

## Decision

`beaver auth login` performs authorization code with PKCE S256 over a loopback redirect. The CLI
binds `127.0.0.1` on a port the operating system chooses, opens the browser, serves exactly one
request, and shuts the listener down.

The device grant is added afterwards, for machines with no browser, behind `--no-browser`.

Requirements, each checkable in review:

- S256 only. Never `plain`, never an absent challenge.
- `state` generated per attempt and compared on return. A mismatch aborts before any exchange.
- The listener binds the loopback address explicitly. Never all interfaces, which would put a login
  callback on the network.
- The page served back never contains the authorization code.

## Consequences

Ordinary desktop login needs nothing built on the server, so this ships first and gets exercised
before the device grant exists.

Until the device grant lands, a headless machine has one option: a personal token pasted into
`beaver auth login --token`. That is documented rather than hidden.

Binding a port the OS chooses is what makes the RFC 8252 port carve-out load-bearing. If the server
ever tightens redirect comparison to include the port, this breaks, so that behaviour is a contract
between the two repositories and not an implementation detail either side may change freely.
