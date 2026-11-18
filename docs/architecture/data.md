# Data

## Three layers, kept apart

| Layer | Where | In git | Examples |
| --- | --- | --- | --- |
| **Spec**: what fleets are | `fleets/<fleet>/*.yaml`, playbooks, skills, the registry of tools and actions | yes (public) | routines, triggers, tools and actions, case kinds, autonomy, budgets, playbooks |
| **Config**: this installation | flotillad's config and the hosts' config, plus secrets files (mode 600), on the hosts; the hosts' allowlists in the installation's private config, reviewed like code | no (public repo) | model endpoints and aliases, projects and their knowledge sources, channels and providers, each host's allowed writes and autonomy ceiling |
| **State**: what happened | Docker volumes on the control-plane host; each host agent's store | no | the control-plane database, fleet data, knowledge indexes, run records |

The public repo ships examples with placeholder values. Nothing that identifies an installation is in
git.

## The control-plane database (SQLite)

One file, owned by flotillad. Workers and host agents never open it; they report through the API.

Every table below is built (`internal/store`, schema step 29 on 2026-11-18).

| Table | Holds |
| --- | --- |
| `runs` | run id, routine, the version that ran it, the spec's content hash (`spec_hash`: there is no separate specs table), trigger, start, end, status, cost, tokens, models, trace id |
| `routine_versions` | each version of each routine ([ADR-0020](../adr/ADR-0020-routine-versions-stations-harnesses.md)): number, state (`live`, `shadow`, `previous`, `retired`), the hash recorded when it went live and a manifest of its files (so a freeze failure names them), when it was deployed and went live, who moved it, when and why, whether it was rolled back, the autonomy chosen at its promote and what that promote recorded (the acceptance in force and the evidence given, JSON). At most one `live` and one `previous` per routine, by partial unique index |
| `cases` | one row per problem: kind, typed target, project, status, holder, lease, urgency, opened and closed at |
| `case_keys` | `(case, kind, value)`: as many keys as the kind's dedup needs; one canonical key per open case |
| `case_events` | the timeline, append-only: detected, diagnosed, handed over, planned, approved, executed, checked, attached signals (`caused_by`, `recurrence_of`), feedback, outcomes; each with its actor, run and evidence references |
| `host_executions` | execution id, case, host, action, parameters, plan hash, how approved, signed at, result (done, failed, unknown, precondition false), check result |
| `target_locks` | the per-target lock: target, case, taken at |
| `plans` | each plan a run proposed for a case: action, parameters, plan hash, the policy's outcome and reasons, the autonomy it ran at and what it would have done above it, when it was decided, the version that made it, and when and by whom a rollback marked it (a marked plan cannot be approved) |
| `holds` | a hold on a scope until a time: who set it and why, and when and by whom it was lifted (a deploy's stand-down is one) |
| `case_feedback` | an operator's verdict on a case's diagnosis (right or wrong), the evidence toward autonomy |
| `notes` | short lessons with a scope; active only once the operator activates them |
| `host_enrolments` | single-use enrolment tokens (their SHA-256), for which host, who made them, expiry |
| `hosts` | each host agent: identity, the current certificate's SHA-256 (the only one accepted) and expiry, version, last contact, allowlist hash, revoked |
| `controls` | pause, STOP, autonomy (per routine and per action), freeze, with who changed them |
| `spend` | per fleet and routine: spend and tokens per day |
| `events` | the platform's own audit log of every change: who, what, when, from where (UI, CLI, Slack, MCP, routine) |
| `run_calls` | each model call of a run, and each step its harness took between calls, as the run wrote them to its calls file (`FLOTILLA_CALLS_FILE`; the worker returns the first 5000 lines, and flotillad adds one `harness` step row saying how many more there were): alias, model, tokens, cost, latency, error, `finish_reason` (`length` is a reply cut at the token cap; `""` when unknown), `step` (the turn within a harness step, 1-based, null when not given) and `decision_key` (what the call served; triage: the signal's fingerprint), `role` (who made the call: `triage`; for a bugfix run `verdict`, `coder`, `reviewer` or `advisor`; `hook` on a step row for a harness hook's effect, ADR-0018; `""` when unknown), `episode` (an id grouping one agent session's calls within the run; triage: the decision's fingerprint, bugfix: the pi session's id), `episode_seq` (the episode's order within the run, 1-based, null when not given; on a step row its position among the run's steps), `tools` (a JSON array of what the model called in that turn, `[{"name":"bash","arg":"grep -n foo src/x.ts","ok":true}]`: `arg` is the call's main argument, redacted and cut at 160 characters; `ok` is false when the tool's result was an error and absent when unknown, as in triage, whose probe has not run when the call is recorded; older rows hold the names only, `["bash","read"]`, and `store.ParseTools` reads both; `""` when none) and `kind` (`call`, or `step` for a harness step: a verdict, a round's gate outcome, an advisor's pin, a hand-off, with no alias, model, tokens or cost, its text in `detail` and its `role` the one it concerns, or `gates` / `harness`). **Sums and counts of calls, tokens or cost skip `kind = 'step'` rows** (`CallsSince` does; a step has no tokens or cost, but it is not a call). A bugfix call's `cost_usd` is its share of its episode's gateway bill (the key's spend over the pi episode, by the turn's input + output tokens, `cost_source` `gateway-share`), null with `cost_source` `""` when the episode's bill could not be measured, never pi's catalog price |
| `run_loaded` | what each run read into its model (playbook, skills, notes, files) and how far each item is trusted |
| `ledger_entries`, `ledger_filings`, `ledger_keys` | the fleets' ledger (RFC-0002 §9 step 5): triage's decisions, the issues it filed and their dedup keys |
| `users`, `sessions` | the UI's and CLI's people: name, role, token hash; sign-in sessions with their CSRF token |
| `slack_asks` | each plan posted to Slack: its single-use nonce, case and plan hash, and the answer |
| `briefs`, `brief_marks` | the morning brief per day, and the operator's marks on its items |
| `eval_results` | what an eval measured for a routine, prompt (`prompt_sha`) and model: metric, value, cases (`n`) and runs it averaged (`repeats`), where it came from, who recorded it and when. Appended, never edited; an operator or admin records one (`flotilla eval record`), an agent cannot; the newest per metric is current |

Nothing is deleted or rewritten. A status change is an event; the case row carries the current
status. The transition table decides which status changes are allowed.

**Every record says which version made it.** `runs`, `run_calls`, `run_loaded`, `plans`,
`ledger_entries` and `ledger_filings` carry a `version` column (1 for what came before versions; a plan
or a ledger row takes its run's), and a `case_events` row carries the acting routine's live version
(0 for an operator's or policy's event). A version's state changes are `events` rows
(`version.deploy`, `version.update`, `version.promote`, `version.rollback`, `version.retire`, subject
`<routine>@v<n>`).

**History from the existing fleets:** the triage fleet's ledgers (`dispositions.tsv`, `filed.tsv`)
are imported as cases with their keys, marked `imported`, keeping their timestamps; the mirror keeps
writing the TSVs while their readers use them ([RFC-0002](../rfc/RFC-0002-job-runtime.md) §9). A
triage decision's row in the store also carries `turns` (model calls made for it), `quirks`
(comma-joined harness quirk names, `""` for none) and `cap_hits` (turns cut at the token cap); the TSV
keeps its columns, and a reader takes columns by name, so older readers ignore the extra keys.

## Memory and knowledge

| | Where | Lookup |
| --- | --- | --- |
| **Memory**: recorded by flotilla | the database above (cases, outcomes, feedback, active notes) | exact, by key; structured fields only |
| **Knowledge**: plugged in | git sources refreshed into a volume, versioned by commit; HTTP and MCP sources read live | path maps, full-text and semantic indexes in a volume, one per trust tier; each chunk with its source, path, version and trust |

## Fleet data

What a fleet collects stays in files the fleet owns, under the state directory: Search Console
history (Parquet), crawl snapshots, the marketing fleet's post register, frozen corpora for replays.
The database stores references (dataset and version), never copies.

## On each host

A host agent keeps a small SQLite store: every execution id it has seen and its result (so a repeat
runs nothing), and its hash-chained audit log, collected off the host by the host's log collector;
flotillad keeps the log's latest chain head per host.

## Retention and backup

- **Kept forever:** the database (cases, executions, events). It is small.
- **Fleet data:** each fleet declares its retention.
- **Run records** (raw requests and replies for replay): as each fleet's evaluation needs; 30 days by
  default.
- **Backup:** the state volumes join the host's offsite backup; a restore is tested like the other
  restores. The CA's root key is not on any host (see [Deployment](deployment.md)).
