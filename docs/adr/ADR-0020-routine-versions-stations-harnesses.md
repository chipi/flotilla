# ADR-0020 — Routine versions, stations and harnesses

**Status:** Accepted (2026-11-18). Step 1 is built (below); steps 2 to 7 are decided here and built
in order. The full design, with every section's reasoning, is
[Routine versions, stations and harnesses](../wip/v2-design.md); this ADR is its decision record
and the two stay in step. Builds on [ADR-0007](ADR-0007-routine.md) (the routine),
[ADR-0008](ADR-0008-autonomy.md) (autonomy),
[ADR-0004](ADR-0004-job-runtime.md) (the runtime) and [ADR-0018](ADR-0018-harness-parts.md)
(harness parts).

## Context

The operator, 2026-11-18: changing a business process means shipping a new version of its routine.
Until now a routine was one spec file, and changing it was editing that file and deploying: there
was no way to run a candidate beside the live routine with no effect on the system, compare them,
go live on the operator's word, or go back. Triage's `cycle-shadow` was a second routine with its
own namespace, built for one comparison. The model-driven part of a routine was a `harness:` block
whose texts and limits lived partly in the spec and partly in the routine's code, and every
harness (pi, then others) was wired into a single routine.

## Decision

A routine has **versions**; each version is worked by its **stations**, one per model step; the
**harnesses** that run a station are the platform's, not a routine's. The sections below say what is
decided; step 1 is built and described as built.

### 1. A routine is a folder, and a version is a folder in it (built)

```text
fleets/<fleet>/<routine>/routine.yaml       the routine: identity, shared by every version
fleets/<fleet>/<routine>/v<n>/spec.yaml     a version
fleets/<fleet>/<routine>/v<n>/<texts>       the texts a version declares; paths stay inside v<n>/
```

- **`routine.yaml`** holds what a routine is whichever version runs: name, description, worker,
  trigger, the case kinds it detects, enabled, autonomy and `max_autonomy`, budget, freshness,
  resume, brief section, chain, and `acceptance`. Its identity, controls, ledger namespace, cases,
  freshness alert and metrics are the routine's; every version shares them.
- **`v<n>/spec.yaml`** holds what a version does: its `run:`, explanations and steps, and (until
  stations arrive) its skills, harness, prompts and models. `routine.yaml` and `spec.yaml` each
  refuse the other's fields. A version number is `v1`, `v2`, ... and unique in the routine.
- **A flat `fleets/<fleet>/<routine>.yaml` still loads**, as a routine with one synthesised version
  `v1` (legacy), so no installation has to move. One layout per routine: both for the same routine
  is a load error. A legacy v1 is exempt from the station rules (step 3) until it is retired.
- A version may change code: its `run:` names the code it runs. The worker image carries the code
  of every version that is not retired; new code ships beside the old, never edited under a version
  that uses it.

### 2. Four states, and the operator's word moves them (built)

A version is **live** (the one that acts; exactly one per enabled routine), **shadow** (a candidate;
any number), **previous** (the version that was live before; the rollback target) or **retired**
(history only). The word shadow belongs to versions only (section 5).

- **A routine's first version goes live when it is deployed**, its hash recorded. A later version
  arrives with a fleets deploy as a shadow. In step 1 a shadow is loaded, validated, hashed and
  listed; it does not run, and records nothing, until step 2.
- **Promote** makes a shadow live (the live one becomes previous, the old previous retired). The
  operator chooses the routine's autonomy for it, pre-filled with the current level and never
  inherited in silence; raising it needs a reason, and the routine's `max_autonomy` caps it. It is
  refused for a disabled routine, one whose worker flotillad does not know, a version whose folder
  does not load, or a version whose hash is not the one recorded. A run in flight finishes as the
  version it started with; the routine is bound to the new version for the next run.
- **Roll back** makes the previous version live at once, with the autonomy and a reason confirmed
  again. The rolled-back version is retired, marked "rolled back at T by X: why"; there is no
  previous until the next promote. Its run in flight is cancelled as a STOP cancels it. Its plans
  awaiting approval are marked rolled back and cannot be approved or rejected (409, "plan from a
  rolled-back version"), and their cases go `awaiting_approval` to `handed_over` (a transition only
  the operator may make; the event is `case.rolled_back`, the target lock is released) so the live
  version plans them again. The previous version's plans stay approvable: it was live when it made
  them. A fix ships as a new number.
- **Retire** stops a shadow. A live or previous version is moved by promote or roll back, never
  retired.
- Every state change is one store transaction with its `events` row (`version.deploy`, `update`,
  `promote`, `rollback`, `retire`, `rerecord`; subject `<routine>@v<n>`; who, via, why). Only an
  operator or an admin may ask (`ActVersion`): never an agent, never a worker, never the MCP server
  (its `Never` list names promote, rollback, retire and rerecord, and a test asserts none is
  registered).
- Every record says which version made it: runs, model calls, what a run loaded, plans and ledger
  rows take their run's version; a case event takes the acting routine's live version, and 0 for an
  operator's or policy's.

### 3. Live and previous are frozen (built)

A version has a hash, recorded when it goes live: the sha256 over the sorted (path, sha256) of every
file under `v<n>/` for the folder layout; for a legacy v1, over each version-owned key's own YAML
(decoded and re-encoded with sorted keys, never the Go structs, so a change to flotilla's own types
cannot move a recorded hash) and the prompt and skill files it reads in place. The routine's own
`Hash` (what `runs.spec_hash` records and a deploy's before and after proof diffs) is unchanged for
every flat spec; a pinned test holds each shipped one.

- At load, the live and the previous version's folder must hash as recorded. A changed, missing or
  renumbered one fails the load and the deploy, as an invalid spec does: `flotillad` exits 2 and
  names the files (`versions: <routine> v<n> is live and its folder changed (recorded A, found B):
  spec.yaml (changed)`). The only way to change a live version is a new number. A shadow may
  change, and its hash follows; a retired version may be gone or broken.
- `flotillad -check` runs the same comparison read-only (`mode=ro`, and it says so when there is
  no store or one older than schema 26). `deploy/server/deploy_fleets.sh` runs it before it pauses
  anything or switches the symlink, ends `FLEETS DEPLOY FAIL: versions ...` when it refuses, and
  prints every version whose state or hash changed (a new version of a routine is one new shadow
  line).
- **A legacy (flat) v1 is not frozen.** A flat routine's playbooks and settings change in the
  ordinary way, so a changed legacy live or previous v1 is an update at the start: the new hash and
  manifest are recorded with a `version.update` event (actor flotillad, the old and new hash, the
  changed manifest keys), `-check` prints a line such as `versions: triage/cycle v1 (flat)
  re-recorded: prompt-file:playbooks/triage.md (changed)`, and `deploy_fleets.sh` repeats it as a
  proof line. A folder version stays frozen, and the refusal for it says the way out (ship the
  change as v<n+1>, or `flotilla versions rerecord` if this change is deliberate).
- **`flotilla versions rerecord ROUTINE vN --reason R`** (API `POST .../versions/{n}/rerecord`,
  `ActVersion`: an operator or an admin) records the folder as flotillad's fleets folder holds it
  now as the live or previous version's hash and manifest, in one transaction with a
  `version.rerecord` event (old and new hash, the manifest diff, who, why). It is for a deliberate
  change to a frozen version: a flat routine moved to the folder layout (its recipe changes, so the
  store that recorded the flat v1 refuses it until this is run; step 6 uses it), a routine removed
  and added again. A shadow follows its folder by itself and a retired version is history, so both
  are refused. A new store (the e2e's, a first deploy) records the folder recipe from the start:
  `platform/heartbeat` is the first routine shipped in the folder layout, its v1 identical to the
  flat spec it replaces, and its runs' `spec_hash` changes once.

### 4. Acceptance names its evidence (built)

`routine.yaml` (or a flat spec) may declare `acceptance:`, a list from `pairs`, `replay`,
`scenario_eval`; `[pairs]` when absent; every listed kind is required. A promote takes the evidence
given: pair counts (the store's own, 0 until step 2), gate results (`{name, result: pass|fail,
ref}`; a `replay` kind needs one) and eval-harness run ids with verdicts (`{run_id, verdict:
pass|fail}`; a `scenario_eval` kind needs one). It records the acceptance in force and the evidence
on the `routine_versions` row and in the `version.promote` event, and it refuses a promote that lacks
a declared kind, or whose given evidence holds a failed gate or verdict, unless the operator gives a
reason; the reason is the acknowledgement (`without_pairs` is only recorded). flotilla stores the
verdict. It never computes one: datasets, scoring and decisions stay in eval-harness (RFC-0002
item 9). The CLI takes `--gate name=pass|fail[@ref]` and `--eval run_id=pass|fail`, repeated.

### 5. The lowest autonomy level is `record` (built)

The lowest level, which records the plan and acts on nothing, was called `shadow`; it is now `record`.
Policy's outcome is `record`, a case that closes on a recorded plan resolves `plan_recorded`, and
the stored values moved in one migration (schema step 28; the events log and the case timeline's
text are history and stay as written). **The value routines read does not change:** `FLEETD_STAGE`
stays `shadow` for autonomy `record` (`spec.FleetdStage` maps it, a test pins all three levels),
because triage's dry-run guarantee and the ops routines test that string; only a new version of a
routine may read the new name. A spec may still say `autonomy: shadow`, read as `record` (the
alias); `-check` prints a deprecation note, and the alias goes at step 6. Triage's own dispositions
keep their `stage: shadow`, which the decisions tab labels as v1's word for record.

### 6. A version's stations (step 3)

A station is one model step. A version declares any number of stations with any names; flotilla
gives a name no meaning. A station has a `harness` (an adopted one, or `flotilla`, our in-process
loop), a `model` alias, `prompts` (every text the model reads, each a file inside `v<n>/`, used as
templates whose rendered text is recorded with each call), `tools` (`all`, `read` or `none`) and
`limits`; `models:` is derived from the stations. A check before anything runs (`-check` and at
load) against `fleets/harnesses.json`, the harnesses' capabilities, refuses what a harness cannot
serve. A run receives `FLOTILLA_STATIONS`; `flotillad -stations` prints what a version's run gets,
offline. No model, text or limit belongs in a version's code or its command's flags.

### 7. Harnesses are the platform's (steps 4 and 7)

The harness layer lives in `runtime/`: one adapter per harness, the shared runner and the seams
(`runtime/HARNESS_SEAMS.md`, S1 to S11). Any routine runs any station on any adopted harness. Every
worker image carries every adopted harness. A harness is adopted when four tests pass: unit, seam
end to end (the real CLI, a fixture repository and a scripted model endpoint), a probe on the
installation, and an eval; pi first, then Claude Code, Codex, opencode, DeepSeek Harness and
mini-SWE-agent, one at a time.

### 8. A shadow has no effect on the system (step 2)

A shadow reads the frozen input the live version got and writes only to its own shadow record, in
three layers: flotillad (every row keyed `<fleet>/<routine>@v<n>`, would-be writes as
`shadow_records`, a per-run token resolving each request to the run's version and state), the
worker (no write credential in any form, its own state snapshot, its own model key, its own worker)
and the network and telemetry (reads only where the proxy can tell, `shadow=1` labels). Pairs are
matched per decision subject. An e2e proves it before and after a shadow run, and gates step 5.

### 9. Evaluation is first-class (steps 2 and 3)

flotilla owns the recording, the entry point and the formats; eval-harness owns datasets, scoring
and decisions. `runtime/episode.schema.json` is one station's recorded run;
`runtime/input.schema.json` is the frozen input, also a scenario's start, optionally with recorded
tool results; `flotillad -stations` and one command run a version's own `run:` on a given input in
the shadow worker, with a models config of its own and recorded tool results standing in for tools
(a call with no recorded result fails, never sent live).

### Build order

| Step | What | State |
| --- | --- | --- |
| 1 | routine and versions: the layout, states, controls, freeze, the version on every record, the routine page, the rename, acceptance and its evidence | **built** (the e2e covers the full lifecycle on a real stack) |
| 2 | shadows with no effect: the per-run token, the shadow record, the worker split, telemetry labels, the frozen input schema, pairs | specified |
| 3 | stations: the format, `fleets/harnesses.json`, `FLOTILLA_STATIONS`, `flotillad -stations`, the episode schema, the offline entry point | specified |
| 4 | the harness layer in `runtime/`: the adapter interface, pi's adapter, the seams | specified |
| 5 | the example fleets move to this format; the e2e covers versions, shadows, rollback, backward-readable state and the no-effect test | specified |
| 6 | the installation moves its routines (each current spec its v1, live, identical, re-recorded with `flotilla versions rerecord`) and writes its v2s | specified |
| 7 | harnesses adopted one by one | specified |

## Consequences

Positive:

- A new version runs in shadow (from step 2) and goes live on the operator's word, with the
  evidence it declared, and a rollback is one move because the previous version's folder is frozen.
- A run, a call, a plan, a ledger row and a case event say which version made them, so a
  comparison and a rollback read from records, not from memory.
- No installation has to move to benefit: a flat spec is v1. The shipped `platform/heartbeat` and
  the e2e's `e2e/versioned` are the folder layout's first routines.
- The rename removes the collision between a version that runs in shadow and an autonomy level of
  the same name, without touching what routines read.

Negative:

- The store grows by schema steps 26 to 29 (`routine_versions` with its promote evidence, the
  version on every record, the rename, the evidence column), and the freeze makes a harmless edit
  of a live or previous folder a failed deploy; the error names the files so the fix (a new number)
  is plain.
- The deprecated alias `autonomy: shadow` lives until step 6.
- Until step 2 a shadow is listed and promotable but unproven: pairs are 0, so the default
  acceptance is always missing and every promote needs the operator's reason.
- Moving an already-live routine to the folder layout needs one `flotilla versions rerecord` (built
  in step 1), because its recipe changes; a flat routine's own edits need nothing.

Neutral:

- **Rolling the image back** past this change is not a version control: the store refuses an older
  binary on a newer schema (`errNewer`), and the undo is the pre-deploy store snapshot
  (`deploy/server/store_snapshot.py`) with the previous image. The e2e migrates an old, real-sized
  store (`deploy/e2e/old_store.py`) and checks the latest schema is reached, the rows are all
  there and no `shadow` value is left; before a deploy of a schema step the same image's `-check`
  and a migration run on a copy of the real store, read-only on the original.
- Non-goals: flotilla does not promote on its own, does not score a comparison, and never edits a
  live version or its own playbooks at runtime.

## Alternatives considered

- **A version as a field inside one spec file.** Rejected: a version changes code, texts and
  limits, which the folder hash covers and one file's hash cannot name by path; a freeze failure
  must say which file changed.
- **Freeze the live version only.** Rejected: a rollback to a previous version that changed since
  it was live is not a rollback.
- **A promote that inherits the routine's autonomy.** Rejected: it loosens in silence. The operator
  chooses the level at every promote and every rollback.
- **Keep `shadow` for both the autonomy level and the version state.** Rejected: two meanings of
  the word in one UI. The state keeps it; the level is `record`, and `FLEETD_STAGE` keeps the old
  string for the routines that test it.
- **Compute the comparison's verdict in flotilla.** Rejected: scoring belongs to eval-harness;
  flotilla records the evidence and the verdicts it is given.
