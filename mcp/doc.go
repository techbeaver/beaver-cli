// Package mcp is the TechBeaver Model Context Protocol surface.
//
// The same package serves two deployments: `beaver mcp` runs it over stdio on a
// customer's own machine, and it also runs as a hosted HTTP service. Anything
// that differs between those is configuration, never a second code path, so the
// locally-run one is not the untested one.
//
// The tool surface is deny-by-default: a tool the caller's scopes do not cover
// is not listed, so a model cannot suggest what it cannot do. Deletions are
// never carried out on a model's say-so; see destructive.go and ADR 0007.
package mcp
