# 0003. Public repository, and what never leaves beaver-api

## Status

Accepted.

## Context

This binary receives a customer's cloud credentials and acts on their infrastructure. A careful
customer is right to refuse a closed-source tool in that position, and to want to build it
themselves and compare.

The platform's API repository is private and will stay private. So the boundary needs stating
precisely, once, rather than being decided per pull request by whoever is looking.

## Decision

This repository is public under Apache-2.0. The platform API repository stays private.

**Never in this repository, permanently:**

- the route allowlist that authorises machine credentials;
- anything from the platform's authentication internals;
- deployment manifests, cluster hostnames, node addresses;
- any credential, key or `.env` file.

A CI job greps every commit for the markers of these and fails the build. It runs from the first
commit rather than being added after the first mistake.

## Consequences

**The security guarantee does not rest on that secrecy, and this is the part that must survive.**
Authorisation is deny-by-default and enforced on the server. It holds against somebody who has read
the allowlist, because reading it grants nothing. The list stays private because it is internal
configuration, not because it is a control.

Nobody should ever reason "the allowlist is private, therefore X is safe" and skip a check on that
basis. If a future decision depends on the list being secret, that decision is wrong.

The practical consequence for contributors: a feature needing a route that is not already allowed
cannot be built here. It needs a change in the private repository first. That is a real constraint
and it is the intended one, because it means this tool cannot quietly acquire reach that the agent
surface was denied.
