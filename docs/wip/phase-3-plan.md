# Phase 3 plan: the job runtime, and triage onto it

**Status:** in progress. The design is [ADR-0004](../adr/ADR-0004-job-runtime.md) (from
[RFC-0002](../rfc/RFC-0002-job-runtime.md)); the order is RFC-0002 §9. Step 1 runs now; steps 2 to 6
start after phase 2's switch ([phase-2-plan](phase-2-plan.md), 2.8).

## Progress

| Piece | State | Evidence |
|---|---|---|
| §9 step 1, filing and stale records (the fleet session's half) | live in homelab since 2026-11-09 07:38Z; ported here | homelab b4f4019, `mini-deploy.sh`: DEPLOY PASS, filing gate PASS; the fleet session's `exact_filing_replay.py` (fleet-eval-data d8816d4) |
| §9 step 1, triage records (`exact` in each capture: model calls as sent and answered, deployment id, gate lookups, the run's time) | live in homelab since 2026-11-09 08:06Z; ported here | homelab 2d752f6, reviewed by the fleet session; `test_capture.py` 25 OK, 6 mutants killed; `mini-deploy.sh`: DEPLOY PASS, 18 gates and the filing gate PASS |
| The candidate worker and `max_stage` (the live proof for steps 2 to 6) | deployed, off (`triage/cycle-shadow` in `disabled_jobs`) | flotilla dfe50ed: E2E PASS (32 checks, 5 new); `sha-dfe50ed` DEPLOY PASS. How to run a comparison: `deploy/compose/README.md` |
| The model client (§3) | built, not wired | `runtime/model_client.py`; `runtime/test_model_client.py`: 13 tests, golden requests byte-identical to `triage._call` through LiteLLM and direct, 7 mutants killed |

## Not done

- **The first `exact` capture read back on the mini:** waits on the first model-made decision after
  08:06Z. The fleet session checks that `outcome_count.py` and the freezer still parse it.
- **The model client's telemetry:** it records every field a span needs, but emits no OpenTelemetry
  span yet. That waits for the runtime's telemetry component (a dependency decision for the Python
  side).
- **The model client's config:** it takes an already-parsed `models` section. The config loader
  (YAML on the host) is phase 3's config pass, and needs a YAML parser for the Python side: a
  dependency to decide.
- **Steps 2 to 6:** after the switch.
