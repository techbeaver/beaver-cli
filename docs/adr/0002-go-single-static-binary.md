# 0002. Go, one static binary, no runtime dependency

## Status

Accepted.

## Context

The customers this targets are on mixed machines, often with no Node, no Python and no package
manager they are used to administering. Anything that starts with "first install a runtime" loses
people before the first command.

The platform's server code is already Go, and the MCP tool surface this CLI reuses
([0006](0006-public-module-is-the-library.md)) is Go. A different language would mean reimplementing
that surface and keeping two implementations agreeing.

## Decision

Go, compiled with `CGO_ENABLED=0`, shipped as one statically linked executable per platform and
architecture. No runtime, no shared libraries, no post-install step. Installing is placing one file
on `PATH`.

Supported targets: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64.

## Consequences

`CGO_ENABLED=0` rules out any dependency needing cgo. That constrains credential storage
([0010](0010-credential-storage.md)) to libraries that reach the platform keychain over IPC or a
subprocess rather than by linking against it.

Binaries are large by scripting-language standards, tens of megabytes. That is the cost of having no
runtime, and it is the right trade for a download people run once.

The same compiled artefact serves `beaver mcp`, so an AI client gets the tool surface with no second
install.
