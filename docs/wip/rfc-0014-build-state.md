# RFC-0014 build: where it stands (2026-11-22)

The hand-off for the next session on RFC-0014 (the visual plane and mashups). The order of work is
§8; this note says which steps are done, what is open, and what needs the operator.

## Done

- **Step 1, the hook** (`hooks_listen`, `POST /hooks/{name}`, signed, `internal/hook`): live on the
  installation since sha-52bfbb9. The installation's plumbing (n8n) ran its smoke flow against it:
  202 on the first send, 200 on the replay, the smoke case closed `resolved_itself`.
- **Step 2, `plumbing.call`** (main at 6c59125; not deployed, and nothing on the installation runs it):
  - `risk: high` caps at propose (policy and `MaxAutonomy`); ADR-0017 amended (reversible means the
    executor can take it back; a check may ask the system it wrote to).
  - five registry actions (`plumbing.create_tasks`, `post_decisions`, `reply`, `post`, `retract`),
    each `executor: routine`, checked by `plumbing_confirmed`.
  - `plumbing/call.py`, the shared sender: plan hash, payload shape, chat text gates (label, no
    crowd call), signed post (delivery `plan-<hash16>`), no retry; `check` asks the callback for the
    delivery's status. A signing vector shared with `internal/hook` keeps both sides together.
  - `fleets/plumbing/` (five send routines and a check) on worker `plumbing` (the ops image, compose
    profile `plumbing`), its proxy `egress-plumbing` the only other service of flotilla's on the
    plumbing network. Setup: `deploy/compose/README.md`, "The plumbing worker".
  - the e2e's `plumbing` section: the hooks section's meeting case planned, approved, posted to a
    stub plumbing that verifies each signature, closed by the check; the proxy refuses other hosts.
- **Fixed on the way:** the pairs page's live side used a whole-ledger scan per input (schema 35,
  `ledger_entries_run`); a rotated hook secret now recreates flotillad on deploy; the e2e's
  `upgrade_start` no longer leaks a container; the no-effect matrix waits for the live side to settle
  before its "before" (a live plan landing in the window read as a shadow's).

- **Step 3, case 3, flotilla's side** (2026-11-22/23, main, not deployed):
  - a plan's `then` (RFC-0013 §2 amended): a verified check hands the case on to the next stage
    instead of closing it (`checking -> handed_over`, the check's move alone).
  - the example fleet `meetings` (`notes` on `triage`, one gated model call on the station
    `extract`, alias `meetings-notes`; `decisions` on `ops-read`), code in `meetings/notes.py`,
    `meetings/test_meetings.py`; documented in `fleets/README.md`.
  - the e2e's plumbing section is case 3's acceptance (§7), on the scripted model; `e2e/meeting-tasks`
    is gone. `E2E_SECTIONS=noeffect,ui,hooks,plumbing`: E2E PASS (278); the full run before the last
    two e2e fixes: 400 PASS, 4 FAIL, all in case 3 (the matrix leaves its routines at record; the
    platform's hourly cap deferred the rejected meeting), both fixed in `run.sh`.
  - the installation's fleets: branch `case3-meetings` in the private fleets repo (the fleets and the
    two registry entries), not pushed.

## Next, in §8's order

1. ~~**Step 3, case 3's live run on the installation**~~: done 2026-11-23. The operator promoted
   `plumbing.create_tasks` and `plumbing.post_decisions` to propose and edited the test Doc;
   `case-20261123-083d19eb2f` opened at 12:36:28Z, both plans were approved, n8n created 3 tasks and
   appended the decisions once (read back from Google by n8n's status calls), and the case closed
   verified at 12:39:24Z.
2. **Step 4, the directory reader**: built, not deployed. `internal/directory` (the `file` source:
   mode 600, known fields only, an id names one person), `store.SyncPeople` and `PersonBy` (the
   `people` and `person_ids` tables; each change a `person.added`, `person.changed` or
   `person.removed` event, actor `directory:file`), flotillad's `directory:` section (sync at start
   and every `every`, default 1h; an unreadable file keeps the copy), `-check`'s count and its
   warning on a pinned Slack user not in the file, and `secrets.sh` copying `config/people.yaml`.
   The installation's people file is on the host (the operator alone: email, name, `github`,
   `flotilla`). At the deploy: add `directory: {source: file, path: /run/flotilla/people.yaml}` to
   flotillad.yaml after the new image, since the running flotillad refuses an unknown field.
   Nothing reads a person yet: the asker of a `question` case (case 1) is the first lookup.
3. Step 5, live reads (read-only MCP tools), then case 1, case 2, the visual plane, the wall screen.

## Open in the code

- ~~Nothing enforces one execute routine per action~~: a second one is a load error now, a disabled
  one included (`spec.Load`, `TestOneExecutorPerAction`, ADR-0017's 2026-11-22 amendment).
- `plumbing.reply` cannot earn auto until its remaining gates exist: the asker in the directory
  (step 4), a citation matching a recorded read (step 5), the thread from the case (case 1), and a
  retracted answer counted as a failed execution.
- ~~No `docs/history` entry for this arc yet~~: [0008](../history/0008-rfc-0014-hook-and-plumbing-call.md).
- ~~Earned autonomy and the per-action counts read the case's resolution, so in a two-plan case the
  second plan's rejection or failure counts against the first too~~: each plan counts its own outcome
  now (2026-11-23, operator; `ActionRecord`, `PromotionStreak`, RFC-0013's 2026-11-23 amendment,
  `TestEarnedCountsPerPlan`). Both meeting actions stay at propose.
- flotillad's slowness in its container, measured 2026-11-23: the pairs answer for `triage/cycle`
  v2 on the 12:00 store copy took 83 ms on the workstation (140 ms at the container's 0.5 CPU),
  340 ms on the mini's idle dev VM (665 ms at 0.5 CPU), and 9 to 10 s live (the CLI through
  `docker exec`, about 0.7 s of which is the exec). The VM runs QEMU with `accel=hvf`, not
  emulation; the 0.5 CPU cap doubles a request, no more. The rest, about 13 times, happens only in
  the live process. The suspect, not yet proven: every store read went through the writer's one
  connection (`SetMaxOpenConns(1)`), behind the scheduler's, the UI's and the metrics' queries.
  Built, not deployed: reads outside a transaction on their own pool (4 connections,
  `query_only`), a run's end and its spend in one transaction (`Store.EndRun`, which the read pool
  showed was not atomic), a cancel of a run the store has ended refused with 404, and
  `flotilla_store_wait_seconds_total{pool}`. To check after the deploy: the same CLI pairs call
  (expected under about 1.5 s with the exec) and the read and write waits' rate. If it stays slow,
  the cause is elsewhere: the production VM's contention (VictoriaMetrics, ClickHouse) or something
  only a CPU profile shows (an opt-in loopback pprof listener: the operator's call).

## Needs the operator

- To run plumbing.call on the installation: its n8n flows for each callback (each answers `send`
  with a `ref` and `status` with done or not, finding the delivery id it wrote into what it made),
  then the worker's token, the callbacks' secrets, `worker-plumbing.env`, the proxy's ACL and its
  `smokescreen-plumbing.yaml` with the plumbing network's own range (smokescreen refuses every
  private address outside `allow_ranges`), and `plumbing` in `COMPOSE_PROFILES`.
- ~~Whether n8n may read the hooks' and callbacks' secrets from a read-only mounted folder (option b)~~:
  option (a), a credential pasted by hand into n8n for each hook and callback (the operator, through
  the homelab session, 2026-11-23).
- The opt-in pprof listener above.
