# 0007. Destructive actions are gated server-side, and --yes cannot bypass a human

## Status

Accepted.

## Context

The platform requires a human to approve destructive actions taken with a machine credential:
deleting an app, destroying a database. The server creates a pending confirmation and the action
does not happen until a person approves it in a browser.

This exists because the same credential type drives an AI agent. An agent that can delete a
production database because it misread a sentence is not a tool anyone should deploy.

A CLI is usually where that kind of protection gets an escape hatch. `--force` and `--yes` are
conventional, and users expect them to mean "stop asking and do it".

## Decision

The gate is server-side and the CLI cannot bypass it, because it is not the CLI's to bypass.

`beaver apps delete` sends the request, receives a pending confirmation, prints the approval URL and
waits. `--yes` suppresses the CLI's own local "are you sure" prompt and nothing else.

A non-interactive run against an unapproved confirmation exits **6**, not 1. Six means "waiting for a
human", which is a retryable state, and a script has to be able to tell it from a failure.

## Consequences

This will surprise people who know `gcloud`, and it will be reported as a bug. The documentation
leads with it rather than burying it in a flag description.

A fully unattended pipeline cannot delete infrastructure. That is the intended outcome. A pipeline
needing to tear down its own preview environments needs a platform-level mechanism designed for it,
not a flag on this binary.

It is also the single property that makes it defensible to hand this binary to an agent, which is
worth more than the convenience it costs.
