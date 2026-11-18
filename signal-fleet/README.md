# signal-fleet — the triage fleet's code

The triage fleet reads live production signals (error-tracker issues and firing alerts; logs,
metrics and traces as probe evidence) and decides, for each, **dismiss, file, escalate or cleanup**.
It never changes production and never fixes code: its output is a GitHub issue, and a code bug it
routes to the bugfix fleet with a label. Gates in code decide what leaves, after the model.

**Start here:** the [triage fleet guide](../docs/operations/triage.md) (the parts, one signal's path,
the ledgers, the reasoning behind each rule) and the [runbook](../docs/operations/triage-runbook.md)
(deploy, operate, replay a change, troubleshoot).

## Where it runs

flotillad runs the routine `triage/cycle` ([fleets/triage/cycle.yaml](../fleets/triage/cycle.yaml))
every 10 minutes on the triage worker, a container built from this directory's `mvp/` and flotilla's
`runtime/` ([deploy/docker/worker-triage.Dockerfile](../deploy/docker/worker-triage.Dockerfile)). A
candidate, `triage/cycle-shadow`, runs beside it at shadow and never files. Its state is a Docker
volume; its credentials come from the worker's secrets volume.

The code holds no installation facts. Which repo each project files into, the probes' labels, the
scrubber's own rules and the rest are read from the fleet folder's `triage/installation.json`
([`mvp/installation.py`](mvp/installation.py)): flotilla ships an example, an installation mounts its
own ([fleets/README.md](../fleets/README.md)).

## Tests and replays

- **Unit tests** (in `make test-unit`, every push): `mvp/test_units.py`, `test_capture.py`,
  `test_scrub.py`, `test_replay_api.py`, `test_dev_filter.py`, `test_probes.py`. They read a fixture
  with example names (`mvp/testdata/installation.json`).
- **Four generic replay gates** stay here: `mvp/gate_harness_replay.py`, `gate_model_client_replay.py`,
  `gate_skills_replay.py`, `gate_ledger_backend_replay.py`.
- **The installation's replay gates** run its recorded real cases with its own facts, and live in its
  private fleets repo: `make replay` with `SF_REPLAYS` (its `triage/gates/run.sh`) and
  `SF_ALERT_RULES`. A change to a decision is replayed before it ships.
- **The replay entry point** for evaluation projects: [`mvp/replay_api.py`](mvp/replay_api.py).

## The record

How the fleet was designed and evaluated, kept as written (the deploy and operations they describe
are the fleetd era, before flotilla):

| Document | What it is |
| --- | --- |
| [`SIGNALS.md`](SIGNALS.md) | the design: the intent gate, dispositions, correlation, sources, state, autonomy |
| [`EVAL.md`](EVAL.md), [`EVAL-LESSONS-0.6.0.md`](EVAL-LESSONS-0.6.0.md) | how it was evaluated, and what round r4 found (2026-11-04) |
| [`PHASE-0-infra.md`](PHASE-0-infra.md) | how each source is read, with the first live evidence |
| [`REVIEW-2026-07-24-fleet1-architectural.md`](REVIEW-2026-07-24-fleet1-architectural.md), [`REVIEW-ASK-round7.md`](REVIEW-ASK-round7.md) | design reviews |
| [RFC-0003, the signal-to-action fleet](../docs/history/origin/RFC-0003-signal-to-action-fleet.md) | the RFC it was built from, in the installation's ops repo |
| [`test-receiver/`](test-receiver/receiver.py) | the webhook test-ingress stub of phase 0 |
