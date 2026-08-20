# Architecture decision records

Rationale for this repository lives here, not in code comments. See [0009](0009-rationale-lives-in-adrs.md).

Format: [Nygard](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions). Every
record has Title, Status, Context, Decision, Consequences.

**A merged ADR is never edited.** A decision that changes gets a new record that supersedes it, and
the old one is marked superseded. A document that quietly self-corrects gives no signal about which
of its remaining claims to trust.

| # | Decision | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-go-single-static-binary.md) | Go, one static binary, no runtime dependency | Accepted |
| [0003](0003-public-repository-boundary.md) | Public repository, and what never leaves beaver-api | Accepted |
| [0004](0004-loopback-pkce-first.md) | Loopback PKCE is the primary login | Accepted |
| [0005](0005-pre-registered-oauth-client.md) | A pre-registered public client, not dynamic registration | Accepted |
| [0006](0006-public-module-is-the-library.md) | The public module is the library; beaver-api imports it | Accepted |
| [0007](0007-server-side-destructive-gate.md) | Destructive actions are gated server-side | Accepted |
| [0008](0008-exit-codes-and-format-are-contract.md) | Exit codes and --format are a public contract | Accepted |
| [0009](0009-rationale-lives-in-adrs.md) | Rationale lives in ADRs, not in comments | Accepted |
| [0010](0010-credential-storage.md) | Credential storage: keychain, with a 0600 file fallback | Accepted |
| [0011](0011-consumer-declared-interfaces.md) | Interfaces are declared by consumers | Accepted |
| [0012](0012-distribution-install-script-first.md) | Install script first, package managers are tier 2 | Accepted |
| [0013](0013-scope-vocabulary.md) | The scope vocabulary, and its two invariants | Accepted |
| [0014](0014-http-only-api-client.md) | The client speaks HTTP only, and re-implements no rule | Accepted |
| [0015](0015-ci-workload-identity.md) | A CI pipeline signs in with its own identity, not with a stored secret | Accepted |
| [0016](0016-the-mcp-edge-limits-before-it-authenticates.md) | The MCP edge limits before it authenticates, and keys on the credential | Accepted |
