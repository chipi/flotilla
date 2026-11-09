# ADR-0005 — The agent pipeline: cases, capabilities, recall, policy and the actuator

**Status:** Accepted
**Date:** 2026-11-09
**Decided by:** the operator, on [RFC-0004](../rfc/RFC-0004-agent-pipeline.md) (revision 8, after
seven passes by an independent reviewer)

## Context

ADR-0004 gave flotilla's job runtime its core concepts, shaped around triage. The operator wants one
pipeline and one set of building blocks for every fleet to come (triage; Ops, the SRE team in action
and the largest fleet; digital marketing; bugfix), and fleets that **act**, trustworthy while the
operator is away. RFC-0004 has the design, the fleets end to end, and the reviews.

## Decision

1. **Every problem is a case** with a lifecycle (detected → diagnosed → handed over → planned →
   awaiting approval → executing → checking → closed, or escalated), enforced by a transition table
   with stuck-state timers. Cases replace decisions, work items, approvals and executions. Hand-overs
   between jobs are a case moving; triage still files GitHub issues as the mirror of bug cases.
2. **One capability registry** for reads and writes: named, typed, scoped, `for_kinds`, risk,
   reversibility. Agents plan writes; they never run them.
3. **Recall** replaces memory and external knowledge: what flotilla recorded (exact lookup by key) and
   what is plugged in (git, HTTP, MCP; path maps, full-text and semantic search), each source declared
   trusted or untrusted per path; a run that read untrusted content is tainted and capped at propose.
4. **Policy in flotillad decides; workers execute.** Autonomy, lowest wins: the job's stage, the
   capability's earned level (shadow → propose → auto, promoted only by the operator, per capability
   on every machine, within each host's ceiling), taint, the autonomy freeze.
5. **Workers per role**, not per fleet; Ops starts with `ops-read` and `ops-act-host`.
6. **Host writes through an actuator**: a host service with an allowlist, idempotent by execution id,
   with its own ADR before it is built.
7. **Guardrails against runaway behaviour:** per-target lock, holds, deploy stand-down, per-host and
   platform caps, a circuit breaker, a host incident rule, `caused_by` linking, the recurrence rule,
   the autonomy freeze, a gateway re-route alert.
8. **Communication channels** (alerts, approvals, notifications) routed to providers; Slack first,
   with approvals in Slack within limits (low and medium risk, reversible or regenerable, bound to the
   plan's hash).
9. **No workflow engine.** Model-written commands are decided later.
10. **v1:** one chain, one host, one write: a code monitor, a `failed_backup` case, the
    `backup.rerun_observability` write in shadow, a timer check.

RFC-0004 §12 lists all 31 decisions.

## Consequences

**Positive:**

- One pipeline and one set of rules for every fleet; a new fault is a case kind, not a new design.
- Every action is bounded by code (registry, policy, actuator), earns its autonomy, and is recorded
  on its case.
- One record per problem: `flotilla case show <id>` is the whole story, and the UI's main screen.

**Negative:**

- New privileged component (the actuator) and new dependencies (`jsonschema`, a vector store, a Slack
  Socket Mode client), each approved when built.
- More workers to run.
- Approvals in Slack make the operator's Slack account an approval key, within limits.

**Neutral:**

- ADR-0002, ADR-0003, ADR-0004, RFC-0001 and AGENTS.md rule 2 are amended (RFC-0004 §13).

## Alternatives considered

RFC-0004 §14.
