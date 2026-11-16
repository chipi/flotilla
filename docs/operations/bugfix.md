# The bugfix fleet

**Why this exists:** the bugfix fleet ([RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md)) needs
things on the host that no other fleet has: two workers, a model key of its own, its own egress
proxy and, before a PR can be opened, a GitHub App. This page says what to put where, and how to see
what it does. It is in beta evaluation: whether its fixes are good is RFC-0010 §8, not this page.

## What runs

| Routine | Worker | When | What it does |
| --- | --- | --- | --- |
| `bugfix/intake` | `bugfix` | every hour | a `code_bug` case per open orrery issue labelled `bug` or `triage-fleet/routed:bugfix` |
| `bugfix/fix` | `bugfix` | a `code_bug` case handed over | the chain: verdict, fixes with the gates, review; a `github.open_pr` plan or an escalation |
| `bugfix/open-pr` | `bugfix-act` | a person approved a `github.open_pr` plan | applies the diff to its base and opens a draft PR from `flotilla/<case>` |
| `bugfix/pr-check` | `bugfix` | the case is in checking | merged: verified; closed without merging: escalated; open: looks again |

`github.open_pr` starts at shadow: plans are recorded on the case and closed (`shadow_plan_recorded`),
nothing reaches GitHub. Promote it to propose on Autonomy, after the coding harness's security review
(RFC-0010 §5), for plans to wait in Approvals. A repo action never goes above propose.

## Per-repo profiles

`bugfix/fix` knows nothing of a repo but its flags, so a repo is a routine spec that sets them
(`fleets/bugfix/fix.yaml` is orrery's; its defaults are orrery's: npm, vitest). One acting routine
per case kind, so a second repo needs a kind of its own.

| Flag | What it sets | Default (orrery) |
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
(unloaded), and so does a requested test file the report does not mention. podcast_scraper's
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
(`venvs/<run>`, outside the worktree, deleted when the run ends). `PIP_CACHE_DIR` is the run's own
(`<run>/pip-cache`, deleted with it): a cache shared between runs would let a wheel that one run's
test code planted be installed by the next, so every run downloads its wheels again.

**The issue as filed.** Intake cuts long issue bodies so its whole report fits flotillad's
64 KiB (the detection's `body_cut`). When a case's body was cut, the fix step reads it whole
from GitHub's public issues API (`api.github.com`, already in the bugfix proxy's allowlist, no
credential) before the verdict, and notes it in the chain; if that read fails it keeps the cut
body and says so.

**What a run cost.** `fix.py --spend-file` writes the run's cost to the routine's `spend_file`, so
the day's budget and the case's limit count it. The cost is the gateway's bill: LiteLLM's spend
of each key the run's aliases use, read (`/key/info`, key in the header) before the run and after
it, until two reads 5 s apart agree (at most 60 s). pi's own figure is its catalog's list price:
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
by default: about 30,000 tokens on orrery, 44% of the input tokens of the fleet's sessions on
2026-11-15 and 16. Every pi step (verdict, advisor, fix, review) runs with `--no-context-files`
and `bugfix/fleet_rules.md` instead (`--pi-rules`; `''` keeps pi's default). The rules keep tool
results small: find before reading, ranges of long files, no generated or vendored files, no
`git log -p`. On orrery pi's first request went from 123,326 to 7,082 characters.

**The review judges what it is given.** The reviewer gets the issue as filed, the diff and the
files the fix touched as they are after it, and no tools (`--review-tools none`, the default): one
call, where an exploring review took 5 to 38 calls on the reviewer model, the fleet's dearest. The
prompt is one argument to pi, which Linux caps at 128 KiB, so the parts are cut to 100,000 bytes
(the diff first, then the issue, then the files), each cut marked. `--review-tools read` gives the
reviewer pi's read tools again. No tools is stricter than read-only, so SR-0001 H8 still holds.

**No fix may delete a test (both formats).** A round whose staged diff deletes a test file, or
renames one out of a test path, is rejected as out of scope, naming the file: otherwise a round could
delete the test its change breaks and add a new passing one, and both repro and the related gate would
pass.

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
directory on `sys.path` anyway, and podcast_scraper's `from tests.conftest import ...` needs the repo
root, which `tests/__init__.py` also gets pytest to insert (checked on a scratch package with pytest
and pytest-json-report from an existing venv, not on podcast_scraper). The shadowing risk is closed
by the scope rule, not by dropping the variable.

**A setup or teardown error is not a failure.** pytest-json-report's outcome `error` (a fixture that
exists only in the fix, say) counts as not run, not as the test failing on the base; only a failed
call phase is a failure.

**The image.** `python3` is bookworm's 3.11.2 and podcast_scraper requires `>=3.10` at every base
checked. The image has `python3-pip` and `python3-venv` and no compiler: a dependency with no wheel
for it will not build.

`fleets/bugfix/fix-podcast.yaml` is the profile for chipi/podcast_scraper (pytest). It is
enabled in shadow since 2026-11-16 (ROADMAP B8, the operator's go), with `intake-podcast`, which
detects its case kind (`code_bug_python`). It relies on the repo in `github.open_pr`'s repo enum
(`host/registry.json`) and `pypi.org` and `files.pythonhosted.org` in the bugfix proxy's allowlist.

## Quick reference

```bash
# on the mini: the fleet's containers
docker ps --filter name=flotilla-worker-bugfix --filter name=flotilla-egress-bugfix
# what it did: the fleet's page in the UI, or
docker exec flotillad /usr/local/bin/flotilla runs bugfix/fix
docker exec flotillad /usr/local/bin/flotilla case list | grep code_bug
```

## Install (once, on the host)

All in `FLOTILLA_CONFIG` (`~/flotilla/config` on the mini), each secret file created mode 600 and never
printed.

1. **Worker tokens:** `worker-bugfix.token` and `worker-bugfix-act.token`, 32 random bytes as hex each,
   made the way the ops workers' were.
2. **The model key:** `bugfix-model.key`, the LiteLLM `fleet-bugfix` virtual key alone in the file (on
   the mini it is `LITELLM_FLEET_BUGFIX_KEY` in `~/signal-fleet/fleet-gateway.env`). `secrets.sh` puts
   it in the bugfix worker's volume as `/run/flotilla/model.key`. Each fix routine has its own key as
   well (`fleet-bugfix-fix`, `fleet-bugfix-fix-podcast`, $5 a day each), mounted on the X3 as
   `/run/flotilla/model-fix.key` and `/run/flotilla/model-fix-podcast.key` and named by the endpoints
   `gateway-bugfix-fix` and `gateway-bugfix-fix-podcast`, so a run's spend is its key's alone.
3. **`flotillad.yaml`:**

   ```yaml
   workers:
     bugfix: {url: "http://worker-bugfix:8081", token_file: /run/flotilla/worker-bugfix.token}
     bugfix-act: {url: "http://worker-bugfix-act:8081", token_file: /run/flotilla/worker-bugfix-act.token}
   models:
     endpoints:
       gateway-bugfix: {base_url: "http://litellm:4000/v1", kind: litellm, key_file: /run/flotilla/model.key}
     aliases:
       bugfix-flash: {endpoint: gateway-bugfix, model: fleet-bugfix-flash}
       bugfix-reviewer: {endpoint: gateway-bugfix, model: fleet-bugfix-reviewer}
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
                       pypi.org, files.pythonhosted.org]   # pypi: fix-podcast's venv
   ```

   A package whose install fetches a binary from elsewhere is denied, and the denial is logged: add
   the domain only after reading what asked for it.
5. **`FLOTILLA_HOME/.env`:** add `bugfix` to `COMPOSE_PROFILES`.
6. Deploy as usual (`deploy/mini/deploy.sh`): `secrets.sh` fills the two workers' volumes.

## The GitHub App

`github.open_pr` is at propose since 2026-11-16: a fix's PR plan waits in Approvals (once the
`bugfix/fix` routine is at propose too), and the approval runs `bugfix/open-pr` on the X3's act worker.

- **The App:** `orrery-bugfix-fleet` (id 4354093), the old homelab bugfix fleet's, reused (operator,
  2026-11-16). Installation 147992211 reaches the operator's 18 repositories. The App itself may write
  contents, issues and pull requests; **every token `open_pr` asks for is narrowed** to the plan's one
  repository and to contents write, pull requests write and metadata read (`TOKEN_PERMISSIONS` in
  `bugfix/open_pr.py`). Checked against GitHub from the act worker: the narrowed token reached 1
  repository with exactly those permissions.
- **Its private key:** on the X3 in `~/flotilla-remote/app/github-app.pem` (mode 600), mounted into
  the act worker alone (`/run/flotilla/app/`). The original is kept in `~/flotilla/retired/bugfix-fleet-env/`
  on the mini, with the old fleet's other credentials.
- **Its ids** (not secrets): `BUGFIX_GITHUB_APP_ID` and `BUGFIX_GITHUB_APP_INSTALLATION_ID` in the X3's
  `~/flotilla-remote/.env`.
- The old fleet's second App, `bugfix-fleet-reviewer` (id 4356712, contents read, pull requests write,
  17 repositories), is not used by flotilla.

Without the App, `bugfix/open-pr` escalates the case ("no GitHub App configured"), and the diff on the
case page can be applied by hand.

## Verification

- `docker exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check` lists
  `github.open_pr` and passes.
- After the first hour, `bugfix/intake` has a run with `BUGFIX INTAKE chipi/orrery: N open issues` in
  its log, and the fleet page shows N `code_bug` cases.
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
