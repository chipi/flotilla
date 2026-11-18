# The bugfix fleet

**Why this exists:** the bugfix fleet ([RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md)) needs
things no other fleet has: four workers on a machine of their own (remote workers, RFC-0011), a
model key per fix routine, its own egress proxy and, before a PR can be opened, a GitHub App.
This page says what to put where, and how to see what it does. It is in beta evaluation: whether
its fixes are good is RFC-0010 §8, not this page.

## What runs

| Routine | Worker | When | What it does |
| --- | --- | --- | --- |
| `bugfix/intake` | `bugfix` | every hour | a `code_bug` case per open issue of its repo labelled `bug` or `triage-fleet/routed:bugfix` |
| `bugfix/fix` | `bugfix-fix` | a `code_bug` case handed over | the chain: verdict, fixes with the gates, review; a `github.open_pr` plan or an escalation |
| `bugfix/open-pr` | `bugfix-act` | a person approved a `github.open_pr` plan | applies the diff to its base and opens a draft PR from `flotilla/<case>` |
| `bugfix/pr-check` | `bugfix` | the case is in checking | merged: verified; closed without merging: escalated; open: looks again |

`github.open_pr` starts at record in a new installation: plans are recorded on the case and closed
(`plan_recorded`), nothing reaches GitHub. Promoted to propose on Autonomy (after the coding
harness's security review, RFC-0010 §5), plans wait in Approvals. A repo action never goes above
propose. The repos it may act on are the installation's: `action_enums` in `flotillad.yaml`
(`{github.open_pr: {repo: [owner/repo, ...]}}`); the registry declares example repos.

## Per-repo profiles

`bugfix/fix` knows nothing of a repo but its flags, so a repo is a routine spec that sets them
(`fleets/bugfix/fix.yaml` is the example shop's; its defaults are the shop's: npm, vitest). One
acting routine per case kind, so a second repo needs a kind of its own.

| Flag | What it sets | Default (shop) |
| --- | --- | --- |
| `--setup`, `--setup-timeout` | install on the base, run under `sh` in the worktree | `npm ci ...`, 1200 s |
| `--test-cmd` | the test command: `%s` is the report's path, the test files are appended | `npx vitest run ...` |
| `--test-format` | the report's shape: `vitest`, or `pytest` (pytest-json-report) | `vitest` |
| `--related` | `vitest`: `--related-cmd` on the changed source files; `pytest-dirs`: see below | `vitest` |
| `--source-root`, `--test-root` | for `pytest-dirs`: the source tree and the test tree that mirrors it | none |
| `--out-of-scope` | a regex of paths a fix may not touch (added to the built-in sets: CI, hooks, JS config; and under `--test-format pytest`, `PYTEST_OUT_OF_SCOPE`) | none |
| `--exclude-path` | a tree not checked out (sparse) | none |

**The pytest format.** The report is read into the one per-file result the gates use for vitest:
a failed or errored test is a failure `file::name`; a file whose collection failed is unloaded;
**a skipped test is not a pass**: a file whose tests were all skipped (or xfailed) counts as not run
(unloaded), and so does a requested test file the report does not mention. the api's
integration files guard themselves with `pytest.importorskip`, so a broken install skips them, and a
run that only skipped must not look green. The test command should pass
`--continue-on-collection-errors`, or one file that cannot load on the base stops the others.

**The related mode `pytest-dirs`.** For each changed source file under `--source-root`
(`src/pkg/a/b.py`) the tests in the mirrored directory (`<test-root>/a/`) run, through `--test-cmd`
and the same report. A module directly under the source root maps to `<test-root>/test_<module>.py`.
A file with no mirror is logged (`related: no mirrored test directory for ...: skipped`), named in the
plan's checked line, and left out; it never widens to the whole suite. When changed files under the
source root exist and **none** maps to a test directory (only `__init__.py`, or non-`.py` files),
the gate **fails closed**: nothing related ran, so it is a kick-back, not a pass. Otherwise it is the
same as for vitest: no failure that the base did not have.

**The environment a profile gets.** `BUGFIX_VENV` is a directory under the scratch volume
(`venvs/<case id>`, outside the worktree, deleted when the run ends with a plan or an escalation).
`PIP_CACHE_DIR` is the case's own (`runs/<case id>/pip-cache`, deleted with it): a cache shared
between cases would let a wheel that one case's test code planted be installed by the next, so every
case downloads its wheels again. A run that is cut short by a restart, a crash or an error leaves the
case's directory and a checkpoint for the case's next run to continue (ADR-0019); one that ends by its
own timeout, or before its first checkpoint, deletes it, and a directory untouched for 24 hours is
deleted at the next run's start. A run holds a lock on the case's directory (`runs/<case id>/.lock`):
a second run of the case waits up to 120 s for it, then ends as an error with exit code 75 ("another
run of this case is still running"), which flotillad does not count as an attempt.

**The issue as filed.** Intake gives every detection the same share of flotillad's 64 KiB
report (869 bytes, whatever the number of open issues, so a case's data changes only when its
issue does), titles at most 200 bytes; most bodies are cut (the detection's `body_cut`). When a
case's body was cut, the fix step reads it whole from GitHub's public issues API
(`api.github.com`, already in the bugfix proxy's allowlist, no credential, 60 reads an hour per
address) before the verdict. If that read fails twice, the case stops with the reason rather
than being fixed from the first few hundred bytes.

**What a run cost.** `fix.py --spend-file` writes the run's cost to the routine's `spend_file`, so
the day's budget and the case's limit count it. The cost is the gateway's bill: LiteLLM's spend
of each key the run's aliases use, read (`/key/info`, key in the header) before the run and after
it, until it has held still for 20 s (at most 60 s; two reads 5 s apart once agreed before LiteLLM
had recorded a run's last call, its review). pi's own figure is its catalog's list price:
5 to 7 times the bill on 2026-11-15, 1.8 times on #335's run of 2026-11-16, not established in
general, so it is used only when the gateway cannot be read in time. A key another run spends on
meanwhile makes the cost larger, never smaller; a key per routine makes it exact. The run's log
ends with `BUGFIX SPEND ... source=`. A run stopped at the routine's timeout reports too: the
worker's SIGTERM ends the step in flight (each step runs in its own session, which the worker's
kill of the run's group does not reach). A SIGTERM before or during the spend read hurries it:
reads of at most 2 s, all of it within 6 s, else pi's figure, inside the worker's 10 s grace
before its SIGKILL. A cancelled run (a STOP of the fleet,
`flotilla cancel`) is stopped the same way and reports the same.

**The fleet's rules, not the repo's AGENTS.md.** pi loads the repo's `AGENTS.md` into every call
by default: about 30,000 tokens on the shop, 44% of the input tokens of the fleet's sessions on
2026-11-15 and 16. Every pi step (verdict, advisor, fix, review) runs with `--no-context-files`
and the fleet's rules file instead: the routine's declared `rules` prompt
(`prompts: {rules: playbooks/rules.md}`, hash-checked by the run, ADR-0018), else
`bugfix/fleet_rules.md` (`--pi-rules`; `''` keeps pi's default). The rules keep tool
results small: find before reading, ranges of long files, no generated or vendored files, no
`git log -p`. On the shop pi's first request went from 123,326 to 7,082 characters.

**The review judges what it is given.** The reviewer gets the gates' result, the diff, the task
the fix was given (the verdict's analysis), the fix agent's own summary, the issue as filed and the
files the fix touched as they are after it, and no tools (`--review-tools none`, the default): one
call, where an exploring review took 5 to 38 calls on the reviewer model, the fleet's dearest. The
prompt is one argument to pi, which Linux caps at 128 KiB, so the parts are cut to 100,000 bytes,
each cut marked (the fix step's own message too). Only regular files inside the worktree are
read, no further than the room left: a symlink the fix adds (to a key file, say) is named, never
followed. `--review-tools read` gives the reviewer pi's read tools again. No tools is
stricter than read-only, so SR-0001 H8 still holds.

**Judged against the issue as filed.** The review reads the issue first and the verdict's task
last, and is told the task can be wrong and to judge against the issue's own acceptance criteria
(on 2026-11-16/17 every #553 re-verdict's task prescribed a `beforeSend` filter the issue never
asks for, and each review approved it). A one-call review anchors on what it reads first.

**The proof comes before the fix, from another agent (2026-11-18).** After the verdict, a
reproducer (`harnesses/repro.sh`, `agents/repro.md`) writes the failing test from the issue's
evidence, or answers that it cannot and what is needed (the case ends as needs-info, before any
fix session). The chain checks its test: one new test file only (it may not edit a file on
main), no source text read, it imports code of this repository (not only a library), the
failure text it names is copied from the issue, and the test fails on the base with that text.
A wrong value's phrase may be the assertion's message; an error's text must be raised by the
repository's own code (the failure's stack has a frame in the repository's source, not in a
package or the test), so a quoted error is refused. The reproducer stops after
`--repro-max-turns` turns (default 40). The test is then frozen: every fix round starts from
it, a round that changes it fails, and the repro gate runs it alone (tests the fixer adds do
not count). On 2026-11-17 five approved #553 plans were five guesses, each proven by its own
fixer's test; on 2026-11-18 a #553 proof tested `@sentry/browser-utils` and wore the issue's
error as an assertion's label. `--no-repro` gives the old chain (the fixer writes its own
test), for comparing the two.

**The review judges the fix against the frozen proof.** It is told the proof is frozen and not
under review; a request for another test is a note. With a frozen proof, `wrong-place` is a
note in the plan, not a kick-back and not `unresolved` (on 2026-11-18 a #335 review turned
down, as wrong-place, a fix that passes the reference fix's tests: it wanted a panel test the
panel's design makes impossible). Without a frozen proof (`--no-repro`) it still sends the fix
back.

**The chain's own checks on #553's failure (2026-11-17).** Each is deterministic:

- an actionable verdict that lists what is `missing` is needs-info, the verdict prompt's own
  contract (2 of 3 #553 first passes);
- a kick-back verdict whose acceptance criteria name a test file or an assertion in code form
  (`.toBeNull()`, `expect(`) escalates: a criterion the issue does not state (5 of #553's 6;
  none of #335's or #466's);
- the advisor may answer "not locatable in this repository" instead of naming a file, and the case
  then escalates as needs-info at once (on #553 another verdict and round cost $0.52 and $0.88
  to reach the same); a pin goes to the re-verdict as one hypothesis, not as strong evidence;
- a round that removes or rewrites an assertion of a test on main fails as out of scope (below);
- a round whose test reads a source file's text (`readFileSync`, `open`, `read_text` of a
  `.svelte`, `.ts`, `.js` or `.py` named outside an import) fails as test-proves-nothing: such a
  test fails on the base and passes with the fix by construction (#553 on 2026-11-17, approved
  by the review; no other of 18 recorded plans had one);
- a fix episode stops after `--fix-max-turns` assistant turns (default 90): every round whose fix
  passed the oracle took at most 67, #553's round 0s took 105 to 194 and gave no right fix.

**What sends a fix back.** A kick-back costs a whole fix round, so the review sends a fix back
only for a fault it can show from what it was given, and names its class (`hides-symptom`,
`over-broad`, `test-proves-nothing`, `breaks-behaviour`, `wrong-place`), at most once
(`--review-kickback-max 1`; all kick-backs together `--kickback-max 2`). The fix step then gets
the whole review and the diff it judged, without a new verdict. Any other objection goes on the
plan as unresolved; a review that did not run, or ran without a VERDICT line (cut off, timed
out), is said on the plan, never read as an approval. The plan's flag heads the PR body after
the body is cut to length, so the cut never removes it.

**A reply long enough to finish.** pi's catalog gives deepseek-v4-flash a reply limit of 4,096
tokens, where the pinned providers allow 131,072 and more; flash spent all of it reasoning and was
cut off before its answer (5 eval runs on 2026-11-16, and a verdict cut off so ends the case "the
verdict step gave no verdict"). `--min-output 32768` (the default) is the floor every model gets.

**Step one, advisory.** Before any model (RFC-0013 §1, `bugfix/stepone.py`), the chain notes
what main has of the names the error calls missing (`x is not a function`, `x is not defined`,
Python's `name 'x' is not defined`; only names that look like the project's own): whether a
source file the issue names has each one, where else it appears (code, tests and docs alike) or
that it appears nowhere, named files main does not have, and a sign of computed names. It never
closes a case (operator, 2026-11-17): two versions of close rules closed real bugs (an attribute
error on `get`, `undefined is not a function`, a missing import, a computed name), and the cases
they were for, dev-branch errors, are left out by intake and triage already. A git failure is
"not checked". `--no-step-one` skips it.

**A kick-back resumes the fix session.** After a failed gate or a review's request-changes, the
next round puts the worktree back as the last round left it (its diff, re-applied) and continues
that round's pi session (`--session`), with the gates' reason and the verdict's renewed task, or
the whole review, as the next message: no new session exploring the repo from nothing. A resumed
round that adds nothing is "the fix changed nothing since its last round". If the diff does not
apply, or no session file was saved, the round starts a new session as before (`--no-resume`
always does).

**No fix may delete a test (both formats).** A round whose staged diff deletes a test file, or
renames one out of a test path, is rejected as out of scope, naming the file: otherwise a round could
delete the test its change breaks and add a new passing one, and both repro and the related gate would
pass. Nor may it remove or rewrite an assertion of a test on main (B11): a line an assertion pattern
matches (`expect(`, `.toBe…(`, `assert…(`, `self.assert…(`) that the diff removes and does not add
back unchanged. Every #553 filter fix inverted `.not.toBeNull()` to `.toBeNull()`, which alone made
it pass the repro gate; none of the seven #335 fixes touched an assertion on main. A fix that must
correct a wrong assertion escalates, naming the lines.

**What a pytest fix may change (an allowlist).** Under `--test-format pytest` a changed path is
allowed only if it is a `.py` under `--source-root`, or a `test_*.py` / `*_test.py` under
`--test-root`; every other changed file anywhere is rejected, with its name and the rule. Both sides
of a rename are checked (`--no-renames`) and paths are read NUL-separated, so neither a move out of
a forbidden path nor a non-ASCII name slips past. The blocklist below only gives the message for
the usual suspects. At any depth: `conftest.py`,
`sitecustomize.py`, `usercustomize.py`, `*.pth`, `pytest.ini`, `.pytest.ini`, `pytest.toml`,
`.pytest.toml`, `tox.ini`, `.coveragerc`, `setup.py`,
`setup.cfg`, `pyproject.toml`, `Makefile`, `.pre-commit-config.yaml`, `requirements*.txt`,
`constraints*.txt`, `*.lock` and `requirements/`. pytest imports a conftest at collection, in the base
run and the fixed run, with the worker's environment. Such a file is never counted as a test file,
and only `.py` files are handed to pytest.

Python that pytest imports outside a test is out of scope too, because the repo root is first on
`sys.path` (the working directory under `python -m pytest`, and `PYTHONPATH`): a model-added
top-level `json.py` or `pytest_socket.py` would shadow the installed module, a `tests/**/__init__.py`
runs at package collection, and a helper module runs when a test imports it. So under
`--test-format pytest` a changed `.py` must be under `--source-root`, or under `--test-root` **and**
named `test_*.py` or `*_test.py`; anything else is rejected with its file and the rule in the
kick-back. Both roots are therefore required with `--test-format pytest`. A fix that needs a new
helper or an `__init__.py` under the tests is a person's change.

`PYTHONPATH` stays in the profile but removes nothing: `python -m pytest` puts the working
directory on `sys.path` anyway, and the api's `from tests.conftest import ...` needs the repo
root, which `tests/__init__.py` also gets pytest to insert (checked on a scratch package with pytest
and pytest-json-report from an existing venv, not on the api). The shadowing risk is closed
by the scope rule, not by dropping the variable.

**A setup or teardown error is not a failure.** pytest-json-report's outcome `error` (a fixture that
exists only in the fix, say) counts as not run, not as the test failing on the base; only a failed
call phase is a failure.

**The image.** `python3` is bookworm's 3.11.2 and the api requires `>=3.10` at every base
checked. The image has `python3-pip` and `python3-venv` and no compiler: a dependency with no wheel
for it will not build.

A profile for a Python repo (pytest), such as `fix-api.yaml` in an installation's fleets, comes with
an `intake-api`, which detects its case kind (`code_bug_python`). It relies on the repo in
`github.open_pr`'s repos (`action_enums`) and `pypi.org` and `files.pythonhosted.org` in the bugfix
proxy's allowlist.

## Quick reference

```bash
# on the bugfix machine: the remote stack's containers (deploy/remote/README.md)
docker ps --filter name=flotilla-remote
# what it did: the fleet's page in the UI, or
docker exec flotillad /usr/local/bin/flotilla runs bugfix/fix
docker exec flotillad /usr/local/bin/flotilla case list | grep code_bug
```

## Install (once)

The workers run on a machine of their own as remote workers: their stack, the link and the keys
they hold are [deploy/remote/README.md](https://github.com/chipi/flotilla/blob/main/deploy/remote/README.md).
On the control plane's host, in `FLOTILLA_CONFIG`, each secret file created mode 600 and never
printed:

1. **Worker tokens:** `worker-bugfix.token` and `worker-bugfix-act.token`, 32 random bytes as hex each,
   made the way the ops workers' were.
2. **The model keys:** one per fix routine (`fleet-bugfix-fix`, `fleet-bugfix-fix-api`, $1 a
   day each), each alone in its file and mounted only into that routine's remote worker, as
   `/run/flotilla/model-fix.key` and `/run/flotilla/model-fix-api.key`, named by the endpoints
   `gateway-bugfix-fix` and `gateway-bugfix-fix-api`, so a run's spend is its key's alone.
3. **`flotillad.yaml`:**

   ```yaml
   workers:   # remote, on the bugfix machine (deploy/remote/README.md)
     bugfix: {remote: w-bugfix-<host>}                       # intake, intake-api, pr-check: no model
     bugfix-act: {remote: w-bugfix-act-<host>}               # open-pr
     bugfix-fix: {remote: w-bugfix-fix-<host>}               # fix: only fleet-bugfix-fix's key
     bugfix-fix-api: {remote: w-bugfix-fix-api-<host>}   # fix-api: only its own key
   models:
     endpoints:
       gateway-bugfix-fix: {base_url: "http://<gateway-host>:4001/v1", kind: litellm, key_file: /run/flotilla/model-fix.key}
       gateway-bugfix-fix-api: {base_url: "http://<gateway-host>:4001/v1", kind: litellm, key_file: /run/flotilla/model-fix-api.key}
     aliases:
       fix-flash: {endpoint: gateway-bugfix-fix, model: fleet-bugfix-flash}
       fix-reviewer: {endpoint: gateway-bugfix-fix, model: fleet-bugfix-reviewer}
       fix-api-flash: {endpoint: gateway-bugfix-fix-api, model: fleet-bugfix-flash}
       fix-api-reviewer: {endpoint: gateway-bugfix-fix-api, model: fleet-bugfix-reviewer}
   ```

   flotillad's `-check` refuses the bugfix specs without these aliases (security T4).
4. **`egress-bugfix-acl.yaml`** (the bugfix proxy's allowlist; the other workers' `egress-acl.yaml`
   is unchanged):

   ```yaml
   version: v1
   services: []
   default:
     project: flotilla-bugfix
     action: enforce
     allowed_domains: [litellm, github.com, codeload.github.com, api.github.com, registry.npmjs.org,
                       pypi.org, files.pythonhosted.org]   # pypi: fix-api's venv
   ```

   A package whose install fetches a binary from elsewhere is denied, and the denial is logged: add
   the domain only after reading what asked for it.
5. **The remote stack** on the bugfix machine: deploy/remote/README.md (its `.env`, the links'
   enrolment, the keys and the App's private key). The control plane's `COMPOSE_PROFILES` does not
   name `bugfix`: no bugfix worker runs there.

## The GitHub App

A fix's PR plan waits in Approvals once `github.open_pr` and the `bugfix/fix` routine are at
propose, and the approval runs `bugfix/open-pr` on the remote act worker.

- **The App:** the installation's own GitHub App. The App itself may write
  contents, issues and pull requests; **every token `open_pr` asks for is narrowed** to the plan's one
  repository and to contents write, pull requests write and metadata read (`TOKEN_PERMISSIONS` in
  `bugfix/open_pr.py`). Checked against GitHub from the act worker: the narrowed token reached 1
  repository with exactly those permissions.
- **Its private key:** on the bugfix machine in `~/flotilla-remote/app/github-app.pem` (mode 600),
  mounted into the act worker alone (`/run/flotilla/app/`).
- **Its ids** (not secrets): `BUGFIX_GITHUB_APP_ID` and `BUGFIX_GITHUB_APP_INSTALLATION_ID` in the
  remote stack's `~/flotilla-remote/.env`.

Without the App, `bugfix/open-pr` escalates the case ("no GitHub App configured"), and the diff on the
case page can be applied by hand.

## Verification

- `docker exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check` lists
  `github.open_pr` and passes.
- After the first hour, `bugfix/intake` has a run with
  `BUGFIX INTAKE <owner>/<repo>: N open issues` in its log, and the fleet page shows N `code_bug` cases.
- A `bugfix/fix` run ends with `BUGFIX FIX case=… outcome=plan` or `outcome=escalate` and a reason.

## Troubleshooting

- **`the run did not start` or a fix run that never starts:** one fix runs at a time; the other cases
  wait without events. A case handed over for 24 hours is escalated by the stuck-state timer.
- **`the repo's setup failed on the base`:** `npm ci` failed in the worker; read the run's log. A denied
  download is in the bugfix proxy's log (`docker logs flotilla-egress-bugfix`).
- **`the verdict step gave no verdict`:** the triage leaf wrote nothing; its stderr tail is in the
  escalation. A model error (an empty answer) is the usual cause: check LiteLLM's key budget.
- **`unknown: …`** on an approved PR case: the pusher's run ended without a result after it may have
  pushed. It is not run again: look at `flotilla/<case>` on the repo before anything else.
