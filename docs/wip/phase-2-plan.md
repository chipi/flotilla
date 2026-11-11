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
| --- | --- | --- |
| 2.1 | **flotillad** (Go, from fleetd): the spec registry (a routine spec wraps today's command), the scheduler with STOP, pause, stage and budget, the store (runs, events, controls), the API and the CLI. Runs go to a worker; flotillad holds no fleet secrets. | unit tests; `flotilla status` and `flotilla run` work against a local worker |
| 2.2 | **Worker** (Python, stdlib): a small HTTP service inside each fleet's container. It runs the routine's command with that fleet's secrets and reports the result. | integration test: flotillad + worker + a stub cycle |
| 2.3 | **Images and compose**: `flotillad`, `worker-triage`, the egress proxy. Hardened as ADR-0002 says. Built in CI, tagged `sha-<7>`. | e2e in the dev engine with stub services |
| 2.4 | **Egress**: workers on an internal network; out only through Smokescreen with the triage allowlist (LiteLLM, Grafana, GlitchTip, VictoriaMetrics/Logs/Traces, Langfuse by container name on their own Docker networks, never through the host's port forwards; `api.github.com`). | a denied host is refused, in the e2e test: **done**, and an open allowlist fails it |
| 2.5 | **Observability**: flotillad metrics (fleetd's series kept until the switch), JSON logs, errors to GlitchTip, traces, a deploy event per deploy, a dashboard, a stale alert per routine. | `deploy-verify` learns project `flotilla` and passes |
| 2.6 | **Heartbeat routine**: a harmless routine that runs every 10 min and exercises the scheduler, worker, store, egress and telemetry. It is the platform's own canary until phase 3 adds a model step. | stale alert fires when it is stopped on purpose |
| 2.7 | **Shadow**: deploy on the mini; the triage routine in `shadow` (no outward action) with its own state, seeded from a copy of the live state. Compare with fleetd per cycle: ran on time, outcome, duration, spend; and the decisions per signal (differences explained, since the model is not deterministic). | a few days without a platform failure; every difference explained |
| 2.8 | **Switch**: STOP on fleetd's triage; flotilla's triage routine to `propose` on the live state folder. fleetd stays installed for rollback (STOP flotilla's routine, remove fleetd's STOP). The fleet session updates its weekly freeze and filing gate for flotilla commits first. From here the live code is flotilla's and homelab's copy is no longer live. | one day of live cycles; the Hub, deploy-verify and the fleet session's gates all green |

The bugfix fleet stays on fleetd (it is disabled there today) until phase 6.

## Progress (2026-11-08)

| Step | State | Evidence |
| --- | --- | --- |
| 2.1 flotillad | done | `make ci-fast`: ci-fast PASS (unit tests for spec, store, sched, api, worker client, telemetry) |
| 2.2 worker | done | 12 worker tests OK; two mutants (inherited environment, leader-only kill) fail them |
| 2.3 images, compose | done | images `sha-75a7491` on ghcr; `deploy/e2e/run.sh`: E2E PASS, 26 checks |
| 2.4 egress | done | e2e: an open allowlist fails it; on the mini every host the triage cycle used was allowed by rule and `example.com` refused |
| 2.5 observability | done | metrics, JSON logs, traces, deploy event, alert rules `flotilla-silent` and `flotilla-job-stale` (homelab 85343a9, live, drift 0), dashboard `flotilla` (homelab 5f39c5c), deploy-verify project `flotilla` (homelab e8a5967: DEPLOY-VERIFY PASS). errors to GlitchTip: project flotilla (id 39), `flotillad -send-test-error` arrived as FLOTILLA-1 through the egress proxy (allowed by rule), then deleted. **All of 2.5 done.** |
| 2.6 heartbeat | done | runs every 10 min through the worker and the proxy. Stale alert fired on purpose (operator's yes): paused 20:40Z, Pending 21:00:05Z, Alerting 21:10:09Z |
| 2.7 shadow | **paused since 18:5xZ** (its gauges were summed into the Hub; fixed by service filters) | since 18:45Z on the mini (`deploy/mini/deploy.sh sha-75a7491` → DEPLOY PASS); first shadow cycle ok: 1 Grafana alert and 10 GlitchTip issues, all already decided, no model call. **Not done:** the comparison with fleetd |
| 2.8 switch | not started | needs the comparison, the fleet session's scripts, and where the live state lives (below) |

**Decided on the way:**

- **State and secrets are Docker volumes, not host folders.** Every stack on the mini mounts host
  folders read-only, and the VM shares `/Users` over sshfs on the lima SSH connection whose failures
  took production down before. `deploy/mini/secrets.sh` copies the secrets into volumes.
- **The shadow state was seeded from a copy of `~/signal-fleet`** without any env file. The folder
  holds ten backups of the fleet's secrets file; the first seed version would have copied them.

**The state mirror (agreed with the fleet session, 2026-11-08; to build before 2.8).** After the
switch the live state is a volume; the Hub and the fleet session's weekly freeze, weekly check and
filing gate keep reading `~/signal-fleet` on the host, kept current by a host-side mirror:

- a launchd job under the operator account, every 2 min; it copies only when no triage run is in
  flight and the last run ended after its stamp (so never mid-write);
- it copies `results/` (dispositions.tsv, filed.tsv, probes/ with capture names unchanged), `queue/`
  and `export/`, each file whole and atomically (temp file + rename); never `frozen/` (the weekly
  freeze writes it) and never `fleet-gateway.env` (stays a host file; the freezer reads it);
- `results/.mirror_stamp`: end time (UTC) and id of the last run the copy includes; the freeze names
  its snapshot by it;
- `results/cycles.tsv`: run_id, start_utc, end_utc, status, stage per run; replaces the first line of
  each day in `~/fleetd/fleetd.log`, which weekly_check.py reads as that day's stale sweep;
- `results/deploys.tsv`: flotilla's deploy appends the same 6 columns plus `repo`;
- the fleet session's code readers move to `~/flotilla/repo` (signal-fleet/mvp at the same path),
  with the homelab-to-flotilla commit map in fleet-eval-data.

Built (flotilla 4bc3ef2..290564e, `deploy/mini/mirror.py`): checked before and after each copy (the
fleet session's race), never `frozen/`, env files or `results/deploys.tsv`. Running as LaunchDaemon
`com.flotilla.mirror` into `~/flotilla/mirror-test` only; the fleet session checked its shape
(2026-11-08, 21:2xZ).

## Where it stands (2026-11-08, 21:45Z)

- **Shadow running again**: the operator resumed both jobs at 21:38:32Z (their operator token is
  `~/flotilla/operator.token` on the mini, mode 600); first runs ok 21:39Z. The comparison with
  fleetd (`deploy/mini/shadow-compare.sh`) builds up from here.
- **The fleet session's reader changes**: operator's go 2026-11-08 21:4xZ, relayed to it. It points
  its weekly freeze, weekly check and filing gate at `~/flotilla/mirror-test` and reports when they
  pass.

## Before the switch (2.8), in order

1. A few days of shadow; `shadow-compare.sh` read and every differing decision explained.
2. ~~The fleet session reports its readers pass against `~/flotilla/mirror-test`.~~ *Done
   2026-11-08 (fleet session): weekly freeze named by `.mirror_stamp` and refusing a stalled mirror,
   weekly check and filing gate reading `cycles.tsv`, deploys.tsv's 7th field by position;
   fleet-eval-data b98d392 on the mini. First real test: the first weekly freeze after the switch
   (Saturdays 11:17 Amsterdam).*
3. ~~The homelab-to-flotilla commit map in fleet-eval-data
   (`projects/signal-fleet-replays/commit-map.tsv`), with the cutover time.~~ *Dropped 2026-11-08
   (fleet session): `hostpaths.periods()` takes the repo from each deploy-log row, and the homelab
   checkout stays on the mini until phase 7. If it goes before the old periods leave the replay
   windows, the fleet session asks for the map then.*
4. flotilla's deploy appends `results/deploys.tsv` with `flotilla` in the 7th field, a rollback too
   (the header stays as it is; the fleet session reads the field by position). *Built:
   `deploy/mini/triage_hooks.sh`, called by `deploy.sh`; the log is `TRIAGE_DEPLOY_LOG`,
   mirror-test's until the switch, since the live fleet still runs homelab code.*
4a. flotilla's deploy runs the filing gate as homelab's `gates.sh` does, with PREV and NOW
   flotilla commits:

   ```bash
   SF_CHECKOUT=~/flotilla/repo python3 ~/fleet-eval-data/projects/signal-fleet-filing/scripts/filing_gate.py PREV NOW
   ```

   A FAIL rolls back unless `SF_FILING_GATE_ACCEPT` names a reason.
   *Built: it runs before the pull, so a refused deploy changes nothing (the checkout goes back to
   the deployed commit); a rollback skips it. `deploy/mini/test_triage_hooks.py`: 13 tests, four
   mutants killed.*
4b. The stale sweep's once-a-day marker survives runs: it is
   `/home/worker/signal-fleet/results/.stale_last_run` in the state volume (checked 2026-11-08 on the
   shadow volume: present, `2026-11-08`, copied by the seed). Still to see: the first write by the
   worker itself, after 00:00Z.
5. The operator's go for the switch. Then, in one window: STOP fleetd's triage
   (`~/signal-fleet/STOP`); seed the live state volume from `~/signal-fleet`; set
   `TRIAGE_STATE_VOLUME` to it, `MIRROR_DEST` to `~/signal-fleet`, `MIRROR_LIVE=1` and
   `TRIAGE_DEPLOY_LOG` to `~/signal-fleet/results/deploys.tsv`; deploy; the
   operator raises the stage (`flotilla stage triage/cycle propose`, their token); watch one day of
   cycles, the Hub, deploy-verify and the fleet session's gates. Rollback: STOP flotilla's triage
   fleet, remove fleetd's STOP.

## Decisions taken

- **Specs and config are YAML**, the SQLite driver, the trace exporter and the base images:
  [ADR-0003](../adr/ADR-0003-dependencies-and-images.md) (operator, 2026-11-08).

## Not covered here

- The runtime, the ledger of decisions and the triage fleet's move onto it: phase 3.
- The UI and MCP: phase 5.
- How the fleet session's scripts map homelab commits to flotilla commits: agreed with it at 2.8.
