// Package mcp is the TechBeaver Model Context Protocol surface.
//
// Two deployments serve it: `beaver mcp` runs this package over stdio on a
// customer's own machine, and the hosted service at mcp.techbeaver.io serves
// the same tool surface from its own copy of this code. They are held in step
// by hand, not shared at build time, so a change here does not reach the hosted
// one. Both are HTTPS clients of the same /api/v1 carrying the caller's own
// token, so ownership checks, gates and the audit trail apply to either
// equally.
//
// The tool surface is deny-by-default: a tool the caller's scopes do not cover
// is not listed, so a model cannot suggest what it cannot do. Deletions are
// never carried out on a model's say-so; see destructive.go and ADR 0007.
package mcp
