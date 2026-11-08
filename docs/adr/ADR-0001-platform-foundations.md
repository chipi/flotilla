# ADR-0001 — Platform foundations

**Status:** Accepted
**Date:** 2026-11-08
**Decided by:** the operator, on [RFC-0001](../rfc/RFC-0001-fleet-platform.md)

## Context

The homelab runs agents in four places, built separately:
- fleetd (Go);
- the triage fleet (Python);
- the bugfix fleet (Go chain plus bash and Python leaves);
- the daily brief (Node).

Each implements its own LLM client, ledger, metrics, config and GitHub access. More fleets are
coming, SEO first. The operator wants one platform that they own and understand from the inside,
not a third-party agent framework, with something working along the way. RFC-0001 has the
inventory and the design.

## Decision

1. **flotilla is our own plain fleet platform.** No agent framework. Hermes Agent, OpenClaw and
   others are inspiration, not dependencies.
2. **Fleets are groups inside one platform:** `triage`, `bugfix`, `seo`, `ops`, and more later.
   Fleets and jobs are **declared in specs**. Code is a library of named building blocks.
3. **A small control plane, `flotillad`, in Go**, grown from fleetd.
   - It owns specs, scheduling, runs, the ledger, approvals, budgets and one API.
   - The API serves a CLI, an MCP server and a small web UI in the same binary, independent of any
     other dashboard.
4. **The job runtime is in Python.** Jobs run as separate processes started by the control plane.
5. **The ledger** records every decision every fleet makes, with its evidence, cost, actor and
   outcome. It lives in SQLite on the host.
6. **An agent is never the operator.** Approving an outward action is operator-only, never through
   MCP. The ledger records who did what.
7. **Public repo, private state.**
   - In git: code, specs, playbooks and schemas.
   - On the host only: config, secrets, the ledger, findings and fleet data.
   - Production payloads stay in the private `fleet-eval-data` repo.
8. **It runs on the homelab mini** (64 GB once fitted) as the `claude` account. *Amended by
   [ADR-0002](ADR-0002-run-in-docker.md): it runs in containers in the production engine, deployed
   by the operator account, as non-root users.*
9. **Local models first.** Each job declares its model policy, and every call goes through LiteLLM.
   The daily brief is local only.
10. **It reports into the homelab's observability stack** like every other service: metrics, logs,
    traces, LLM traces, errors, ops events, dashboards and alerts.
11. **MIT licence. The docs site is MkDocs Material**, strict, on GitHub Pages.

## Consequences

**Positive:**
- One implementation of each shared piece.
- Fleets become configuration.
- Every decision is queryable and auditable.
- The operator learns the machinery by building it.

**Negative:**
- Two languages (Go and Python) at a process boundary.
- We maintain the agent loop and the UI ourselves.
- The migration must keep the live triage fleet running throughout.

**Neutral:** the existing fleets keep their behaviour; the migration is replay-proven, not a rewrite
of their logic.

## Alternatives considered

- **Adopt Hermes Agent or OpenClaw as the runtime.** Evaluated 2026-11-08 (see
  [agentic-ai-homelab's evaluation](https://github.com/chipi/agentic-ai-homelab/blob/main/docs/wip/agent-platform-evaluation.md)).
  Rejected for now: Hermes is unsupported on the Intel mini, has self-editing skills and cloud
  web search by default; OpenClaw has a heavy CVE stream. Both are kept as inspiration.
- **Everything in Python** (rewrite fleetd) or **everything in Go** (rewrite the triage fleet).
  Neither buys enough to justify the rewrite.
- **A separate frontend for the UI.** Deferred: the Go binary serves server-rendered pages.
