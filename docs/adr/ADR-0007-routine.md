# ADR-0007 — "Routine" replaces "job"

**Status:** Accepted (operator, 2026-11-11)
**Date:** 2026-11-11

## Context

"Job" named flotilla's declared unit of work: a trigger, a worker, steps and a stage, inside a fleet
(`ops/remediate-backup`, `triage/cycle`). The word caused three problems:

1. **It collides in the domain the fleets work on.** What the Ops fleet fixes is often itself a job:
   launchd and cron jobs, systemd timers, CI jobs. "The remediate-backup job reruns the backup job"
   needs a second reading.
2. **It collides in the metrics.** flotillad pushes `flotilla_run{job="triage/cycle"}` into the same
   VictoriaMetrics where `job` is the Prometheus scrape-job label (`up{job="postgres"}`). The same label
   means two things in one database.
3. **It says nothing about the work.**

The term review that came out of the UI mockup (2026-11-11) took "job" first.

## Decision

**A routine** is a declared unit of work in a fleet: its trigger (time or an event on a case), its
worker, its steps, its stage and budget. A routine's id stays `<fleet>/<name>`. One execution of a
routine is still **a run**.

Renamed everywhere flotilla owns the word:

| Where | Before | After |
| --- | --- | --- |
| Docs, UI, CLI output | job | routine |
| Spec files | `job: cycle` | `routine: cycle` |
| flotillad config | `disabled_jobs` | `disabled_routines` |
| API and CLI | `/runs?job=`, JSON `job` | `/runs?routine=`, JSON `routine` |
| The store | `runs.job` | `runs.routine` (a forward schema step) |
| flotillad ↔ worker messages | `job` | `routine` |
| Worker settings (env files) | `JOB_*` | `ROUTINE_*` |
| The homelab's stale alert | `flotilla-job-stale` | `flotilla-routine-stale` |
| Metrics | label `job`; `flotilla_job_last_success_timestamp_seconds`, `flotilla_job_freshness_seconds` | label `routine`; `flotilla_routine_last_success_timestamp_seconds`, `flotilla_routine_freshness_seconds` |
| The Python side | "the job runtime" | "the runtime" |

**Not renamed:**

- other systems' jobs: launchd, cron, systemd, CI, the Prometheus scrape job;
- accepted RFCs and ADRs, which are records. They keep "job"; this ADR is the mapping. Living
  pages (architecture, security, testing, README, AGENTS.md, plans) use "routine".

## Rollout

1. The docs and the UI mockup.
2. The code in one release, deployed together with the homelab dashboard (`flotilla.json`) and the
   freshness alert that read the old metric names. The installation's config key is renamed in the
   same deploy. flotillad parses its config strictly, so a leftover `disabled_jobs` stops it at start
   instead of being ignored (an ignored key would enable a routine the operator disabled).
3. The old metric series stop and age out; nothing is backfilled.

## Consequences

**Positive:** one meaning per word in Ops conversations and in the metrics database.

**Negative:** a coordinated change across flotilla and the homelab's dashboard and alert; old runs'
metrics keep the old label until they age out.

**Neutral:** the store keeps every run; only the column's name changes.

## Alternatives considered

- **Keep "job":** no work, but all three problems stay.
- **duty:** fits the fleet theme, but reads like a person's shift.
- **task:** collides with Celery, Ansible and agents' own task lists.
- **agent:** wrong for code-only monitors, and blurs with worker and host agent.
