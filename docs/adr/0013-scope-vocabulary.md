# 0013. The scope vocabulary, and its two invariants

## Status

Accepted.

## Context

Authorisation across the platform is a role plus an organisation role. A role cannot express "this
credential may read applications and stream logs, but may not delete a database or spend money".

A credential that can do everything its owner can do is a credential that can drop a production
database because a model misread a log line. The scope vocabulary exists to make that expressible.

This package moved here from the private repository ([0006](0006-public-module-is-the-library.md)),
and its package documentation was trimmed to Go's standard form
([0009](0009-rationale-lives-in-adrs.md)). The reasoning that was in those comments is here.

## Decision

Two invariants hold, and tests enforce them rather than convention.

**There are no administrative scopes.** Not "off by default": absent from the vocabulary entirely. A
machine credential is rejected on every administrative route before any scope is consulted, so no
combination of grants reaches one. A test fails if a scope name ever contains an administrative word.

**A defined scope is not necessarily issuable.** `billing:spend` is the case: it would let a
credential move money without a human at a payment page, by spending account credit against an
invoice. The column, the vocabulary entry and the enforcement all ship, so enabling it later is a
consent-screen change rather than a migration. `Issuable` reports false for it and nothing may grant
it.

Two further rules on the consent wording:

- descriptions describe what can be done to the customer's account, not HTTP verbs, because the
  audience is a person deciding whether to approve;
- **descriptions never quote a price, a plan name or a limit.** Those are read from the API at call
  time. A number in a constant is a number that goes stale silently, and a consent screen is the
  worst place for a stale number. A test bans currency symbols, plan names and percentages.

The default grant excludes both destroy scopes and `billing:spend`, so the ordinary grant cannot tear
anything down.

## Consequences

The vocabulary is now a published module, so adding a scope means releasing this module and then
bumping the dependency in the private repository. That is the friction accepted in
[0006](0006-public-module-is-the-library.md), taken in exchange for there being one spelling of the
list rather than two that drift.

`Parse` returns unrecognised scopes rather than dropping them. Silently ignoring one is how a client
comes to believe it holds a permission it was never granted, and then fails much later with a refusal
it cannot explain.

Adding an administrative scope is not a decision anyone can make in a pull request. The test fails,
and getting past it means arguing with this record.
