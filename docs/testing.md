# Testing and validation

flotilla needs three kinds of confidence:

1. **The code is right:** the test pyramid.
2. **The platform works on its own,** before any real fleet depends on it.
3. **Each fleet makes good decisions as it runs:** fleet validation.

Language models are not deterministic, so the tests below separate code that must be exact from
model behaviour that is measured.

## 1. The test pyramid

| Level | What | Examples | Where it runs |
|---|---|---|---|
| **Unit** (most tests) | one component, no network, no model | scheduler (triggers, budget, STOP, stage), spec validation, the ledger's status transitions, tool scope checks, gates, the agent loop driven by a **scripted fake model**, output scope checks, config loading | every push (`ci-fast`) |
| **Contract** | the agreements between parts | the spec JSON Schema (every spec in `fleets/` validates), the flotillad ↔ worker messages, the public API against its OpenAPI document, MCP tool schemas, each agent step's output schema | every push |
| **Security** | the threat model's controls | injection fixtures never produce an output; tools refuse out-of-scope requests; MCP has no way to approve; a local-only job refuses a cloud model; secrets never appear in logs or traces | every push |
| **Integration** | flotillad and a worker together, real SQLite | a run from trigger to ledger with a **stub LiteLLM** returning recorded responses and stub sources (fixture alerts, fixture Search Console rows); approval applies an output to a **stub GitHub** | every push to main and PR (`ci`) |
| **End to end** | the deployment itself | the compose stack (flotillad plus workers) in the dev Docker engine with stub LiteLLM, stub sources and stub GitHub; drive it through the CLI and the API; check metrics and logs come out | before every release |
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

- **`canary/heartbeat`** runs every 10 minutes and exercises every building block:
  - a source (synthetic facts) and a detector;
  - an agent step against a **real local model** with one read-only tool and a fixed question whose
    answer can be checked;
  - a gate, and a decision in the ledger;
  - a proposal that waits in the queue (it targets a no-op output);
  - metrics, logs and traces.
- **What it proves:**
  - the scheduler fires and the worker runs;
  - the model endpoint works with tool calls;
  - the ledger, approvals and telemetry work end to end;
  - the stale alert fires when it stops.
- **It keeps running in production** as the platform's own heartbeat. If the canary is stale or
  failing, the platform is broken, whatever the real fleets say.
- **Acceptance for phase 2** of the migration:
  - the canary runs green for a week;
  - every failure mode in [A run, end to end](architecture/run-lifecycle.md#failure-behaviour) is
    triggered once on purpose and behaves as documented.

## 3. Validating each fleet as it runs

| Check | What it answers | How |
|---|---|---|
| **Replay corpus** | does a change alter decisions on known cases? | recorded inputs with the expected decisions (the triage fleet already has 15 such gates, `signal-fleet/replays.sh`). Every change to a playbook, gate, detector or spec replays them; any changed decision fails unless accepted with a reason. |
| **Shadow comparison** | does the new platform decide like the old one? | during migration, the fleet runs in `shadow` on flotilla beside the existing implementation for N days; decisions are compared per subject; differences are explained before switching |
| **Stage ladder** | is it ready to act? | `shadow` → `propose` → `live`, each step on evidence: shadow agreement, then the operator's approval rate in `propose` |
| **Decision quality, live** | is it still making good decisions? | from the ledger: approval and rejection rate, the reasons for rejections, duplicates caught by gates, `gated` and `invalid_output` rates, outcomes (issues fixed, findings that improved a page). Each fleet declares the levels that should raise an alert. |
| **Drift** | did something change underneath? | the mix of decisions per week against the last weeks; model, playbook and spec versions are in every row, so a shift can be traced to a change |
| **Model and prompt evaluation** | is a different model or playbook better? | evaluated offline on the replay corpus (with eval-harness), never by switching production and watching |
| **Health** | is it running? | per job: freshness (last success), run success rate, duration, cost against budget; dashboards and alerts in the homelab stack |

Each fleet's spec states its **validation contract**:
- its replay corpus;
- its quality metrics and alert levels;
- its freshness;
- what evidence moves it up a stage.

A fleet without one cannot move past `shadow`.

## CI tiers

| Tier | Runs | Contains |
|---|---|---|
| `ci-fast` | every push and PR | lint (gofmt, go vet, ruff), unit, contract, security tests, spec schema validation, secrets scan |
| `ci` | PRs to main and main | `ci-fast` plus integration, the docs build (strict), vulnerability scans, image builds |
| release | before deploy | `ci` plus end to end in the dev Docker engine |
| private replay | on the host, before switching a fleet's code | the fleet's replay corpus with real recorded data (from the private eval repo) |

Until phase 1 brings code, the Makefile's gates say "no code yet" and pass. Phase 1 replaces them
with this.
