# 0016. The MCP edge limits before it authenticates, and keys on the credential

## Status

Accepted.

## Context

`beaver mcp` is the same package that runs as a hosted service in front of the TechBeaver API.
In that setting every customer's traffic arrives from one place, and a caller can reach the
endpoint before proving anything about itself.

Three properties of that setting make the obvious rate-limiting choices wrong:

- **One address is every customer.** Behind a hosted MCP service the API sees the pod, not the
  caller. Keying a limiter on the address puts the whole customer base in one bucket, where they
  throttle each other.
- **A caller guessing credentials gets a fresh bucket per guess.** Anything keyed on the
  presented token is free to an attacker, who simply presents a different invented token each
  time.
- **A 429 on a health check is read as a dead container.** Liveness probes arrive from the node
  and share an address. Answering one with 429 gets the container killed, which turns a busy
  minute into a restart loop.

## Decision

- **Limit before authenticating, not after.** A caller guessing tokens that is only refused once
  the API has answered has still cost a round trip and an indexed lookup per guess.
- **Key on the credential when there is one, on the address when there is not.** That is the
  only split that survives a hosted deployment.
- **Count failed authentication by address.** It is the only counter a token-guessing caller
  cannot reset, and it is checked before the API is called at all.
- **Discovery keeps an address-keyed allowance; `/healthz` gets none.** Discovery is the one
  thing an uncredentialed caller may legitimately fetch, so it is limited rather than exempt.
  The health check is exempt, for the restart-loop reason above.
- **Sweep expired buckets on the path that creates them**, not from a goroutine. The map holds
  only keys seen in the last window, so it is bounded by traffic rather than by time, and it
  cannot leak while the process is idle. If one window brings a great many keys the table is
  dropped wholesale, which costs one window of accounting rather than unbounded memory.
- **Refuse an unrecognised `Origin`.** DNS rebinding is what stops a page in a customer's
  browser driving their agent's session.
- **Bound the request body well below the default.** A tool call here is a handful of
  identifiers and some environment variables. Nothing legitimate is megabytes.

## Consequences

Running `beaver mcp` on a laptop inherits limits sized for a shared service. They are generous
enough that a person will not meet them, and the alternative is two code paths where the
locally-run one is the untested one.

The trusted-proxy list is load-bearing and fails quietly: a typo narrows it, and the symptom is
every customer sharing one bucket, seen much later and somewhere else. It is logged loudly at
startup for that reason.

Negative authentication answers are never cached. Caching them would mean a customer who has
just reconnected a client still cannot use it, and would hand a token-guessing caller a free way
to keep the cache warm.
