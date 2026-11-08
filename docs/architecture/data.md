# Data

## Three layers, kept apart

| Layer | Where | In git | Examples |
|---|---|---|---|
| **Spec**: what fleets are | `fleets/<fleet>/*.yaml`, `fleets/<fleet>/playbooks/*.md`, `schemas/` | yes (public) | jobs, triggers, steps, tools, gates, outputs, stages, budgets, playbooks |
| **Config**: this installation | `~/.config/flotilla/config.yaml` plus secrets files (mode 600) on the host | no | endpoints, repo maps, site lists, notification targets, key file paths |
| **State**: what happened | `~/.local/share/flotilla/` on the host (a Docker volume) | no | the control-plane database, fleet data, run artefacts |

The repo ships `config.example.yaml` with placeholder values. Nothing that identifies the
installation (host names, tailnet addresses, user paths, real repos) is in git.

## The control-plane database (SQLite)

One file, owned by flotillad. Workers never open it; they report through the API.

| Table | Holds |
|---|---|
| `specs` | each loaded spec version: fleet, job, content hash, loaded at, git commit |
| `runs` | run id, job, spec version, trigger, actor, start, end, status, cost, tokens, model(s), trace id |
| `decisions` | **the ledger** (below) |
| `proposals` | the outward action a decision wants: kind, target, payload reference, status, approver, approved at, result |
| `events` | an append-only audit log of every state change: who, what, when, from where (UI, CLI, MCP, job) |
| `budgets` | per fleet and job: limits and spend per day |
| `controls` | pause, STOP, stage per fleet and job, with who changed them |

## The ledger (`decisions`)

| Field | Meaning |
|---|---|
| `id`, `fleet`, `job`, `run_id` | where it came from |
| `subject` | what it is about: an alert fingerprint, an issue, a page URL, a day |
| keys | what makes a repeat recognisable: rows in `decision_keys(decision_id, kind, value)`, as many as the fleet's dedup needs, looked up in the order its spec declares |
| `decision` | the conclusion: file, dismiss, escalate, propose-fix, finding, brief-section … |
| `evidence` | references to what the tools returned: source, query, dataset version, time range |
| `model`, `playbook_version`, `spec_version` | what produced it |
| `cost_usd`, `tokens` | what it cost |
| `status` | `recorded`, `gated`, `pending`, `approved`, `rejected`, `done`, `superseded`, `incomplete`, `invalid_output`, `deferred` |
| `actor` | the job, the operator, or an agent (with session) for each status change |
| `outcome`, `outcome_at` | what happened afterwards, filled in by a later job |
| `trace_id` | the run's trace, to open the full investigation |

Rows are never deleted or rewritten. A status change is a new event, and the row carries the
current status. A recurrence of the same subject is an event on its decision, not a new decision.
Operator feedback on a decision is an event of kind `feedback`; an agent's feedback is `proposed`
until the operator confirms it.

**Notes** (`notes` table): short lessons with a scope (fleet, job, subject pattern), who proposed
and activated them, a review-by date and what they supersede. Only active notes reach a model; see
[ADR-0004](../adr/ADR-0004-job-runtime.md). History imported from the existing fleets (the triage fleet's `dispositions.tsv`
and `filed.tsv`) keeps its original timestamps and is marked `imported`.

## Fleet data

Analytical data a fleet collects stays in files the fleet owns, under the state directory:
- Parquet partitions for Search Console history;
- crawl snapshots;
- frozen corpora for replays.

The database stores references to it (dataset and version), never copies.

## Retention and backup

- **Kept forever:** the database and the ledger. It is small: one row per decision.
- **Fleet data:** each fleet's spec declares its retention (Search Console history: forever, since
  Google keeps only 16 months).
- **Run artefacts** (raw tool outputs kept for debugging): 30 days.
- **Backup:** the state directory joins the host's offsite backup. A restore is tested like the
  homelab's other restores.
