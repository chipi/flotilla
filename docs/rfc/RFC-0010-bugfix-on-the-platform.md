# RFC-0010 — Bugfix on the platform: from a bug to a draft PR, as a case

**Status:** Proposed; the six questions taken as proposed, as starting points the evaluation (§8)
may overturn (operator, 2026-11-15). Bugfix is in beta evaluation, not a fleet known to work. Step 4
of the [vision plan](../wip/vision-plan.md).
**Builds on:** [RFC-0002](RFC-0002-job-runtime.md) (the `external` step),
[RFC-0004](RFC-0004-agent-pipeline.md) §8.4 (the `code_bug` case),
[ADR-0016](../adr/ADR-0016-post-incident-review.md) (repo actions, capped at propose),
[ADR-0008](../adr/ADR-0008-autonomy.md) (shadow, propose, the evidence bar).

## What exists (checked 2026-11-15)

- **Two codebases, neither running.** `fleetd chain` (`fleetd/chain.go`) is the deterministic
  orchestrator: it reads issues labelled `triage-fleet/routed:bugfix`, runs the bake-off's `pi` leaves,
  gates the diff, and opens a draft PR at stage `propose`. Its fleetd entry is disabled. The TypeScript
  MVP (`bugfix-fleet/src/`) ran one batch against a sandbox repo and is retired (RFC-0001 §7).
- **The production chain has never run.**
  - The mini has no `~/.bugfix-fleet/real/` (its ledger directory).
  - `chipi/orrery` has no `fleet/fix-*` PR.
  - No issue in orrery, podcast_scraper or flotilla has ever carried the routed label. The label is
    `ROUTE_LABEL` in `signal-fleet/mvp/filing.py`.
  - orrery has 11 open `bug` issues, and podcast_scraper has 35.
- **The bake-off** (`bugfix-fleet/BAKEOFF.md` §8, the 2026-07-26 seat auction):
  - The harness is not the lever. pi and opencode had identical success on every cell (k=3).
  - Claude sonnet-4.6 had the same outcomes at about 20 times the cost. The hard bugs are bounded by
    the spec (ticket quality), not by the model or the harness.
  - The seats chosen were: worker pi + deepseek-v4-flash (the closed loop shipped 3/3), advisor
    glm-5.2, and Claude as the reviewer.
  - pi stops its own grinds. opencode ran to the 1200 s cap and needs an outside budget. pi reports
    zero cost; opencode reports real cost.
  - The closed loop has been measured on pi only.
- **Not built in flotilla:**
  - no `external` step;
  - no `code_bug` kind;
  - no `fleets/bugfix/` directory;
  - no bugfix worker;
  - no `github.open_pr` action.

  The LiteLLM aliases `fleet-bugfix-flash`, `-pro` and `-reviewer` (glm-5.2) exist, under their own
  key with a $100 budget.

## Proposal

### 1. `code_bug` cases from issues

`bugfix/intake` is code only. It runs every hour, reads the open issues with the intake label in each
configured repo, and opens one case per repo and issue (target `repo:chipi/orrery#123`).

- A known issue is absorbed into its case.
- A closed issue closes its case (RFC-0004's mirror).

The issue body is untrusted evidence. v1 configures orrery only, because the bake-off corpus and the
gates were built on it.

### 2. `bugfix/fix`: the chain as a routine

A case trigger on `code_bug`, on the `bugfix` worker. It ports `chain.go`'s steps, keeping its order
and its gates:

1. **Verdict** (flash): actionable, needs-info or rejected, with the reason. needs-info and rejected
   escalate the case with that reason (a person answers the reporter or closes it); nothing is
   written to GitHub in v1.
2. **Fix** (pi + flash through LiteLLM, in the worker's container): a scratch worktree of the repo's
   main, cloned one commit deep, without blobs it does not check out, and sparse (orrery's images,
   audio and LFS masters left out: its history is 14 GB and its tree 6.8 GB). The step is limited by
   a 20-minute wall clock per episode.
3. **Gates**, mechanical, as `chain.go` has them:
   - **repro first:** the diff adds or changes a test file (chain.go's check; that the test fails on
     the base is not checked, as chain.go did not);
   - **the delta suite:** no new failures against the base's red set;
   - **the scope:** the diff stays inside the repo, with no CI or workflow files.
4. **Kick-back:** up to 3 rounds with the advisor (glm-5.2) on a failing gate, then the acceptance
   transition (at the advisor's pin and still failing, the case escalates as stuck).
5. **Review** (`fleet-bugfix-reviewer`): a comment on the diff. It goes in the plan and is shown in
   Approvals; it never blocks.
6. **Plan:** `github.open_pr` with the repo, the base commit, the diff (stored and bound by the plan's
   hash), the title and a body (the reproduction, the gate results, the review).

The day's budget is $5 in the spec. pi reports no cost through a custom provider, so no chain dollar
budget is kept in the routine: LiteLLM's budget on the fleet's key ($100) is the hard bound, and the
rounds (1 + 3) and wall clocks bound a chain.

### 3. `github.open_pr`: the repo write

This is a registry action with `executor: repo`. ADR-0016 caps every repo action at propose, so a
person always approves a PR. Approving it starts the action's executing routine (a routine trigger
`execute: {action}`, run once per plan), `bugfix/open-pr` on the `bugfix-act` worker, the only role
with repo write, which:

1. recomputes the plan's hash and refuses a plan that is not the approved one;
2. checks the base commit still exists and the diff still applies (otherwise it escalates);
3. pushes `flotilla/<case>` and opens a **draft** PR with exactly the approved diff;
4. reports the PR, and the case moves to checking.

`bugfix/pr-check` then reads the PR: merged is verified; closed without merging escalates the case;
open looks again, for 14 days. **The operator merges, always**: there is no merge action, now or
later. A run of the executing routine that ends without a result is escalated as unknown and never
run again (it may have pushed).

`repo.process_change` uses the same action. An approved process change opens a draft PR on
`chipi/flotilla` instead of waiting to be applied by hand. Without an executing routine (the bugfix
fleet not running), the "Applied in commit" form is the fallback.

### 4. The worker and its credentials

- **Image:** `bugfix` with git, Node (orrery's toolchain) and a pinned `pi`. 4 GiB and 2 CPUs. The
  worktree is on a scratch volume, deleted at the end of every run. It has no Docker inside.
- **Egress:** the bugfix workers' own proxy, `egress-bugfix`, with its own allowlist (GitHub's
  github.com, codeload and api, `registry.npmjs.org` for `npm ci`, the LiteLLM gateway). The other
  workers' allowlist is unchanged: opening git and npm to a fleet that runs repo code opens them to
  no one else.
- **The fix step holds no GitHub credential.** It runs the repo's code (installs, tests) and reads an
  untrusted issue, so the `bugfix` worker gets only the model key. Pushing is a second worker,
  `bugfix-act`, which holds the GitHub App's key and no model key, and runs no repo code (it fetches
  the base without a checkout and applies the diff to the index).
- **GitHub:** a GitHub App installed on the target repos (contents, pull requests: write; metadata:
  read). Its private key is a mode-600 file in the `bugfix-act` worker's volume only, with an
  installation token minted per execution (it expires within an hour).
- **Shadow needs no GitHub credential at all:** orrery is public.

### 5. Shadow first, then the evidence bar

`github.open_pr` starts at shadow like every action. The plans, the diffs and the reviews show on the
case page and the bugfix fleet page, and nothing leaves the mini. Promotion to propose is the
operator's, on Autonomy, after the coding harness's security review (T-row in
[security.md](../security.md): it is due before this fleet acts).

### 6. Tests and the replay

- **The port runs the chain's flow.** One of the bake-off's orrery bugs (`bugfix-fleet/bakeoff/bugs/`)
  goes through `bugfix/fix` end to end to a plan. Matching the seat auction's results is phase B (§7).
- **Gate mutants:**
  - a diff with no test;
  - a test that passes on the base;
  - a new failure in the suite;
  - a workflow file in the diff.

  Each must stop the plan.
- **`open_pr` tests:**
  - against a fake GitHub: the approved diff is exactly what is pushed;
  - a changed diff is refused by its hash;
  - a base commit that is gone escalates.
- **Done** (the vision plan): bugfix runs in shadow on the mini on orrery's open bugs, its runs on its
  fleet page.

### 7. Two phases: the form now, making it work later

The operator, 2026-11-15: get the form and the flow right first; making the fixes good (the harness,
the models, the prompts) is a separate activity over the following days and weeks. The success
criterion now is a better place, not working fixes.

- **Phase A, now (step 4's done check):**
  - `code_bug` cases from issues (§1);
  - `bugfix/fix` with its gates and plan (§2);
  - `github.open_pr`, with `process_change` on it (§3);
  - the `bugfix` worker (§4);
  - shadow on the mini, with its runs on the fleet page (§5);
  - the tests (§6).

  The baseline (pi, flash, the generic prompt) runs as it is. Whether its fixes are any good is not
  part of phase A.
- **Phase B, later:**
  - the evaluation (§8): the dataset, the oracle check, the baseline's measured result, the arms;
  - promotion of `github.open_pr` to propose after the security review.

### 8. The evaluation (phase B): does the loop work, and what moves it

The bake-off found every harness and model equal, Claude included. That is not evidence that they are
equal: it measured 5 orrery bug families (with description variants of each) at k of 1 to 3, every cell
was all pass or all fail, and every arm used the same generic 7-line prompt (`BAKEOFF.md` §4.3). A
benchmark like that cannot tell arms apart. Not shown different is not the same as equal. So nothing
the bake-off chose is assumed to work; each is an arm to measure.

The evaluation runs in [eval-harness](https://github.com/chipi/eval-harness), as the triage fleet's
does (`examples/triage-fleet`: a frozen dataset, arms, k runs, paired tests, cost per arm):

- **The dataset: `orrery-bugfix`.**
  - Orrery's closed `bug` issues (40 on 2026-11-15), each with the commit that fixed it where one is
    linked (how many have one is not yet counted).
  - The fix commit's own tests are the hidden oracle, and its parent is the base.
  - Items whose fix has no test are kept for the verdict step only.
  - It is frozen in the private fleet-eval-data, with the bake-off's 5 families as a subset.
  - podcast_scraper's closed bugs follow as a second repo.
- **The adapter** runs `bugfix/fix` itself, the ported code, on an item, with GitHub stubbed. The
  routine and the evaluation are the same code, so a result in the evaluation is a result for the
  fleet.
- **Metrics per item:**
  - the verdict (against the issue's real outcome);
  - the repro gate;
  - the hidden oracle passing;
  - no new suite failures;
  - kick-backs used;
  - cost and wall clock from LiteLLM;
  - for the shipped diffs, a judged comparison with the real fix.
- **Arms, one change each, on the baseline:**
  - baseline: the port as built (pi, flash, the glm advisor, the glm reviewer, the generic prompt);
  - the specialist prompts in `bugfix-fleet/agents/` (written, never measured);
  - opencode;
  - deepseek-v4-pro, and Claude as the worker;
  - no advisor;
  - the ticket's level (as filed vs rewritten by the verdict step).
- **The rule:** an arm replaces the baseline only when the harness shows it better on the primary
  metric (hidden oracle passing, at the same or lower cost), not on a point estimate.

Shadow on live issues (§5) runs beside the evaluation and feeds it: each shadow case whose issue is
later fixed by a person becomes a new item.

## 9. Revision, 2026-11-15: what a fix runs, and where

**Accepted** (operator, 2026-11-15), with the placement as [RFC-0011](RFC-0011-remote-workers.md). Not
built; `bugfix/fix` is paused on the mini until it is.

The first live runs on the mini (shadow, 2026-11-15) showed the port's cost.

- **Every fix ran orrery's whole suite 2 to 5 times:** once on the base for its red set, then once a
  round. That is 7,505 tests in 433 files: 195 s on the X3 at 2 CPUs, more than 20 minutes on the mini,
  where #646 escalated on the suite's timeout. `chain.go` did the same; the port copied it unexamined.
- **The mini is for orchestration and services, not for dev work** (operator, 2026-11-15).

**What a fix checks (proposed).** Only what the fix touches. The PR's own CI runs the whole suite
once, when the operator marks the draft ready.

1. **Repro:** the test files the diff adds or changes fail on the base (with the fix's source files
   reverted) and pass with the fix. This is stronger than today's "a test file is in the diff", which
   `chain.go` never checked either.
2. **Related:** `vitest related <changed source files> --run`: no failure there that the base does not
   have, computed on those files only.
3. **Scope:** as now (nothing in CI, workflow or hook files).

There is no base-wide red set: the baseline is the related tests on the base, run once, in seconds.

**Where it runs (proposed).** The mini orchestrates, the X3 works.

- flotillad stays on the mini and dispatches. `bugfix/intake` and `bugfix/pr-check` (light API reads)
  stay on a mini worker.
- `worker-bugfix` (the fix) and `worker-bugfix-act` (the push), with their egress proxy, run on the
  X3 as one small compose stack on its local Docker engine. Their secrets, the model key and later
  the GitHub App key, live on the X3, mode 600.
- When the X3 is off, its cases wait without events, as a busy routine's do, and the stuck-state timer
  is the only limit.

**Open for the design (each with a proposal):**

- **The transport.** A worker run is one HTTP request held open for the run, and flotillad's only way
  out is its egress proxy. Superseded: the operator asked for placement as a generic capability, on
  any machine. [RFC-0011](RFC-0011-remote-workers.md) proposes remote workers that dial in, as the host
  agents do.
- **The X3's rules** (`infra/x3/AGENTS.md`): its deploy checkout and user units, and the eval-harness
  GPU rule (a fix run uses no GPU).
- **Cost of the move:** a second compose stack to deploy, and the mini's worker-bugfix and its proxy
  removed after it.

Not changed: intake, cases, `github.open_pr`, autonomy, the evidence bar.

## Open questions (the operator's)

1. **Intake label.** The routed label has never been applied, so triage hands nothing over today.
   **Proposed:** the `bug` label (the original RFC-0002 trigger, 11 open in orrery), plus the routed
   label when triage starts applying it. The alternative is the routed label only, which gives shadow
   no input until triage routes something.
2. **Harness.** **Proposed:** pi. The success rates are identical; pi stops its own grinds; the
   closed loop and the advisor were measured only on pi; cost comes from LiteLLM, so pi's zero
   report does not matter. The alternative is opencode (real cost reports, less adapter work, needs
   the outside cap).
3. **Reviewer.** **Proposed:** `fleet-bugfix-reviewer` (glm-5.2) through LiteLLM, so the worker
   holds only its own key. The alternative is Claude (`claude -p` on your account), the bake-off's
   choice: it would put your Claude login inside a container that runs repo code.
4. **GitHub credential.** **Proposed:** a new App `flotilla-bugfix`, which you create (Settings →
   Developer settings), installed on orrery and flotilla. The alternatives:
   - reuse the July App, whose key is `~/.bugfix-fleet/reviewer.pem` on the mini, made for the sandbox;
   - a fine-grained PAT (one token for every repo, no expiry per execution).
5. **`process_change` through `open_pr`.** **Proposed:** in this step (flotilla in the App's
   installation). The alternative is to keep the hand-applied form until bugfix's PRs have a record.
6. **Writing to issues.** `chain.go` labelled issues and commented its verdict. **Proposed:** not in
   v1; the verdict lives on the case. A `github.comment_issue` action (for needs-info questions)
   comes later, at propose too.

## Alternatives considered

- **Run `fleetd chain` as a wrapped command** (as triage's cycle was in phase 2): faster, but its PR
  is opened by the chain itself, not as a plan, so neither autonomy nor approvals would apply. That
  is the point of this step.
- **Port the TypeScript MVP:** it is retired; its harness adapters are stubs.
- **Let the fix step push its own branch:** puts a write token next to untrusted repo code and an
  untrusted issue.

## Discussion

- 2026-11-15: drafted after the operator's go on step 4 ("Yes"), from the bake-off results and the
  checks above.
- 2026-11-15, the operator: "let's not assume that the bug fix fleet works fine … it's in some kind
  of beta evaluation … move what we can and then fully review how it works and actually use the
  tools we have, including Eval Harness, to figure out how to close this loop", and the six
  questions taken as proposed as the starting ground. The evaluation (now §8) added.
- 2026-11-15, the operator: "focus on form and overall flow first, and separate making it work in terms
  of fine-tuning harness and models etc. as a separate follow-up. Success criteria is to get to a
  better place now, but not end up with fixes." §7 added; the evaluation is phase B (§8).
- 2026-11-15, the census for §8's dataset (read-only, GitHub API and each repo's history): closed `bug`
  issues fixed (not duplicates or not planned), with a commit naming them, whose commits touch tests:
  podcast_scraper 177, 142, 136; orrery 28, 17, 8; agentic-ai-homelab 15, 9, 4. None is yet checked
  as an oracle (failing on the parent, passing at the fix).
- 2026-11-15, after the first live runs, the operator: "we have run the entire test suite for a massive
  project, which we cannot do at agent issue level … I want to run heavy agentic work like this on the
  X3 … the mini as orchestrator and services platform, no dev work … minimal cockpit and CI/test effort,
  and indeed we have the PR to validate all." §9 added.
