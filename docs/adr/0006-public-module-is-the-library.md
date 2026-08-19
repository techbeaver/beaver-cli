# 0006. The public module is the library; beaver-api imports it

## Status

Accepted.

## Context

The MCP tool surface and the scope vocabulary already existed inside the private API repository,
serving the hosted MCP endpoint. The CLI needs both: `beaver mcp` offers the same tools over stdio,
and every command needs the scope names.

Two ways to arrange that. Copy the code into this repository and keep two versions agreeing by
discipline, or move it here and have the private repository depend on the public one.

Copying is the trap this organisation has already been bitten by, where one fact gets two spellings
and they drift apart silently. The scope vocabulary is exactly that kind of fact.

The move turned out to be cheap. The MCP server package depended on precisely one package inside the
private repository, the scope vocabulary, and that had no internal dependencies at all and needed no
database, no web framework and no driver. An import guard test had been keeping it that way.

## Decision

`mcp/`, `scopes/` and `client/` live here and are public. The private API repository imports this
module for its hosted MCP server, which becomes thin wiring.

Three public packages, and no more. Everything else is under `internal/`, where Go's own rule makes
it unimportable by other modules. An exported symbol in a published module is a promise, so moving
something out of `internal/` is a decision rather than a default.

## Consequences

Dependency direction is now public to private. The private repository consumes this one, never the
reverse. Any change here that breaks compilation breaks a production service, so the public API is
load-bearing from the first release.

Adding a scope becomes two steps: release this module, then bump the dependency there. That is real
friction on a file that changes reasonably often, and it is the price of having one spelling.

The import guard test moves with the code. It is what keeps this package free of the private
repository's dependencies, and dropping it in transit would quietly undo the property that made the
move cheap.
