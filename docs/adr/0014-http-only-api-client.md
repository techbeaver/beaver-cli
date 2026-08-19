# 0014. The client speaks HTTP only, and re-implements no rule

## Status

Accepted.

## Context

This client, and the MCP tool surface built on it, could reach the platform two ways: call the
domain services directly, or go over HTTPS to the same public API a browser uses.

Ownership enforcement on this platform lives in the request handlers. Anything calling the services
underneath them would have to re-implement those checks. A re-implementation that drifts from the
original is a cross-tenant data leak, and the platform has paid for that failure before.

## Decision

Every call is an HTTPS request to `/api/v1` carrying the caller's own token. There is no second path.

The consequence is that the same ownership check, service gate, rate limiter, audit trail and
human-approval gate apply to a command and to a browser click, because they are literally the same
code. There is nothing second to keep honest.

Four properties of the client follow from being on that boundary:

**Responses are capped** at 2 MiB, and log reads at 1 MiB. An unbounded list or a live log stream
piped into an AI client's context window is a context-window incident wearing the costume of a
successful call.

**Log endpoints have two modes.** `Tail` reads the replay, stops at whichever of the line limit, the
byte limit or the deadline comes first, and returns a bounded tail: that is what an agent gets.
`Stream` follows the endpoint until cancelled: that is what a person running `--follow` gets. The
same endpoint, different discipline, because the two callers fail in different ways.

**A non-JSON body is reported as such**, with code `upstream_not_json`, rather than as a parse error.
An API that always speaks JSON returning HTML means something between the caller and the API answered
instead: an edge error page, a proxy, a firewall. That is a different problem with a different fix,
and saying "invalid character '<'" sends people to the wrong one.

**`Retryable` excludes a paused service.** A service gate arrives as a 503, which is otherwise
retryable, but it is an operator's deliberate circuit breaker. Retrying into one turns a maintenance
window into a thundering herd. The check is on the error code, not the status.

**`DecodeList` tolerates three shapes**: a plain list, a paginated wrapper, and a single object. All
three exist on this API, and a client that understood only one would tell a customer they have no
applications when they have several.

## Consequences

Latency includes a full HTTP round trip through the platform's middleware. That is the cost, and it
buys the property that this code cannot silently disagree with the platform about who owns what.

`Retryable` deviating from a plain status check is the kind of subtlety that gets "simplified" later
by someone reading only the function. It has a test naming the reason.

The 2 MiB cap will eventually cut off a legitimately large response. When it does, the fix is
pagination on the endpoint, not a bigger constant here.
