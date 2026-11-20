# 0007 — A flat routine moves to the folder layout in one deploy (2026-11-20)

**Date:** 2026-11-20
**Operator:** Marko Dragoljevic
**Agent:** Claude Code (Sonnet 5.5), the flotilla session
**Outcome:** An installation can move a live flat routine to the folder layout. A move that
provably loads to the same thing is re-recorded at the start and reported, the way a flat v1's edit
already is; anything else is refused as before, naming the key or file. Built on `main`, not
pushed. Decided in [ADR-0020](../adr/ADR-0020-routine-versions-stations-harnesses.md) section 3
(amended).

## The problem

A flat v1's hash is over each version-owned key's YAML; a folder version's is over the bytes of the
files under `v1/`. So a move always changed the hash, `-check` refused it (`versions: R v1 is live
and its folder changed`) and `deploy_fleets.sh` died before it switched. `flotilla versions
rerecord` only reads the folder flotillad already has mounted, and the new fleets folder is mounted
by the deploy the refusal stopped, so no move could be deployed. The installation has 18 routines
to move.

## What was built

- **The proof (`spec.ProveMoved`).** The recorded manifest of a flat v1 is enough to prove the
  version's content key by key: the sha256 of each of `run`, `explain`, `steps`, `skills`,
  `harness` and `models` in `v1/spec.yaml` equals the recorded one; each prompt file is matched to
  the recorded `prompt-file:` entry with the same bytes (the path changes with the move), and the
  recorded `prompts` sha must equal the hash of the role-to-old-path map so rebuilt; each
  `skill-file:` entry is equal. A v1 with stations or limits is refused (no flat counterpart).
- **The plan, the start and the report.** `versions.Check` turns a proven move into an `Update`
  with the list of what was compared; `EnsureVersions` re-records the row from the folder (hash and
  manifest) with a `version.update` event carrying the same list. `-check` prints `versions: R v1
  moved to the folder layout, unchanged (run, prompt-file:playbooks/x.md -> v1/x.md, prompts):
  re-recorded`, and `deploy_fleets.sh` prints it under "re-recorded at the start". A move with any
  difference is refused, with the old message plus the difference (`... not the same as the
  recorded flat spec: prompt-file:v1/x.md (changed: no recorded prompt file has these bytes)`).
- **The routine's own fields, as a record (schema step 33).** `routine.yaml`'s budget, trigger,
  worker and the rest are outside the freeze for every routine, and the recorded manifest does not
  hold them, so a move is not refused for them. The operator's go (2026-11-20) was to record them
  instead: a new table `routine_snapshots` holds, per routine, its own fields as canonical JSON with
  a hash (`spec.Routine.RoutineFields`). Every start compares and writes: a routine with no
  snapshot gets one and no event; a changed one gets the new snapshot and one `routine.update`
  event listing each field that moved with old and new value; an unchanged start writes nothing.
  `-check` reads the snapshots read-only and prints the same diff (`routine R: budget
  {"day_usd":1} -> {"day_usd":2}`), or `routine fields: no snapshot yet` on the first deploy after
  the step; `deploy_fleets.sh` prints both under "routine fields". For a moved routine -check adds
  `versions: R v1 routine fields: identical to the stored snapshot` (or `changed since ...`),
  ending "information only, not a gate". The routine page lists the latest changes in a short
  "Changed" card. Nothing is refused for a field.

## Decisions worth knowing

- **Option chosen after a stop.** The first reading of the brief also asked the gate to prove the
  routine's own fields equal. The recorded manifest cannot (it holds only version-owned keys and
  files), so the work stopped and reported; the operator's answer was that routine fields were
  never frozen, so the gate is the manifest and the routine fields are told, not enforced.
- **Prompt files are matched by content, not by name.** A prompt's path changes with the move and a
  file may be renamed in it; the recorded `prompts` sha then checks the role-to-file map, so two
  roles pointing at each other's files is caught.
- **`-moved-from` was built, then dropped.** The first version of the information line compared
  the folder with the running flat fleets, which `deploy_fleets.sh` handed to a second `-check`.
  The snapshot makes that folder unnecessary: the store already holds what the flat spec had at its
  last start, so the flag, its second `-check` call and its mounted folder are gone.
- **`platform/heartbeat` is the real-data proof.** The test that said a store holding the flat
  heartbeat refuses the folder layout now says it is re-recorded: the shipped folder v1 is provably
  the flat one.

## Tests, and the mutants that went red

Unit (`internal/spec`, `internal/versions`, `internal/store`, `cmd/flotillad`), `test_deploy.py`
and the e2e rows: a pure move re-recorded with the comparison; one changed byte refused; a changed
budget reported against the snapshot, not refused; a budget changed between two starts shown by
`-check` and then as one `routine.update` event; the agent, no-reason and shadow rerecord rows
kept; the migration of a schema-25 store checked for the new table.
Each guard was broken in turn and its test went red: a key's sha not compared, a prompt matched by
name instead of bytes, the prompts-role sha not rechecked, a skill sha not compared, a stations
version accepted, the gate accepting any move, the store ignoring the proof, the event or the line
without the comparison, an unchanged start writing an event, a changed field not listed or
every field listed, the first snapshot not written, the start recording nothing, `-check` ignoring a
moved field or saying identical always, the "no snapshot yet" line missing, the migration without
the table, the page omitting the changes, and the deploy script dropping the fields block.

## Not done

- The operator's own 18 routines are not moved; that is the fleet session's deploy.
- `flotilla versions rerecord` of a moved routine that is not provably the same still reads only
  the folder flotillad has mounted, so such a move ships as a new number.
- The first deploy after step 33 has no snapshot, so it reports `no snapshot yet` and nothing to
  compare: a move deployed in that same step gets the version proof but no field comparison. The
  migration must be checked on a copy of the real store before it ships (schema 33, additive: one
  new table, no existing row touched).
