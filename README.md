# beaver

The command line interface for [TechBeaver](https://techbeaver.io). Create projects, deploy
applications, run managed Postgres, tail logs and settle invoices from a terminal.

One static binary. No runtime to install.

## Install

```sh
curl -fsSL https://techbeaver.io/install.sh | sh
```

Windows, in PowerShell:

```powershell
irm https://techbeaver.io/install.ps1 | iex
```

Prefer not to pipe a script into a shell? That is a reasonable position. Download the binary for your
platform from [Releases](https://github.com/techbeaver/beaver-cli/releases), verify the checksum and
signature as shown there, `chmod +x` it and move it onto your `PATH`.

## Getting started

```sh
beaver auth login          # opens a browser to approve access
beaver auth whoami
beaver projects list
```

On a machine with no browser:

```sh
beaver auth login --no-browser    # prints a code to enter on another device
```

## Using it

```sh
beaver apps list
beaver apps deploy --wait
beaver apps logs --follow
beaver db create --plan starter
beaver billing invoices list
```

Pin a project and application once, and the commands in that directory get shorter:

```toml
# beaver.toml
project = "my-project"
app     = "my-api"
```

## Output

`--format json|yaml|table|value(...)`. **When output is not a terminal the default is JSON**, so a
pipe never receives a table you have to parse.

```sh
beaver apps list --format value(name,status)
beaver apps describe my-api | jq .data.url
```

Exit codes are part of the interface and are safe to branch on:

| code | meaning |
|---|---|
| 0 | success |
| 1 | error |
| 2 | usage error |
| 3 | not authenticated |
| 4 | not found |
| 5 | payment required |
| 6 | awaiting human approval |
| 7 | rate limited |

## Deleting things works differently here

`beaver apps delete` does not delete anything by itself. The platform creates a pending confirmation
and a human approves it in a browser. The CLI prints the URL and waits.

`--yes` skips the local prompt. It cannot skip the human, because that gate is enforced by the server
and is not the CLI's to waive. Unattended runs exit `6`, meaning "waiting for a person", which is a
state a script can retry rather than a failure.

This is deliberate. The same credential can drive an AI agent, and an agent that can drop a
production database because it misread a sentence is not something to hand anybody.
See ADR 0007.

## Use it as an MCP server

`beaver mcp` speaks the Model Context Protocol over stdio, exposing the same tools with your own
credential, so a local AI client can drive your infrastructure without a hosted connection.

```json
{
  "mcpServers": {
    "beaver": { "command": "beaver", "args": ["mcp"] }
  }
}
```

For a hosted connection instead, point your client at `https://mcp.techbeaver.io/mcp` and approve it
in the browser.

## Security

- Authentication is OAuth 2.1, authorization code with PKCE S256, over a loopback redirect. No client
  secret is shipped, because a distributed binary cannot keep one.
- Tokens go in your platform keychain. Where there is none, a `0600` file, and a credential file with
  looser permissions is refused rather than warned about.
- Tokens are never accepted as command line arguments, because arguments are visible to other users
  in the process table.
- This binary enforces nothing. Ownership, scopes, spend limits and the approval gate are all decided
  by the server, so an old, patched or forked copy gains nothing.
- The update check reports a new version. It never downloads or executes anything.

Report a vulnerability to security@techbeaver.io rather than in a public issue.

## Why it is built this way

Decisions are recorded as numbered ADRs in TechBeaver's internal register rather than as
code comments, and the code cites them by number: 0003 covers why this repository is open
at all, 0004 the login flow, 0007 the deletion gate. The rules those set are stated in full
in SECURITY.md and CONTRIBUTING.md, so nothing you need in order to read or build this is
behind that register.

## Licence

Apache-2.0. See [LICENSE](LICENSE).
