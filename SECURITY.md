# Security policy

## Reporting

Email **security@techbeaver.io**. Please do not open a public issue.

Include what you did, what happened, and what you expected. If it needs a specific account state to
reproduce, say so rather than testing against another customer's resources.

## Scope

This repository is the client. It enforces nothing: ownership, scopes, spend limits and the
destructive-action approval gate are all decided by the platform, so a finding of the form "the CLI
can be patched to skip a check" is expected and is not a vulnerability. The interesting question is
always whether the server accepts the request.

Findings that are in scope here:

- a credential reaching disk, a log, an error message or the process table;
- anything weakening the OAuth flow: a missing `state` check, a downgrade from S256, a redirect that
  is not a loopback literal;
- a path where the update check could execute code;
- a dependency shipping something we then hand a customer's token to.

## The client_id is not a secret

A `client_id` is compiled into this binary and visible in the source. That is how a public OAuth
client works. It is an identifier, not a credential, and it grants nothing without a customer
completing a consent screen. See ADR 0005.

## Verifying a release

Every release is signed and carries build provenance. The verification steps are on the release page.
The risk worth defending against is not us publishing something bad, it is somebody else publishing
something bad under this name, and a signature you check is the defence.
