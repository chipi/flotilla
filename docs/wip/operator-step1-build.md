# Operator fleet, step 1: build plan (2026-11-26)

The design is [the operator fleet plan](operator-fleet-plan.md). Step 1 has four parts:

- never silent;
- answers in the core;
- the SOP loop;
- the rename from ops to operator.

This plan was made from the code as it is, with file references from a read of it. Nothing in it
has been run yet.

## What the code changes in the design

1. **A recorded plan closes its case.**
   - `applyDecision` (`internal/api/plans.go`) maps `Record` to `closed/plan_recorded`.
   - `DetectIncident` (`internal/store/incidents.go`) then absorbs later detections into that
     closed case, by canonical key, until an `incident.cleared` event.
   - `ClearIncidents` clears only cases whose detector is the reporting monitor.
2. **Clear means an OK run that omits the key.** A failed run clears nothing, and that stays.
3. **The promotion gate's queries** (`UnjudgedRecorded`, `PromotionStreak`, in
   `internal/store/evidence.go`) select on the resolution `plan_recorded`. They must key on the
   plan's outcome instead, or watched cases stop counting.
4. **Triage opens no flotilla cases.** Its escalations are GitHub issues.
5. **Slack is not configured on the installation.**
   - `flotillad.yaml` has no `slack:` block.
   - No connector is attached to the mini's host agent: its `/etc/flotilla-host/config.json` has no
     `slack` block (`grep -c '"slack"'` returned 0, exit 1, 2026-11-26).
6. **Slack approvals resolve the actor through the `slack.users` pin,** not the directory.
7. **The connector wire knows only plan approval.**
   - `hostwire` has `notify`, `approval` and `answered`.
   - `slackconn` accepts only `approve` and `reject`.
8. **An escalated case has no dismiss move:** only close with a reason, or reassign.
9. **There is no read-only host action class,** so host diagnosis (disk, logs) is not possible
   yet. Step 1 proves the SOP loop on the backup case, where diagnosis reads metrics. Host reads
   come with step 2's disk case, under their own ADR.
10. **SOP proposals have no path into the fleets repo.**
    - The nearest path is `process_change`: a `github.open_pr` plan opened by `bugfix/open-pr`.
    - That runs on the X3's workers, which have been refused since sha-07c2272.
11. **A renamed routine is a new routine.**
    - It starts at v1, with its autonomy back to the spec's value.
    - The trap: an old, uncleared `plan_recorded` case under `ops/backup-monitor` would absorb
      the new monitor's detections and never be cleared. A drain check before the deploy is
      mandatory.

## Decisions taken for step 1

The operator gave the go to build. Where the code left a choice, these are the defaults, each
reversible:

1. **Never silent is opt-in per routine** (`case.on_record: watch`, default `close`).
   - The operator fleet uses `watch`.
   - Every other fleet behaves exactly as today.
   - Flipping the default for all fleets is a later decision.
2. **Triage's escalation buttons come right after step 1.** Step 1 ships:
   - answers for plans and notices;
   - the `triage-fleet/routed:operator` label and an operator intake;
   - Slack buttons for triage's escalations, which need triage to open flotilla cases. That is a
     triage behaviour change with its own replay, and it is the next commit after step 1.
3. **The Slack connector stays in the mini's host agent for step 1.**
   - It is limited to low and medium risk, under the daily cap.
   - ADR-0006's second instance, under its own user, comes when an operator action is promoted
     past `propose`.
4. **SOPs are skills:** `fleets/operator/sops/<id>/SKILL.md`, with JSON-valued front matter,
   declared as the fleet's knowledge in its `fleet.yaml` (C6b).
   - This reuses the skills loader, its hash and `-check`.
   - It needs no YAML dependency in the Python runtime.
5. **Until the X3's workers are fixed** (case 7), an accepted SOP proposal is committed to the
   fleets repo by hand from its case. Opening the PR through `open-pr` follows the X3 fix.

## Contracts

### Case status

There is a new open status, `watching`. The index `cases_one_open` (`status <> 'closed'`) already
holds it to one open case per key.

| Move | Role | Resolution |
| --- | --- | --- |
| planned -> watching | policy | none |
| watching -> closed | detector | `resolved_itself` |
| watching -> handed_over | operator | none (a replan after autonomy is raised) |
| escalated -> closed | operator | `dismissed` (new) |

Every move writes an event with its actor.

### Spec

- **The field:** the acting routine's `case:` block gains `on_record: close|watch`.
- **Replay proof for rule 3:** `policy.Decide` does not change, and
  `policy/testdata/default_ceiling_replay.jsonl` stays byte-identical.

### Notices

- **Table:** `notices(id, case_id, kind, n, due_at, sent_at, acked_by, acked_at)`, added in one
  numbered `migrate()` step.
- **Events:**
  - `case.watching`;
  - `notice.due`, `notice.sent`, `notice.acked`;
  - `signal.attached`, which counts repeats on a watched case.
- **Schedule** (`case_timers` config):
  - a notice at once, then at 4h and 24h, then every 24h, at most 7;
  - Acknowledge snoozes it for 24h;
  - it is independent of how many signals arrive.

### Answers API

```text
GET  /v1/cases/{id}/answers
  {"case":"..","kind":"plan|escalation|notice","question":{"title":"..","text":"..",
   "plan_hash":".."},"answers":[{"id":"approve","label":"Approve","allowed":true,"why":""}]}
POST /v1/cases/{id}/answers
  {"answer":"approve","plan":"<hash>","reason":"..","note":".."}
```

- **Kind follows the status:**
  - `awaiting_approval` is a plan (`approve`, `reject`);
  - `escalated` is an escalation (`operator`, `bugfix`, `dismiss`, `mute`, `comment`, as the case
    kind declares);
  - `watching` is a notice (`acknowledge`).
- **One `answerPolicy`** holds ADR-0005's limits for every connector:
  - Slack approves only low or medium risk, bound to the plan hash, under the daily cap;
  - a frozen target or a hold refuses.
- **Only operators answer.** MCP never reaches `/answers` (`ActApprove` is for operators and admins
  only).
- **The actor** is `user:<flotilla id>`, resolved through the directory.
  - The `slack.users` pin stays the allowlist.
  - A directory id that disagrees with the pin is refused.
- **The old `/approve`, `/reject` and `/close` routes** become thin aliases over `Answer`.

### Connector wire

- **The ask:** `NotifyBody` gains `kind`, `answers` and `repeat`.
- **The click:** `ApprovalBody.Action` carries any answer id. flotillad checks it against the
  ask's stored answers.
- **Rendering:** `slackconn` shows one button per allowed answer.
- **After a restart:** when a lost message `ts` is unknown, the connector posts a new message.
- **Deploy:** flotillad upgrades first (the agent's hello gains a `caps` field that an older flotillad refuses).
  - An agent without the `notify_answers` capability is sent plans in the old shape and no notices. Its notices wait
    unsent, logged once, until an upgraded agent connects.
  - From now on the agent reads a notify body leniently.
- **A notice's ask is spent by its first click,** when a newer notice's ask replaces it, when the notice is
  acknowledged anywhere, and when the case leaves watching.

### SOP file

In `fleets/operator/sops/<id>/SKILL.md`, declared in `fleet.yaml` under `knowledge:`.

```text
---
name: backup-stale
description: Use when a backup's age passes its limit on a host with a backup job.
applies_to: {"kinds":["failed_backup"],"hosts":["*"],"alerts":[]}
diagnosis: [{"read":"metric","name":"backup_age"}]
fix: [{"action":"backup.rerun_observability"}]
verify: {"check":"backup_newer_than_execution","within":"60m"}
rollback: []
rung: 2
evidence: ["case-..."]
---
The human steps, and why.
```

- **`rung` is a request.** Policy still takes the lowest of the routine, action, host and
  installation caps.
- **`fix` names registry actions only.** The loader refuses any other name, so an SOP never adds a
  command.

## Commits, each green on its own

| # | Commit | Gates beyond `make lint` and `make test-unit` |
| --- | --- | --- |
| C1 | ADR-0023: never silent, answers in the core, connectors at the edge | docs-lint, strict docs build |
| C2 | `watching`, `on_record`, clear, the gate's queries, the notice schedule | new e2e `watching`; `cases`, `business`, `world`, `confighash` unchanged; no-effect sections |
| C3 | the answers API, CLI, UI, the `dismiss` move | e2e approvals sections |
| C4 | Slack: any answer, notices, directory resolution | Go slack tests; e2e host frame |
| C5 | `routed:operator` label and the operator intake | `make replay` on the host before it ships |
| C6 | `runtime/sops.py`, the SOP-aware act routine, investigate, the SOP draft | new e2e `sops`; installation-facts lint |
| C6b | fleet knowledge: `knowledge:` in `fleet.yaml` (`internal/spec` strict load, `-check`, the fleet's version hash), a Knowledge tab per declaring fleet with its fields and use, proposals with Accept and Reject | spec tests; UI tests; e2e fleet with and without knowledge |
| C7 (private) | the rename, `on_record: watch`, the first SOP | `deploy_fleets.sh --check`, the drain check, one deploy |

## Tests that prove it

- **The 2026-11-21 shape as a fixture:**
  - Input: one detection, a recorded plan, 136 unchanged detections, then a clear.
  - Expect: one case, one notice, 136 repeats, closed `resolved_itself`.
- **Unchanged behaviour:** `TestOneCasePerIncident` passes unmodified, so the default `close` is
  unchanged.
- **The gate:** a watched case, once closed, still counts towards the gate's evidence.
- **The answers API:**
  - an approve given through the API equals the old approve (same hash binding, same 409);
  - Slack's risk limit holds there;
  - MCP cannot answer.

## Not covered by step 1

- **The rest of the plan:** steps 2 to 5, and the host agents on the VPS, the DGX and the X3.
- **The host agent:** new precondition kinds, and host read actions.
- **SOPs:** per-SOP promotion; only the action's streak exists.
- **Triage:** its escalation buttons (the commit after), and its direct detection path.
- **Replay of Go case events:** no tool exists for it. The fixture test stands in, and each PR
  says so.
