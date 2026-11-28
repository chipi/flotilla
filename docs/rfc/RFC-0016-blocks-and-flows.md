# RFC-0016 — Blocks and flows: a library of reusable steps, and flows that wire them

**Status:** Accepted: built and live on the installation (the operator's go, 2026-11-27;
history 0010). The second draft took the advisor's review of the first, which is in this file's
history.

**Builds on:**

- [ADR-0001](../adr/ADR-0001-platform-foundations.md) decision 2: specs declare fleets, and code is
  a library of named building blocks;
- [ADR-0005](../adr/ADR-0005-agent-pipeline.md): one pipeline, and hand-overs are case moves, not a
  workflow engine;
- [ADR-0020](../adr/ADR-0020-routine-versions-stations-harnesses.md): a routine is a folder, a
  version is a folder, and new code ships beside the old;
- [ADR-0023](../adr/ADR-0023-never-silent-and-answers-in-the-core.md);
- [the operator fleet plan](../wip/operator-fleet-plan.md), case 2 (disk).

## 1. Why

The operator fleet has one chain today, for a failed backup: a monitor, an acting routine, a verify
step, an investigation, an SOP draft and a case review. The next chain, disk, has the same shape.
Only three things differ: what it watches, the fix, and the check. Every chain after it (OOM,
pipeline jobs, the DGX's mode) has the same shape again.

The operator's model (2026-11-27): a **library of blocks**. A **flow** wires library blocks together
with blocks of its own. A block that proves useful in one flow moves into the library for the next.

Today's routines are already most of the way there. `backup-monitor/v1/spec.yaml` runs a shared
script with its parameters in the environment, and the e2e fleet reuses `ops/sop_act.py` for
another case kind. What is missing is:

- parameters typed and checked before a deploy;
- the cap a block may run at;
- the stages a block hands a case to;
- one shared vocabulary for all of it.

## 2. The seam: committed generation

**A flow is expanded into ordinary routine folders by a generator,** and the expansion is committed:
`flotilla flow expand` writes `fleets/<fleet>/<routine>/routine.yaml` and `v<n>/spec.yaml` from
`flow.yaml` and the blocks it pins. `-check` refuses when the committed folders differ from what the
generator writes now, the way `make lint` refuses a stale `llms.txt`.

What this keeps unchanged: versions, the freeze, shadows, promote, rollback, the UI, the ledger and
`deploy_fleets.sh`. ADR-0020's "a routine is a folder" stays literally true, and a block update is a
reviewable diff in each routine it touches.

**The other seams, and why not:**

- **Expansion at load** (this RFC's first draft) would need several ADR-0020 amendments:
  - flows would carry `v<n>/flow.yaml`;
  - flotillad would synthesise versions that have no folder;
  - reconcile's messages, `rerecord` and the version page would all change.
- **Runtime composition** is a workflow engine, which ADR-0005 rules out.

## 3. A block

A block is a numbered folder, `blocks/<name>/v<n>/`, immutable once published, like a routine's
version. It holds the block's code and a `block.yaml` that states its interface:

```yaml
block: metric-monitor
version: 1
role: monitor                 # monitor | act | stage | verify | learn
description: Opens a case per group whose metric crosses its action's precondition; clears it.
consumes: {every: true}       # what triggers it, per role (below)
emits: [detection]
hands_to: []                  # the stages it may hand a case to (an act block: [investigate])
actions: [precondition_of]    # what it needs in the registry: here, the action whose `pre` it reads
params:                       # typed; -check validates every flow's values
  action:     {kind: registry_action}
  metric:     {kind: metric_name}
  group_by:   {kind: label_list}
  exclude:    {kind: label_match, optional: true}
  clear_after:{kind: count, optional: true}   # hysteresis: N clear readings before it clears
env: [VM_URL]                 # what its worker must provide
model: none                   # none, or a station: the block ships its prompts and tools
max_autonomy: record          # a flow may lower it, never raise it
```

The role sets what `consumes` may say:

| Role | Triggered by | Example |
| --- | --- | --- |
| monitor | a schedule | `metric-monitor` |
| act | a case kind, stage "" | `sop-act` |
| stage | a case kind and a named stage | `sop-investigate` |
| verify | a registry check name | `metric-verify` |
| learn | a schedule | `case-review`, `sop-draft` |

**Each setting has one home.** Thresholds and verify windows stay on the registry action. A
monitor's threshold is the action's precondition (`pre`), so the monitor and the act agree, and
case-review's proposals change it in that one place. A flow names the action and never repeats the
number.

**A block with a model is a station** (ADR-0020 §6): harness, prompts, tools and limits, shipped
with the block. A flow may name the model alias only.

## 4. Pinned, and adopted by choice

A flow's step pins its block: `use: metric-monitor@1`.

- **Publishing a new block version changes no flow.** Each flow adopts it when its owner bumps the
  pin. Its expanded routines then change, which makes a new routine version, shadowed and promoted
  like any other.
- **A routine's version hash stays what ADR-0020 §6 says:** the files of its version folder. A
  block's code is reached through that folder's command and pin, never edited under a live version.
  ADR-0020 is amended only to say that a pinned block counts as the version's code.

## 5. A flow

A flow is a folder in a fleet. It holds a `flow.yaml`, its own blocks, and its knowledge. Each step
names the routine it becomes, so the ids are the operator's choice:

```yaml
flow: disk
case_kind: disk_low
steps:
  - name: disk-monitor
    use: metric-monitor@1
    every: 10m
    with: {action: disk.cleanup_known, metric: node_filesystem_avail_bytes,
           group_by: [instance, mountpoint], exclude: {fstype: "tmpfs|overlay|squashfs|autofs"},
           clear_after: 2}
  - name: disk-act
    use: sop-act@1
    worker: ops-act-host
    case: {on_record: watch}
  - name: disk-investigate
    use: sop-investigate@1
    model: triage-flash
  - name: disk-verify
    use: metric-verify@1
knowledge:
  - {name: sops, path: sops/, format: skill, fields: [applies_to, fix, rung], proposals: true}
```

The fleet declares its learning blocks once, over all its flows:

```yaml
learn:
  - {name: case-review, use: case-review@1}
  - {name: sop-draft, use: sop-draft@1, with: {flows: [backup, disk]}}
```

**`-check` validates the wiring:**

- every stage a step's block hands to has a step;
- one acting step per case kind and stage;
- each parameter's value has the declared kind;
- every action a block needs is in the registry;
- no flow raises a block's cap;
- the committed folders match the generator's output.

**The fleet's pack hash covers its flows,** the same way it covers its knowledge (C6b).

## 6. A flow brings its own

- **Knowledge:** its SOPs, shown on the Knowledge tab.
- **Registry actions:** the exact commands its fixes may run on each host (ADR-0006), with the
  preconditions and checks that hold the flow's thresholds and windows.
- **Its own blocks:** a step the library has no block for. For example, the DGX's mode check reads
  `gpu-mode-swap.sh status`. Once a second flow needs it, the block moves into the library.

**Where block code runs from.** Library blocks ship in the worker images and are tested in this
repo's CI. An installation's private blocks ship in an image too, built from its fleets repo and
tested there. They are never run straight from the fleets mount, which a fleets deploy switches
without a build or its tests.

## 7. The library

The library is extracted from today's code, unchanged in behaviour:

| Block | Role | From |
| --- | --- | --- |
| `metric-monitor@1` | monitor | `ops/backup_monitor.py` |
| `sop-act@1` | act | `ops/sop_act.py` |
| `sop-investigate@1` | stage | `ops/sop_investigate.py`, with its prompts |
| `metric-verify@1` | verify | the backup verify |
| `sop-draft@1` | learn | `ops/sop_draft.py` |
| `case-review@1` | learn | `ops/case_review.py` |
| `process-change@1` | act | `ops/process_change.py` |

`never silent` belongs to the acting step's case trigger (`case: {on_record: watch}`), as it does
today. It is not set on a monitor.

## 8. The first two flows

**Backup, re-expressed as a flow, with no new behaviour.** Its steps keep today's names
(`backup-monitor`, `rerun-backup`, `verify-backup`, `sop-investigate`).

- **Proof:**
  - the generator's output is byte-equal to the live routine folders;
  - the start re-records them with no new version, as a proven move does (`spec.ProveMoved`,
    ADR-0020 §3);
  - nothing is renamed and nothing is shadowed.
- **What stands in for replay:** there is no replay tool for the operator chain (step 1's build
  plan). Its unit fixtures and the e2e `sops` and `watching` sections stand in, and the PR says so.

**Disk, across five hosts:** the mini, the operator's laptop, the X3, the DGX and the VPS. All five
report `node_filesystem_avail_bytes` to VictoriaMetrics today (checked 2026-11-27).

- **Step 1 prerequisites,** each named because today's code lacks it:
  - a disk fact in `sop_act` (it knows only `backup_age_above`);
  - a disk precondition in the host agent (`hostexec` knows only `launchd_not_running`);
  - metric reads per mountpoint, not only per instance (`sop_investigate.read_metric`);
  - label exclusion and hysteresis in `metric-monitor`, so 54 episodes in 60 days do not become as
    many notices. Measure each host's mountpoints first.
- **Step 1 itself:**
  - the disk flow at record, with no cleanup action;
  - the registry action `disk.cleanup_known` exists only to hold the threshold and the check, and
    runs nothing;
  - every case is diagnosed from metrics, and the operator is told.
- **Step 2:**
  - host agents on the laptop, the X3, the DGX and the VPS;
  - read-only host actions (section 9), so the investigation sees what grew.
- **Step 3:** named cleanups as SOPs, written from what the diagnoses found. They run at rung 2 (the
  operator approves), except on the VPS, where they stay at rung 3: prepared, and the operator
  applies them. No broad prune.

## 9. Needed outside the blocks

- **Read-only host actions:** an ADR of their own. A class of host action that reads and never
  writes: `du` over a named root, a journal's size, `docker system df`. Typed parameters, and the
  output bounded and recorded.
- **flotillad sees `blocks/`:** in its image, so `-check` can validate pins and parameters.
- **Shadows for the ops roles:** not needed for the backup proof (above). A flow that wants a week
  in shadow needs an `ops-read` shadow worker in the stack first. Its acting steps pair only when
  their case occurs.

## 10. Not decided here

- Whether flows can be nested or chained. If they are, it is always as case hand-overs, never a
  workflow engine.
- Blocks shared across installations (a public block catalogue).
- A UI for flows. A flow shows as its routines and its Knowledge tab until a flow page earns its
  place.

## 11. Order of work

1. **Blocks:**
   - `block.yaml`, numbered block folders, the generator and its `-check`;
   - the seven blocks extracted;
   - backup expanded byte-equal and re-recorded.
2. **Disk step 1's prerequisites,** then the disk flow at record on five hosts.
3. **The read-only host actions ADR,** and host agents where the disk flow needs them.
4. **Disk cleanups as SOPs,** one host at a time.

## 12. As built, step 1 (2026-11-27)

Step 1 is on the branch `rfc16/blocks`. Where the build fixed what the sketch left open:

- **The seven blocks** are in `blocks/<name>/v1/`. Their code stays at `ops/*.py`, so every
  `/opt/flotilla/ops/...` command is the live one. `code:` in `block.yaml` lists the repo files, and
  a test checks they exist.
- **`block.yaml`** also carries `autonomy`, `budget_usd`, `timeout`, `command`, `case_kind` (a
  block's own kind: process_change, sop_proposal), `texts` (the prompts each routine version
  carries, copied byte for byte) and `spec_tail` (a file appended to `v<n>/spec.yaml`: stations,
  limits, explain; `$MODEL` and `$WORKER` are the step's). `actions` is a map from a need
  (`precondition_of`, `check_of`) to the param that names the action.
- **v1 params are today's.** `metric-monitor@1` takes `action` and `metric`, because
  `backup_monitor.py` reads no more; `group_by`, `exclude` and `clear_after` come with the disk
  prerequisites, as a new block version.
- **A step** names the routine, pins the block, and carries what is the installation's:
  `description` (one line of the folded text per line), `worker`, `every`, `freshness`,
  `max_autonomy`, `model`, `case: {on_record}`, `verify`, `with`. `version: n` is the routine
  version folder it writes; a bumped pin writes a new folder and leaves the old.
- **The fleet's learning blocks** are `fleets/<fleet>/flows/learn.yaml`, not `fleet.yaml`, so a
  flotillad built before this does not refuse the fleet. `sop-draft`'s `with: {flows: [...]}` gives
  it the flows' case kinds.
- **A flow's own block** is `flows/<flow>/blocks/<name>/v<n>/`, and wins over the library's. The
  example `jobs` fleet has one (`rerun-backup`).
- **Where it is checked:** `flotilla flow expand [-check]`, `make lint` (every fleet with flows,
  against `host/registry.json`), and `flotillad -check` and its start (`blocks_dir`, default
  `/opt/flotilla/blocks`, in the image).
- **Proof:** the example `jobs` chain is a flow and its committed folders are byte-equal to the
  expansion (a Go test and `make lint`). The installation's operator chain was expanded from its own
  `flows/` into a fresh folder, and `diff -r` against the live folders shows nothing but the added
  `flows/`.
- **The cap rule.** A block's `max_autonomy` is written into the generated `routine.yaml` for every
  act and stage step (the step's, if it lowers it), so a routine that plans an action can never
  be raised past its block. A monitor, a check and a learning routine never plan: only a case
  trigger reaches policy (`DecidePlan`), and a monitor's report is detections only. Their
  `routine.yaml` has a cap line only when the step writes one.
- **The manifest.** `flows/.generated` lists the routine folders the flows own, and is part of the
  fleet's pack. `-check` flags a listed folder that is no longer a step, a file beside
  `routine.yaml`, a folder that is not a version, and a symlinked routine or version folder. A step
  whose name is a routine folder not in the manifest is refused, so a flow never takes over a
  hand-written routine by its name alone: list the folder to adopt it, and its bytes must then equal
  the expansion.
- **Values after a flag.** A value of a param written as `arg:` may not start with a dash.
- **Not built:** the `knowledge:` key of a flow (the fleet's `knowledge:` in `fleet.yaml` is
  unchanged); a freeze of block folders (the expansion check is what stops an edit under a pin); the
  shadow of a flow's routines.

### 12.1 Disk step 1 (2026-11-27)

The prerequisites of section 8 and the disk flow at record, in the public repo. The installation's own
flow, its thresholds and its excluded mounts are in its private fleets repo.

- **New block versions, never edits.** `metric-monitor@2`, `sop-act@2`, `sop-investigate@2` and
  `metric-verify@2` are new folders. Their `@1` folders are byte-identical, backup still pins them,
  and its expansion and the shipped goldens are unchanged.
  - `metric-monitor@2` takes `size_metric`, `group_by`, `exclude`, `clear_after` and `step`, and
    runs `ops/metric_monitor.py`. Its one form is the free ratio, avail over size, both read; a
    series of no size is dropped.
  - `sop-act@2` and `sop-investigate@2` make the backup-only params optional (`metric`,
    `instance`) and add the disk's two metric names. `sop-investigate@2`'s prompt tells the
    station it may read a `mountpoint`.
  - `metric-verify@2` runs `ops/verify_free_ratio.py`.
- **The threshold stays in the registry.** The monitor, the acting step's re-check and the verify all
  read the action's `free_ratio_below(<fraction>)` precondition for the run's stage. A flow names the
  action and never the number.
- **Quoting.** The generator still quotes nothing, except for a `label_match`. That param is
  rendered as one single-quoted JSON object, keys sorted, in one environment variable
  (`MONITOR_EXCLUDE`). Its label names and regexes are limited to characters that mean nothing
  inside single quotes, and none is a single quote, a double quote, a backslash, a space, a brace
  or a control character. `-check` refuses anything else, and the block validates the value again
  before it builds a query. A test runs the rendered command through a real shell. A block's
  command may also name `{{case_kind}}`.
- **Hysteresis keeps no state.** `clear_after: N` reads the last N readings from the metrics' own
  history, one per `step`. A group is reported while it is below the threshold now or in any of
  them, so it clears after N clear readings in a row, and a disk that hovers at the threshold is
  one case. `step` is the step's `every`; the flow states it twice, and nothing checks that they
  agree.
- **One case per group.** The case target is `host:<instance><mountpoint>`, for example
  `host:host-01/data`, and the root mountpoint is `host:host-01/`. Policy and the SOP matcher read
  the host as everything after `host:`, so a hold on `host:host-01` does not cover its disk cases.
  That is harmless while nothing runs. **Step 3 decides it:** an action that executes needs the
  host split from the mountpoint.
  - A group whose labels cannot make a target (a space in a mountpoint, a character outside letters,
    digits and `. _ / -`) is named in the run's summary and not reported. Exclude such a mount or
    accept that it is unmonitored.
  - The monitor reports at most 50 groups, the fullest first, because flotillad refuses a longer
    report.
- **The disk fact.** `sop_act` re-checks `free_ratio_below` with a fresh read of the case's own
  instance and mountpoint. No reading is unknown and escalates; it is never "no longer holds".
  `sop_investigate.read_metric` takes an optional `mountpoint`, validated as a plain path. A read
  without it is the same expression as before.
- **Host agent.** `hostexec` knows `disk_free_ratio_below` (`path`, `ratio`) and `path_exists`
  (`path`) beside `launchd_not_running`. The first reads statfs, with no command, and holds while the
  filesystem has less than `ratio` of its size available. Both take only a clean absolute path, are
  checked when the host's config loads, and treat a read that fails as unknown (nothing runs).
- **A record-only action.** The registry has no argv: it names a fix, and each host's own config says
  how it runs. `disk.cleanup_known` therefore holds its threshold (`pre`) and window (`check`) and
  has `executor: record`, a fourth executor beside host, repo and routine. Policy takes `record`
  as a cap for it, whatever the ceilings say, and what it would have done is shown as a proposal.
  Its autonomy can never be promoted. Step 3 changes the executor when a cleanup exists; until
  then the action cannot run, even if a host's config named it.
- **The example flow** is `fleets/jobs/flows/disk`: four steps at record, with `on_record: watch`
  on the acting and the investigate steps. Its folders are committed and checked like the backup
  flow's. A learning block's `flows:` list is the fleet's to extend (`sop-draft` learns from the
  flows it names); the example fleet has none.
- **Order of deploy.** The record executor is new to flotillad's registry loader. An installation's
  registry may declare `executor: record` only after the flotillad image that carries this change is
  running, or flotillad refuses its registry at start.
- **Not covered:** a shadow of the disk routines; a check that a flow's `step` equals its
  monitor's `every`; a minimum on a `count` param (the block refuses 0 when it runs, not at
  `-check`); a read of `node_filesystem_readonly` (read-only images are excluded by mountpoint
  instead); a mount with a space in its path.

### 12.2 The service watch, step 1 (2026-11-28)

The DGX not serving prod, on section 8's model, at record. The plan is
[operator-dgx-plan](../wip/operator-dgx-plan.md).

- **New blocks and versions, never edits.** `service-monitor@1`, `service-verify@1`, `sop-act@3`
  and `sop-investigate@3`. The `@2` folders keep their params and prompts; only their `code:` lists
  gain `ops/service_up.py`, which `sop_act.py` now imports.
- **The condition has one home.** The registry action's `service_down(<service>)` precondition
  names the service. The monitor refuses a flow whose `services` omit it, the acting step re-checks
  it with a fresh read of the case's instance, and the verify reads the same service.
- **An absent series is unknown.** The monitor replays the last day of the gauge's own history, one
  reading per step. A group opens after `readings` down readings in a row and closes after
  `clear_after` up readings in a row. A missing reading counts for neither and breaks both runs, so
  a host that stops reporting keeps its case until its history is empty. A failed query fails the
  run. `metric-monitor@2` clears a group that vanishes; a `@3` can adopt the same replay.
- **A `series` read** in the investigate station returns every series of a metric with its labels,
  at most 20 lines, plain characters only. A station declares it in `tool_names`; `@2` does not.
- **Not covered:** the target of a group with a second label, `host:<instance>/<value>`, is read as
  a host of its own by the SOP matcher (as disk's is); the example flow uses one case per instance.
