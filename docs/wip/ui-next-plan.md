# The UI, next: three buckets

The operator's split (2026-11-16) of their feedback on the live UI and what is open. Bucket 2 has the
operator's go; bucket 1 starts with a mockup for the operator to approve (mockups are the contract);
bucket 3 is the list of the rest, each with who decides.

## The goal: useful for triage and bug fixing, daily

The operator, 2026-11-16: the tool does not help much today, for the reasons in the buckets below.
The work is done when the operator can answer each of these in about a minute, from the UI alone:

- **Triage, the day:** what did triage decide since yesterday, by outcome and project, and is that
  more or less than usual? (Bucket 2.)
- **Triage, one decision:** why was this signal escalated, dismissed or filed: what it read, what the
  model concluded with what certainty, which gate fired? (Bucket 1, 4 and 7.)
- **Triage, a change:** did a change (T0.9 today, T0.1 next) move the decisions the way it should?
  (Bucket 2, deploys on the chart.)
- **Bug fixing, one case:** what is this bug, does it happen on main, what did the fix change, which
  tests prove it, what did the review say, and what does it need from me? (Bucket 1, 4 and 5; the
  case page, with RFC-0013's question round.)
- **What needs me now,** and what is waiting on something else. (The brief and Decide, kept honest by
  bucket 1's fixes.)

## What the data holds today (checked on the mini, 2026-11-16)

- **Model calls** are recorded for two routines only: `platform/heartbeat` (248 runs, 1 call each) and
  `platform/model-contract` (19 runs, 8 each). Live `triage/cycle` records none, though it calls a
  model per signal; bugfix's pi calls are not recorded either.
- **Spans:** flotillad's run span for every run; the heartbeat's `model.chat` span under it. The model
  contract records its 8 calls but sends no span (`contract_routine.py` never starts tracing); triage
  starts no tracing at all.
- **Triage's decisions are in the ledger** (`ledger_entries`): per decision the disposition, the gates
  (`i:passed/c:n/a/d:n/a/e:n/a`), the fingerprint (hence the project), `cycle_id`, `run_id`, model,
  probes used and certainty, for the live fleet and the old shadow fleet alike.

## Bucket 1: the UI, made to show what happens inside

From the operator's feedback on the fleet, run and case pages:

1. **Two bugs on the fleet page:** the ended `triage-candidate` still shows as a healthy worker; the
   workflow's "host agents" block counts the X3 worker links as "2 down".
2. **The workflow's blocks lead somewhere:** a click opens the routine's page (today it only fills the
   side panel).
3. **A routine page that shows what is inside it:** its steps as a pipeline (for `triage/cycle`:
   sources, dedup, the model per signal with its tools, the four gates, file / dismiss / escalate /
   recurrence / cleanup), each step with its last-24h counts; its spec, its last runs.
4. **The run page as steps:** what it read, each model call (model, tokens, cost, latency, the
   tools it called), each gate, each decision and what it wrote, on one timeline with the spans.
   For a triage run, its decisions come from the ledger by `run_id` until the run records its own steps.
5. **The case timeline condensed:** the start, the moments that changed the case, the end; repeats
   ("17 more signals attached") folded and expandable; paging on very long ones.
6. **The legend's states drawn:** agent steps and "waiting for you" appear where they happen.
7. **The data under it:** the model contract starts tracing (one line, a test); triage records its
   model calls and steps (with the fleet session: it is triage's code).

Mobile first (360 px, no sideways scroll). **First a clickable mockup of 2 to 6, on real data, for the
operator to approve; then the build.**

## Bucket 2: the decision mix (T0.4), and what it needs (operator's go)

So the operator sees what the fleet actually decides, and T0.9's effect from today:

1. **The data:** an API read over the ledger's decisions: per day, per project, counts by disposition
   and by gate fired (a gate fired = neither `passed` nor `n/a`), for 14 days. An MCP tool on the same
   read.
2. **On the fleet page:** a Decisions tab and a card: 14 days of stacked bars by disposition, a
   row of gates fired, a sortable per-project table; **deploys marked on the chart**, so T0.9 (11:38Z
   2026-11-16) shows as a line with the days before and after.
3. **In the workflow:** the "Decide and act" column shows the last 24 h by disposition on the policy
   block, and gate counts on the routine block.
4. **In the brief:** one line a day ("Triage yesterday: N decisions: … filed, … escalated (gate d: …),
   … dismissed, … recurrences; top project …").
5. **The check:** the fleet session's `decision_mix.py` gives the same counts for a day, run once
   as a test against a copy of the store.
6. **Agreement with a shadow fleet** when one runs (M2b): the same view, with a row of how often the
   shadow decided like live, per day.

## Bucket 3: the rest, and who decides

| Item | State | Who |
| --- | --- | --- |
| T0.1 dev filter: dev errors as notes (recommended) or not filed | reviewed, ready | operator decides, then I deploy |
| One triage cycle skipped per deploy (the deploy's pause outlives the due run) | known | mine, a deploy-script change |
| Service levels (homelab RFC-0009): the plan's 4 decisions | plan pushed | operator decides, then I build |
| The three endpoints under target (two Umami trackers, airandspacehistory.com) | not looked at | mine, once the SLO view exists |
| RFC-0013 build: §1 step one in `fix.py`, then stages, the clarifier | accepted | fleet session (§1) and me (stages); B6 needs the go |
| M2a / M2b: Qwen on the DGX's prod vLLM as a triage alias, a shadow on it | blocked | operator's go and the prod side's word on load |
| K4: the knowledge volume | waiting | operator's go |
| SEO findings: airandspacehistory's `/home` canonical, evals not indexed, 3 ranking opportunities | open cases | operator (the sites' owner) |
| The Slack app; the robotics assessment | the vision plan's last items | operator |

## Order

Bucket 2 first (it has the go and watches T0.9), with bucket 1's two bugs and the model contract's
tracing in the same deploy; bucket 1's mockup in parallel, for approval; bucket 3 as decided.
