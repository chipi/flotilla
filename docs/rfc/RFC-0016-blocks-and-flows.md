# RFC-0016 — Blocks and flows: a library of reusable steps, and flows that wire them

**Status:** Draft, for the operator's review (2026-11-27).
**Builds on:**

- [ADR-0001](../adr/ADR-0001-platform-foundations.md) decision 2: specs declare fleets, and code is
  a library of named building blocks;
- [ADR-0005](../adr/ADR-0005-agent-pipeline.md): one pipeline and one set of building blocks for
  every fleet;
- [ADR-0023](../adr/ADR-0023-never-silent-and-answers-in-the-core.md);
- [the operator fleet plan](../wip/operator-fleet-plan.md), case 2 (disk).

## 1. Why

The operator fleet has one chain today, for a failed backup: a monitor, an acting routine, a verify
step, an investigation, an SOP draft and a case review. The next chain, disk, has the same shape.
Only three things differ: what it watches, the fix, and the check. Every chain after it (OOM,
pipeline jobs, the DGX's mode) has the same shape again.

The operator's model (2026-11-27): a **library of blocks**. A **flow** wires library blocks together
with blocks of its own. A block that proves useful in one flow moves into the library for the next.

## 2. A block

A block is a folder of code and a `block.yaml` that states its interface:

```yaml
block: metric-monitor
role: monitor            # monitor | act | stage | verify | learn
description: Opens a case for each instance whose metric crosses a threshold; clears it below.
params:                  # typed; -check validates every flow's values
  metric:    {kind: metric_name}
  form:      {kind: enum, of: [value, age, free_ratio]}
  threshold: {kind: number}
  group_by:  {kind: label_list}
  case_kind: {kind: case_kind}
emits: [detection]       # detections of case_kind, keyed per group
model: none              # none, or a station that goes through the one model client
max_autonomy: record     # a flow can lower it, never raise it
worker: [ops-read]       # the roles whose image carries it
```

- **Blocks are code, reviewed and versioned like any code.** A flow cannot change what a block
  does, only its declared parameters.
- **A block's hash is part of every routine made from it.** A changed library block is a new
  version of every flow that uses it, and goes through shadow and promote in each flow.

## 3. The library

flotilla ships generic blocks under `blocks/`. An installation can add private ones in its fleets
repo, the same way.

| Block | Role | From | What it does |
| --- | --- | --- | --- |
| `metric-monitor` | monitor | the backup monitor, generalised | a metric's value, age or free ratio per group, against a threshold; never silent through `on_record: watch` |
| `sop-act` | act | `ops/sop_act.py` | matches the flow's SOPs, re-checks the preconditions, plans the SOP's fix; with no SOP, hands over to investigate |
| `sop-investigate` | stage | `ops/sop_investigate.py` | read-only diagnosis through the one model client; the fix is chosen among registry actions by code |
| `metric-verify` | verify | the backup verify, generalised | after an execution, the metric condition holds within a window, else escalates |
| `sop-draft` | learn | `ops/sop_draft.py` | drafts the SOP a verified investigation teaches, as a proposal |
| `case-review` | learn | `ops/case_review.py` | the post-incident review over every flow's closed cases |
| `process-change` | act | `ops/process_change.py` | plans a reviewed change to a repo file as a diff |

## 4. A flow

A flow is a folder in a fleet, holding a `flow.yaml`, its own blocks, and its knowledge:

```yaml
flow: disk
case_kind: disk_low
steps:
  - use: library/metric-monitor
    every: 10m
    with: {metric: node_filesystem_avail_bytes, form: free_ratio, threshold: 0.10,
           group_by: [instance, mountpoint], case_kind: disk_low}
  - use: library/sop-act
    worker: ops-act-host
  - use: library/sop-investigate
    with: {reads: [metric, host]}      # host reads once their ADR is accepted (section 7)
    model: triage-flash
  - use: library/metric-verify
    with: {within: 30m}
knowledge:
  - {name: sops, path: sops/, format: skill, fields: [applies_to, fix, rung], proposals: true}
```

The fleet declares the learning blocks once, over all its flows:

```yaml
learn:
  - use: library/case-review
  - use: library/sop-draft
    with: {flows: [backup, disk]}
```

**How it runs.** flotillad expands a flow into ordinary routines when it loads the specs, named
`<fleet>/<flow>-<step>` (`operator/disk-monitor`, `operator/disk-act`).

- **Nothing changes at run time:** the ledger, versions, autonomy, shadows and replay all see
  routines, as today.
- **`-check` validates the wiring:**
  - every stage the flow's blocks hand over to has a step;
  - one acting step per case kind and stage;
  - each parameter's value has the declared kind;
  - no flow raises a block's cap.

## 5. A flow brings its own

- **Knowledge:** its SOPs (`knowledge:`, RFC C6b), shown on the Knowledge tab per flow.
- **Registry actions:** the commands its fixes may run on each host, as exact argv templates in the
  host's allowlist (ADR-0006).
- **Its own blocks:** a step the library has no block for. For example, the DGX's mode check reads
  `gpu-mode-swap.sh status`.
  - Once a second flow needs it, the block moves into the library.

## 6. The first two flows

**Backup, re-expressed as a flow.** No new behaviour: the same five steps, now made of library
blocks.

- **Proof:**
  - its expanded routines decide exactly what today's routines decide, replayed on its two recorded
    cases;
  - a shadow beside the live version for a week shows the same decisions;
  - then it switches.
- This is how the library is shown to hold before a second flow relies on it.

**Disk, across five hosts:** the mini, the operator's laptop, the X3, the DGX and the VPS. All five
report `node_filesystem_avail_bytes` to VictoriaMetrics today (checked 2026-11-27).

- **Step 1:**
  - `disk-monitor`, `disk-act` and `disk-verify` at record;
  - the shared investigation reads metrics;
  - no cleanup action yet. Every case is diagnosed and the operator is told.
- **Step 2:**
  - host agents on the laptop, the X3, the DGX and the VPS;
  - read-only host actions (section 7), so the investigation sees what grew;
  - the host agent's new preconditions: free space below a value, a path exists.
- **Step 3:** named cleanups as SOPs, written from what the diagnoses found. A cleanup runs at
  rung 2 (the operator approves it), except on the VPS, where it stays at rung 3: prepared, and the
  operator applies it. No broad prune.

## 7. Needed outside the blocks

- **Read-only host actions:** an ADR of their own. A class of host action that reads and never
  writes: `du` over a named root, a journal's size, `docker system df`. Typed parameters, and the
  output bounded and recorded.
- **A stage step for several kinds:** today a stage routine serves one case kind. A library block
  used by several flows expands to one routine per flow, which keeps the uniqueness rule as it is.
  So no platform change is needed for it.

## 8. Not decided here

- Whether flows can be nested or chained (one flow's verify failure opening another flow's case).
- Blocks shared across installations (a public block catalogue).
- A UI for flows. Flows show as their routines and their Knowledge tab until a flow page earns its
  place.

## 9. Order of work

1. **Blocks:** `block.yaml`, the library folder, flow expansion and its `-check`, and the five
   operator blocks extracted from today's code. Then backup re-expressed as a flow, replayed, run in
   shadow and switched.
2. **Disk step 1** on all five hosts, at record.
3. **The read-only host actions ADR,** and host agents where the disk flow needs them.
4. **Disk cleanups as SOPs,** one host at a time.
