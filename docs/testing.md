# Testing and validation

flotilla needs three kinds of confidence:

1. **The code is right:** the test pyramid.
2. **The platform works on its own,** before any real fleet depends on it.
3. **Each fleet makes good decisions as it runs:** fleet validation.

Language models are not deterministic, so the tests below separate code that must be exact from
model behaviour that is measured.

## 1. The test pyramid

The target. **Today:** Go unit tests, the runtime's and the triage fleet's Python tests, and the
e2e stack (`deploy/e2e/run.sh`); the case, policy, host-agent and Slack tests come with what they
test.

| Level | What | Examples | Where it runs |
| --- | --- | --- | --- |
| **Unit** (most tests) | one component, no network, no model | triggers (budget, STOP, autonomy), spec and registry validation, the case transition table, case keys and dedup, policy decisions (autonomy, locks, limits, holds, taint), the guardrails, capability scope checks, the harness driven by a **scripted fake model**, the model client against a fake endpoint, config loading; in `flotilla-host`: signature, expiry, replay, allowlist and autonomy-cap checks, idempotency, the audit chain | every push (`ci-fast`) |
| **Contract** | the agreements between parts | the spec JSON Schema (every spec in `fleets/` validates), the flotillad ↔ worker messages, the public API against its OpenAPI document, MCP tool schemas, each harness step's output schema, the capability registry, the host protocol and the signed execution envelope, the model configuration (`FLOTILLA_MODELS`, shared fixtures in Go and Python), the model client against real endpoints (`runtime/contract_check.py`) | every push |
| **Security** | the threat model's controls | injection fixtures never lead to an executed write and taint caps them at propose; capabilities refuse out-of-scope requests; MCP has no way to approve; a local-only routine refuses a cloud model; secrets never appear in logs or traces; the host agent refuses forged, replayed, expired, out-of-allowlist and above-cap executions; a Slack approval from another user, for high risk or for a changed plan is refused | every push |
| **Integration** | flotillad and a worker together, real SQLite | a case from trigger to closed with a **stub model endpoint** returning recorded responses and stub sources (fixture alerts, fixture Search Console rows); an approval starts the execute run; repo writes go to a **stub GitHub**, host writes to a host agent running a no-op allowlist | every push to main and PR (`ci`) |
| **End to end** | the deployment itself | the compose stack (flotillad, workers, egress proxy) and a host agent in a container, in the dev Docker engine, with a stub model endpoint, stub sources, stub GitHub and a stub provider; drive it through the CLI and the API; check metrics, logs and traces come out (`deploy/e2e/run.sh`) | before every release |
| **Live smoke** | the deployed system on the mini | the canary fleet (below) runs on schedule; `flotilla status` is healthy; the deploy-verify check passes | after every deploy, then continuously |

**Rules:**

- **A bug gets a failing test first.**
- **No real model in the pyramid.** Model behaviour is measured in fleet validation (section 3),
  never asserted in unit tests.
- **Fixtures with real data stay private.** Recorded alerts and Search Console rows from production
  live in the private eval repo. The public repo's fixtures are synthetic.

## 2. Validating the platform on its own: the canary fleet

Before any real fleet moves over, the platform proves itself with a **canary fleet** shipped in
this repo. It is synthetic, deterministic and harmless:

- **`platform/heartbeat`** runs every 10 minutes:
  - the scheduler fires and the worker runs;
  - egress is enforced both ways: an allowed host answers, a host off the list is refused;
  - optionally one model call through the model client (`HEARTBEAT_MODEL_ALIAS`);
  - metrics, logs and traces arrive, and the stale alert fires when it stops.
- **Added with ADR-0005's v1:** a case with a planned no-op write that waits for approval, so cases,
  policy and approvals are exercised end to end.
- **It keeps running in production** as the platform's own heartbeat. If the canary is stale or
  failing, the platform is broken, whatever the real fleets say.
- **Acceptance for phase 2** of the migration:
  - the canary runs green for a week;
  - every failure mode in [A case, end to end](architecture/run-lifecycle.md#failure-behaviour) is
    triggered once on purpose and behaves as documented.

## 3. Validating each fleet as it runs

| Check | What it answers | How |
| --- | --- | --- |
| **Replay corpus** | does a change alter decisions on known cases? | recorded inputs with the expected decisions (the triage fleet already has 15 such gates, `signal-fleet/replays.sh`). Every change to a playbook, gate, detector or spec replays them; any changed decision fails unless accepted with a reason. |
| **Evaluation modes** | which proof does a change need? | four modes: code replay, exact replay, model replay and shadow comparison, with a table of which change needs which ([ADR-0004](adr/ADR-0004-job-runtime.md), RFC-0002 §8) |
| **Shadow comparison** | does the new platform decide like the old one? | during migration, the fleet runs in `shadow` on flotilla beside the existing implementation for N days; decisions are compared per subject; differences are explained before switching |
| **Routine autonomy** | is a routine ready to act? | `shadow` → `propose` → `auto`, each step on evidence: shadow agreement, then the operator's approval rate in `propose` |
| **Autonomy ladder** | may a write capability run without asking? | per capability, on every host: a gate of shadow plans the operator judged right, then approved executions that verified; promoted only by the operator, a failed or unknown result drops an `auto` capability to propose ([ADR-0005](adr/ADR-0005-agent-pipeline.md)) |
| **Decision quality, live** | is it still making good decisions? | from cases: approval and rejection rate, the reasons for rejections, duplicates caught by the triage fleet's code checks before filing, `gated` and `invalid_output` rates, outcomes (issues fixed, findings that improved a page). Each fleet declares the levels that should raise an alert. |
| **Drift** | did something change underneath? | the mix of decisions per week against the last weeks; model, playbook and spec versions are in every row, so a shift can be traced to a change |
| **Model and prompt evaluation** | is a different model or playbook better? | evaluated offline on the replay corpus (with eval-harness), never by switching production and watching |
| **Health** | is it running? | per routine: freshness (last success), run success rate, duration, cost against budget; dashboards and alerts in the homelab stack |

Each fleet's spec states its **validation contract**:

- its replay corpus;
- its quality metrics and alert levels;
- its freshness;
- what evidence moves it up a step of autonomy.

A fleet without one cannot move past `shadow`.

## CI tiers

The target. **Today:** `ci-fast` is lint (gofmt, go vet, Python compile) and the unit tests, on
every push; `make ci` adds the e2e stack, the Markdown lint and the strict docs build; images build
in their own workflow; the secrets scan is a pre-commit hook.

| Tier | Runs | Contains |
| --- | --- | --- |
| `ci-fast` | every push and PR | lint (gofmt, go vet, ruff), unit, contract, security tests, spec schema validation, secrets scan |
| `ci` | PRs to main and main | `ci-fast` plus integration, the docs build (strict), vulnerability scans, image builds |
| release | before deploy | `ci` plus end to end in the dev Docker engine |
| private replay | on the host, before switching a fleet's code | the fleet's replay corpus with real recorded data (from the private eval repo) |
