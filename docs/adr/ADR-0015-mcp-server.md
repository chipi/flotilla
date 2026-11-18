# ADR-0015 — The MCP server

**Status:** Accepted

**Date:** 2026-11-15

**Decided by:** the operator, on [RFC-0008](../rfc/RFC-0008-mcp-server.md), its five questions

## Context

Other agents (Claude sessions, the second machine's assistant) had no way into flotilla but SSH and
the container's CLI token, one identity for all. RFC-0001 promised an MCP server from the start.

## Decision

1. **The API's own functions as tools, under the caller's identity.** Every tool calls the function
   the CLI and the UI use, as the caller, so `internal/api/authorize.go` decides; MCP adds no rule and
   bypasses none. Read tools, and tightening action tools (run, dry run, pause, STOP, freeze, hold,
   propose a note). No tool approves, rejects or loosens (security T3); a test asserts it.
2. **The official Go SDK** (`github.com/modelcontextprotocol/go-sdk` v1.8.0), approved as a dependency
   (operator, Q1), not our own protocol code.
3. **Streamable HTTP, stateless, JSON responses, at `/mcp` on the UI listener** (Q2), so the UI's
   tailnet node serves it with TLS; one bearer token per caller (`callers.yaml`, role `agent`); the
   UI's cookie is refused there.
4. **Agents read the ledger** (Q3): `ledger-read` is open to the `agent` role.
5. **Agents propose notes** (Q4): a new action, `note-propose`, open to agents; verdicts and activating
   a note stay people's (`feedback`, `notes`).
6. **A dry run** (Q5): `api.DryRunNow` reports what a run starting now would be and whether it would
   start, from the scheduler's own skip check; nothing starts.
7. **Each call is an event** (`mcp.<tool>`, the caller, the outcome), and a caller may make 60 calls
   a minute.

## Consequences

- **Positive:** any MCP client gets the same tools with no glue; each agent's actions are recorded
  under its own name and can be cut off alone.
- **Negative:** nine more modules in `go.sum` (the SDK and what it pulls: a JSON-schema library,
  `segmentio/encoding` and `asm`, `uritemplate`, `golang.org/x/oauth2`, `x/sync`, `x/time`), recorded
  in [ADR-0003](ADR-0003-dependencies-and-images.md).
- **Neutral:** stateless sessions mean no server-to-client requests; none of the tools needs one.

## Alternatives considered

In RFC-0008: our own small server; stdio over SSH; the REST API to agents; a separate gateway.
