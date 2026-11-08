# Phase 2 plan — flotillad runs the triage fleet

**Status:** in progress (operator's go, 2026-11-08). RFC-0001 §9, phase 2; ADR-0002 for Docker.

## Goal

flotillad replaces fleetd for the triage fleet, in Docker on the mini. The fleet's code does not
change. It is observable from its first deploy, and the live fleet never stops: flotillad runs the
fleet in `shadow` beside fleetd first, then takes over.

## Facts this rests on (checked 2026-11-08)

- fleetd runs the triage cycle every 10 min (`python3 orchestrator.py --cycle --limit 10
  --no-dry-run`), stage `propose`, budget $2/day. Spend over the last 7 days peaked at $0.08 a day
  (`max_over_time(fleetd_spend_day[7d])`), so a shadow run beside it costs next to nothing.
- The triage model is the LiteLLM alias `fleet-triage-pro`, reached at `localhost:4001`.
- Containers in the mini's production engine:
  - reach host-published ports as `host.docker.internal` (the LiteLLM compose file);
  - bind-mount folders under `/Users` (the fleet-evals stack mounts a fleet-eval-data checkout).
  So the fleet's state can stay in `~/signal-fleet`, where the Hub reads `export/inbox.json`, and
  where the fleet session's weekly freeze and filing gate read the ledgers and the deploy log.

## Steps

| # | Step | Done when |
|---|---|---|
| 2.1 | **flotillad** (Go, from fleetd): the spec registry (a job spec wraps today's command), the scheduler with STOP, pause, stage and budget, the store (runs, events, controls), the API and the CLI. Runs go to a worker; flotillad holds no fleet secrets. | unit tests; `flotilla status` and `flotilla run` work against a local worker |
| 2.2 | **Worker** (Python, stdlib): a small HTTP service inside each fleet's container. It runs the job's command with that fleet's secrets and reports the result. | integration test: flotillad + worker + a stub cycle |
| 2.3 | **Images and compose**: `flotillad`, `worker-triage`, the egress proxy. Hardened as ADR-0002 says. Built in CI, tagged `sha-<7>`. | e2e in the dev engine with stub services |
| 2.4 | **Egress**: workers on an internal network; out only through Smokescreen with the triage allowlist (LiteLLM, Grafana, GlitchTip, VictoriaMetrics/Logs/Traces, Langfuse by container name on their own Docker networks, never through the host's port forwards; `api.github.com`). | a denied host is refused, in the e2e test: **done**, and an open allowlist fails it |
| 2.5 | **Observability**: flotillad metrics (fleetd's series kept until the switch), JSON logs, errors to GlitchTip, traces, a deploy event per deploy, a dashboard, a stale alert per job. | `deploy-verify` learns project `flotilla` and passes |
| 2.6 | **Heartbeat job**: a harmless job that runs every 10 min and exercises the scheduler, worker, store, egress and telemetry. It is the platform's own canary until phase 3 adds a model step. | stale alert fires when it is stopped on purpose |
| 2.7 | **Shadow**: deploy on the mini; the triage job in `shadow` (no outward action) with its own state, seeded from a copy of the live state. Compare with fleetd per cycle: ran on time, outcome, duration, spend; and the decisions per signal (differences explained, since the model is not deterministic). | a few days without a platform failure; every difference explained |
| 2.8 | **Switch**: STOP on fleetd's triage; flotilla's triage job to `propose` on the live state folder. fleetd stays installed for rollback (STOP flotilla's job, remove fleetd's STOP). The fleet session updates its weekly freeze and filing gate for flotilla commits first. From here the live code is flotilla's and homelab's copy is no longer live. | one day of live cycles; the Hub, deploy-verify and the fleet session's gates all green |

The bugfix fleet stays on fleetd (it is disabled there today) until phase 6.

## Decisions taken

- **Specs and config are YAML**, the SQLite driver, the trace exporter and the base images: [ADR-0003](../adr/ADR-0003-dependencies-and-images.md) (operator, 2026-11-08).

## Not covered here

- The job runtime, the ledger of decisions and the triage fleet's move onto it: phase 3.
- The UI and MCP: phase 5.
- How the fleet session's scripts map homelab commits to flotilla commits: agreed with it at 2.8.
