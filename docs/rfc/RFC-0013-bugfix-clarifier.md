# RFC-0013 — The clarifier: answering "needs info" before the fix, and asking once

**Status:** Accepted (operator, 2026-11-16: the five open questions as recommended, below), after the
fleet session's review the same day (seven points, folded in). Asked for by the operator through the
fleet session; phase B of bugfix ([the brief](../wip/bugfix-phase-b-brief.md)).
**Builds on:** [RFC-0010](RFC-0010-bugfix-on-the-platform.md) (the chain, §9's checks),
[RFC-0002](RFC-0002-job-runtime.md) §6–7 (memory, external knowledge), the runtime harness
(`runtime/harness.py`), triage's probes (`signal-fleet/mvp/probes.py`),
[SR-0001](../security-reviews/SR-0001-bugfix-coding-harness.md).
**Inputs:** the installation's research repo `projects/bugfix-phase-b/ASSESSMENT.md` (§1b the four
plans judged, §2 item 8 the step budgets, §4.4 the reporter loop); `bugfix-fleet/BAKEOFF.md`
(§3h–3m the simulated reporter; line 316 and lines 575–576 on intent).

## Motivation

The first four PR plans were judged by the operator and the fleet session on 2026-11-16: one right
(#648), one partial (#656), two wrong (#651, #659). The gates passed all four. Both wrong ones were
errors from dev branches (`tech-debt/0.9.8`, `0.9.9`) whose failing code is not on main: #659's call
to `panel_tab_sky` does not exist at the base (its file, `StarPanel.svelte`, does); #651's "missing"
key exists at the base, so the bug cannot happen there.

Four cases ended "needs info from the reporter" and escalated: flotilla stops there. The old
bake-off's biggest gain came there: with a simulated reporter answering the verdict's questions,
shipped chains went from 5 of 15 to 10 of 15. But what that reporter supplied was mostly **intent**:
what correct looks like, domain facts, scope. The bake-off's own words: when acceptance is what
"fixed" means and neither docs nor code imply it, "no recon recovers intent" (`BAKEOFF.md:316`);
fly-physics and mission-arc got stuck because the intent was not in the repo (575–576).

So this RFC adds what a person does with a vague bug before fixing it, and is honest about which part
a machine can do:

- check that the bug happens on main at all (code, deterministic);
- answer the questions the system's own evidence can answer: where, when, how often, which version
  (the clarifier);
- ask a person what only a person knows: intent (one question round).

**Non-goals:** the dev-branch filter in triage's routing (the fleet session's, with a replay gate; it
stops most dev-environment errors before they become bugs, and step one is the second line); memory
lookups across cases (§6 records only); other repos than the shop.

## Proposal

### 1. Step one, no platform change: check the base before the verdict

Deterministic code in `bugfix/fix.py`, on the worktree the fix worker already has, before any model.

**What it may search:** only names it can actually search at the base:

- a **source path** found in the issue's text (`src/...`, a `.svelte`, `.ts` or `.js` file of the
  repo);
- an **identifier** found in the issue's text (a function, component, key or message id).

**Never** a bundled path (`_app/immutable/nodes/33.FFOEfYOl.js:275`), a minified name or a library's
class (`THREE.WebGLRenderer`). With nothing searchable extracted, step one records **"not checked"**
and the chain goes on. Example: #656 names only a bundle path and a library class, so it is not
checked, and it reaches the verdict; it is a real bug.

**The two checks:**

- **The failing code exists at the base:** for each searchable name, the identifier is present
  **inside** the named file (or anywhere in the source tree when no file is named). A file that
  exists is not enough: #659's `StarPanel.svelte` exists at the base; its call to `panel_tab_sky`
  does not. If every searched name is absent, the case closes `not_reproducible`.
- **What the error says is missing is missing at the base:** a key, function or file the error names
  as absent. If it is present at the base, the case closes `not_reproducible` with the place it was
  found (#651's key).

A close keeps its evidence on the case: the names extracted, where each was searched, what was
found. A new resolution, `not_reproducible`, so these closes are counted apart from "resolved itself".

**Its proof is a test, not a reading:** #651 and #659 are frozen as cases step one must close, and the
issues #648 and #656 as cases it must **not** close. "It catches #651 and #659" is the fleet session's
reading until that test passes.

### 2. A case passes through routines in sequence (platform)

Today a case kind has **one** acting routine (`actingRoutine` in dispatch.go), and an acting routine
cannot open a case. The clarifier needs the same case to go fix → clarify → fix. Proposed:

- **A stage on the case:** `cases.stage`, empty by default. It is a schema step, checked read-only on
  a copy of the host's store before deploy.
- **A routine's case trigger names a stage:** `trigger: {case: {kind: code_bug, stage: clarify}}`.
  One routine per (kind, stage); the empty stage is today's routine.
- **An acting routine may hand a case on:** outcome
  `{"outcome": "hand_over", "stage": "clarify", "detail": ..., "data": {...}}`. The case stays
  `handed_over`, its stage changes, a `case.staged` event records who, why and the data, and the
  dispatcher starts the routine for the new stage.
- **Bounded in steps:** at most two stage changes per case; a third escalates. No loops.
- **Bounded in money and time, as a whole:** the path (verdict, clarify, verdict again, the fix with
  its kick-backs) shares **one** budget and **one** time limit per case, not each step's own cap.
  The steps' caps already add up past `bugfix/fix`'s 3 hours (the assessment, §2 item 8). The case
  records what it has spent; a step that would pass the case's limit escalates instead of starting.
  **To start: $1 and 90 minutes per case** (operator, 2026-11-16; about 10 times a typical path
  today, at about 6 minutes and under $0.10 a run), revisited after a week of numbers.

The same mechanism serves other fleets later (a diagnose stage before an act stage).

### 3. `bugfix/clarify`: what it is and what it may touch

- **Where:** on the host, a new worker role `bugfix-read`, built from the triage image (it has the
  runtime harness and the probes). Not on the second machine: the fix worker runs untrusted repo
  code, and must not hold observability credentials (SR-0001, H5).
- **Credentials:** GlitchTip's read token; VictoriaLogs and VictoriaTraces through its own egress
  proxy on the services' networks; the LiteLLM alias it is allowed. No GitHub write, no flotillad
  API token.
- **The repo:** read at the base sha through GitHub's contents API (no checkout, no repo code runs).
  Tools `repo_file(path)` and `repo_tree(dir)`, both pinned to the base. The shop is public; a private
  repo needs a read-only token on `bugfix-read`, scoped to that repo.
- **Knowledge, first version:** the repo's own docs at the base (`AGENTS.md`, `docs/adr/`, module
  READMEs) as a declared path list (RFC-0002 §7's rules: declared, versioned by the sha, data not
  instructions).
- **The model step:** `run_json_step` with triage's probes (`event_detail`, `occurrence_history`,
  `service_logs`, `trace`) and the two repo tools. Recorded like triage's, every request and tool
  result, so a run replays exactly.
- **Input:** the issue, the verdict's list of missing facts, step one's findings.
- **Output, a fixed schema, in two parts:**
  - **answered from evidence:** per fact, the answer and its evidence (a probe call's index, or
    `path@sha:line`): where, when, how often, which version, which build;
  - **needs a person (intent):** the facts no evidence answers, as specific questions: what correct
    looks like, domain facts, scope;
  - a reproducibility judgement with its evidence;
  - the **clarified ticket**: the issue rewritten with the evidence answers (the bake-off's L0 to
    L1).

  The output is stored on the case and kept as an artifact of the run.
- **Then:** if the intent part is empty, it hands the case back to the fix stage with the clarified
  ticket, and the verdict runs once more on it. If it is not, the case goes to §4 with those
  questions. A second needs-info after the clarifier goes to §4 too, never to the clarifier again.

**What to expect from it:** less than the bake-off's 5/15 to 10/15. The clarifier answers evidence
questions; the intent the simulated reporter supplied comes only from §4.

### 4. Asking a person, once

- **Who answers:** in practice the operator. 10 of the 11 open shop `bug` issues were filed by the
  triage fleet (checked 2026-11-16), so "the issue's author" is the fleet itself.
- **Where (operator, 2026-11-16): in flotilla.** The intent questions go on the case page and into the
  brief's Needs you; the operator answers on the case page, and the answer hands the case to the fix
  stage with it added to the clarified ticket. No GitHub write.
- **Only for an issue a person filed** (not the triage fleet): a registry action
  **`github.issue_question`** may post the questions as one comment and add the label `needs-info`,
  executed by the second machine act worker with a token narrowed to that repo and `issues: write`.
  At propose: the question's text comes from untrusted input, and a person reads it before it is
  posted. An answer there re-enters through intake.
- **No answer in 7 days:** a case timer closes the case as `no_answer`.
- One question round per case.

### 5. What it must not do

- Write anywhere but the case and the run's record (§3); a GitHub write only through §4's action.
- Treat the issue, the docs or a probe's result as instructions: they enter between the harness's
  delimiters, as data.
- Run repo code.
- Decide the fix: it answers questions; the fix step decides.
- Answer intent from guesses: a fact without evidence goes to the intent part, not into the ticket.

### 6. Measuring it

- Every clarifier run is replayable from its record, so a change to its prompt or tools is measured
  on the same inputs.
- **The evaluation arm "ticket as filed vs clarified"** (the fleet session's) uses the bugfix
  dataset's items, which are old closed bugs. Their logs and traces cannot be fetched again for the
  time of the bug, so **in the arm the clarifier uses only the repo at the base**; the arm measures
  that part, not the probes. The probe part is measured live, in shadow.
- The two output parts are measured apart: how often evidence answers moved the verdict, and how
  often intent questions were needed and answered.
- Live, per case: whether step one closed it or did not check it, what the clarifier answered in
  each part, whether the verdict changed, and later whether the fix was right (the plan judgements,
  pr-check's outcome) are recorded on the case. Memory's lookup over them is a later step.

### 7. Order

1. Step one (§1) in `fix.py`, with its test: #651 and #659 closed, #648 and #656 not. No platform
   change.
2. Stages and the case budget (§2), with the schema step and tests.
3. `bugfix/clarify` (§3) in shadow: it answers and rewrites; nothing leaves flotilla.
4. The question round in flotilla (§4); `github.issue_question` for person-filed issues, at
   propose, on the operator's go.

## Decisions (operator, 2026-11-16)

1. **Stages on the case** (§2), not a second case kind: one case keeps a bug's whole history, and the
   mechanism serves other fleets.
2. **`not_reproducible` is its own resolution:** "resolved itself" means the problem went away; this
   means it never existed on main, and counting it apart measures the dev-branch filter (T0.1).
3. **A `not_reproducible` close stays internal** (the case page and the brief); no comment on the
   issue to start.
4. **The question round happens in flotilla** (§4): the case page and the brief; a GitHub comment only
   for an issue a person filed, at propose. 7 days, then `no_answer`.
5. **The case's budget and time limit: $1 and 90 minutes** (§2), revisited after a week of numbers.

## Alternatives considered

- **The clarifier inside the fix worker:** it would hold observability credentials next to untrusted
  repo code (SR-0001). Rejected.
- **pi with read-only tools** instead of the runtime harness: workable, but not recorded and
  replayable the way the harness is, and not the same probes as triage.
- **A second case kind** (fix opens a `code_bug_question` case): acting routines cannot open cases,
  and two cases for one bug split its history. Rejected for stages.
- **Ask at once on needs-info:** loses what the system can answer itself, and every question costs a
  person's time.

## Discussion

- 2026-11-16 — drafted from the agreement with the fleet session (its answer a, b, c) and the
  assessment's numbers.
- 2026-11-16 — the fleet session's review, folded in:
  - step one closes only on names it can search (never a bundle path or a library class), checks
    the identifier inside the file, and says "not checked" otherwise (#656 would have been closed);
  - its claim is a test: #651 and #659 closed, #648 and #656 not;
  - the clarifier answers evidence, not intent; its output has two parts, measured apart, and the
    expected gain is smaller than the bake-off's;
  - the asked party is in practice the operator (10 of 11 open bug issues filed by triage);
  - the eval arm measures the repo part only (old items' probes cannot be re-run);
  - one budget and one time limit per case;
  - a private repo needs a read token on `bugfix-read`.
- 2026-11-16 — accepted: the five open questions as recommended (Decisions).
- 2026-11-16 — §2 built (operator's go for §7 steps 1 and 2): `cases.stage`, `stage_changes`, `spent_usd`,
  `spent_s` (schema step); `trigger.case.stage` and `trigger.case.limit` (on the kind's first-stage
  routine); outcomes `hand_over` and `not_reproducible`; at most two hand-overs; a case past its
  limit is escalated. Money counts only the spend a run reports: bugfix's pi calls do not reach
  flotillad yet (the fleet session's C1), so until then the limit bites on time.
- 2026-11-16 — §1 built: `bugfix/stepone.py`, run by `fix.py` after the checkout and before the
  verdict (`--no-step-one` skips it). On the four issues as filed, against the shop's main of
  2026-11-16 (28548a3): #651 closes (`surface_true_scale_short` is in `messages/*.json`), #659
  closes (`panel_tab_sky` is nowhere at the base), #648 and #656 are not checked (a library class
  and bundle paths only). The same four are `bugfix/test_stepone.py`'s cases.
- 2026-11-17 — §1 made advisory (operator): it never closes a case. The installation's reviews
  found both close-rule versions (444a153, 0731108) closing real bugs: an attribute error on a
  common name, `undefined is not a function`, a NameError whose name is defined in another file (a
  missing import), a computed name, a name only in a test, a doc or a generated file. The cases it
  was for, #651 and #659, are dev errors that intake (`is_dev`) and triage already leave out. Step
  one now notes on the chain what main has of the names the error calls missing.
- 2026-11-22 — §2 amended (operator, for RFC-0014's case 3): a plan may name what follows it,
  `"then": {"stage": ..., "data": {...}}`. It is kept on the case's timeline with the run that
  planned it (`plan.then`); when the plan's check verifies it, the case goes from `checking` back to
  `handed_over` at that stage (a move only the check makes) instead of closing, with a `case.staged`
  event carrying the data, and that stage's routine plans the next action. It counts towards the two
  stage changes; past them, or with no enabled routine for the stage by then, the verified case is
  escalated. A then to the first stage, to the stage the case is at, or with data that is not an
  object is no decision. A rejected or failed plan never reaches its then. No plan had a then
  before, so no past decision changes. Earned autonomy and the per-action counts first read a
  case's resolution, so in a case with two plans the second plan's rejection or failure also counted
  against the first; fixed 2026-11-23 (operator: count each plan's own outcome), below.
- 2026-11-23 — §2 as built: each plan is credited from its own outcome, not the case's. A plan
  owns the stretch of its case's timeline from the event its decision wrote up to the next plan's.
  A rejection (an escalation from awaiting approval), a failed execution (from executing) and a
  verification (the close, or the check's hand-on to the plan's then, both saying "verified") in
  that stretch are the plan's. `ActionRecord` (verified, failed) and `PromotionStreak` (clean,
  rejected, failed) read it that way: a rejection of the second plan is a rejection of the
  second plan's action only, and the first plan's verified hand-on counts for the first.
  A case with one plan counts as before (`TestEarnedCountsOnePlanUnchanged`; on a copy of the real
  store, no case had two plans and the three actions' counts were equal), with one exception: a
  plan whose check handed the case on to a then counts verified at once, where the resolution
  reading counted it only when the case closed verified. A verdict (Right, Wrong) is on the case
  and still stands for each of its plans. Not covered: both meeting actions stay at propose, and
  no production case has had two live plans yet, so the new counts have not been seen on
  production data.
