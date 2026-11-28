# The health contract

**Status:** Requirements, for the systems flotilla watches (the operator, 2026-11-28). The producer
side is built by each system's owner as part of its current work; flotilla's side (the
`health-probe@1` block and the re-run action) ships in the same deploy. An RFC follows once one
producer and the consumer are live.

## Why

Deciding whether a system is up today means reading a different source for each one: a metric for a
GPU host, an error tracker for errors, a health route that only says the API process answers, logs
for a batch pipeline. Every case rebuilds that by hand. The first case that needs one answer: a
model server is taken by another job for hours, a scheduled batch run fails its episodes, and the
fleet must wait while the holder runs, then re-run the batch once **everything it depends on** is up
again, the same night rather than the next.

One contract, one probe, three uses:

- **monitor:** a component went down: open a case;
- **readiness gate:** every component an action needs is up for N readings in a row: plan the
  action;
- **verify:** it came back after the fix.

## The producer's side

**H1. One route, one body.** `GET /health/ready` (any path the system chooses, named in host config)
returns JSON:

```json
{
  "contract": "flotilla.health/1",
  "status": "up",
  "checked_at": "2026-11-28T10:00:00Z",
  "version": "sha-1234567",
  "components": [
    {"name": "model-prod", "kind": "model", "status": "down", "since": "2026-11-27T03:10:00Z",
     "reason": "connection refused", "latency_ms": null, "checked_at": "2026-11-28T10:00:00Z"}
  ],
  "capabilities": {
    "scheduled-run": {"ready": false, "needs": ["model-prod", "asr", "database"],
                      "blocked_by": ["model-prod"]}
  }
}
```

**H2. Live checks of every dependency the system calls**, not only "the process answers". Each check
has its own timeout of 2 s or less, the whole route answers in 5 s or less, and a result may be
cached for at most 30 s. A model server is checked by the call the system makes (its models list or
a 1-token request), not by a TCP connect.

**H3. Status values and their meaning, the same everywhere.**

- `up`: serves;
- `degraded`: serves with a fallback or reduced capacity (say which in `reason`);
- `down`: cannot serve;
- `unknown`: the check itself did not run (a timeout, an error in the check). Never reported as up.

The top-level `status` is the worst status among the components any capability needs.

**H4. `since`** is when the component entered its current status (kept by the system across checks),
so a consumer can tell a blip from an outage without its own history.

**H5. Capabilities** name the jobs the system runs and the components each needs. `ready` is true
only when every one of its `needs` is `up`, and `blocked_by` lists the ones that are not. This is
the field flotilla gates an action on.

**H6. Stable names.** Component and capability names are kebab-case and never renamed. A new one is
added, an old one is retired.

**H7. Always a body.** The route answers 200 with the body when it can run at all (503 is allowed,
but the body must be the same). A consumer reads the body, never only the code.

**H8. No secrets, internal only.** The body carries no credentials, URLs with tokens, or personal
data. The route is reachable on the internal network from the fleet's read workers (through the
egress proxy). A read-only token is acceptable if the system requires one.

**H9. The same answer as metrics:** `health_component_up{system, component}` (1 up, 0.5 degraded, 0
down; absent for unknown) and `health_capability_ready{system, capability}`, so alerts, dashboards
and history agree with the route.

**H10. Failures count as failures.** A circuit breaker or fuse that guards a dependency counts a
refused connection and a timeout as failures, not only 429 and 5xx, and its state (`open`,
`half-open`, `closed`) is reported as that component's `degraded` or `down` with the reason. A
breaker's state lives in the process that calls: a route served by another process checks live
(H2) and does not read it.

**H11. Busy is not down.** A single-flight server that is busy with a long job stops answering HTTP
while it still serves. A check that finds the port open but the server busy reports `up` with
`busy` in `reason`, never `down`; `down` needs a refused connection, or a `down` the server reports
itself.

**H12. Configured is not reachable.** A gateway's model list says an alias is configured, not that
its backend answers. A component behind a gateway is checked through the backend (a 1-token request
through the alias, or the backend's own readiness), and a 200 that says it is still loading is not
`up`.

## A re-run on demand (for a system with a scheduled batch)

**T1.** An authenticated trigger (`POST`, or a CLI the fleet's host agent runs) starts the scheduled
run now with the same command and configuration as the schedule, plus a reason and a requester
recorded on the run.

**T2. Safe to repeat.** It skips what is already done (`--skip-existing` or its equivalent), so a
re-run processes only what failed or never ran.

**T3. Refuses cleanly.** 409, with the reason, when a run is active, when the capability is not
`ready` (H5), or when a window the system declares (a quiet window, a cost cap) forbids it.

**T4. Returns a run id**, and the run's events and index carry it, with `trigger: on_demand`.

**T5. A run's outcome is readable by id:** status, episodes processed, failed (with their stage and
error type), skipped.

## flotilla's side (built 2026-11-28, step 1)

What is built, all at record, none deployed:

- `ops/health.py`, the reader: the route is a source NAME (`url_source`; its URL is `SOURCE_<NAME>`
  in the worker's environment, host config `ROUTINE_SOURCE_<NAME>`), read through the egress proxy
  over https (plain http only for a host named in `HEALTH_ALLOW_HTTP`, a test's), with a whole-fetch
  deadline, a size cap, no redirect to another host and no URL in any message. A 503 is read for its
  body (H7). A body that is not `flotilla.health/1`, or is mis-shaped, is refused, and a refused
  body is unknown, never up. A component or capability the body does not name is unknown. `up` is up
  whatever its reason says (H11: `busy` in the reason is still up). A capability whose `ready` a
  need contradicts is not trusted.
- `health-probe@1`, a library block (monitor, code only, `ops/health_probe.py`). It watches one
  capability or a list of components, the ones its registry action's precondition names
  (`capability_ready(x)` or `component_up(x)`: one home). `report: down` opens a case after
  `readings` not-up readings and holds it until `clear_after` up readings; `report: recovered` is
  the readiness gate: after an outage it reports the recovery once the capability has read up for
  `clear_after` readings, for at most 12 runs. Unknown breaks both streaks, so it never opens,
  clears or recovers a case. State is the probe's own snapshot in its ledger: the route has no
  history, and `since` says when a status began, not how long a case was open. A route that cannot
  be read, a refused body or an unreadable snapshot fails the run: nothing is reported, no case is
  cleared. No `step` param: the readings are the probe's own runs, and the step's `every` is their
  interval.
- Facts in `sop_act` (`capability_ready(x)`, `component_up(x)`): one fresh read of the route named
  by the step's `health_source`; true holds, false does not (the case resolves), unknown (an
  unreadable or refused body, a name the body lacks) escalates and never resolves. The N-readings
  gate is the probe's, not the act step's.
- `pipeline.rerun_scheduled` (kind `batch_window_failed`, medium risk, regenerable): record-only in
  this step, with the precondition `capability_ready(scheduled-run)` and the check
  `window_failed_episodes_processed` within 6 hours. Record-only caps it at record; its executor
  changes to a routine action held at propose when the producer's trigger is live. The trigger is an
  HTTP call through the egress proxy with the operator key as a host-config secret name, never in a
  spec or a plan, not a host CLI.
- `health-verify@1` (`ops/verify_run_processed.py`): the check, written against T5's shape.
  Finished, episodes processed and none failed is verified; failed episodes escalate; anything
  unreadable is not yet. **Unexercised**: no producer serves T5, and `FLOTILLA_CHECK` carries no run
  id, so it reads the newest on-demand run that started after the case went to checking.
- The example flows: `fleets/jobs/flows/batch` (probe, act, investigate, verify) and the
  holder-aware `service` flow (see the DGX plan). The e2e world answers an all-up health body.

## Not covered by flotilla's side

- No producer serves the contract yet; the probe has run against fixtures and the e2e stand-in only.
- The re-run is not triggered by anything: T1 to T4 are not live, so no run id is recorded and no
  clean refusal is read. The check is unexercised (above).
- `component_up` as a monitor use has tests, and no example flow runs it.
- The probe's recovery report is bounded (12 runs) and a case it opens is closed by the monitor
  clearing, not by the re-run's verification; an operator who approves late may find the case gone.
- H9 (the metrics that agree with the route) is not read by flotilla.

## Not covered yet

- Which systems adopt the contract after the first; the GPU host's own health is metrics today, not
  a route.
- Health of flotilla itself on the same contract (its `status` tool predates it).
- Auth for the trigger, beyond "authenticated": each system's owner names it.
