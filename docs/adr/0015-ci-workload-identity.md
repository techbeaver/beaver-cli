# 0015. A CI pipeline signs in with its own identity, not with a stored secret

## Status

Accepted.

## Context

The only credential that worked unattended was a personal token pasted into a CI secret. It has
three problems, and only the first is the one people notice:

1. The platform caps it at 90 days, so a pipeline breaks four times a year, at whatever hour it
   runs, on a day nobody changed anything.
2. It is a long-lived bearer secret. Every workflow the repository will ever run can read it,
   and so can anything that prints the environment.
3. A deploy made with it is attributable to "somebody holding the org token" and nothing more.

Raising the cap fixes the first and makes the second worse. A year-long credential in a CI
variable is the thing most likely to end up in a public build log.

GitHub, GitLab, CircleCI and Buildkite all run an OIDC provider that issues a short-lived signed
token describing the running job. That token is not a secret in the same sense: it expires in
minutes, and it names an audience.

## Decision

The CLI exchanges the runner's OIDC token for a machine token (RFC 8693), against a binding the
customer created in the portal.

- **No flag is required.** `beaver auth login` picks the runner's identity when it detects one,
  and any command with no stored credential signs itself in the same way. A workflow needs one
  `permissions: id-token: write` line and nothing else. `--ci` forces the path so a
  misconfigured runner fails loudly rather than silently opening a browser nobody will see.
- **The audience is discovered, never hardcoded.** The CLI reads the platform's RFC 9728
  protected-resource document and asks the runner for a token minted for exactly that. A
  hardcoded audience would be wrong on every deployment but one, and would fail with the wrong
  error message.
- **No refresh token is kept.** The exchange returns an access token and nothing else. The job
  holds the thing that mints tokens, so it can always exchange again. Keeping a refresh token
  would put back the long-lived secret this exists to remove.
- **The server's refusal is passed through verbatim.** It names what did not match: the
  repository, the ref, the audience.

## Consequences

Nothing this CLI does can make the exchange safe on its own. Everything load-bearing is on the
server: the audience check, the repository binding, the replay guard. This ADR describes a
client of that design, not the design. It is written down in the platform repository at
`docs/ci_workload_identity_plan.md`.

The exchanged token is written to the credential store like any other, so a job's later commands
do not each exchange again. That is a deliberate trade against keeping it in memory: memory does
not survive between `run:` steps, and a runner is destroyed at the end of the job.

`WorkloadName()` reads `GITHUB_ACTIONS`, which any process can set. A developer who sets it on a
laptop gets a failed exchange, not a credential, because the runner's token endpoint is what
actually issues the proof. Detection is a routing decision, not a security one.

Only GitHub is implemented. The others differ from it in the issuer and the claim names, so
adding one is a table entry on the server plus a `workloadSource` here.
