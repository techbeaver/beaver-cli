# Contributing

## Before a pull request

```sh
go build ./... && go test -race ./... && go vet ./...
golangci-lint run
./scripts/check-no-secrets.sh
```

## House rules that differ from most Go repositories

**Verbose comments are forbidden.** Doc comments on exported identifiers, and inline comments only
where the code genuinely cannot say it: a protocol requirement, a third-party bug, an ordering
constraint. One line.

Everything else that wants to be a comment goes in `docs/adr/`. If a pull request explains itself in
comments, it is asking for an ADR. See [0009](docs/adr/0009-rationale-lives-in-adrs.md).

**Merged ADRs are never edited.** A decision that changes gets a new record saying "Supersedes NNNN",
and the old record gains one line saying it was superseded. Nothing else changes.

**Interfaces are declared where they are consumed**, holding only the methods that consumer calls,
not next to the implementation. See [0011](docs/adr/0011-consumer-declared-interfaces.md).

**Three packages are public** (`client`, `scopes`, `mcp`) and everything else lives under `internal/`.
An exported symbol in a published module is a compatibility promise, so moving something out of
`internal/` needs a reason in the pull request.

**A new dependency needs a reason in the pull request.** This binary handles cloud credentials and
every dependency is code that runs with them.

## What cannot be merged here

Per [ADR 0003](docs/adr/0003-public-repository-boundary.md), and enforced by CI:

- anything copied from the private platform repository;
- internal hostnames, cluster addresses or deployment manifests;
- any credential or key.

A feature needing an API route that is not already permitted for machine credentials cannot be built
here. The server decides that, and it has to change there first.

## Security-sensitive areas

Changes to `internal/auth`, `internal/credential` or `client` transport get a closer read. The rules
in [0004](docs/adr/0004-loopback-pkce-first.md) and
[0010](docs/adr/0010-credential-storage.md) are requirements, not preferences:

- PKCE S256 only, `state` verified, the loopback listener bound to the loopback address explicitly
  and never to all interfaces;
- no token in `argv`, no token in an error message, no `InsecureSkipVerify` behind any flag;
- credential files `0600` in a `0700` directory, and a looser mode refused on read.

## Exit codes and output are a contract

Someone's pipeline depends on them. Adding an exit code is fine; changing what one means is a major
version. Same for the shape of documented JSON.
See [0008](docs/adr/0008-exit-codes-and-format-are-contract.md).

## Reporting a vulnerability

security@techbeaver.io, not a public issue.
