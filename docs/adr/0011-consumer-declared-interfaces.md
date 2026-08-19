# 0011. Interfaces are declared by consumers

## Status

Accepted.

## Context

The habit Go inherits from other languages is to define an interface next to its implementation, so
`client` exports both a concrete client and a `Client` interface describing everything it does. The
interface then grows to mirror the struct, and every consumer depends on all of it regardless of what
it uses.

Go's own convention is the reverse, and it is what makes packages testable without a mocking
framework.

## Decision

A package that needs a dependency declares the interface it needs, at the point of use, containing
only the methods it calls.

A command needing to list applications declares an interface with the one method it calls, and takes
that. `client` exports concrete types and does not export an interface describing itself.

## Consequences

Tests are written against small local interfaces with hand-written fakes. No mocking library, and no
generated mocks to keep in step.

The same method may appear in several small interfaces across the tree. That duplication is the point,
not a DRY violation: each declares what one consumer needs, and they change independently.

`client` can gain methods without any consumer noticing, because no consumer depends on a description
of everything it can do.

It also keeps [0006](0006-public-module-is-the-library.md)'s public surface small. An exported
interface is a promise about every method in it, and this avoids making that promise at all.
