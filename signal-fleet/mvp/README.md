# signal-fleet/mvp — the triage fleet's cycle code

What the triage worker runs for the routine `triage/cycle` (`orchestrator.py --cycle`), every 10
minutes, scheduled by flotillad. The directory keeps its MVP name; the code has been the live fleet
since August 2026, on flotilla since 2026-11-11.

- **How it works and why:** [Triage fleet guide](../../docs/operations/triage.md).
- **How to deploy, operate and change it:** [Triage fleet runbook](../../docs/operations/triage-runbook.md).

Stdlib-only Python, plus flotilla's `runtime/` (the model client, the harness, skills, recording).
No secrets and no installation facts in the code: credentials come from the worker's secrets volume,
state lives in its state volume, and the installation's facts in the fleet folder's
`triage/installation.json`.

## Files

| File | Role |
|---|---|
| `orchestrator.py` | one cycle: the alert pass, the error-tracker pass, triager-down, inbox, stale pass, GitHub → error-tracker sync, spend |
| `sources.py` | read firing alerts and unresolved error-tracker issues; normalize to signals |
| `correlate.py` | probe evidence for the triager: logs, metrics, traces (by the OTel trace id) |
| `triage.py` | operational states (no model), the model's investigation and decision on the shared harness, then the gates (intent, cleanup, dismiss, edge-5xx) |
| `actions.py` | the dispositions ledger, idempotency, recurrence rows |
| `filing.py` | the filed ledger and the GitHub decision tree (routing, dedup keys, storm groups, rollups, duplicates, reused short ids) |
| `installation.py` | the installation's facts, read from `triage/installation.json` in the fleet folder |
| `glitchtip_sync.py` | mirror the operator's GitHub closes onto the fleet-filed error-tracker issues |
| `stale.py` | the daily "looks resolved?" nudge |
| `inbox.py` | operator-inbox metrics; ships audit events to the log store |
| `observ.py` | metrics and the LLM trace per triage |
| `guards.py` | OOM episodes per container and container restarts near an edge-5xx burst |
| `capture.py`, `callrec.py` | per-decision capture of what the model saw and what filing looked up, for an exact replay |
| `replay_api.py` | the replay entry point evaluation projects call (ADR-0013) |
| `ledger_sync.py` | the ledgers and flotillad's store: import once, check that they agree (RFC-0002 §9 step 5) |
| `evaldata.py` | where the installation's replay corpora are (`SF_REPLAY_DATA`) |
| `config.py`, `http_util.py`, `scrub.py`, `freeze.py` | settings from env, HTTP helpers, scrubbing and freezing corpora |
| `probe.py`, `probes.py`, `triage_probe.py`, `score.py` | live probes and the triager scorer |
| `test_*.py` | unit tests, in `make test-unit`, against the fixture `testdata/installation.json` |
| `gate_*.py` | four generic replay gates (harness, model client, skills, ledger backend); the installation's own gates are in its private fleets repo (`make replay`) |
| `run.sh` | the fleetd-era runner for a host checkout, kept for a hand run outside the worker |

## Run one signal by hand

A cycle is not read-only, even without `--no-dry-run`: it calls the model (cost), appends to the
dispositions ledger and can queue proposals. The runbook's "run one signal through by hand" says how
to do it safely inside the worker.
