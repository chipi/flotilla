# The UI, next: three buckets

The operator's split (2026-11-16) of their feedback on the live UI and what is open, reviewed by the
advisor (Fable) the same day under the operator's guidance: **simplicity is a virtue; do all that is
needed, nothing more.** A plan; nothing is built until the operator agrees it.

## The goal: useful for triage and bug fixing, daily

The operator: the tool does not help much today, for the reasons below. The work is done when the
operator can answer each of these in about a minute, from the UI alone:

1. **Triage, the day:** what did triage decide since yesterday, by outcome and project, and is that
   more or less than usual?
2. **Triage, one decision:** why was this signal escalated, dismissed or filed: what it read, what the
   model concluded with what certainty, which gate fired?
3. **Triage, a change:** did a change (T0.9 today, T0.1 next) move the decisions the way it should?
4. **Bug fixing, one case:** what is this bug, does it happen on main, what did the fix change, which
   tests prove it, what did the review say, and what does it need from me?
5. **What needs me now,** and what is waiting on something else.

## What the data holds today (checked 2026-11-16)

- **Triage's model calls are lost by a one-line bug:** the runtime's model client already writes every
  call to the run's calls file (`model_client.py:253`, the file set by the worker,
  `flotilla_worker.py:190`), but `triage.py:267-268` builds its client with an environment holding
  only the API key, so the file is never named. The model contract and triage also never start
  tracing (`telemetry.setup()`, which the heartbeat calls at `heartbeat.py:70`), so they send no
  `model.chat` spans.
- **Triage's decisions are in the ledger** (`ledger_entries`): per decision the disposition, the gates,
  the fingerprint (hence the project), `run_id`, model, `prompt_sha`, probes used and certainty.
- **Not recorded and not planned:** the tools a model called per call (triage's json protocol keeps
  `n_probes` per decision, which is what is shown); counts for steps before the model (sources,
  dedup); bugfix's pi calls (pi does not use the runtime's client: a separate job, below).

## Order

1. **One small deploy, first** (every day without it loses run data):
   - triage passes the calls file to its client (one line, with the fleet session: it is triage's
     code) and starts tracing (one line); the model contract starts tracing (one line);
   - the fleet page's two bugs: the ended `triage-candidate` shown as a healthy worker; the X3
     worker links counted as "2 down" host agents.
2. **Bucket 2,** the decision mix (the operator's go stands).
3. **Bucket 1,** one mockup on real data, then the build.

## Bucket 2: the decision mix (T0.4)

- **One query over the store** (no new API, no MCP tool yet): the ledger's decisions per day and
  project, counted by disposition and by gate fired (a gate fired = neither `passed` nor `n/a`).
- **A Decisions tab on the fleet page:** 14 days by disposition, the gates fired, a per-project table.
  **Segmented where `prompt_sha` or the model changes**, from the rows themselves, so a change shows
  as a before and after (the store has no deploy events). If T0.9 changed no prompt text, its line
  is drawn from the deploy time instead (to check when building).
- **One line in the brief** a day.
- **The check:** the fleet session's `decision_mix.py` gives the same counts for a day, against a copy
  of the store.
- Later, when a shadow fleet runs (M2b): its agreement with live in the same tab.

## Bucket 1: the UI shows what happens inside

1. **A routine page,** which the workflow's blocks open (today a click only swaps the side panel,
   which a phone does not show): the routine's spec, its last runs, and the counts the store holds (the
   ledger's decisions, `run_calls`); what is not recorded says so. For `triage/cycle` a short drawn
   pipeline (sources, model per signal, four gates, outcomes); for that one routine only.
2. **The run page as one timeline:** its model calls (`run_calls`: model, tokens, cost, latency) and
   its decisions (the ledger rows by `run_id`: signal, outcome, gates, certainty, probes used), in time
   order, beside the spans. One store query added: the ledger entries of a run.
3. **The case timeline condensed:** start, the events that changed the case, end; repeats folded
   ("17 more signals attached") and expandable.

First a clickable mockup of these three on real data, mobile first (360 px), for the operator to
approve; then the build.

## Bucket 3: the rest, and who decides

| Item | State | Who |
| --- | --- | --- |
| T0.1: dev errors not investigated, never filed (the operator's choice) | 2397af0 to review | me (review), then the operator's deploy go |
| Bugfix's pi calls recorded (pi does not use the runtime's client) | not started | the fleet session (phase B) |
| One triage cycle skipped per deploy | known | mine, a deploy-script change |
| Service levels (homelab RFC-0009): the plan's 4 decisions | plan pushed | operator, then I build |
| The three endpoints under target (two Umami trackers, airandspacehistory.com) | not looked at | mine, with the SLO view |
| RFC-0013's build: §1 in `fix.py`, then stages and the clarifier | accepted | fleet session (§1), me (stages); the go |
| M2a / M2b: Qwen on the DGX's prod vLLM, a shadow on it | blocked | operator's go, the prod side's word |
| K4: the knowledge volume | waiting | operator's go |
| SEO findings: airandspacehistory's `/home` canonical, evals not indexed, 3 opportunities | open cases | operator |
| The Slack app; the robotics assessment | the vision plan's last items | operator |

## What the advisor's review changed

- Recording triage's calls is a one-line fix, not a project; no step protocol (calls + ledger rows by
  time answer every goal question).
- No per-call tools column; `n_probes` per decision is what exists.
- Bucket 2 in two places (the tab, the brief), not four; no API or MCP tool until asked.
- Before/after segments from `prompt_sha` and model in the rows, since the store holds no deploys.
- The routine page shows what the store records and names the rest "not recorded"; the drawn
  pipeline is for `triage/cycle` only, since a drawing of another repo's code drifts.
