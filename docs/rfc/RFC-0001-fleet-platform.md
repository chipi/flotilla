# RFC-0001 — one fleet platform from the fleets we have

**Status:** Accepted → [ADR-0001](../adr/ADR-0001-platform-foundations.md). The operator decided
§10 on 2026-11-08. Drafted in agentic-ai-homelab and moved here as flotilla's first RFC.
**Date:** 2026-11-08
**Inputs:**
- [agent-platform-evaluation.md](https://github.com/chipi/agentic-ai-homelab/blob/main/docs/wip/agent-platform-evaluation.md) (agentic-ai-homelab);
- a read-only inventory of fleetd, the triage fleet, the bugfix fleet and the daily brief
  (2026-11-08; file and line citations kept in the session notes, summarised in §2);
- the operator's decisions of 2026-11-08 (§1).

## 1. Context and goals

The homelab already runs agents in four places, built at different times in different ways:
- the **triage fleet** (Fleet 2, live, every 10 minutes);
- the **bugfix fleet** (Fleet 1, a hand-run chain plus a dormant TypeScript MVP);
- the **daily brief** (07:00);
- **fleetd**, the supervisor that runs the first two.

New work (SEO for the operator's sites, the infra reviews done by hand today) would be a fifth and a
sixth. The operator decided, 2026-11-08:

- **Build our own "plain" platform**, named **flotilla**, as a new **public** repo. The goal is
  twofold: learn how agent systems work from the inside, and have something working. Hermes Agent,
  OpenClaw and others are inspiration for later, not dependencies.
- **Fleets are groups inside the platform:** `triage`, `bugfix`, `seo`, `ops` (the daily brief and
  operational reviews), and more later.
- **The platform is spec- and config-driven:** fleets and jobs are declared, not coded.
- **A small control plane** with its own small web UI (independent of the Hub), a CLI, and an MCP
  server from the start, so other agents can see and steer it.
- **A ledger of every decision** is a first-class piece.
- **Order:**
  1. rebuild what exists as the platform;
  2. add the SEO fleet;
  3. remove all fleet code from agentic-ai-homelab.

**Non-goals for this RFC:** the SEO fleet itself (RFC-0002), multi-user or multi-tenant use, any
cloud-hosted part, and adopting a third-party agent framework.

## 2. What exists today (inventory summary)

| | fleetd | triage fleet | bugfix fleet | daily brief |
|---|---|---|---|---|
| Language | Go, stdlib only, ~1,000 LOC + tests | Python, stdlib only, ~8,200 LOC | Go chain (in fleetd) + bash/Python leaves; TS MVP (~1,700 LOC) mostly unused | Node, no deps, ~190 LOC + Hub libs |
| Trigger | LaunchDaemon, KeepAlive; a ticker per fleet | fleetd, every 10 min | by hand (`make chain`); fleetd entry disabled | launchd 07:00 (not fleetd) |
| Prompt lives in | — | code (`triage.py`), hashed as `PROMPT_SHA` | `agents/*.md` and inline in Go | inline in code |
| Model | — | `fleet-triage-pro` (OpenRouter via LiteLLM) | OpenRouter **directly**, bypassing LiteLLM; Claude for review | `assistant` (X3, falls back to OpenRouter) |
| Tools for the LLM | — | a probe menu, up to 3 probes | a coding harness (pi) | none |
| Gates | — | before the LLM (classifier) and after it (intent, cleanup, dismiss, edge); rich dedup | repro-first test, delta suite | none (facts computed in code) |
| Output | — | GitHub issues and comments, GlitchTip and Grafana annotations, queue | branch, draft PR, review comment | Hub page, iPhone push |
| Stage | owns shadow, propose, live | reads `FLEETD_STAGE` | reads its own `chain.json` | none |
| State | `state.json` (daily spend) | `~/signal-fleet/`: `dispositions.tsv` (18 cols), `filed.tsv`, queue, dot-files | `~/.bugfix-fleet/` TSVs, and some output **inside the git checkout** | `www/briefing/` |
| Metrics | `fleetd_cycle`, `_seconds`, `_spend_day` | `signal_fleet_*` | `bugfix_fleet_*` (no service label) | `hub_briefing_*` |
| Alerts | `fleetd-cycle-failing`, `fleetd-silent` | `glitchtip-sync-failing` | `bugfix-mass-stuck` | `hub-briefing-missing` |

**Implemented more than once:**
- the LLM client: 6 ways;
- cost accounting: 3;
- Langfuse: 4;
- VictoriaMetrics push: 7 places;
- VictoriaLogs shipping: 3;
- the GitHub client: 5 (urllib, `gh`, Octokit, Node, `gh api`);
- env loading: 6, with drifting names (`SF_QUEUE` vs `SF_QUEUE_DIR`, `GRAFANA_ADMIN_USER` vs
  `GRAFANA_USER`);
- TSV ledgers: 5 writers, 3 separate readers;
- labels and stage: 3 sources of truth.

**Coupling that blocks a clean move:**
- `signal-fleet/deploy/deploy.sh` is the mini's deploy for the **whole** homelab checkout. It pulls,
  runs fleet replays as a gate, and rolls everything back on failure.
- The Hub reads the triage fleet's credentials file, ledgers and queue (`hub-sources.js`,
  `hub-status.js`, `gen.sh`).
- Real production payloads are committed:
  - `signal-fleet/reference*` (~4 MB, with tailnet names and IPs);
  - `bugfix-fleet/bakeoff/results-snapshots`.
- Hard-coded environment specifics in about 40 places: user paths, `homelab`, tailnet IPs and names,
  the GlitchTip org, `chipi/*` repos, `mobile_app_iphone`.
- fleetd runs as the operator's account, while ADR-0009 says agents on the mini run as `claude`.

## 3. Principles

1. **Files own the facts, code owns the calculations, the model investigates, humans decide.** (From
   the SEO brief; it describes what the triage fleet already does.)
2. **Declared, not coded.** A fleet is a spec. Code is a library of named building blocks the specs
   use. A new fleet needs new code only when it needs a new building block.
3. **Plain and inspectable.** No agent framework. Our own agent loop, a few hundred lines you can read
   end to end.
4. **One of everything:** one LLM client, one ledger, one GitHub client, one config loader, one
   metrics pusher.
5. **Gates are code.** What leaves the platform is decided by deterministic checks after the model,
   never by the model alone.
6. **Behaviour changes are reviewed and replayed.** A playbook, gate or spec change is a commit,
   tested against recorded past cases before it goes live. Nothing rewrites itself.
7. **An agent is never the operator.** Approvals of outward actions are operator-only, whoever
   (Claude, the X3 assistant, a future runtime) calls through the API or MCP. The ledger records who
   did what.
8. **Public repo, private state.** Specs, playbooks, schemas and code go in git. Environment config,
   secrets, the ledger, findings and fleet data live on the host.
9. **Local models first.** Each fleet declares its model policy (local-only, or cloud allowed with a
   budget). Every call goes through LiteLLM, and none bypasses it.
10. **Observable like everything else** (§6.2): metrics, logs, traces, LLM traces, errors, ops
    events, dashboards and alerts in the homelab's existing stack, with the same conventions as
    every other service, and a freshness metric plus stale alert per job.

## 4. Concepts

| Term | Meaning |
|---|---|
| **Fleet** | a group of jobs with one purpose and one owner: `triage`, `bugfix`, `seo`, `ops` |
| **Job** | one declared unit of work in a fleet: trigger, inputs, steps, output |
| **Run** | one execution of a job: start, end, status, cost, model, version of the spec |
| **Step** | a building block a job runs in order: `collect`, `detect`, `agent`, `gate`, `output` |
| **Building block** | named code the specs refer to: a source, a detector, a tool, a gate, an output, a notifier |
| **Playbook** | the system prompt and rules for an agent step, a Markdown file in git |
| **Decision** | what a job concluded about one subject (an alert, a page, an issue), with evidence. One ledger row. |
| **Proposal** | a decision that wants an outward action (file an issue, open a PR, change a page); it waits for approval when the stage requires it |
| **Stage** | the trust level of a job: `shadow` (decide, record, do nothing), `propose` (act only on approval, or within the job's declared allowance), `live` (act within declared limits) |
| **Model policy** | which model aliases a job may use, and whether cloud is allowed |

## 5. The spec

Three layers, kept apart:

| Layer | Where | Example |
|---|---|---|
| **Spec** (what a fleet is) | the repo, `fleets/<fleet>/*.yaml`, plus playbooks next to them | jobs, triggers, steps, tools, gates, outputs, stage, budget |
| **Config** (this installation) | on the host, `~/.config/flotilla/config.yaml` plus secrets files (mode 600) | LiteLLM URL and keys, Grafana and GlitchTip URLs, GitHub App, repo maps, site lists, notification targets |
| **State** (what happened) | on the host, `~/.local/share/flotilla/` | the control-plane database, fleet data (for example Parquet), run artefacts |

A job spec, sketched with the triage fleet as it works today:

```yaml
fleet: triage
job: signals
trigger: { every: 10m }
steps:
  - collect: [grafana.alerts, glitchtip.issues]
  - gate:    [operational_classifier]            # before the model
  - agent:
      playbook: playbooks/triage.md
      model:    { alias: fleet-triage-pro, cloud: allowed }
      tools:    [obs.metrics, obs.logs, obs.traces]
      max_steps: 4
      output_schema: schemas/triage-decision.json
  - gate:    [intent, cleanup, dismiss, edge, dedup]
  - output:  { kind: github_issue, repo_map: config.repo_map, labels: triage }
stage: propose
budget: { usd_per_day: 2, timeout: 20m }
```

The same format carries the daily brief (`ops/daily-brief`: collect from five sources, an agent step
with no tools, outputs as a page and a push) and, later, the SEO jobs. A JSON Schema validates every
spec at load and in CI.

## 6. Architecture

```
            UI (small web app)      CLI (flotilla …)      MCP server
                    \                    |                   /
                     +------ control-plane API (HTTP) ------+
                                         |
   +-------------------------- flotillad (control plane) --------------------------+
   | registry: load + validate specs   scheduler: triggers, budgets, STOP, stage    |
   | runner: start a run as a process  ledger + decisions   approval queue          |
   | run history                       health: metrics, logs, freshness            |
   +------------------------------------------+-------------------------------------+
                                              |  starts `flotilla-run <fleet>/<job>`
                                              v
   +---------------------------- job runtime (a library) ----------------------------+
   | sources (collect)  detectors  agent loop  tools (read-only, scoped)  gates      |
   | outputs (GitHub, page, push, finding)  LLM client (via LiteLLM)  ledger client  |
   +----------------------------------------------------------------------------------+
        |              |               |                 |                |
     LiteLLM     Grafana/VM/VL    GlitchTip, GitHub    Langfuse     data files (Parquet)
```

**Control plane, `flotillad`.** It grows from fleetd, which already owns scheduling, budgets, stage,
STOP, process-group timeouts and metrics.
- **Registry:** reads the specs and validates them against the schema.
- **Scheduler:** fires triggers (`every`, `cron`, events: a Grafana alert, a deploy, a manual run).
- **Runner:** starts each run as a separate process (`flotilla-run`). A crashed or hung job cannot
  take the control plane down, and job code can be any language, as fleetd allows today.
- **Owns:** the run history, the ledger, the approval queue and the budgets.
- **Serves:** the API that the UI, the CLI and MCP all use.

**Job runtime.** The library every job uses: one LLM client, one agent loop, one tool registry, one
ledger client, one set of outputs. A run gets its spec, config and identity from the control plane
and reports back to it.

**Agent loop.** The model is offered the job's declared tools. Each call goes through code that
checks scope (which site, which repo, which time range) and caps the result size. The loop stops at
`max_steps`, or when the model answers in the job's output schema. A malformed answer gets one retry.
Every step is traced to Langfuse with the spec and playbook versions.

**Interfaces:**
- **CLI:** `flotilla status`, `flotilla run <job> [--dry-run]`, `flotilla pause <fleet>`,
  `flotilla ledger <query>`, `flotilla approve <id>` (operator only).
- **UI:** fleets and jobs, run history, the ledger, the approval queue (approve or reject with a
  reason), and controls (pause, STOP, stage, budget).
- **MCP:**
  - read tools (status, runs, ledger, findings, queue);
  - action tools (dry-run or trigger a job, pause, add a note, propose).
  - It cannot approve. It runs over the tailnet, with an identity per caller.

**State store.** SQLite (one file, transactional, easy to back up) for:
- runs;
- decisions (the ledger);
- proposals and approvals;
- budgets and spend;
- the registry's loaded spec versions.

Fleet data that is analytical by nature (the SEO fleet's Search Console history) stays in Parquet
files. The control plane stores references to it, not copies.

### 6.1 The ledger

One table of decisions, the same shape for every fleet:

| Field | Meaning |
|---|---|
| `id`, `fleet`, `job`, `run_id` | where it came from |
| `subject` | what it is about: an alert fingerprint, a GitHub issue, a page URL, a day |
| `dedup_key` | the key that makes a repeat recognisable (fingerprint, group key, finding key) |
| `decision` | the job's conclusion (file, dismiss, escalate, propose-fix, finding, brief-section …) |
| `evidence` | references to what the tools returned (query, dataset version, time range) |
| `model`, `playbook_version`, `spec_version` | what produced it |
| `cost_usd`, `tokens` | what it cost |
| `proposal` | the outward action wanted, if any |
| `status` | `recorded`, `pending`, `approved`, `rejected`, `done`, `superseded` |
| `actor` | who moved it: the job, the operator, or an agent through MCP (with session id) |
| `outcome` | what happened afterwards (an issue closed, a PR merged, a measured effect), filled in later |

### 6.2 Observability: one of our services, like the others

flotilla reports into the homelab's existing stack exactly like every other platform here. It
brings no stack of its own; it uses the same conventions so the existing dashboards, alerts and
skills see it without special cases.

| Signal | What flotilla emits | Where it lands |
|---|---|---|
| **Metrics** | one family per platform with stable labels (`fleet`, `job`, `stage`, `outcome`, `model`, plus `service="flotilla"`, `environment`, `instance`). It replaces `fleetd_*`, `signal_fleet_*`, `bugfix_fleet_*` and `hub_briefing_*`. Covers runs by outcome, run duration, steps and tool calls, gate verdicts, decisions by kind, queue depth and oldest pending age, spend and tokens per fleet and model, budget left, and `flotilla_job_last_success_timestamp`. | VictoriaMetrics |
| **Logs** | structured JSON lines with `service`, `fleet`, `job`, `run_id`, `trace_id`, `event`; one line per run start and end, per decision, per approval; no secrets, no full prompts | VictoriaLogs (through Alloy, like other services) |
| **Traces** | OpenTelemetry spans: run → step → tool call / LLM call / gate / output, with the run's `trace_id` in its logs and ledger rows | VictoriaTraces (OTLP to Alloy) |
| **LLM traces** | every model call with prompt, answer, tokens, cost, model, playbook and spec version | Langfuse, through LiteLLM's callback plus the run's trace id |
| **Errors** | exceptions in flotillad and the job runtime, with release `sha-<7>` and environment | GlitchTip (Sentry SDK), its own project |
| **Ops events** | deploys and releases of flotilla, and spec changes, as `ops_event/v1` | VictoriaLogs, so `deploy-verify` and the deploy stand-down in the alert rules see them |
| **Dashboards** | a `flotilla` folder: platform overview (all fleets), one dashboard per fleet, the ledger and approval view, cost | Grafana, provisioned from the flotilla repo |
| **Alerts** | per job: stale (no success within its declared freshness), failing (N failed runs), budget exhausted, queue too old. Per platform: flotillad down, scheduler silent, state store not backed up. | Grafana alert rules, provisioned and drift-checked like `rules.yaml`, with runbook lines |

The ledger is not an observability signal. It is the platform's record of decisions, and the
signals above point into it (`run_id`, `trace_id`).

The existing skills learn flotilla as a project:
- `o11y-review` gets a `flotilla` profile;
- `deploy-verify` checks it after a release;
- `triage-fleet-review` reads its ledger through the API instead of TSV files over ssh.

The Hub's fleet cards read metrics and the API, never flotilla's files.

The triage fleet's `dispositions.tsv` and `filed.tsv` map into this table; they are imported as
history at migration. Prior decisions are part of every agent step's context, so the operator's
rejections teach the next run without the model rewriting itself.

## 7. Language

There are two languages, split at the process boundary the control plane already uses:

- **Control plane in Go.** It grows from fleetd: stdlib only, a single binary, already tested and
  deployed. The UI is served by the same binary (server-rendered pages plus a little JavaScript; no
  separate build). The MCP server is also part of it.
- **Job runtime in Python.** The triage fleet (8,200 LOC of the richest logic) is Python. The SEO
  fleet's data work (Parquet, DuckDB) is most natural in Python. A plain agent loop is short in any
  language.
- **The bugfix chain's leaves and the brief** move to the Python runtime when their turn comes. The
  TypeScript MVP is retired, not ported.

The alternative, everything in Python, means rewriting fleetd. The other alternative, everything in
Go, means rewriting the triage fleet. Neither buys enough to justify the rewrite. **Decided
2026-11-08: Go control plane, Python job runtime** (§10).

## 8. Security

- **Identities:** the operator (UI, CLI on the mini, signed in); jobs (one identity per fleet); agents
  through MCP (one per caller). The ledger records the identity on every change.
- **Approvals:**
  - Approving an outward action is operator-only: in the UI or the CLI, never through MCP.
  - What a job may do without approval at each stage is declared in its spec and enforced by the
    runner, not by the job.
- **Tools** are read-only unless a spec declares an output. Outputs (GitHub, push, files) are the only
  write paths, and each checks scope (repo map, site list).
- **Secrets:**
  - Mode-600 files on the host, named in config, never in the repo.
  - The secrets scan runs before every push.
  - Each fleet gets its own LiteLLM key and budget.
- **Untrusted input** (alert text, issue bodies, web pages) reaches the model as data, never as
  instructions to the runner. Gates check outputs against evidence.
- **Runs as `claude`** on the mini (ADR-0009). Today fleetd runs as the operator; the move fixes that.
  *Superseded by ADR-0002: non-root containers in the production engine, deployed by the operator
  account.*
- **Network:** API, UI and MCP listen on the tailnet only (ACL implicit deny).
- **Public repo hygiene:**
  - No production payloads.
  - The reference corpora that hold real data move to the private `fleet-eval-data` repo.
  - Specs and examples use placeholder hosts.

## 9. Migration plan (step 1)

Each phase leaves everything working. The triage fleet never stops.

0. **Untangle the deploy.** Give agentic-ai-homelab its own mini deploy script, out of
   `signal-fleet/deploy/`, and keep the fleet replays as a separate gate. Move the Hub's reads of
   fleet state behind something stable (a metrics query, or later the flotilla API).
1. **Create `flotilla`** (public, operator's go).
   - Copy fleetd, signal-fleet and bugfix-fleet with their git history.
   - Move the production corpora to the private eval repo.
   - Turn hard-coded specifics into config.
   - Add CI (tests, spec schema, secrets scan).
2. **flotillad**, observable from its first deploy (§6.2: metrics, logs, traces, GlitchTip,
   ops events, its dashboard and alerts, with the old fleetd alerts kept until the switch). It is
   fleetd plus the registry, the SQLite store, the ledger, the API and the CLI. It
   runs the triage fleet unchanged through a spec that wraps today's command (`steps: [{ run:
   "python3 orchestrator.py --cycle …" }]`). Shadow-compare it against the old fleetd for a few days,
   then switch.
3. **Job runtime and the triage fleet onto it:**
   - one LLM client;
   - the ledger client writing decisions, with the TSV history imported;
   - the playbook moved out of `triage.py` into a file;
   - the gates and dedup kept as they are, as building blocks.

   Replay-proven against the recorded cases before each switch.
4. **The ops fleet:**
   - the daily brief as `ops/daily-brief`, with its prompt in a playbook;
   - its model policy decided: keep or drop the cloud fallback;
   - the daily Postgres review as `ops/postgres-review`, replacing the scheduled prompt.
5. **UI and MCP:** the approval queue, run history and ledger views; MCP read tools first, then
   action tools.
6. **The bugfix fleet** onto the runtime: its chain becomes a job with the repro-first and delta
   gates as building blocks. Its leaves call LiteLLM, not OpenRouter directly.
7. **Remove the fleet code from agentic-ai-homelab:**
   - fleetd, signal-fleet, bugfix-fleet, the briefing job;
   - the fleet docs (RFC-0002 to RFC-0005, ADR-0008, the guides), moved;
   - redirects left in the docs.

   Step 2 (RFC-0002, the SEO fleet) starts once phases 2–3 are done; it needs the runtime, not the UI.

## 10. Decisions (operator, 2026-11-08)

1. **Language split:** Go control plane plus a Python job runtime (§7). **Accepted.**
2. **Where flotillad runs:** the mini (64 GB once fitted), as the `claude` account. **Accepted.**
3. **The UI:** served by the Go binary, with no separate frontend build. **Accepted.**
4. **The daily brief:** **local models only**; the cloud fallback is dropped.
5. **The reference corpora with production payloads:** **moved to the private `fleet-eval-data`
   repo**, not into flotilla. agentic-ai-homelab's public history is **not scrubbed** (operator, 2026-11-08: "These are not
   super sensitive information").
6. **License:** **MIT.**
7. **Docs site:** like every other project here, MkDocs Material built strict and published to
   GitHub Pages.

Recorded as [ADR-0001](../adr/ADR-0001-platform-foundations.md).

## 11. Not covered

- Detailed specs per job: the triage fleet's gates, the bugfix chain's states.
- The approval UX in detail.
- Event triggers beyond the existing ones: which events, delivered how (Grafana webhook, polling).
- Concurrency: two jobs touching the same subject.
- How the X3 assistant or Claude sessions will use MCP day to day.
- Backups of the state store (it should join the mini's offsite backup).
- Cost of the move in hours.
- Whether the bugfix TypeScript MVP holds anything worth keeping (the inventory says the chain does
  not use it).
