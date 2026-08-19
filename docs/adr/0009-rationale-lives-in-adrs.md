# 0009. Rationale lives in ADRs, not in comments

## Status

Accepted.

## Context

The private API repository uses long explanatory comments, sometimes many lines, carrying the reason
a thing is the way it is. That suits a repository read mainly by the people who wrote it.

This one is public and is read by customers evaluating whether to trust it. Dense internal commentary
reads differently in that setting: it is where speculation, stale reasoning and internal detail leak
out, and it ages badly because a comment near changed code is rarely updated with it.

## Decision

**Verbose comments are forbidden in this repository.**

- Doc comments on exported identifiers, standard form. Enforced by the linter's exported rule.
- Inline comments only where the code cannot be made to say it: a protocol requirement, a workaround
  for a third-party bug, a non-obvious ordering constraint. One line.
- Everything else that wanted to be a comment goes in an ADR, and the code cites it by number where
  the link is worth having.

This is a deliberate break from the other repository rather than drift, which is why it is written
down instead of being left to whoever reviews.

## Consequences

The ADR folder is load-bearing, not decorative. Forbidding explanatory comments without giving the
explanation somewhere to live produces code nobody can safely change, so
[0001](0001-record-architecture-decisions.md) and this record are one system and neither works alone.

Reviewers gain a specific job: a pull request explaining itself in comments is asking for an ADR.

Code must carry more of its own meaning, through naming and structure, because it no longer has prose
to lean on. That is the intended pressure.
