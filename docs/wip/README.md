# WIP — work in progress

This directory holds work-in-progress notes: plans, drafts, analyses,
prioritized task lists. Promotion targets:

| Promote to | When |
| --- | --- |
| `docs/adr/` | The decision is structural and final |
| `docs/rfc/` | The decision needs a proposal-and-review process before adoption |
| A reference doc | The content is stable enough to be a reference |
| `docs/history/0NNN-...md` | The session that produced it has wrapped |
| *delete* | Turned out not to matter |

## Open notes

- [Closing the initial vision](vision-plan.md): the MVP, its steps in the operator's order, where
  each stands, and what else is open.
- [Pipeline v1 plan](pipeline-v1-plan.md): cases, policy and the first write in shadow (ADR-0005,
  ADR-0006); built, its gate (5 judged shadow cases) still open.
- [UI mockup parity](ui-mockup-parity.md): every mockup v3 element against what is built,
  the overrides and the data gaps.
- [The UI, next](ui-next-plan.md): the operator's three buckets; the first two largely built.
- [Bugfix phase B brief](bugfix-phase-b-brief.md): the handover for the bugfix fleet's evaluation.
- [Fleets to working](fleets-to-working.md): keep every fleet and make each work (the operator,
  2026-11-26); where each stands and what to settle first.
- [The operator fleet](operator-fleet-plan.md): ops becomes operator, on every host; the cases backed
  by 60 days of incidents, the rungs, and the order (2026-11-26).

Closed notes are listed in their history entry ([0003](../history/0003-wip-notes-closed.md)).

WIP notes that haven't been touched in a quarter should be either promoted
or deleted. Stale plans rot, and stale plans that look "official" rot worst.

## Convention

- Files here are tracked in git (so they're findable + diffable).
- Names are descriptive (`NEXT_STEPS.md`, `MIGRATION_PLAN_X.md`) —
  not dated (dates change; intent doesn't).
- One promotion target stated at the top of each doc ("→ becomes
  `docs/adr/ADR-NNNN` once decided", "→ becomes a section of
  `docs/architecture.md` when stable").
