# 0001. Record architecture decisions

## Status

Accepted.

## Context

This repository is public, and customers read it before trusting a binary with their cloud
credentials. It also forbids explanatory comments in code ([0009](0009-rationale-lives-in-adrs.md)),
which removes the usual place a reader looks for "why is it like this".

Something has to hold the reasoning, or the code becomes unchangeable: a future maintainer cannot
tell a deliberate constraint from an accident, so they either preserve bugs or delete safeguards.

## Decision

We record architecturally significant decisions as ADRs in `docs/adr/`, in Nygard format, numbered
sequentially.

A merged ADR is immutable. Corrections are new records: the new one says "Supersedes 0004" and the
old one is edited only to add "Superseded by 0011". Nothing else about a merged record changes.

"Architecturally significant" means it constrains future work: a boundary, a protocol choice, a
security property, a public contract. Not code style, not a library version bump.

## Consequences

Writing one is friction on every non-trivial decision, which is the intended effect.

The immutability rule means the folder accumulates records that are no longer true. That is not
clutter. A superseded record with a pointer to its replacement tells a reader what was tried and
why it changed, which is exactly what a silently rewritten document destroys.

The ADRs, not the planning documents in the private repository, are authoritative. A plan is an
input written before the code existed. When the two disagree, the ADR is right.
