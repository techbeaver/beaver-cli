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

Both installers refuse to install when the checksum is missing or does not match. When `cosign` is
installed they also verify the signature on `checksums.txt` against this repository's release workflow.
Without `cosign` they say so and continue on the checksum alone, which shows the download is intact but
not who built it. Set `BEAVER_REQUIRE_SIGNATURE=1` to refuse to continue without `cosign`.

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
beaver db create my-db --plan PLAN_ID      # plan ids: beaver plans list --db
beaver apps env set LOG_LEVEL=debug        # other variables are kept; --replace drops them
beaver apps env list                       # names only, values are masked; --reveal needs secrets:read
printf %s "$VALUE" | beaver apps env check DATABASE_URL   # is it set, does it match, without revealing it
beaver billing invoices list
beaver billing invoices pay INVOICE_ID     # prints a payment link; add --idempotency-key K to retry safely
beaver apps deployments                    # revisions, newest first; the live one has isActive true
beaver apps rollback --revision REVISION   # put a named earlier revision back, nothing is rebuilt
beaver --debug apps list                   # one stderr line per request, never credentials or bodies
```

## Sandboxes

Sandboxes need scopes that the default sign in does not include. Tick them in the agent connection's
permissions, or sign in with them:

```sh
beaver auth login --scope sandbox:read,sandbox:write,sandbox:files,sandbox:exec,sandbox:destroy,billing:read,billing:checkout,projects:read
```

`sandbox:exec` also covers files. Sandbox commands take the project id (not its name): `--project ID`.

```sh
beaver sandbox plans                                  # plans and prices, read live
beaver sandbox quote --plan PLAN_ID --slots 2         # what it costs your account
beaver sandbox buy --plan PLAN_ID --slots 2           # prints a payment link; beaver cannot pay
beaver sandbox usage                                  # balance, hourly burn, hours remaining
beaver sandbox subscriptions                          # slots bought and in use, and whether auto-renew or the card top-up is on
beaver sandbox statements list                        # monthly statements of by-the-hour use, newest first
beaver sandbox statements current                     # the month so far, with line items
beaver sandbox statements show STATEMENT_ID
beaver sandbox add-slots quote SUBSCRIPTION_ID --slots 1

beaver sandbox create "my box"                        # uses a free slot (a slot holds one sandbox, running or stopped), sends an Idempotency-Key
beaver sandbox list | get ID | start ID | stop ID
beaver sandbox destroy ID                             # needs your approval in a browser, like apps and databases

beaver sandbox exec ID -- python3 -c 'print(1)'       # stdout, stderr and the exit status are the command's own
beaver sandbox exec ID --sh 'make test | tail' --timeout 5m --cwd /workspace/app --env CI=1
echo data | beaver sandbox exec ID -- wc -c           # stdin is sent when it is a pipe (--no-stdin to skip)
beaver sandbox cp ./app.tar ID:/workspace/app.tar     # uploads in 3 MiB chunks
beaver sandbox cp ID:/workspace/out.log ./out.log     # streamed; --force to replace a local file
beaver sandbox ls ID:/workspace
beaver sandbox mkdir ID:/workspace/new
```

`exec` exits with the remote command's status. It waits 30 seconds, then polls (each call at most 60
seconds, so nothing outlasts the edge) and prints the command's own output once, at the end. A command
may run up to `--timeout` (1 second to 1 hour, default 2 minutes); past it, it is killed and `beaver`
exits 124. Ctrl-C cancels the command and exits 130, and a second Ctrl-C leaves at once. Output beyond
4 MiB per stream is cut and a warning is printed on stderr. `--json` prints the whole result.

```sh
beaver sandbox exec ID --detach -- ./long-job.sh     # prints the exec id and returns
beaver sandbox exec-status ID EXEC_ID --wait 60      # running with its last output, or the result
beaver sandbox exec-cancel ID EXEC_ID
```

Results are kept for 10 minutes after a command ends.

A statement is a summary of charges that were already invoiced and settled, never an invoice, and the
statements commands only read (`billing:read`). An open statement lists the windows settled so far, not
the hours still running: `beaver sandbox usage` has that estimate.

By the hour: creating or starting a sandbox on a by-the-hour subscription spends your balance, which needs
`billing:spend`. No sign in or agent connection can be granted it, so beaver is refused with
`billing_spend_required` and nothing is created or charged. A monthly slot needs only `sandbox:write`.
`beaver sandbox subscriptions` shows which kind each subscription is.

Not available to the CLI, because the platform keeps those routes closed to tokens: remote file
delete, subscription changes other than the add-slots quote (reduce, cancel, resume), turning
auto-renew or the by-the-hour card top-up on or off (use the portal), by-the-hour activation and events.
`buy` only returns a payment link: paying from a balance is not offered. When a dollar wallet is short,
the link is a wallet top-up page, and the purchase is paid from the wallet by itself once it lands.

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

What the client can reach is decided by whoever starts the server, never by the model:

- `beaver mcp --read-only` offers only the tools that change nothing.
- `beaver mcp --scope projects:read,paas:read` offers only tools whose required scopes are all listed.
- `beaver mcp --allow-secrets` also offers the tools that put secret values (environment variables,
  database passwords) into the conversation. They are off by default, and need a credential granted
  `secrets:read`.

Sandboxes are covered by tools for plans, usage, monthly statements (`list_sandbox_statements`,
`get_current_sandbox_statements`, `get_sandbox_statement`), buying, lifecycle, commands and files.
Destroying a sandbox is not a tool: use `beaver sandbox destroy`.

Servers and the dollar wallet are covered too, by `list_server_plans`, `quote_server_order`,
`order_server` and `create_wallet_topup_link`. An agent can never pay. A naira server order returns a
Paystack payment link, a dollar order returns a portal link where you confirm the wallet payment, and a
wallet top-up is a Stripe hosted link. `order_server` creates a draft only: nothing is provisioned or
charged until you pay.

For a hosted connection instead, point your client at `https://mcp.techbeaver.io/mcp` and approve it
in the browser.

## Security

- Authentication is OAuth 2.1, authorization code with PKCE S256, over a loopback redirect. No client
  secret is shipped, because a distributed binary cannot keep one.
- Tokens go in your platform keychain. Where there is none, a `0600` file, and a credential file with
  looser permissions is refused rather than warned about.
- A token is only ever sent to the host it was issued for. `beaver.toml` cannot set the API host (the
  CLI warns and ignores it), so a cloned repository cannot redirect your credentials. Plain `http` is
  accepted only for `localhost`.
- Secret values are masked unless you ask for them with `--reveal`, which needs a credential granted
  `secrets:read`. That scope is never in the default grant: sign in with
  `beaver auth login --scope ...,secrets:read` when you need it.
- In a CI job the runner identity is only exchanged with the default host. Running your own deployment
  needs `BEAVER_ALLOW_CUSTOM_HOST=1` in the job.
- `beaver auth logout` revokes the refresh and access tokens on the platform before removing them here,
  and tells you if revocation failed.
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
