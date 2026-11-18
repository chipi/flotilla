# Triage fleet (Fleet 2) — how it works and why

This page was brought up to date on 2026-11-17. Its fleetd-era text, from the installation's ops
repo, is in git history. The example fleets:
[fleets/README.md](https://github.com/chipi/flotilla/blob/main/fleets/README.md).

This guide explains the triage fleet (`signal-fleet`, Fleet 2): its parts, one signal's path through
it, the two ledgers that hold its memory, and the reasoning behind each rule. It is the "why" for
the next agent who has to change it.

- **How to run, deploy, roll back and improve it:** [Triage fleet runbook](triage-runbook.md).
- **The full design history:**
  [RFC-0003 (origin)](../history/origin/RFC-0003-signal-to-action-fleet.md) and
  [`SIGNALS.md`](https://github.com/chipi/flotilla/blob/main/signal-fleet/SIGNALS.md).
- **How this fleet fits with the others:** [Fleet
  architecture](../history/origin/fleet-architecture.md) (history) and
  [RFC-0004](../rfc/RFC-0004-agent-pipeline.md).

Where it stands (2026-11-17): the live routine `triage/cycle` runs on flotilla at autonomy
`propose`. It files, comments on and reopens GitHub issues in the repos the installation maps its
projects to, and mirrors the operator's GitHub closes onto the GlitchTip issues it filed (the sync,
below). It went live on flotilla on 2026-11-11 and has run on flotilla's own images since
2026-11-14. Until 2026-11-16 a separate supervisor, fleetd, ran the same cycle from a git checkout
on the host. It is retired, and so is its LaunchDaemon.

## What it is, and what it is not

The fleet watches production signals (Grafana alerts and GlitchTip errors) and decides, for each
one, whether a human or another fleet needs to act. The result is a GitHub issue, a comment on an
existing issue, or a recorded decision to do nothing.

It **never** touches production, never changes code, and never closes a GitHub issue. Closing is the
operator's call (fleet-architecture invariant 6); the sync only copies that call onto GlitchTip.
Fixing belongs to Fleet 1 (bug-fix) and Fleet 3 (remediation).

Two more limits hold by construction:

- **Dev is never filed.** GlitchTip projects whose slug ends in `-dev`, and flotilla's own
  project, are left out of the query (`sources._left_out`). A dev build runs unmerged code; its
  errors belong to whoever works on that branch.
- **Gates are code.** What leaves the platform is decided by deterministic checks after the model
  (section 3 below). Filing uses the fleet's GitHub token, held by the triage worker alone.

## The moving parts

```text
flotillad (Go, one binary: scheduler, ledger store, API, UI, CLI)
  └─ every 10 min: routine triage/cycle (fleets/triage/cycle.yaml) on worker-triage
       runs: python3 orchestrator.py --cycle --limit 10 --no-dry-run
       cwd:  signal-fleet/mvp (/opt/flotilla/signal-fleet/mvp in the worker image)
       env:  the worker's secrets file + FLEETD_STAGE (the routine's autonomy) + FLEETD_CYCLE_ID
       │
       ├─ 1. Grafana pass    firing, non-meta alerts        (sources.firing_alerts)
       ├─ 2. GlitchTip pass  up to 100 unresolved errors read, at most 10 triaged
       │       each signal → triage → act → file (below)
       ├─ 3. triager-down    if the LLM is down: ONE aggregate issue, never one per signal
       ├─ 4. inbox push      operator-inbox metrics, and the published export/inbox.json
       │                     (fleet_inbox/v1; "needs you" counts open escalations only)
       ├─ 5. stale pass      once per UTC day: nudge open issues whose signal went quiet
       ├─ 6. GitHub → GlitchTip sync (SF_GLITCHTIP_SYNC=1): mirror the operator's closes
       └─ 7. spend file      cycle cost → flotillad's daily budget for the routine
```

| Part | Where | Role |
| --- | --- | --- |
| `flotillad` | [`cmd/flotillad`](https://github.com/chipi/flotilla/tree/main/cmd/flotillad), a container | runs the routine on its interval. Owns the autonomy level, the timeout, the budget, pause and STOP, the ledger store, and the metrics. |
| `worker-triage` | [`deploy/docker/worker-triage.Dockerfile`](https://github.com/chipi/flotilla/blob/main/deploy/docker/worker-triage.Dockerfile), a container | holds the fleet's secrets and egress; runs the cycle as a process. No other routine uses its access. |
| the routine | [`fleets/triage/cycle.yaml`](https://github.com/chipi/flotilla/blob/main/fleets/triage/cycle.yaml) | the schedule, timeout, autonomy, daily budget, freshness, class-hint rules and harness settings |
| cycle code | [`signal-fleet/mvp/`](https://github.com/chipi/flotilla/tree/main/signal-fleet/mvp), baked into the worker image | `orchestrator.py` (cycle), `sources.py` (read Grafana/GlitchTip), `correlate.py` and `probes.py` (probe evidence: logs, metrics, traces), `triage.py` (decide), `guards.py`, `actions.py` (dispositions ledger), `filing.py` (GitHub + filed ledger), `glitchtip_sync.py` (GitHub → GlitchTip), `stale.py`, `inbox.py`, `observ.py` (metrics, Langfuse), `scrub.py`, `installation.py` |
| runtime | [`runtime/`](https://github.com/chipi/flotilla/tree/main/runtime) | the model client, the harness (the probe loop), skills, the ledger client |
| playbook and hints | [`fleets/triage/playbooks/triage.md`](https://github.com/chipi/flotilla/blob/main/fleets/triage/playbooks/triage.md), [`fleets/triage/skills/`](https://github.com/chipi/flotilla/tree/main/fleets/triage/skills), [`harness-default.json`](https://github.com/chipi/flotilla/blob/main/fleets/triage/harness-default.json) | the triager's brief, the class hints, the harness settings for a run outside flotillad |
| installation facts | `triage/installation.json` in the fleet folder | which repo each project files into, and the rest (see the runbook). flotilla ships an example; an installation's real file is in its private fleets repo, mounted over `/opt/flotilla/fleets`. |
| state | a Docker volume, `/home/worker/signal-fleet` in the worker (`TRIAGE_STATE_VOLUME`), never in git | `results/` (the TSV ledgers), `queue/`, `export/` |
| ledger | flotillad's store (`SF_LEDGER_BACKEND=api`), with the TSVs still written | the fleet's memory (below) |
| LLM | any OpenAI-compatible endpoint; the reference deployment is the installation's LiteLLM gateway, model alias set by `SF_TRIAGE_MODEL` (the code's default is `deepseek/deepseek-v4-flash`) | the investigation, the choice of file / dismiss / cleanup / escalate, and the issue's title and acceptance criteria when it files (see "How the triage LLM works"). Never dedup, never routing. |
| surfaces | the UI's fleet page for `triage` (tabs: Workflow, Routines, Decisions, Costs and stats, Logs) and the run pages; the `signal_fleet_*` and `flotilla_*` metrics; the alerts the runbook lists; Langfuse | how the operator and the next agent see it |

The code is stateless between cycles. Everything the fleet "remembers" is in the two ledgers and the
GitHub issues themselves.

A candidate routine, `triage/cycle-shadow`
([`cycle-shadow.yaml`](https://github.com/chipi/flotilla/blob/main/fleets/triage/cycle-shadow.yaml)),
runs beside the live one on `worker-triage-candidate` with a state volume of its own. Its autonomy
is capped at record: it never files, whatever a control says. It exists to compare another model
or image against the live fleet (ADR-0004).

## One signal's path

### 1. Normalize

`sources.to_signal` (Grafana) and `sources.to_error_signal` (GlitchTip) turn each item into a signal
with two identities:

- **`fingerprint`** — the same problem across time. Grafana: the alert's label-set fingerprint.
  GlitchTip: `glitchtip:<SHORTID>`, e.g. `glitchtip:SHOP-WEB-5E`.
- **`occurrence_id`** — one episode of it: fingerprint + `startsAt` (Grafana) or `firstSeen`
  (GlitchTip). The cycle is idempotent on this key.
- **The host-wide OOM alert is split per container** (2026-10-27, `sources.to_signals` +
  `guards.oom_episodes`). "Kernel OOM killer fired" fires per host, so while one container was
  OOM-killed every ~5 min the alert never stopped, and another container's kills produced no new
  occurrence. The fleet now reads the kernel's `oom-kill:` lines (VictoriaLogs), maps each docker
  cgroup to its container (cadvisor), and emits one signal per container: fingerprint
  `<alert>:oom:<container>`, occurrence = the container's kill *episode* (kills under 30 min apart;
  an episode that began with the firing is keyed on the firing's start), so a crash loop stays one
  occurrence and a later kill is a new one. A failed lookup keeps the host-wide signal.

Test and synthetic signals are suppressed at this stage.

### 2. Seen before?

`orchestrator.run_grafana` / `run_glitchtip`:

- **Same occurrence already decided:** skip. A GlitchTip error whose event count rose since its last
  triage gets a `recurrence` row: one per count change, not one per cycle. It is re-triaged in full
  only after `SF_RETRIAGE_HOURS` (24 h), or at once in the two cases below.
- **Exception — promote at once** (2026-10-17, `orchestrator._promote_now`): when the last decision
  was `file` but the low-signal gate folded the error into a rollup, its first recurrence re-triages
  immediately, so filing promotes it to its own issue. Before this, fingerprints stayed rollup lines
  for good, including a fatal one.
- **Exception — overturn at once** (2026-11-07, `orchestrator._overturn_now`): a dismissed error that
  recurred is re-triaged at once. Of 64 dismissals that came back, 49 were overturned at their
  re-triage, a median of 85 hours later. Once per dismissal: a dismissal that was itself an early
  re-triage waits out the window.
- **A new Grafana firing of a known fingerprint inside 24 h:** a `recurrence` row, no new triage.
- **The GlitchTip queue:** a cycle reads up to 100 unresolved issues (`POLL_WIDTH`, most recently
  seen first) and triages at most `--limit` (10) of them: issues never decided first, oldest first
  seen first, then the re-triages that are due. The rest wait for the next cycle. Reading only the
  newest few left a burst's older issues undecided until they happened to recur.

### 3. Triage: deterministic rules, the LLM, deterministic gates

`triage.triage()`:

1. **Operational states → `dismiss`, no LLM** (`OPERATIONAL_MARKERS`):
    - cost cap;
    - provider budget / HTTP 402;
    - failed fallbacks;
    - upstream shared-pool rate limits (`upstream-rate-limit`: a 429 with
      `limit_source=upstream_provider_shared_pool`: not our key, not our quota, not our code);
    - a call to the assistant backend that LiteLLM failed over (`assistant-failover`, RFC-0008): a
      `litellm` issue whose latest event names only the assistant's backend (the installation's
      `assistant_backend`, or `SF_ASSISTANT_BACKEND`). The orchestrator reads that event just before
      triage (`sources.attach_backend`).

    These are states, not bugs, and they hold even while the triager is down.

2. **Investigation by the LLM:** up to `SF_MAX_PROBES` (3) probes — logs, metrics, traces, source
   state, occurrence history — then one terminal decision. The model's brief is the playbook
   (`fleets/triage/playbooks/triage.md`), and a signal of a known class gets a hint after it
   (`signal_class`: `external-transient`, `external`, `environment` or `recoverable`; the hints are
   skills under `fleets/triage/skills/`). The hint biases the choice and never gates it. A GlitchTip
   error's trace is looked up by the OTel id the apps stamp as `tags.trace_id` (the SDK's own
   `contexts.trace.trace_id` never exists in VictoriaTraces). Each triage is **one Langfuse trace**
   (project "agents"): the fleet mints the trace id, and the gateway adds its costed generations to
   it as `triage-turn-N`. The decision:
    - `file`: a real problem; becomes an issue with `work_type` `bug` or `config-enhancement`. The
      app failing to load its own code (a failed dynamic import or module script, a service-worker
      register or update failure) is filed even at a low count (2026-11-04); a CSS preload or a
      plain fetch that failed, with a benign history, may still be dismissed;
    - `dismiss`: benign, with probe evidence that shows it;
    - `cleanup`: test or synthetic noise;
    - `escalate`: a specific question only a human can answer.

3. **Deterministic gates after the LLM** (`triage._gate`; the routine's `harness.gates` lists
   them): each can only turn a decision into an escalation.
    - **Intent gate** (`_intent_gate`): a `file` whose acceptance criteria do not each cite an
      allowed source (reporter, spec, repo data, code invariant, baseline, operator rule, SLO)
      escalates. Investigation earns the what and where; it never earns the intent.
    - **Cleanup gate** (`_cleanup_gate`): a `cleanup` needs a machine-checkable test or noise marker
      (`CLEANUP_MARKERS`, or `environment` test/dev), else it escalates.
    - **Dismiss gate** (`_dismiss_gate`): a `dismiss` must cite its evidence and rest on an
      independent probe that returned content (service logs, metric, trace, source state) or an
      occurrence history that is benign (count ≤ 1, a low level, or resolved). It makes most of the
      fleet's escalations (39 of 49 on 233 decisions of one project). Replayed on the
      owner-labelled decisions (dataset v2, one pass of production's model) it stopped 8 dismissals
      of real problems and turned 7 right dismissals into false alarms; at five false alarms per
      missed problem it pays for itself, so it is narrowed by evidence, never loosened wholesale:
      [`EVAL-LESSONS-0.6.0.md`](https://github.com/chipi/flotilla/blob/main/signal-fleet/EVAL-LESSONS-0.6.0.md)
      F1 and section 7.
    - **Edge gate** (`_edge_gate`, 2026-10-27): an **edge-5xx burst** is never dismissed when a
      container on the edge's host started or was OOM-killed within 10 min before to 5 min after it
      (the edge instance is the installation's `edge_instance`): probes run after the restart look
      healthy because the container is back. It escalates naming the container; a failed lookup
      escalates too.

4. **Triager failures:**
    - a 401/402/403 means the triager is down: one aggregate issue for the fleet, not one per
      signal;
    - a timeout or 5xx defers the signal to the next cycle (`.deferred.json`, at most
      `SF_DEFER_LIMIT` = 6 deferrals, then escalate);
    - an empty reply on the last turn defers the same way, counted;
    - a reply that never parses as JSON becomes the escalation "no terminal decision";
    - a cycle where every call deferred (two or more, none succeeded) counts as triager-down.

### 4. Act and file

`actions.act` writes the disposition row; `filing.file_or_update` decides what happens on GitHub
(the decision tree under "The filed ledger"). The routine's autonomy gates the writes. The worker
hands it to the code as `FLEETD_STAGE` (`record` becomes `shadow`, `auto` becomes `live`), and the
ledger rows record it as `stage`:

- **`record` (stage `shadow`, v1's word for it):** nothing external. The orchestrator forces a dry run.
- **`propose`:** issues, comments and reopens on GitHub. Cleanup stays queued.
- **`auto` (`live`):** everything, including resolving a cleaned-up error in GlitchTip.

### 5. Mirror onto GlitchTip: the sync

GitHub is where the operator decides; GlitchTip used to never hear about it (2026-10-15: 312 of 363
filed GitHub issues were closed, their GlitchTip issues still unresolved). At the end of each cycle
`glitchtip_sync.run` reads, for every GlitchTip fingerprint in the filed ledger, its GitHub issue
and copies the decision:

| GitHub | GlitchTip |
| --- | --- |
| closed as fixed | resolved "in next release": a later recurrence counts as a regression |
| closed as not planned | resolved |
| closed as duplicate of #M | follows #M: merged into #M's GlitchTip issue only if it is the **same error** (same project, same fleet skeleton), then takes #M's state; otherwise resolved when #M is closed |
| open / reopened | untouched |

Every mapped issue gets one `Tracked in GitHub: <url>` comment. A row whose short id was reused by a
newer issue is skipped (`reused_short_id`). Nothing is lost by resolving: a recurrence reopens the
GlitchTip issue, triage runs before the sync in the same cycle, and filing reopens (< 7 days) or
files a regression.

- **Switch:** `SF_GLITCHTIP_SYNC=1` in the worker's secrets file, token `GLITCHTIP_WRITE_TOKEN`
  (`event:write`). Writes only when the cycle writes (not at `record`). The candidate worker sets
  it to `0`, so it mirrors nothing.
- **o11y:** metric `signal_fleet_glitchtip_sync{kind=mapped|merged|resolved|commented|errors}` every
  run; alert `glitchtip-sync-failing` (errors in every run for 25 min); one VictoriaLogs line per
  write, `{fleet="triage", event="glitchtip_sync"}` (the audit trail).
- **By hand:** `python3 glitchtip_sync.py` is a dry run (counts and samples); `--apply` writes. The
  runbook says how to run it in the worker.

## How the triage LLM works

One signal is one new conversation (`triage.investigate`, on the shared harness in
`runtime/harness.py`); nothing carries over from one signal to the next. The fleet's memory is the
ledgers and GitHub, which code reads; the model never sees past decisions or open issues.

```text
system   the playbook (what file / dismiss / cleanup / escalate mean, how rare escalation must be)
         + the probe menu (event_detail, occurrence_history, service_logs, metric, trace, source_state)
user     the signal: source, alertname, labels, summary, between data delimiters
         (+ the class hint, for some error shapes)
turn 1   model → {"action":"probe","probe":"event_detail"}           JSON mode, temperature 0
         code runs the probe, appends its answer (cut to 1,800 characters)
turn 2   model → {"action":"probe","probe":"occurrence_history"}
turn 3   model → {"action":"decide","disposition":"file", ... title, symptom, acceptance criteria ...}
         at most SF_MAX_PROBES = 3 probes, 4 turns, one probe per turn; the 4th turn must decide
then     the gates (section 3 above) may turn the decision into an escalation
```

- **The signal's text is data.** An error message or alert summary was written by a system the
  fleet watches. It goes between delimiters the playbook names as data, never instructions, and a
  delimiter inside it is neutralised.
- **Cost:** the API is stateless, so each turn sends the conversation so far. A signal takes 2 to 4
  calls, about 3,500 input and 1,300 output tokens, about $0.005 at the reference model's prices.
  A reply is capped at `SF_TRIAGE_MAX_TOKENS` (8192); a cut reply is bad JSON and costs a turn.
- **What the model reads, measured** (233 decisions of one project, EVAL-LESSONS F4): 0 probes 10,
  1 probe 88, 2 probes 96, 3 probes 39. `event_detail` is the usual first look; logs and traces are
  rarely asked for. Replayed on the labelled set, the decisions that used all 3 probes were right
  as often as the rest (0.67) and those that stopped at 2 the least (0.47): wrong decisions come
  from judgment after the event and history were read, not from the cap.
- **One reply does two jobs:** the decision, and, for `file`, the issue's text. The intent gate
  checks the second.
- **JSON mode is required:** a model is usable only through a provider that offers
  `response_format: json_object`.
- **Each model call is recorded** on the run (the run's page shows the calls and the harness
  steps), as a Langfuse trace (project "agents"), and as spans under the run's trace.

## How the fleet is evaluated

- **Replay gates:** deterministic replays and unit tests that must pass before a change ships; they
  say "nothing regressed", not "how often it is right". The replays of the fleet's rules read real,
  scrubbed production data, so they live with the installation, in its private fleets repo
  (`triage/gates/run.sh`, run by `make replay`). Four generic ones stay in this repo, in
  `signal-fleet/mvp/`: `gate_harness_replay.py` (the probe loop), `gate_ledger_backend_replay.py`
  (dedup through flotillad's ledger against the TSVs), `gate_model_client_replay.py` and
  `gate_skills_replay.py`. The deploy also runs a filing gate (see the runbook). Until 2026-11-16
  eighteen gates ran from `signal-fleet/deploy/gates.sh` in the installation's ops repo.
- **Decisions against what the operator did:** eval-harness
  (`example/eval-harness`), `examples/triage-fleet`. It
  replays a recorded decision through the fleet's own `triage.investigate` with the probe answers
  frozen as of the decision, so the brief, the probes and the gates run as in production and only
  the model or the fleet's code changes between arms. The measure is `triage_cost`: a missed real
  problem counts as five false alarms.
- **What it has shown:** no model is better than production's (80+ arms); the brief and the gates
  are the lever. The brief change shipped on 2026-11-04 cut `triage_cost` from 0.872 to 0.446 on
  dataset v2 (p 0.0011) in round r4. The results and their limits:
  [`EVAL-LESSONS-0.6.0.md`](https://github.com/chipi/flotilla/blob/main/signal-fleet/EVAL-LESSONS-0.6.0.md)
  section 7.
- **Replays from outside:** `signal-fleet/mvp/replay_api.py` runs one of the fleet's decisions over
  a recorded world with nothing sent, so an evaluation project never patches the code's internals.
  `capture.py` records each decision with its probe answers so it can be replayed exactly.
- **How to measure a change:** [runbook, the improvement loop](triage-runbook.md#5-improving-the-fleet-the-loop-every-change-follows).

## The two ledgers

Both are tab-separated files under `results/` in the state volume, and flotillad's store holds the
same rows (`SF_LEDGER_BACKEND=api`, RFC-0002 §9 step 5): the fleet writes the store first and then
the file. Filing reads its dedup keys from the store; the recurrence check still reads the TSV.
**They are the fleet's memory**: lose both and it forgets every decision and re-files old
problems. The store is in flotillad's snapshots (the runbook); the TSVs are in the triage state
volume.

### `dispositions.tsv` — every decision, append-only

One row per decision or recurrence. Columns:

```text
ts  occurrence_id  fingerprint  source  alertname  disposition  work_type  followup
model  prompt_ver  prompt_sha  gates  n_probes  certainty  reason  signal_count
cycle_id  stage
```

- **`disposition`:** `file | dismiss | escalate | cleanup | recurrence`.
- **`signal_count`:** the source's event count at decision time. Recurrence rows compare against it.
- **`gates`:** which gates passed, e.g. `operational:upstream-rate-limit`.
- **`cycle_id`:** the flotillad run id of the cycle (`FLEETD_CYCLE_ID`), so a row joins to its run.
- **`stage`:** the autonomy the run had, as the code names it (`shadow` for record, `propose`, `live`
  for auto).
- The store row also carries the decision's model `turns`, `quirks` and `cap_hits`; the TSV header
  stays as above, because a changed header archives the file.

It answers "what did the fleet decide, and why?" and feeds:

- the Decisions tab of the fleet page, and the brief's decision counts;
- the dispositions dashboard (metric `signal_fleet_disposition{disposition=...}`);
- the stale check (when a signal was last seen);
- every replay.

### `filed.tsv` — which GitHub issue each signal maps to

One row per fingerprint that reached GitHub. Columns:

```text
fingerprint  repo  issue  group_key  filed_at  last_comment_day  norm_key  alert_key  linked_via
```

It is upserted by fingerprint, so a fingerprint has exactly one row. `linked_via` marks a row that
a signal got when it matched an issue on a key other than its own fingerprint (2026-11-12): the row
points at that issue so the stale check and the GlitchTip sync see it. `ledger_lookup` never
returns a linked row, so a mute is never inherited through a fuzzy match. `SF_FILING_LINK=0` turns
linking off.

#### The four matching keys, strongest first

| Key | Built from | Matches |
| --- | --- | --- |
| `fingerprint` | the source's own identity | the same alert or error again |
| `group_key` | curated storm rules (`GROUP_RULES`, `PROJECT_GROUP_RULES`) and the low-signal rollup (`low-signal:<project>`) | members of one incident / one rollup bucket |
| `norm_key` (`v1:`) | the message with volatile parts masked: run ids, timestamps, UUIDs, hex ids, paths, dollar amounts, numbers of 4+ digits, `[n]` counters | the same bug under a new fingerprint |
| `alert_key` (`a2:`) | repo + source + alert name with counters/measurements masked | the same alert, coarsest; **open issues only** |

#### The lookup: `filing.ledger_lookup`

1. Exact `fingerprint` → that row.
2. `group_key` → newest row.
3. `norm_key` → the newest row **whose issue is open and is not a rollup bucket**. If none is open,
   the newest row (reopen / regression path).
4. `alert_key` → the newest row whose issue is open.

#### The filing decision: `filing.file_or_update`

| Prior match | Fleet does |
| --- | --- |
| issue has label `triage-fleet/muted` and the match is exact (fingerprint/group) | nothing, forever |
| muted, but the match is fuzzy (norm/alert key) | ignores the mute and files fresh: a fuzzy match must never bury a different bug |
| issue closed **as a duplicate** | follows GitHub's `duplicateOf` (up to 3 hops) to the canonical issue, then applies the rows below to *that* issue; the ledger row is remapped to it |
| issue **open** | one comment per day: `Recurred <date>… (matched on <key>)` |
| issue closed **< 7 days** (`REOPEN_WINDOW_DAYS`) | reopens it with a comment |
| issue closed **≥ 7 days** | files a new issue linking the old one (regression) |
| no match | files a new issue, labelled `triage-fleet/filed` |
| prior is a low-signal rollup, signal is no longer low-signal | promotes: a new issue of its own |

**Which repo:** the installation's `repo_map` (GlitchTip project prefix → repo, in its order) and
`instance_repo_map` (a Grafana rule's `instance` label → repo). A project that maps to nothing
falls back to `SF_OPS_REPO`, else the installation's `ops_repo`, as an unmapped Grafana signal
does. A fix for the `bugfix` chain is routed only for the installation's `chain_repos`.

**Low-signal:** a GlitchTip error with ≤ 1 event, 0 users and no code location. It goes to a
per-project rollup issue (`[low-signal] aggregate — <project>`) as one line, instead of an issue of
its own. A **fatal** event (a crash, a watchdog kill) is never low-signal: it gets its own issue on
its first hit (2026-10-17: three fatal errors had been buried in one rollup).

**GitHub state is read at filing time** (`issue_state`): the operator's close, label or comment *is*
the acknowledgement. The local ledger never overrides GitHub.

**Retired rows:** `glitchtip-retired:<SLUG>-<n>`. These belong to a deleted GlitchTip project. A
project recreated under the same slug restarts its short ids, and its new errors would match the
dead project's rows (2026-10-01: a rate limit reopened an unrelated issue).
`filing.py --retire-glitchtip-project SLUG BEFORE` moves them out of the matching namespace.

**Reused short ids are ignored automatically** (2026-10-17): a ledger row filed *before* the
GlitchTip issue now holding its short id was first seen belongs to another, older issue. Filing
(`ledger_lookup`, GlitchTip signals only) and the GitHub → GlitchTip sync both ignore such a row
(`reused_short_id`). Before this, a recreated dev project gave a new probe error an old decision
and the sync resolved it. Retiring rows is still tidy, but no longer required.

### Other state files

All under the state volume, `/home/worker/signal-fleet/` in the worker.

| File | Purpose |
| --- | --- |
| `results/.deferred.json` | per-signal count of transient triager failures; absent when nothing is deferred |
| `results/.stale_last_run` | UTC day of the last stale pass |
| `results/.inbox_seen` | operator-inbox bookkeeping |
| `results/glitchtip-marked.json` | the GlitchTip issue ids known to carry the sync's "Tracked in GitHub" comment |
| `export/inbox.json` | **the fleet's published inbox**, the one file other readers may use. Schema `fleet_inbox/v1`: `schema`, `fleet`, `generated_at`, `needs_you` (bug proposals + open escalations of the last 7 days, the same rows as `signal_fleet_escalations_7d`), `escalations_7d` (`ts`, `alertname`, `reason`, `fingerprint`, `issue`: `repo`, `number`, `url`, `state`, `title`, and `error` when GitHub could not be read, or null when never filed), `bug_queue` (`title`, `fingerprint`, `queued_at`), `queue_depth` (per kind). Rewritten atomically each cycle; a change to the fields is a new schema version |
| `results/.last_cycle_spend` | cycle cost in USD, read by the worker after the run and counted in the routine's daily budget |
| `results/probes/` | per-decision capture (`capture.py`), on when `SF_CAPTURE_PROBES=1` (the code default is off): one JSON per decision (mode 600) with the signal, every probe answer the model got (raw and the trimmed text it read) and, with `SF_CAPTURE_EAGER=1` (default), the rest of the probe menu at default args, so `triage.investigate(signal, probe_table=item["probes"])` replays the decision exactly; also `model_decision`, the model's own decision before the gates. About 12 KB each. Real production data: keep it in the volume, `scrub.py` before sharing |
| `queue/` | proposals the fleet may not act on alone (`cleanup`, `config-enhancement`; everything at `record`) as `*.json`; handled ones renamed `.filed` / `.resolved`; `queue/archive/` holds batches set aside |

## The operator's interface: GitHub labels

| Label | Set by | Meaning |
| --- | --- | --- |
| `triage-fleet/filed` | fleet | the fleet filed this issue |
| `triage-fleet/escalated` | fleet | the fleet needs a human answer |
| `triage-fleet/actionable` | fleet | machine-fixable candidate for Fleet 1 |
| `triage-fleet/low-signal` | fleet | a rollup bucket issue |
| `triage-fleet/stale-candidate` | fleet | the signal has been quiet 7 days with no human comment ("looks resolved?") |
| `triage-fleet/substrate` | fleet | the fleet's own infrastructure (e.g. triager down) |
| `triage-fleet/routed:bugfix` | operator | dispatch to Fleet 1 |
| `triage-fleet/muted` | operator | never act on this signal again (exact matches only) |

Closing an issue is the operator's dismissal; a recurrence inside 7 days reopens it. A comment from
the operator is reporter input on the next triage.

## Why it is built this way

These rules came from incidents. Keep them unless the evidence changes.

1. **Deterministic before the LLM.** Dedup, routing, operational states, recurrence and low-signal
   are code, not prompts. They are cheap, testable, and they don't drift. The LLM only investigates,
   picks file, dismiss, cleanup or escalate, and writes the issue it files; code gates check its
   choice.
2. **Fail closed, in one place.** When the triager is down, the fleet files one aggregate issue, not
   one per signal. The August 2026 flood turned a 401 into about 90 escalation issues.
3. **GitHub is the source of truth for state.** The fleet reads issue state at filing time and never
   acts on a cached copy.
4. **A fuzzy match never inherits a mute.** Muting is exact (fingerprint / group key). Otherwise one
   mute could silence a different bug forever.
5. **Prefer the bug's open issue, through the precise key only.** Norm-key matches prefer an open
   issue; alert-key matches don't override them. Tried on 2026-10-02: the alert key's moves mostly
   re-derived GitHub's duplicate links, and where they differed the link was more exact. Explicit
   links beat inferred ones, so duplicates are followed directly (rule 6).
6. **A duplicate is not the bug's issue; its canonical is.** When the operator closes an issue as a
   duplicate, a recurrence goes to the issue it duplicates. The link comes from GitHub (GraphQL
   `ClosedEvent.duplicateOf`; REST only carries the reason). Before this, one disk alert reopened
   four duplicates of one issue.
7. **A rollup bucket is not a bug's issue.** Never route a specific bug's recurrence to
   `[low-signal] aggregate`.
8. **One condition, one alert.** Fan-out has to be fixed at the source rule, not deduplicated
   afterwards. Example: per-mountpoint disk alerts gave six issues for one disk; the fix groups by
   disk.
9. **Every behaviour change is proven on real history before it ships.** The runbook's improvement
   loop requires a replay against a frozen corpus, a baseline-reproduces-history check, and a
   mutation check.
10. **A GlitchTip short id is not an identity on its own.** A recreated project restarts the
    counter, so a ledger row counts only if it was filed after the issue now holding that id was
    first seen. A manual retire step was forgotten for four projects; the rule needs no step.
11. **A crash is never a rollup line.** Fatal events skip the low-signal gate, and a signal the
    fleet decided to file is promoted on its first recurrence. The rollup exists to quiet one-off
    noise, not to hold bugs the fleet already recognised.
12. **GitHub decides, GlitchTip follows.** The sync copies the operator's call and never makes one;
    a merge needs the same error, not just a duplicate link, because an umbrella issue can hold
    several errors.
13. **A failover that worked is not an error.** LiteLLM reports every failed deployment call,
    including the assistant backend's when the `assistant` alias then answers from another provider;
    LiteLLM 1.96 has no hook to skip that. So the fleet reads the event: only the assistant
    backend's address, and nothing else, makes it `assistant-failover`. A second backend, another
    project or a failed lookup leaves it with the triager. The backend being down is a separate
    alert's job. On 2026-11-03 the failover tests filed five issues before this rule.
14. **Dev is not production.** A `-dev` GlitchTip project is never read: its errors belong to the
    branch's agent, and filing them as bugs on main produced wrong plans for the bugfix fleet.
15. **A deploy proves filing did not change.** Before a new image goes live, the deploy replays the
    newest frozen week of filing decisions with the old and the new code, with the installation's
    own facts, and fails on any difference the operator has not accepted.

## Known gaps (as of 2026-11-17)

| Gap | Effect | Status |
| --- | --- | --- |
| **Warnings never reach the fleet** | systematic warnings (e.g. speaker-attribution loss in 26 of 28 runs on 2026-10-01) are invisible to it | by design today: the sources are GlitchTip errors and Grafana alerts |
| A one-event GlitchTip error with no culprit is **low-signal by rule** | a real bug can be buried as one line in the rollup (2026-10-01: a `File name too long` error) | open for non-fatal errors; fatal events get their own issue since 2026-10-17 |
| **One cause, several GlitchTip issues** | a provider-auth failure is logged by each layer it passes through: 4 issues in 31 minutes on 2026-10-16 | fixed for provider auth 2026-10-17 (`PROJECT_GROUP_RULES`, one group per project). Other cause classes still fan out until a rule names them. |
| The GlitchTip pass reads **100 issues, newest seen first** | an issue outside the 100 most recently seen waits until it is seen again | the busiest 10-minute window in the 14 days to 2026-10-17 had 6 issues |
| **An ok run's output is not kept** | a failed run keeps the last 800 characters of its output in its detail; an ok run keeps none | its decisions are in the ledger and the Decisions tab, its calls on the run page, the sync's writes in VictoriaLogs; for anything else, rerun the step by hand |
| `.deferred.json` entries **never expire** | an occurrence that deferred once and never came back keeps a count of 1 | harmless; pruning needs a last-deferred time per entry, which the file does not store |
| **No way to mute a fragmenting bug** | a bug whose fingerprint keeps changing files new issues; `triage-fleet/muted` only silences one fingerprint, and fuzzy matches never inherit a mute | needs a `muted-class` label (2026-08-28 design, never built) |
| **`assistant-failover` reads one event** | an issue grouping both assistant failovers and real errors is judged by its latest event only | GlitchTip groups by message, so "Connection error." from the assistant and from another backend share one issue; the latest event decides each time it is triaged. Not seen yet |
| **Chained-exception ordering is spec-only** | the cause-chain handling (2026-08-27 enhancement #5) was built against the spec; no real chained exception had passed through it as of 2026-11-05 | re-check when the first real chain appears |
| **Replay gates are outside this repo** | `ci-fast` does not run the 18 rule replays; they run on a host with the private repo (`make replay`) | the four generic gates and the filing gate at deploy remain |
