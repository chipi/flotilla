# Components

Two programs, split at a process boundary:
- **flotillad**, the control plane, in Go;
- **the job runtime**, in Python.

flotillad never runs fleet code inside itself, and the job runtime never decides what it is allowed
to do.

## flotillad (Go)

| Component | Responsibility |
|---|---|
| **Registry** | loads `fleets/<fleet>/*.yaml`, validates them against the JSON Schema, keeps the loaded version of each spec, and refuses a spec that does not validate. Specs are reloaded on deploy, never edited at runtime. |
| **Scheduler** | fires triggers: `every`, `cron`, events (a Grafana alert webhook, a deploy event, a manual run). It checks the job's STOP flag, pause state and daily budget before every run. Grown from fleetd. |
| **Runner** | asks the fleet's worker to start a run, with the job spec, a run id, a trace id and the job's identity. It enforces the timeout, and records the start, end, status and cost. |
| **Ledger** | the append-only table of decisions (see [Data](data.md)). Only the runner (on a job's behalf) and the operator write to it. Every change records its actor. |
| **Approval queue** | proposals waiting for the operator. It applies an approved proposal by asking the fleet's worker to run the output, so the control plane holds no fleet credentials. |
| **Budgets and stages** | per job and per fleet: spend per day, runs per day, current stage. What a stage allows is enforced here, not by the job. |
| **API** | HTTP+JSON on the tailnet. One API behind every interface. |
| **CLI** | `flotilla status / run / dry-run / pause / stop / stage / ledger / approve / reject`. The same binary. |
| **UI** | server-rendered pages from the same binary: fleets and jobs, runs, the ledger, the approval queue, controls. No separate frontend build. |
| **MCP server** | read tools (status, runs, ledger, findings, queue) and action tools (dry-run, trigger, pause, note, propose). It has no approve tool. Each caller has its own identity. |
| **Telemetry** | its own metrics, structured logs, OpenTelemetry traces, and GlitchTip errors. |

## The job runtime (Python)

A library, plus the `flotilla-run` entry point that a fleet worker executes for each run.

| Component | Responsibility |
|---|---|
| **Sources** | collect facts (alerts, errors, metrics, Search Console rows, crawl snapshots) into the run's working data. No model involved. |
| **Detectors** | deterministic checks that turn facts into candidates (a regression, an opportunity, an alert worth triage). |
| **Agent loop** | for each candidate: the playbook as system prompt, the evidence and prior decisions as context, the job's declared tools offered to the model. It answers each tool call through the tool registry. It stops at `max_steps` or on an answer that matches the output schema; a malformed answer gets one retry. |
| **Tool registry** | the read-only functions a model may call. Each checks its scope (site, repo, time range, size cap) and returns provenance with the data. |
| **Gates** | deterministic checks on the model's answer: evidence cited and real, not a duplicate, in scope, plus the fleet's own gates. A gate failure records the decision as `gated`, not dropped silently. |
| **Outputs** | the only write paths: a GitHub issue, comment, PR or label; a finding file; a page; a push. Each checks scope against config and runs only when the stage allows it or after approval. |
| **LLM client** | one client for every call, to any OpenAI-compatible endpoint (LiteLLM is the reference deployment), under the job's model policy. It records model, tokens, cost and latency, and traces every call ([RFC-0002](../rfc/RFC-0002-job-runtime.md) §3). |
| **Ledger client** | reports decisions to flotillad. It never writes the database directly. |
| **Config loader** | reads `~/.config/flotilla/config.yaml` and the secrets files it names. One set of names, no drift. |

## Fleet workers

A **fleet worker** is the job runtime packaged with what one fleet needs:
- its building blocks;
- its secrets, and only its own;
- its toolchain (for the bugfix fleet: git, the language toolchains, the coding harness).

flotillad talks to each worker over a local API. The deployment view is in
[Deployment](deployment.md).

## Interfaces between the two

| From → to | What | Contract |
|---|---|---|
| flotillad → worker | start run (job id, spec version, run id, trace id, deadline), cancel run, apply approved output | versioned JSON schema |
| worker → flotillad | run started, decision, proposal, run ended (status, cost, tokens) | versioned JSON schema |
| any client → flotillad | the public API (CLI, UI, MCP, the Hub) | OpenAPI document, versioned |
