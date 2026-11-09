# Data

## Three layers, kept apart

| Layer | Where | In git | Examples |
| --- | --- | --- | --- |
| **Spec**: what fleets are | `fleets/<fleet>/*.yaml`, playbooks, the capability registry | yes (public) | jobs, triggers, capabilities, case kinds, stages, budgets, playbooks |
| **Config**: this installation | flotillad's config and the hosts' config, plus secrets files (mode 600), on the hosts; the hosts' allowlists in the installation's private config, reviewed like code | no (public repo) | model endpoints and aliases, projects and their recall sources, channels and providers, each host's allowed writes and autonomy ceiling |
| **State**: what happened | Docker volumes on the control-plane host; each host agent's store | no | the control-plane database, fleet data, recall indexes, run records |

The public repo ships examples with placeholder values. Nothing that identifies an installation is in
git.

## The control-plane database (SQLite)

One file, owned by flotillad. Workers and host agents never open it; they report through the API.

Built today: `runs`, `events`, `controls`, `spend`. The rest is the ADR-0005 and ADR-0006 design; v1
brings the case tables.

| Table | Holds |
| --- | --- |
| `specs` | each loaded spec version: fleet, job, content hash, loaded at, git commit |
| `runs` | run id, job, spec version, trigger, start, end, status, cost, tokens, models, trace id |
| `cases` | one row per problem: kind, typed target, project, status, holder, lease, urgency, opened and closed at |
| `case_keys` | `(case, kind, value)`: as many keys as the kind's dedup needs; one canonical key per open case |
| `case_events` | the timeline, append-only: detected, diagnosed, handed over, planned, approved, executed, checked, attached signals (`caused_by`, `recurrence_of`), feedback, outcomes; each with its actor, run and evidence references |
| `executions` | execution id, case, host, capability, parameters, plan hash, how approved, signed at, result (done, failed, unknown, precondition false), check result |
| `locks` | the per-target lock: target, case, taken at |
| `autonomy` | per write capability: level, who promoted or demoted it and why; per host: its ceiling |
| `notes` | short lessons with a scope; active only once the operator activates them |
| `nodes` | each host agent: identity, certificate serial and expiry, version, last contact, allowlist hash, revoked |
| `controls` | pause, STOP, stage, hold, freeze, with who changed them |
| `spend` | per fleet and job: spend and tokens per day |
| `events` | the platform's own audit log of every change: who, what, when, from where (UI, CLI, Slack, MCP, job) |

Nothing is deleted or rewritten. A status change is an event; the case row carries the current
status. The transition table decides which status changes are allowed.

**History from the existing fleets:** the triage fleet's ledgers (`dispositions.tsv`, `filed.tsv`)
are imported as cases with their keys, marked `imported`, keeping their timestamps; the mirror keeps
writing the TSVs while their readers use them ([RFC-0002](../rfc/RFC-0002-job-runtime.md) §9).

## Recall

| Origin | Where | Lookup |
| --- | --- | --- |
| Recorded by flotilla | the database above (cases, outcomes, feedback, active notes) | exact, by key; structured fields only |
| Plugged in | git sources refreshed into a volume, versioned by commit; HTTP and MCP sources read live | path maps, full-text and semantic indexes in a volume, one per trust tier; each chunk with its source, path, version and trust |

## Fleet data

What a fleet collects stays in files the fleet owns, under the state directory: Search Console
history (Parquet), crawl snapshots, the marketing fleet's post register, frozen corpora for replays.
The database stores references (dataset and version), never copies.

## On each host

A host agent keeps a small SQLite store: every execution id it has seen and its result (so a repeat
runs nothing), and its hash-chained audit log, collected off the host by the host's log collector;
flotillad keeps the log's latest chain head per node.

## Retention and backup

- **Kept forever:** the database (cases, executions, events). It is small.
- **Fleet data:** each fleet declares its retention.
- **Run records** (raw requests and replies for replay): as each fleet's evaluation needs; 30 days by
  default.
- **Backup:** the state volumes join the host's offsite backup; a restore is tested like the other
  restores. The CA's root key is not on any host (see [Deployment](deployment.md)).
