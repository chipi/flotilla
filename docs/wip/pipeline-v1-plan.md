# Pipeline v1 plan: cases, policy and the first write, in shadow

→ becomes a `docs/history/` entry when v1's gate is met (5 shadow plans that match what the operator
would have done).

**Status:** started 2026-11-13 (operator: "yes" to building the cases backend, so that the UI's
slice 2,
Decide, has something to show). The design is [ADR-0005](../adr/ADR-0005-agent-pipeline.md) (from
[RFC-0004](../rfc/RFC-0004-agent-pipeline.md)) and, for the host side,
[ADR-0006](../adr/ADR-0006-node-agent.md); v1's scope is RFC-0004 §10 and ADR-0006 §12. This plan
only orders the work and says how each step is proven.

## What v1 is

One chain, one host, one write, in shadow. A code-only Ops monitor reads the observability backup's
age on the mini and opens a `failed_backup` case; an `ops-act-host` job re-checks it and plans
`backup.rerun_observability(mini)`; policy records the plan; in shadow the case closes as "shadow,
plan recorded". Nothing is sent to a host in v1 (ADR-0006 §12).

## Corrections the design needs on the way

- **The freeze caps at `shadow`, not `propose`.** RFC-0004 §5.1 and §5.3 predate ADR-0014, which
  changed it; policy follows the ADR. The freeze is already built (`sched.FreezeSubject`).
- **Cases do not replace the ledger tables yet.** RFC-0004 §3.5 says the case tables take their place;
  the ledger was built first (schema step 4) and triage moves onto it on 2026-11-14. Triage as a case
  producer is not in v1 (§10), so v1 adds case tables beside the ledger; folding the ledger into
  cases is its own step, with the fleet session, later.

## Steps

Each step lands on main with its tests and is not deployed until the operator says so. Schema steps
5 (users, sessions) and 6 (cases) then deploy together, with the store-copy check and rollback of
RFC-0007 §7.

| # | Step | Proven by |
| --- | --- | --- |
| 1 | **The case store** (schema step 6): cases, their keys, their timeline; the transition table in Go (from, to, who, the longest stay); one open case per canonical key; events appended to closed cases; `flotilla case list` and `case show`; `GET /v1/cases`, `GET /v1/cases/{id}` (viewer and up) | every allowed transition taken and every other refused; two detects of one key at once give one case; a closed case takes events but not a status change |
| 2 | **Holders, leases and stuck-state timers**: a case held by one job; a lease renewed while it runs; `detected` too long alerts, `handed_over` too long escalates, `executing` past its deadline becomes `unknown` and holds the target | a fake clock walks each timer; an expired lease returns the case unless an execution is open |
| 3 | **The capability registry**: entries as files in the repo (`capabilities/*.yaml`), strict, validated at load like specs; `backup.rerun_observability` first | a bad entry stops flotillad at load; the entry's fields match RFC-0004 §3.4 |
| 4 | **Policy** in flotillad, RFC-0004 §5.1 in order: registry (scope, `for_kinds`, no retargeting, cited numbers), autonomy lowest wins (job stage, capability level, host ceiling, taint, the freeze), the per-target lock and limits, holds (STOP, `flotilla hold`, deploy stand-down), the platform cap and circuit breaker; every refusal recorded with its reason | a table of plans and states, each with its expected decision and reason; mutants for each rule |
| 5 | **Event triggers and the job contract**: a case handed over to a kind starts that kind's job with the case and its lease; a job returns a plan (`plan` output, writes only); checks are timer events | e2e: a case handed over starts a run on the worker, which gets the case |
| 6 | **The Ops monitor** (code only, `ops-read`): the backup's age from VictoriaMetrics; opens `failed_backup` at the shadow threshold (20 h) | against recorded metric values; dedup holds across cycles |
| 7 | **The `ops-act-host` planning job, in shadow**: the code re-check, the plan, the case closed as "shadow, plan recorded" | e2e end to end with a stand-in metric; then live on the mini in shadow (operator's go) |
| 8 | **Slice 2 of the UI** (ADR-0014): Cases and Case, with approvals by CLI first | the UI checks of RFC-0007 §7 |
| 9 | **Before any write at propose:** the node agent of ADR-0006 §1-§7 on the mini, the audit of §8, the `notifications` channel | its own plan, when v1's shadow gate is met |

## Not in v1

RFC-0004 §10's list: triage as producer, the brief, recall, the reviewer agent, config-change PRs,
`cost_spike`, marketing, actuators on the DGX and the VPS. Also Slack approvals (with the first
promotion to propose).

## Progress

- **Step 1, the case store:** built 2026-11-13. Schema step 6 (`cases`, `case_keys`, `case_events`;
  one open case per canonical key is a partial unique index); `internal/cases` holds the transition
  table, tested against the diagram written out separately; `flotilla case list` and `case show`,
  `GET /v1/cases` and `GET /v1/cases/{id}`. 7 mutants killed. The longest stay per status moved to
  step 2, with the timers. Before deploying: the store-copy check again, now for steps 5 and 6.
