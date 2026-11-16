# Bugfix phase B: handover brief

Written 2026-11-16 for the agent that takes phase B. Phase A built the form (RFC-0010 §7); phase B finds
out whether it works, and what to change, before anything is widened. **The operator's rule for now:
nothing new and no expansion until what exists is understood and solid.** Read this, then
[RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md) (§8 is the evaluation, §9 what a fix checks),
[the runbook](../operations/bugfix.md) and [SR-0001](../security-reviews/SR-0001-bugfix-coding-harness.md).

## 1. What exists today

The chain, end to end (live since 2026-11-15, all in shadow):

1. **`bugfix/intake`** (hourly, mini): orrery's open issues labelled `bug` or routed by triage become
   `code_bug` cases, one per issue (`bugfix/intake.py`).
2. **`bugfix/fix`** (on a case, X3): the fleetd chain ported to the platform (`bugfix/fix.py`).
   - A **verdict** step (pi, `fleet-bugfix-flash`) decides actionable or needs-info.
   - A **fix** step writes the change and its test, with an **advisor** (`fleet-bugfix-reviewer`)
     for kick-backs, up to 2 rounds.
   - **Gates** (§9): the repro (the fix's test fails on the base, passes with the fix) and the
     related tests (`vitest related`) with no new failures. Not the whole suite.
   - A **model review** (`fleet-bugfix-reviewer`), then a plan: `github.open_pr` with the diff.
3. **Policy** records the plan. At shadow the case closes as a shadow plan.
4. **`bugfix/open-pr`** (on an approved plan, X3 act worker): one commit on `flotilla/<case>`, a draft
   PR, with an installation token narrowed to the plan's repo and contents/PR write.
5. **`bugfix/pr-check`**: merged is verified, closed unmerged is failed.

| Setting | Now |
| --- | --- |
| `bugfix/fix` autonomy | **shadow**: plans are recorded, nothing reaches GitHub |
| `github.open_pr` action | propose (the operator, 2026-11-16); no effect while the routine is at shadow |
| Repos | orrery only (intake's `--repo`) |
| Models | through LiteLLM key `fleet-bugfix` ($100 budget, $4.01 spent); routine cap $5 a day |
| Where | workers on the X3 (RFC-0011), the mini orchestrates |
| GitHub App | `orrery-bugfix-fleet`, reused from the homelab fleet; installed on the operator's 18 repos; flotilla asks only for narrowed tokens |
| Reviewer App | `bugfix-fleet-reviewer` (17 repos): exists, unused by flotilla |

## 2. What it has done (2026-11-15 to 11-16)

- **10 cases:** 4 ended with a PR plan recorded (#648, #651, #656, #659), 4 needs-info from the
  reporter, 2 with no verdict (#657, #661: the verdict step's answer had no JSON).
- **55 fix runs:** 30 ok, 23 skipped (a worker not connected during deploys), 2 interrupted (deploys).
  About 6 minutes a run on the X3; under $0.10 a run in model spend.
- **Nobody has judged the 4 plans.** Whether they are right fixes is unknown; that is phase B's first
  question.

What went wrong in the form, each fixed or open:

| Seen | State |
| --- | --- |
| Tests served on loopback went through the egress proxy and failed | fixed (`NO_PROXY` loopback) |
| orrery's `prepare` script wrote `core.hooksPath` into the shared repo cache; 8 runs escalated | fixed (the config is put back after setup) |
| GitHub refused anonymous blob fetches in a burst (5 runs in 3 minutes); not reproduced since | retry added (30 s, 90 s); cause not proven |
| The verdict step sometimes returns no JSON (#657, #661) | **open**: harness or prompt |
| The same issue got different verdicts on different runs (#646) | **open**: nondeterminism, unmeasured |
| The advisor is not read-only: it started a dev server | **open**: the harness gives it tools it should not have |
| The repro gate rejected a test that did not reproduce the bug | working as meant: a gate catching a weak fix |

## 3. Phase B: what to do

**First, re-evaluate how the chain works**, before tuning it (the operator, 2026-11-16). Questions:

- Is verdict → fix → advisor → gates → review the right shape, or are some steps theatre? (The model
  review has never rejected anything we know of; is it adding value?)
- Are the gates the right evidence? Repro + related tests is cheap; is it enough to trust a PR?
- What does a person need to see to approve a PR plan in a minute?
- What should "needs info" do: ask the reporter on the issue, or stay a case?

**Then measure, as RFC-0010 §8 says,** in eval-harness:

1. **The dataset `orrery-bugfix`:** orrery's closed `bug` issues with the commit that fixed them; that
   commit's tests are the hidden oracle, its parent the base. Frozen in fleet-eval-data. Count how
   many have a linked fix first.
2. **The adapter:** run `bugfix/fix` itself on an item, GitHub stubbed: the routine and the evaluation
   are the same code.
3. **The baseline** (the port as built), then **one change per arm:** the specialist prompts, opencode,
   other worker models (deepseek-v4-pro, Claude), no advisor, the ticket as filed vs rewritten.
4. **The rule:** an arm replaces the baseline only when the harness shows it better on the hidden
   oracle at the same or lower cost, not on a point estimate.
5. **Shadow keeps running beside it:** each shadow case whose issue a person later fixes becomes a new
   item.

Fix the three open items in §2's table as part of this, each with a replay or a test.

## 4. Where points 2 and 3 belong

Both were raised on 2026-11-16 and deliberately not done; phase B decides them.

**Point 2: `bugfix/fix` to propose** (plans wait in Approvals; an approval opens a draft PR).

- What it would mean: review load on the operator (yesterday 3 plans from 8 issues); draft PRs on
  GitHub; nothing merges.
- **The bar before it:** the evaluation shows the baseline (or a better arm) passing the hidden oracle
  at a rate the operator accepts, and the approval view shows what a person needs (§3). Write the bar
  down first.

**Point 3: more repos.** Open `bug` issues on 2026-11-16:

| Repo | Open `bug` | Notes |
| --- | --- | --- |
| podcast_scraper | 35 | Python; heavy ML extras that may not install in the worker; many issues filed by the triage fleet |
| orrery | 11 | the only repo today |
| agentic-ai-homelab | 6 | shell and infra, no test suite: a fix could not be proven |
| eval-harness | 3 | Python |
| 14 others | 0 | nothing to do |

- What it would take: a per-repo profile read by `fix.py` (setup, test command, related-tests
  command, excluded paths), because one acting routine serves the `code_bug` kind. The worker image
  may need a Python toolchain.
- **The bar before it:** orrery solid by §3; then podcast_scraper as its own dataset and profile, in
  shadow first. Watch the triage → bugfix loop: triage files what bugfix fixes.

## 5. Decisions for the operator

1. The bar for point 2 (an oracle pass rate, a cost, a review time).
2. Whether `github.open_pr` stays at propose until then, or goes back to shadow to say "not decided".
3. Whether "needs info" should comment on the issue.
4. The order of repos for point 3, and whether agentic-ai-homelab is ever in scope.
5. Whether the reviewer App has a role (a separate identity for review comments) or is retired.

## 6. Where things are

| What | Where |
| --- | --- |
| The chain | `bugfix/fix.py`, `bugfix/intake.py`, `bugfix/open_pr.py`, `bugfix/pr_check.py`; the bake-off leaves in `bugfix-fleet/bakeoff/` |
| Specs | `fleets/bugfix/*.yaml` |
| Tests | `bugfix/test_*.py` (`make test-unit`) |
| Remote stack | `deploy/remote/` (X3: `~/flotilla-remote`, unit `flotilla-remote.service`) |
| Records | `flotilla case list`, `flotilla runs bugfix/fix`, the fleet page; X3 worker logs `BUGFIX FIX …` lines |
| Phase A history | [bugfix-phase-a.md](bugfix-phase-a.md) |
