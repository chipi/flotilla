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

## Progress (2026-11-08)

| Step | State | Evidence |
|---|---|---|
| 2.1 flotillad | done | `make ci-fast`: ci-fast PASS (unit tests for spec, store, sched, api, worker client, telemetry) |
| 2.2 worker | done | 12 worker tests OK; two mutants (inherited environment, leader-only kill) fail them |
| 2.3 images, compose | done | images `sha-75a7491` on ghcr; `deploy/e2e/run.sh`: E2E PASS, 26 checks |
| 2.4 egress | done | e2e: an open allowlist fails it; on the mini every host the triage cycle used was allowed by rule and `example.com` refused |
| 2.5 observability | **partial** | metrics, JSON logs, traces, deploy event, alert rules `flotilla-silent` and `flotilla-job-stale` (homelab 85343a9, live, drift 0). **Not done:** errors to GlitchTip, a dashboard, `deploy-verify` project `flotilla` |
| 2.6 heartbeat | **partial** | runs every 10 min through the worker and the proxy; the stale alert is not yet seen firing on purpose |
| 2.7 shadow | **running** | since 18:45Z on the mini (`deploy/mini/deploy.sh sha-75a7491` → DEPLOY PASS); first shadow cycle ok: 1 Grafana alert and 10 GlitchTip issues, all already decided, no model call. **Not done:** the comparison with fleetd |
| 2.8 switch | not started | needs the comparison, the fleet session's scripts, and where the live state lives (below) |

**Decided on the way:**
- **State and secrets are Docker volumes, not host folders.** Every stack on the mini mounts host
  folders read-only, and the VM shares `/Users` over sshfs on the lima SSH connection whose failures
  took production down before. `deploy/mini/secrets.sh` copies the secrets into volumes.
- **The shadow state was seeded from a copy of `~/signal-fleet`** without any env file. The folder
  holds ten backups of the fleet's secrets file; the first seed version would have copied them.

**Open before the switch (2.8):** the live fleet's state is `~/signal-fleet` on the host, read by the
Hub (`export/inbox.json`) and by the fleet session's weekly freeze and filing gate. With the state in
a volume they need another way in: the flotilla API, or a read-only export the host copies out. To
decide with the fleet session.

## Decisions taken

- **Specs and config are YAML**, the SQLite driver, the trace exporter and the base images: [ADR-0003](../adr/ADR-0003-dependencies-and-images.md) (operator, 2026-11-08).

## Not covered here

- The job runtime, the ledger of decisions and the triage fleet's move onto it: phase 3.
- The UI and MCP: phase 5.
- How the fleet session's scripts map homelab commits to flotilla commits: agreed with it at 2.8.
