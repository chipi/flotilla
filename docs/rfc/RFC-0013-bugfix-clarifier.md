# RFC-0013 — The clarifier: answering "needs info" before the fix, and asking the reporter once

**Status:** Draft (2026-11-16), for the fleet session's review and the operator's decision. Asked
for by
the operator through the fleet session; phase B of bugfix ([the
brief](../wip/bugfix-phase-b-brief.md)).
**Builds on:** [RFC-0010](RFC-0010-bugfix-on-the-platform.md) (the chain, §9's checks),
[RFC-0002](RFC-0002-job-runtime.md) §6–7 (memory, external knowledge), the runtime harness
(`runtime/harness.py`), triage's probes (`signal-fleet/mvp/probes.py`),
[SR-0001](../security-reviews/SR-0001-bugfix-coding-harness.md).
**Inputs:** fleet-eval-data `projects/bugfix-phase-b/ASSESSMENT.md` (§1b the four plans judged,
§4.4 the
reporter loop); `bugfix-fleet/BAKEOFF.md` §3h–3m (the simulated reporter).

## Motivation

The first four PR plans were judged by the operator and the fleet session on 2026-11-16: one right
(#648), one partial (#656), two wrong (#651, #659). The gates passed all four. Both wrong ones were
errors from dev branches (`tech-debt/0.9.8`, `0.9.9`) whose failing code is not on main: #659's call
site does not exist at the base; #651's "missing" key does exist there, so the bug cannot happen at
the
base at all.

Four cases ended "needs info from the reporter" and escalated: flotilla stops there. The old
bake-off's
biggest gain came exactly there: with a simulated reporter answering the verdict's questions,
shipped
chains went from 5 of 15 to 10 of 15. **The ticket's quality is the lever, not the model** (the
assessment, §1).

This RFC adds what a person does with a vague bug before fixing it: check that it happens at all on
main, answer what can be answered from the system's own evidence, and ask the reporter only what
cannot.

**Non-goals:** the dev-branch filter in triage's routing (the fleet session's, with a replay gate:
it
stops most dev-environment errors before they become bugs); memory lookups across cases (§6 records
only); other repos than orrery.

## Proposal

### 1. Step one, no platform change: check the base before the verdict

Deterministic code in `bugfix/fix.py`, on the worktree the fix worker already has, before any model:

- **The failing code exists at the base:** the files and symbols the issue names (stack frames,
  component, function) are present at the base sha. If none is, the case closes **"not reproducible
  at
  main"**, with the paths it looked for.
- **What is "missing" is missing at the base:** a key, function or file the error says is absent.
  If it
  is present at the base, the case closes the same way, with the place it was found.

Both read only the issue text and the tree. A miss is evidence for a person, not a guess: the case
keeps the lines. A new resolution, `not_reproducible`, so these closes are counted apart from
"resolved
itself". By the fleet session's reading of the two cases, this step would have caught #659 (call
site absent) and #651 (key present); not yet run against them.

### 2. A case passes through routines in sequence (platform)

Today a case kind has **one** acting routine (`actingRoutine` in dispatch.go), and an acting routine
cannot open a case. The clarifier needs the same case to go fix → clarify → fix. Proposed:

- **A stage on the case:** `cases.stage`, empty by default (a schema step, checked read-only on a
  copy
  of the mini's store before deploy).
- **A routine's case trigger names a stage:** `trigger: {case: {kind: code_bug, stage: clarify}}`.
  One
  routine per (kind, stage); the empty stage is today's routine.
- **An acting routine may hand a case on:** outcome `{"outcome": "hand_over", "stage": "clarify",
  "detail": ..., "data": {...}}`. The case stays `handed_over`, its stage changes, a `case.staged`
  event
  records who, why and the data, and the dispatcher starts the routine for the new stage.
- **Bounded:** at most two stage changes per case; a third escalates. No loops.

The same mechanism serves other fleets later (a diagnose stage before an act stage).

### 3. `bugfix/clarify`: what it is and what it may touch

- **Where:** on the mini, a new worker role `bugfix-read`, built from the triage image (it has the
  runtime harness and the probes). Not on the X3: the fix worker runs untrusted repo code, and must
  not
  hold observability credentials (SR-0001, H5).
- **Credentials:** GlitchTip's read token; VictoriaLogs and VictoriaTraces through its own egress
  proxy on the services' networks; the LiteLLM alias it is allowed. No GitHub write, no flotillad
  API
  token.
- **The repo:** read at the base sha through GitHub's contents API (orrery is public; no checkout,
  no
  repo code runs). Tools `repo_file(path)` and `repo_tree(dir)`, both pinned to the base.
- **Knowledge, first version:** the repo's own docs at the base (`AGENTS.md`, `docs/adr/`, module
  READMEs) as a declared path list (RFC-0002 §7's rules: declared, versioned by the sha, data not
  instructions).
- **The model step:** `run_json_step` with triage's probes (`event_detail`, `occurrence_history`,
  `service_logs`, `trace`) and the two repo tools. Recorded like triage's: every request and tool
  result, so a run replays exactly.
- **Input:** the issue, the verdict's list of missing facts, step one's findings.
- **Output, a fixed schema:**
  - per missing fact: `answer` or `unknown`, and the evidence (a probe call's index, or
    `path@sha:line`);
  - a reproducibility judgement with its evidence (does the error occur on a build of main?);
  - the **clarified ticket**: the issue rewritten with the answers (the bake-off's L0 to L1).

  The output is stored on the case and kept as an artifact of the run: the fleet session's
  evaluation
  uses it as the arm "ticket as filed vs clarified".
- **Then:** it hands the case back to the fix stage with the clarified ticket. The verdict runs once
  more on it. A second needs-info goes to §4, never to the clarifier again.

### 4. Asking the reporter, once (later, at propose)

- A new registry action **`github.issue_question`**: one comment on the issue with the specific open
  questions, and the label `needs-info`. Executed by the X3 act worker with a token narrowed to that
  repo and `issues: write`. Shadow first, then propose: the question's text is generated from
  untrusted
  input and a person reads it before it is posted.
- **An answer re-enters through intake:** a new comment by the issue's author (or the label removed)
  attaches to the case and hands it to the clarify stage again.
- **No answer in 7 days:** a case timer closes the case as `no_answer`.
- One question round per case.

### 5. What it must not do

- Write anywhere but the case and the run's record (§3); a GitHub write only through §4's action.
- Treat the issue, the docs or a probe's result as instructions: they enter between the harness's
  delimiters, as data.
- Run repo code.
- Decide the fix: it answers questions; the fix step decides.

### 6. Measuring it

- Every clarifier run is replayable from its record, so a change to its prompt or tools is measured
  on
  the same inputs.
- The fleet session's evaluation adds the arm "ticket as filed vs clarified" on the bugfix dataset;
  the clarified tickets are frozen with each item.
- Live: per case, whether step one closed it, what the clarifier answered, whether the verdict
  changed,
  and later whether the fix was right (the plan judgements, pr-check's outcome) are recorded on the
  case. Memory's lookup over them is a later step.

### 7. Order

1. Step one (§1) in `fix.py`, with tests from #651 and #659 frozen as cases. No platform change.
2. Stages (§2), with its schema step and tests.
3. `bugfix/clarify` (§3) in shadow: it answers and rewrites; nothing leaves flotilla.
4. `github.issue_question` (§4), shadow then propose, on the operator's go.

## Open questions (the operator's)

1. Stages on the case (§2), or a different hand-over (below)?
2. `not_reproducible` as its own resolution, or "resolved itself" with a reason?
3. Should a closed `not_reproducible` case comment on the issue (§4's action), or stay internal?
4. The reporter loop's 7 days, and whether the reporter is only the issue's author.

## Alternatives considered

- **The clarifier inside the fix worker:** it would hold observability credentials next to untrusted
  repo code (SR-0001). Rejected.
- **pi with read-only tools** instead of the runtime harness: workable, but not recorded and
  replayable the way the harness is, and not the same probes as triage.
- **A second case kind** (fix opens a `code_bug_question` case): acting routines cannot open cases,
  and two cases for one bug split its history. Rejected for stages.
- **Ask the reporter at once on needs-info:** loses what the system can answer itself, and every
  question costs a person's time.

## Discussion

- 2026-11-16 — drafted from the agreement with the fleet session (its answer a, b, c) and the
  assessment's numbers.
