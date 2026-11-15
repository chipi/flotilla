# Bugfix phase A: the build order

[RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md) §7, phase A: the form and the flow. The fixes
being good is phase B. Each item is one commit with its tests; deploy and push only with the operator's
go.

| # | What | Done when |
| --- | --- | --- |
| A1 | **The execute step for repo actions.** A routine trigger `execute: {action}`: after a person approves a plan for a repo action, flotillad starts that action's executing routine with the case and the approved plan. Its outcome moves the case to checking with a reference (the PR), or escalates it. A check routine on `pr_resolved` reads the PR: merged is verified, closed without merging closes the case as rejected. | Go tests: approval starts the routine; its outcome moves the case; a repo action with no executing routine keeps today's hand-applied form |
| A2 | **`github.open_pr`** in `host/registry.json`: `executor: repo`, params `repo` (enum), `base` (a commit), `diff` (text, bound by the plan's hash), `title`, `body`. `process_change` plans it with a real diff of `host/registry.json` on `chipi/flotilla`; a proposal with no value stays `repo.process_change` (by hand). | the case-review replay gives a `github.open_pr` plan whose diff applies to the registry at the image's commit |
| A3 | **The `bugfix` worker image**: Python, git, jq, Node (orrery's version) and pi `@earendil-works/pi-coding-agent@0.80.10` pinned (the mini's), with pi's `models.json` naming the LiteLLM gateway. The key is read from a file (`"!cat …"`), never from the environment. Compose service `worker-bugfix` (profile `bugfix`): 4 GiB, 2 CPUs, a scratch volume for worktrees. Its egress allowlist: github.com, codeload.github.com, api.github.com, registry.npmjs.org, the gateway. | the image builds on the dev engine; pi answers one prompt through the gateway from inside it |
| A4 | **`bugfix/intake`** (code, every hour): orrery's open issues with the `bug` label (or the routed label), one `code_bug` case per issue (target `repo:chipi/orrery#N`), the issue's number, title and body as the detection's data; a closed issue clears its case. | tests on recorded issue lists |
| A5 | **`bugfix/fix`** (on `code_bug`): `chain.go`'s steps in Python, calling the bake-off's leaves (`triage_run.sh`, `advisor_run.sh`, the pi harness) copied into the image, through LiteLLM. It plans `github.open_pr` with the diff, the gate results and the review, or escalates (needs-info, rejected, stuck) with the reason. | one bake-off bug, end to end to a plan, on the dev engine |
| A6 | **`bugfix/open-pr`** (execute on `github.open_pr`) and **`bugfix/pr-check`** (check on `pr_resolved`): apply the approved diff at its base, push `fleet/fix-…`, open a draft PR with a GitHub App installation token minted for this execution only. | tests against a fake GitHub: the pushed diff is the approved one; a changed diff is refused by its hash; a missing base escalates |
| A7 | **The fleet** `fleets/bugfix/fleet.yaml`; the case page shows the diff; e2e; deploy to the mini at shadow. | bugfix runs in shadow on the mini on orrery's open bugs, its runs on its fleet page |

## Status (2026-11-15)

- **Built and tested, not yet deployed:** A1 (`9935b11`), A2 (`467a29d`, `7606dfd`), A3 (`d86e326`),
  A4 (`1b8d017`), A5 (`b08161b`), A6 (`368d340`). A7: the e2e configuration, the bugfix workers' own
  egress proxy (`egress-bugfix`) and [the operations page](../operations/bugfix.md).
- **Changed while building:**
  - Pushing is a second worker, `bugfix-act`, so no fix run (which runs repo code) can read the
    GitHub key.
  - The bugfix workers have their own egress proxy: the shared allowlist is one list for every
    worker, and GitHub and npm should not be opened to the others.
  - A fix run clones one commit deep, without blobs, sparse: orrery's history is 14 GB and a full
    mirror timed out.
  - The dispatcher no longer records a no-decision for a case whose routine is busy.
- **End to end on the dev engine:** orrery #661, a real open bug, went through the image to a
  reasoned escalation in 3.5 minutes (needs info: a stale dev cache or a real defect).

## Not in phase A

- whether the fixes are any good: the dataset, the oracle check, the baseline's result, the arms (§8);
- the GitHub App: created by the operator before `github.open_pr` leaves shadow (at shadow nothing
  is pushed);
- the security review of the coding harness, due before promotion;
- podcast_scraper as a second repo (its suite and toolchain).
