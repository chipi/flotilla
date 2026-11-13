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

- **Words:** ADR-0011 renamed RFC-0004's terms: a write capability is an **action**, a read
  capability a **tool**, the capability registry **the registry**; ADR-0009 renamed the actuator to
  the host agent's **`actions` role**. This plan and the code use the new words.

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
| 3 | **The registry** (ADR-0011: actions and tools; RFC-0006 §10 step 4's design): the host agent's `actions` role declares `backup.rerun_observability` in `host/registry.json`, carried by flotillad's image and validated at load | a bad declaration stops flotillad at load; the entry's fields match RFC-0004 §3.4 |
| 4 | **Policy** in flotillad, RFC-0004 §5.1 in order: registry (scope, `for_kinds`, no retargeting, cited numbers), autonomy lowest wins (job stage, the action's autonomy, host ceiling, taint, the freeze), the per-target lock and limits, holds (STOP, `flotilla hold`, deploy stand-down), the platform cap and circuit breaker; every refusal recorded with its reason | a table of plans and states, each with its expected decision and reason; mutants for each rule |
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
- **Step 2, stuck-state timers:** built 2026-11-13 for `detected` (30m: an ERROR to GlitchTip, once
  per case, and a `timer.stuck` event) and `handed_over` (24h from the first hand-over, a deferral
  keeping the clock and a reassignment restarting it: escalated by the timer), checked every minute
  in flotillad's heartbeat; `case_timers` in flotillad.yaml (operator, 2026-11-13). 6 mutants killed.
  **Moved:** the `executing` and `checking` deadlines need each action's timeout and check window
  (step 3), and leases need jobs to hold them (step 5); they land with those steps.
- **Step 3, the registry (actions only):** built 2026-11-13, option 1 of the operator's choice
  ("the mechanism and the one action now"). `internal/registry` loads strict JSON declarations;
  `host/registry.json` (the host agent's `actions` role, ADR-0009) declares `backup.rerun_observability`
  as RFC-0004 §3.4 has it, with two choices made here: its `host` parameter is `target_host` (the case's
  host; which hosts exist is an installation fact, ADR-0013), and its `timeout` is 30m (ADR-0006 §7
  needs one; RFC-0004 gives none). flotillad's image carries it; `-check` lists the actions; a bad
  declaration stops flotillad. 6 mutants killed; the e2e checks `-check` in the image.
  **Not done (the gap, on purpose):** the workers' tools and gates, `GET /v1/registry` on the worker
  and the comparison with flotillad's copy, and the test keeping a declaration equal to its code. Those
  need the fleet session (triage's tools and gates live in its code) and come before the `requires`
  check of RFC-0006 §10.
- **Step 4, policy:** built 2026-11-13. 4a (092a7a3): `internal/policy`, the decision as a pure
  function, RFC-0004 §5.1 in order, shadow recording what it would have done; 14 mutants killed.
  4b: schema step 7 (`plans`, `target_locks`, `holds`); `DecidePlan` gathers the state and applies the
  decision with the case's move and the target's lock in one transaction (a race on the lock is
  decided again as a defer); the lock stays held after a failed or unknown execution and goes on
  close, rejection or reassignment; `flotilla hold` and `/v1/holds`; 12 more mutants killed, the e2e
  holds a host through the CLI. Choices made here, all config-free for now: a host's ceiling is
  propose (ADR-0006: a new host starts there; per-host config comes with the host agent); a run is
  never tainted (recall is not in v1); the guardrail numbers of 4a; the host incident window 15m.
  **Not yet:** promoting an action's autonomy (`flotilla action autonomy …`, operator only, ADR-0008)
  and the deploy stand-down (a hold the deploy takes); both before the first plan above shadow.
  Found on the way: flotillad's heartbeat read the scheduler's clock before `Loop` set it up (a race
  since phase 2, which the new case timers made likelier); `Scheduler.Clock()` sets it up first.
- **Step 5, triggers and the job contract:** built 2026-11-13. 5a (c4dda78): `trigger: {case: {kind}}`,
  one acting routine per kind, no interval and no freshness. 5b (8f8cd43): the run's plan file
  (`FLOTILLA_PLAN_FILE`, one JSON object, read and removed by the worker; never a failed run's, never
  an old run's) and `FLOTILLA_CASE`. 5c: `internal/dispatch`, every 15s in flotillad: expired leases
  freed (never with an execution open), each handed-over case leased atomically to its routine and
  run with the case; a plan goes to policy, `resolved_itself` closes, `escalate` escalates, anything
  else is a `run.no_decision` event and a 10m backoff (also after a defer); schema step 8
  (`cases.lease_until`). 8 mutants: 7 killed, 1 equivalent (the dispatcher's holder check, a fast
  path: `LeaseCase` is the guard, and its mutant is killed). Race detector clean.
  **Not yet:** the chain end to end in the e2e: it needs a monitor to open cases (step 6) and an
  acting routine to run (step 7). Lease renewal by a job is not built: a lease lasts the routine's
  timeout plus 2m, which covers a run that ends; a longer job would need it.
- **Steps 6 and 7, the monitor and the acting routine:** built 2026-11-13. 6a (c818394): a routine
  on time declares `detects: [kind]` and reports detections in its plan file; flotillad computes the
  keys, opens or attaches, and hands a new case over. 6b/7: `ops/` (stdlib Python): `backup_monitor.py`
  reads `homelab_backup_offsite_last_success_timestamp` (the homelab alert's expression) and detects
  above the threshold of `backup.rerun_observability`'s `backup_age_above` for its stage, read from
  `host/registry.json` (20h in shadow); `rerun_backup.py` re-checks and plans, or closes as resolved,
  evaluating the registry's preconditions itself (`not_running` recorded as the host's to check);
  a test
  keeps the declaration's precondition names equal to what the code implements. One image,
  `flotilla-worker-ops`, two services `worker-ops-read` and `worker-ops-act-host` (compose profile
  `ops`), tokens only, HOME a tmpfs. `fleets/ops/rerun-backup.yaml` is capped at shadow.
  **The chain runs end to end in the e2e:** the monitor's detection (the VictoriaMetrics stand-in at
  20.5h) becomes a case, handed over, planned by `ops/rerun-backup` citing its re-check, closed by policy
  as a shadow plan that would have been a proposal. Found on the way: flotillad's `NO_PROXY` did not
  list the new workers, so it reached them through the egress proxy and was refused (407).
  **Deploy:** a host must run the ops workers or disable their routines before it takes this tag
  (`deploy/compose/README.md`). Live in shadow on the mini only on the operator's go; then step 8 (the
  UI's slice 2) and the gate (5 shadow plans the operator agrees with).
- **Step 8a, the UI's Cases and Case pages (read side):** built 2026-11-13. `/cases` (open, or all),
  `/cases/{id}` (the case, each plan with its outcome, autonomy, "would have been", hash and reasons,
  the whole timeline), "Cases that need you" on Now (escalated or awaiting approval); every role reads
  them. The e2e's browser check opens the chain's case. **8b, approving and rejecting,** waits for the
  host agent (step 9): an approval would move a case to executing with nothing to execute it.
  **Resolved (operator, 2026-11-13): one case per incident** (ADR-0005 decision 1, amended). Found in
  the e2e: RFC-0004 §3.5 matches a new detection only against
  open cases and dismissed ones, so while the backup stays over 20h every monitor run after a shadow
  close opens a new case (about 24 a day, not RFC-0004 §10's "daily shadow cases").
- **Promoting an action** (before any plan above shadow): built 2026-11-13. `flotilla action list`,
  `flotilla action autonomy NAME LEVEL --reason`, `GET /v1/actions`, `POST /v1/actions/{name}/autonomy`,
  and the Actions table on the Autonomy page (a form for operators and admins). One step up at a time,
  with a reason, by an operator or an admin; down any number of steps without one;
  `reversible: no` at most propose. The record shown: plans, what shadow plans would have been,
  verified, failed. 5 mutants
  killed. **Not yet:** automatic demotion (failed or unknown → propose; rejected proposals → shadow),
  which needs executions and approvals (the host agent, 8b).
- **The deploy stand-down** (before any plan above shadow): built 2026-11-13.
  `deploy/mini/standdown.sh` takes a hold on `host:HOST` by the installation's `deploy` caller (role
  agent; its token read by path from flotillad's secrets volume) and never fails its caller. flotilla's
  `deploy.sh` takes one for 30m before `up` (`FLOTILLA_STANDDOWN_HOST`); the homelab's `mini-deploy.sh`
  from a pre-pull hook. The e2e takes one and checks it is the host's hold, and that one that
  cannot be
  taken is skipped.
- **Automatic demotion:** built 2026-11-13, in the case's move (one transaction): a failed or unknown
  execution (executing → escalated) lowers the case's action from auto to propose; an operator's
  rejection (awaiting_approval → escalated by the operator) lowers it to shadow, on the first one
  (RFC-0004 §5.2 says "rejected proposals" without a count; the cautious reading). An expiry is not
  a rejection; a demotion never raises; an `action.demoted` event goes on the case. 4 mutants killed.
- **Deploy checklist** for the mini, with the UI and v1: **Saturday 2026-11-21, after the scheduled
  weekly freeze and check pass on the promoted code** (operator, asked again; the first answer,
  "Saturday 11-15", rested on a wrong calendar of mine: 11-14 is the Saturday, its freeze runs before
  the promotion, and the next scheduled one is 11-21; the fleet session caught it). On the operator's
  go:
  the store-copy check for schema steps 5 to 8; `ui_listen`/`ui_host`; the ops workers' tokens,
  `worker-ops.env` and profile `ops` (or their routines disabled); the `deploy` caller, its token and
  `FLOTILLA_STANDDOWN_HOST`; then the homelab proxy node.
