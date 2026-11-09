# Phase 3 plan: the job runtime, and triage onto it

**Status:** in progress. The design is [ADR-0004](../adr/ADR-0004-job-runtime.md) (from
[RFC-0002](../rfc/RFC-0002-job-runtime.md)); the order is RFC-0002 §9. Step 1 runs now; steps 2 to 6
start after phase 2's switch ([phase-2-plan](phase-2-plan.md), 2.8).

## Progress

| Piece | State | Evidence |
| --- | --- | --- |
| §9 step 1, filing and stale records (the fleet session's half) | live in homelab since 2026-11-09 07:38Z; ported here | homelab b4f4019, `mini-deploy.sh`: DEPLOY PASS, filing gate PASS; the fleet session's `exact_filing_replay.py` (fleet-eval-data d8816d4) |
| §9 step 1, triage records (`exact` in each capture: model calls as sent and answered, deployment id, gate lookups, the run's time) | live in homelab since 2026-11-09 08:06Z; ported here | homelab 2d752f6, reviewed by the fleet session; `test_capture.py` 25 OK, 6 mutants killed; `mini-deploy.sh`: DEPLOY PASS, 18 gates and the filing gate PASS |
| The candidate worker and `max_stage` (the live proof for steps 2 to 6) | deployed, off (`triage/cycle-shadow` in `disabled_jobs`) | flotilla dfe50ed: E2E PASS (32 checks, 5 new); `sha-dfe50ed` DEPLOY PASS. How to run a comparison: `deploy/compose/README.md` |
| The model client (§3) | built and production-hardened; not wired into triage until §9 step 2 | `runtime/model_client.py`. First version: golden requests byte-identical to `triage._call` (through LiteLLM and direct), 7 mutants killed. Hardened 2026-11-09 (the operator: "really not done"): keys by `key_env`/`key_file` with typed errors; a 4 MB response cap; `max_tokens`/`seed`/`stop`; a missing `usage` flagged and never priced; the job's declared fallback (`chat_policy`); an OpenTelemetry span per call under the run's trace; 24 tests, 7 more mutants killed |
| Model config from flotillad (operator's choice, 2026-11-09) | built | `models:` in flotillad.yaml, validated by `internal/models` (Go) and `model_client.load` (Python) with the same rules, tested on the same fixtures; every run gets it as `FLOTILLA_MODELS`, with `FLOTILLA_OTLP_TRACES_URL` and `FLOTILLA_ENVIRONMENT` |
| Job tracing (the OTel Python SDK, operator's yes 2026-11-09) | built | `runtime/telemetry.py`; the SDK pinned by hash in the worker image (`deploy/docker/worker-requirements.txt`, ADR-0003); `runtime/test_telemetry.py` passes in the image (spans parented on TRACEPARENT) |
| The heartbeat's model step | built | `JOB_HEARTBEAT_MODEL_ALIAS`; e2e: a call through the client to the LiteLLM stand-in with the key from the fleet's secrets file, through the proxy, and its `model.chat` span at the VictoriaTraces stand-in |
| The client against real endpoints | done for two | `runtime/contract_check.py` (json_object, native tools, max_tokens, an unknown model; cost as claimed), 2026-11-09: the mini's LiteLLM (`fleet-triage-flash`, cost reported by the body, deployment id set) CONTRACT PASS; the X3's Ollama directly (qwen3 30b, no key, cost zero) CONTRACT PASS. Found: LiteLLM answers an alias the key may not use with 403, so it is AuthOrBilling (fails closed); the error now carries the endpoint's words |
| flotillad's metric pushes (found while testing this: a phase 2 defect) | fixed | pushes were never retried: on the mini they failed during every deploy (`lookup egress`) and on 500/502 now and then (VictoriaLogs, 2026-11-08/09). Now retried after 2 s and 5 s; 4 Go tests, 3 mutants killed |

## Not done

- ~~**The first `exact` capture read back on the mini.**~~ *Done 2026-11-09 (fleet session): the
  first live decision after both deploys has `exact` with 4 turns, model_id on each, 55 KB, and still
  parses for `outcome_count.py` and the freezer; its filing record replayed with "EXACT FILING
  REPLAY: PASS". A shadow capture arrived in mirror-test at mode 600 in a 700 folder.*
- **The first sweep records** (live and shadow): at the first cycle after 00:00Z, 2026-11-10.
- **What shadow comparison can compare at the filing level** (fleet session, 2026-11-09): a shadow
  run never files. With dry_run, a file decision goes to `queue_proposal` and an escalation only
  prints, so no filing record starts. Exact filing replays come from the live fleet only. Comparing
  live and a candidate at the filing level means live's `results/filing/` records against the
  candidate's `queue/` proposals, decision by decision; `shadow_compare.py` compares dispositions
  today, not that.
- **The DGX's vLLM** (directly, or as a LiteLLM alias): not checked. The DGX is in `prod` mode
  (`gpu-mode-swap.sh --mode-only`, 2026-11-09), so its vLLM is production serving; a contract check
  there waits on the operator's go. The fleet's LiteLLM key may not use the DGX model either.
- **The mini's `models` section and the heartbeat's model step there:** not configured; needs the
  operator's go (a model call every 10 minutes).
- **The deployment-id-to-model map** (`x-litellm-model-id` is a hash) and an alert when an alias's
  deployment changes (a re-route inside LiteLLM): not built.
- **VictoriaLogs shows flotillad's lines as "missing _msg field":** its JSON message key is `msg`; the
  Alloy pipeline on the mini does not map it. Searching works by field (`msg:"..."`); the message column
  does not. Not fixed.
- **Steps 2 to 6:** after the switch.
