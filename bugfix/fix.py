"""bugfix/fix: the acting routine for code_bug cases (RFC-0010 §2), fleetd's chain.go on the platform.

Started by flotillad with the case (FLOTILLA_CASE, the issue in its data). It runs the chain's steps in
chain.go's order, calling the bake-off's leaves through LiteLLM, and decides:

  1. verdict (triage_run.sh): actionable, or it escalates (needs-info, rejected: with the reason)
  2. the base: the repo at main (or --base), installed
  3. up to 1 + kickback_max rounds of: a fix (harnesses/<--harness>.sh) from the base, then the gates,
     which run only what the fix touches (RFC-0010 §9: the PR's CI runs the whole suite)
       the scope: a test file in the diff; nothing in CI, hooks, the repo's config, manifest or lockfile
       repro: the fix's test files, on the base with the fix's source left out, load and fail; with the
         fix they pass (a test that cannot load on the base, because it imports what the fix adds, is
         noted, not counted as a failure)
       related: the tests related to the changed source files show no failure the base did not have
     a failing gate kicks it back: the advisor (advisor_run.sh) pins the owner once per round unless the
     fix was already at a pin (the acceptance transition), then the verdict is asked again with that
     evidence; needs-info or rejected escalates
  4. a review of the clean diff: request-changes sends it back once (--review-kickback-max), with the whole
     review and the previous diff, straight to the fix step (no new verdict: the review did not dispute that
     the bug is actionable); a review that did not run is said on the plan, never read as an approval; then:
       {"outcome": "plan", "action": "github.open_pr", "params": {repo, base, title, body, diff}}
     or {"outcome": "escalate", "detail": why}

Nothing leaves the worker: the plan goes to flotillad's policy (shadow records it; at propose a person
approves it and bugfix/open-pr opens the PR). The run's directory on the scratch volume is deleted at the
end, whatever happened; the npm cache stays for the next run.

Whether the fixes are any good is phase B (RFC-0010 §8). This routine is the form: the same steps, gates
and limits as the chain the bake-off measured.

  python3 bugfix/fix.py --repo chipi/orrery --worker-model bugfix-flash --advisor-model bugfix-reviewer \\
      --reviewer-model bugfix-reviewer
Environment: FLOTILLA_CASE, FLOTILLA_PLAN_FILE, FLOTILLA_MODELS (the worker's); BUGFIX_SCRATCH (the
scratch volume); GIT_REMOTE_BASE (default https://github.com, a test's local path).
"""
import argparse
import glob
import json
import os
import re
import shlex
import shutil
import signal
import subprocess
import sys
import time
import traceback

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pi_config  # noqa: E402
import spend  # noqa: E402
import stepone  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
BAKEOFF = os.environ.get("BUGFIX_BAKEOFF_DIR", os.path.join(os.path.dirname(HERE), "bugfix-fleet", "bakeoff"))
# SR-0001 H8: the steps that only read (verdict, advisor, review) get pi's read tools, no shell and no
# writes; "read only" in a prompt did not stop the advisor starting a dev server. The fix step keeps pi's own.
READ_ONLY_TOOLS = "read,grep,find,ls"
REVIEW_PROMPT_MAX = 100_000   # bytes; pi takes the prompt as one argument and Linux caps one at 128 KiB
REQUEST_CHANGES = re.compile(r"VERDICT:\s*\**\s*request[- ]changes", re.I)
VERDICT_LINE = re.compile(r"^[\s*_>#-]*VERDICT:", re.I)
VERDICT_LINE_REQUESTS = re.compile(r"^[\s*_>#-]*VERDICT:\s*\**\s*request[- ]changes", re.I)
# The objections a review can judge from what it is given (C4b, the advisor's review of 2026-11-16): only these send a
# fix back, since a kick-back costs a whole fix round (#335: $0.11 -> $0.31 under C4a for "uninvestigated, unverified,
# invented"); anything else the review raises goes on the plan for the operator.
KICK_CLASSES = ("hides-symptom", "over-broad", "test-proves-nothing", "breaks-behaviour", "wrong-place")
REVIEW_CLASS = re.compile(r"CLASS:\s*\**\s*([a-z-]+)", re.I)
TEST_FILE = re.compile(r"\.(test|spec)\.[cm]?[tj]sx?$|(^|/)tests?/|(^|/)test_[^/]*\.py$")
# CI and hooks, and what `vitest related` cannot see: the repo's config, manifest and lockfile (the
# advisor's review, 2026-11-15); a spec adds its repo's own with --out-of-scope
OUT_OF_SCOPE = re.compile(r"^(\.github/|\.husky/|\.gitlab-ci|\.circleci/)|(^|/)(package\.json|package-lock\.json|"
                          r"npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|tsconfig[^/]*\.json|"
                          r"(vitest|vite|svelte|playwright)\.config\.[cm]?[tj]s)$")
# What a pytest run executes or is configured by, wherever it sits in the tree: conftest.py and the
# sitecustomize/.pth hooks are imported at collection or at interpreter start, with the worker's
# environment; the config, manifest, lock and constraints files decide what is installed and how it runs.
# Under --test-format pytest the scope is an allowlist (pytest_scope); this blocklist only gives its
# error message the more specific reason for these.
PYTEST_OUT_OF_SCOPE = re.compile(r"(^|/)(conftest\.py|sitecustomize\.py|usercustomize\.py|[^/]*\.pth|\.?pytest\.(ini|toml)|tox\.ini|"
                                 r"\.coveragerc|setup\.(py|cfg)|pyproject\.toml|Makefile|\.pre-commit-config\.yaml|"
                                 r"requirements[^/]*\.txt|constraints[^/]*\.txt|[^/]*\.lock)$|^requirements/")
DIFF_MAX = 16000   # the registry's max for github.open_pr's diff
BODY_MAX = 8000
TITLE_MAX = 200


def review_requests_changes(review):
    """The review's decision is its LAST line that starts with `VERDICT:` (the prompt asks it to end with
    one); a review that quotes the instruction or names a rejected alternative mid-text is not a request.
    With no such line, the match anywhere is kept: a reviewer that buried its verdict in a sentence still
    asked for changes, and a spurious kick-back costs one round while a missed one ships the objection."""
    lines = [ln for ln in (review or "").splitlines() if VERDICT_LINE.match(ln)]
    if lines:
        return bool(VERDICT_LINE_REQUESTS.match(lines[-1]))
    return bool(REQUEST_CHANGES.search(_unquoted(review or "")))


def review_class(review):
    """The class the review's last VERDICT line names (CLASS: x), "" without one."""
    lines = [ln for ln in (review or "").splitlines() if VERDICT_LINE.match(ln)]
    m = REVIEW_CLASS.search(lines[-1]) if lines else None
    return m.group(1).lower() if m else ""


def _unquoted(text):
    """The text without what only quotes the instruction: `>` lines, code fences, `inline code` and "double quotes"."""
    text = re.sub(r"```.*?```", " ", text, flags=re.S)
    text = "\n".join(ln for ln in text.splitlines() if not ln.lstrip().startswith(">"))
    text = re.sub(r"`[^`\n]*`", " ", text)
    return re.sub(r"[\"\u201c][^\"\u201d\n]*[\"\u201d]", " ", text)


class Stop(Exception):
    """Ends the chain with an outcome: escalate with a reason."""


class NotReproducible(Exception):
    """RFC-0013 §1: step one found the bug cannot happen on the base; ends the run as not_reproducible."""


class Terminated(BaseException):
    """The worker's SIGTERM at the routine's timeout (a BaseException: no step's error handling may catch it).
    The worker SIGKILLs 10 s later (worker/flotilla_worker.py KILL_GRACE_S), so main has that long to stop
    the step in flight and report the run's spend."""


def cap_bytes(s, n):
    """s cut to at most n UTF-8 bytes, keeping its start, the cut marked."""
    b = s.encode()
    if len(b) <= n:
        return s
    return b[:max(n - 80, 0)].decode(errors="ignore") + f"\n[... cut: {len(b) - n + 80} more bytes]"


def tail(s, n):
    s = (s or "").strip()
    return s if len(s) <= n else "…" + s[-(n - 1):]


def sh(cmd, cwd, env=None, timeout=None, check=False):
    """Run a command in its own process group; (exit code, combined output). A timeout kills the whole
    group and is exit 124: subprocess.run's timeout kills only the direct child, and a timed-out pi or
    vitest under `bash pi.sh` or `sh -c npx` kept running (holding the CPUs, editing the worktree the next
    round resets) until the worker's own run limit (the advisor's review, 2026-11-15)."""
    p = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                         shell=isinstance(cmd, str), start_new_session=True)
    try:
        out, _ = p.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        out, _ = p.communicate()
        if check:
            raise Stop(f"{cmd if isinstance(cmd, str) else ' '.join(cmd)}: no result in {timeout}s")
        return 124, out or ""
    except BaseException:
        # the child has its own session, out of reach of the worker's kill of the run's group: a run stopped
        # at its timeout must not leave a coding agent calling the model
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        raise
    out = out or ""
    if check and p.returncode != 0:
        raise Stop(f"{cmd if isinstance(cmd, str) else ' '.join(cmd)}: exit {p.returncode}: {tail(out, 300)}")
    return p.returncode, out


# A fetch GitHub refused or that broke off: worth trying again. Anonymous fetches were refused in bursts
# on the X3 ("could not read Username": a 401 to an anonymous request), 5 of 8 runs in 3 minutes on
# 2026-11-15 while the same checkout a minute later and the next day succeeded.
FETCH_REFUSED = re.compile(r"could not read Username|unable to access|RPC failed|early EOF|unexpected disconnect|"
                           r"The requested URL returned error: (429|5\d\d)", re.I)
FETCH_RETRY_PAUSES = (30, 90)


ERR_LINE = re.compile(r"(npm (error|ERR!)|gyp ERR!|^error|Error:|ERR_)", re.I)


def errors_of(out, n=400):
    """The error lines of a tool's output, not its last lines: npm ends a failed install with an upgrade
    notice, which is all a tail showed (orrery #490 on the mini, 2026-11-15)."""
    lines = [ln.strip() for ln in (out or "").splitlines() if ERR_LINE.search(ln)]
    return tail(" | ".join(lines), n) if lines else tail(out, n)


class Tests:
    """One test run's result, from a vitest or a pytest JSON report: the failed tests (file::name), the files
    that failed to load or ran no test (vitest: no assertions and a failed status; pytest: a failed
    collection, or every test skipped), and the files that ran."""

    def __init__(self, fails, unloaded, ran):
        self.fails, self.unloaded, self.ran = fails, unloaded, ran


def run_tests(wt, cmd, files, env, timeout, fmt="vitest"):
    """cmd with %s for the report and the files appended. A red run is data, not an error; a run that did
    not finish, or wrote no readable report, is an error (it names which). fmt: the report's shape."""
    report = os.path.join(wt, ".flotilla-suite.json")
    if os.path.exists(report):
        os.remove(report)
    code, out = sh(cmd.replace("%s", report) + "".join(" " + shlex.quote(f) for f in files), wt, env, timeout)
    try:
        if code == 124:
            raise Stop(f"the tests did not finish in {timeout} s: {' '.join(files)[:200]}")
        try:
            with open(report) as f:
                rep = json.load(f)
        except (OSError, ValueError) as e:
            raise Stop(f"the tests wrote no readable report (exit {code}, {e}): {errors_of(out, 300)}")
    finally:
        if os.path.exists(report):
            os.remove(report)
    if fmt == "pytest":
        return parse_pytest(rep, files)
    return parse_vitest(rep, wt)


def parse_vitest(rep, wt):
    fails, unloaded, ran = set(), set(), set()
    for tr in rep.get("testResults") or []:
        name = os.path.relpath(tr.get("name", ""), wt)
        ran.add(name)
        assertions = tr.get("assertionResults") or []
        if not assertions and tr.get("status") == "failed":
            unloaded.add(name)
        for ar in assertions:
            if ar.get("status") == "failed":
                fails.add(name + "::" + ar.get("fullName", ""))
    return Tests(fails, unloaded, ran)


def parse_pytest(rep, files):
    """pytest-json-report's {"tests": [{nodeid, outcome}], "collectors": [{nodeid, outcome}]}. A skipped
    test is not a pass: podcast_scraper's integration files guard themselves with pytest.importorskip, so a
    broken install skips them and a run that only skipped must not look green. A file with no test that
    passed or failed, a collector that did not pass, and a requested file with no result at all are all
    unloaded (not run)."""
    fails, unloaded, seen = set(), set(), {}
    for t in rep.get("tests") or []:
        nodeid = t.get("nodeid") or ""
        file, _, name = nodeid.partition("::")
        outcome = t.get("outcome")
        # "error" is a failed setup or teardown (a fixture that does not exist on the base, say), not a
        # test that ran and failed: it is a failure only when its call phase failed, else not run
        if outcome == "error" and (t.get("call") or {}).get("outcome") == "failed":
            outcome = "failed"
        executed = outcome in ("passed", "xpassed", "failed")
        seen[file] = seen.get(file, False) or executed
        if outcome == "failed":
            fails.add(nodeid)
        elif outcome == "error":
            unloaded.add(nodeid)
    unloaded |= {f for f, executed in seen.items() if not executed}
    for c in rep.get("collectors") or []:
        if c.get("outcome") != "passed":
            unloaded.add((c.get("nodeid") or "").partition("::")[0] or "(collection)")
    unloaded |= {f for f in files if f.endswith(".py") and f not in seen}
    ran = {f for f, executed in seen.items() if executed} - unloaded
    return Tests(fails, unloaded, ran)


def assistant_text(stream):
    """The assistant's final text from a pi --mode json stream: the last message_end of the assistant."""
    text = ""
    for line in (stream or "").splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            e = json.loads(line)
        except ValueError:
            continue
        m = e.get("message") or {}
        if e.get("type") == "message_end" and m.get("role") == "assistant":
            t = "".join(p.get("text") or "" for p in m.get("content") or [] if isinstance(p, dict) and p.get("type") == "text")
            if t.strip():
                text = t
    return text.strip()


def stream_error(stream):
    """The error of a pi stream's last assistant message, "" when it did not end in one."""
    err = ""
    for line in (stream or "").splitlines():
        try:
            e = json.loads(line)
        except ValueError:
            continue
        m = e.get("message") or {}
        if e.get("type") == "message_end" and m.get("role") == "assistant":
            err = (m.get("errorMessage") or "error") if m.get("stopReason") == "error" else ""
    return err


def usage(stream):
    """turns and output tokens of a pi stream (chain.go's parseUsageFull), for the record."""
    turns = out = 0
    for line in (stream or "").splitlines():
        try:
            e = json.loads(line)
        except ValueError:
            continue
        m = e.get("message") or {}
        if e.get("type") == "message_end" and m.get("role") == "assistant":
            turns += 1
            out += (m.get("usage") or {}).get("output") or 0
    return turns, out


def issue_for_case(issue, repo, environ):
    """The issue as filed: intake cuts long bodies so its whole report fits flotillad's 64 KiB (body_cut), and the
    fix step should not reason from a cut report. When the body was cut, read it whole from GitHub's public issues
    API for `repo` (GITHUB_API, default https://api.github.com; the bugfix proxy allows api.github.com, no credential).
    Intake gives every detection the same small share (869 bytes), so most bodies are cut and this read is the fix's
    only full view of the issue: after one retry (BUGFIX_BODY_RETRY_S, 5 s) a failure stops the case, visibly, rather
    than fixing from the first few hundred bytes (homelab's review, 2026-11-16; the unauthenticated API allows 60 reads
    an hour per address). Returns the issue and a note for the chain's log ("" when nothing was done)."""
    if not issue.get("body_cut"):
        return issue, ""
    api = (environ.get("GITHUB_API") or "https://api.github.com").rstrip("/")
    url = f"{api}/repos/{repo}/issues/{int(issue['number'])}"
    why = ""
    for attempt in range(2):
        if attempt:
            time.sleep(float(environ.get("BUGFIX_BODY_RETRY_S") or 5))
        try:
            import urllib.request
            req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json",
                                                       "User-Agent": "flotilla-bugfix-fix"})
            with urllib.request.urlopen(req, timeout=20) as r:
                body = json.load(r).get("body")
        except Exception as e:  # noqa: BLE001 -- any failure: try once more, then stop
            why = f"{type(e).__name__}: {e}"
            continue
        if isinstance(body, str):
            return dict(issue, body=body, body_cut=False), f"issue body: read the full body ({len(body)} chars) from GitHub"
        why = "no body in GitHub's answer"
    raise Stop(f"the issue's body was cut to fit intake's report and could not be read whole from GitHub ({tail(why, 160)}): "
               "not fixing from its first few hundred bytes")


class Chain:
    def __init__(self, a, case, run_dir, environ):
        self.a, self.case, self.run_dir = a, case, run_dir
        self.issue = ((case.get("data") or {}).get("issue")) or {}
        if not self.issue.get("number"):
            raise Stop("the case carries no issue (its detection's data has none)")
        self.repo = self.issue.get("repo") or a.repo
        if self.repo != a.repo:
            raise Stop(f"the case's issue is in {self.repo}, not {a.repo}")
        self.n = int(self.issue["number"])
        self.tid = f"real-{self.repo.split('/')[1]}-{self.n}"
        self.root = os.path.join(run_dir, "bakeoff")       # BAKEOFF_ROOT: the leaves' results
        self.wt = os.path.join(run_dir, "wt")              # BAKEOFF_WT: the worktree
        self.triaged = os.path.join(run_dir, "triaged")    # BAKEOFF_TRIAGED_DIR: the manifests
        self.log = []
        self.issue, body_note = issue_for_case(self.issue, self.repo, environ)
        if body_note:
            self.note(body_note)
        self.rel_targets, self.rel_unmapped, self.rel_considered = [], [], []
        self.venv = os.path.join(a.scratch, "venvs", os.path.basename(run_dir))
        upstream = dict(x.split("=", 1) for x in a.upstream)
        cfg = pi_config.write(os.path.join(run_dir, "pi"), sorted({a.worker_model, a.advisor_model, a.reviewer_model}), environ, upstream)
        self.provider, self.worker = pi_config.pick(cfg, a.worker_model)
        _, self.advisor = pi_config.pick(cfg, a.advisor_model)
        _, self.reviewer = pi_config.pick(cfg, a.reviewer_model)
        self.env = dict(environ)
        # every pi step reads the fleet's rule file, not the repo's AGENTS.md: on orrery that file is ~30k tokens
        # in every call (pi loads it by default), 44% of the input tokens of 2026-11-15/16's sessions
        if a.pi_rules:
            self.env["BUGFIX_PI_RULES"] = a.pi_rules
        else:
            self.env.pop("BUGFIX_PI_RULES", None)
        self.env.update({"PI_CODING_AGENT_DIR": os.path.join(run_dir, "pi"), "PI_PROVIDER": self.provider,
                         "BAKEOFF_ROOT": self.root, "BAKEOFF_WT": self.wt, "BAKEOFF_TRIAGED_DIR": self.triaged,
                         "TRIAGE_MODEL": self.worker, "ADVISOR_MODEL": self.advisor,
                         "PI_MODEL": self.worker, "npm_config_cache": os.path.join(a.scratch, "npm-cache"),
                         "HOME": run_dir,
                         # a Python repo's venv and pip cache: outside the worktree; both are the run's own
                         # and deleted with it (a cache shared between runs would let a wheel that one
                         # run's test code planted be installed by the next)
                         "BUGFIX_VENV": self.venv, "PIP_CACHE_DIR": os.path.join(run_dir, "pip-cache"),
                         # no git hook runs, in any git this run starts: the repo cache is shared between
                         # runs, and a hook planted by one run's code would run in the next (the review,
                         # docs/security-reviews)
                         "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": "/dev/null"})
        self.env.pop("FLOTILLA_PLAN_FILE", None)  # the leaves write nothing of the run's own

    def note(self, step, detail=""):
        line = f"{step}: {detail}" if detail else step
        self.log.append(line)
        print(f"BUGFIX FIX #{self.n} {line}", flush=True)

    # 2 · the base
    def checkout(self):
        """main, one commit deep, its large blobs fetched only for the paths checked out: orrery's history is
        14 GB and its tree 6.8 GB, most of it images the suite does not read. --exclude-path leaves such trees out;
        a test that needs them fails on the base too, so it is in the base's red set, not a new failure."""
        remote = self.env.get("GIT_REMOTE_BASE", "https://github.com").rstrip("/") + "/" + self.repo
        ref = self.a.base or "main"
        # One partial clone per repo on the scratch volume, kept between runs; each run fetches only what is
        # new and checks out a worktree of its own from it, and the large blobs a checkout fetched stay for
        # the next run. Before, each run cloned 850 MB of orrery anonymously, and GitHub refused some of the
        # checkout's fetches that followed one another (#643 and #648 on the X3, 2026-11-15, "could not read
        # Username": a 6 KB request answered 4.5 KB, where #646's same request had been answered 140 MB).
        # blob:limit, not blob:none: with no blobs at all a checkout asks for every file in one batch, and
        # GitHub answered such a request with HTTP 400 (#643 on the mini, 2026-11-15).
        cache = os.path.join(self.a.scratch, "repos", self.repo.replace("/", "__") + ".git")
        self.cache = cache
        if not os.path.isdir(cache):
            os.makedirs(os.path.dirname(cache), exist_ok=True)
            # not --bare: with per-worktree config a bare repo's worktrees inherit core.bare=true
            sh(["git", "init", "-q", cache], self.run_dir, self.env, 60, check=True)
            sh(["git", "-C", cache, "remote", "add", "origin", remote], self.run_dir, self.env, 60, check=True)
            sh(["git", "-C", cache, "config", "extensions.worktreeConfig", "true"], self.run_dir, self.env, 60, check=True)
        sh(["git", "-C", cache, "worktree", "prune"], self.run_dir, self.env, 60)  # a run that died left its worktree
        # what an earlier run's code could have left in the shared cache: hooks, another origin, a URL rewrite
        shutil.rmtree(os.path.join(cache, ".git", "hooks"), ignore_errors=True)
        sh(["git", "-C", cache, "remote", "set-url", "origin", remote], self.run_dir, self.env, 60, check=True)
        _, cfg = sh(["git", "-C", cache, "config", "--local", "--list"], self.run_dir, self.env, 60)
        bad = [ln for ln in cfg.splitlines() if re.match(r"(url\.|core\.(hookspath|fsmonitor|sshcommand)|protocol\.)", ln, re.I)]
        if bad:
            raise Stop(f"the repo cache's config was changed ({'; '.join(bad)[:200]}): remove {cache} by hand")
        sh(["git", "-C", cache, "fetch", "-q", "--depth", "1", f"--filter=blob:limit={self.a.blob_limit}", "origin", ref],
           self.run_dir, self.env, 1200, check=True)
        sh(["git", "-C", cache, "worktree", "add", "-q", "--no-checkout", "--detach", self.wt, "FETCH_HEAD"],
           self.run_dir, self.env, 120, check=True)
        if self.a.exclude_path:
            sh(["git", "-C", self.wt, "sparse-checkout", "set", "--no-cone", "/*"] + [f"!/{x.strip('/')}/" for x in self.a.exclude_path],
               self.run_dir, self.env, 60, check=True)
        self.with_fetch_retry(["git", "-C", self.wt, "checkout", "-q", "--detach", "HEAD"], 1200)
        _, base = sh(["git", "-C", self.wt, "rev-parse", "HEAD"], self.run_dir, self.env, 30, check=True)
        self.base = base.strip()
        self.env["TRIAGE_BASE"] = self.base  # the leaves reset to it
        self.note("base", self.base[:12])
        # The worktrees share the cache's config, and a repo's own scripts write to it: orrery's `prepare`
        # runs `git config core.hooksPath .husky` on every npm ci (8 runs escalated by the check above on
        # the X3, 2026-11-15). Nothing runs from it (GIT_CONFIG_* pins hooksPath for every git the run
        # starts); what must not happen is that it stays for the next run. So the config checked above is
        # put back after the setup and when the run ends, and the check above stays for what got past.
        self.cfg_path = os.path.join(cache, ".git", "config")
        with open(self.cfg_path) as f:
            self.cfg_good = f.read()
        if self.a.setup:
            code, out = sh(self.a.setup, self.wt, self.env, self.a.setup_timeout)
            self.restore_config("setup")
            if code != 0:
                raise Stop(f"the repo's setup failed on the base (exit {code}): {errors_of(out)}")

    def restore_config(self, when):
        """The cache's config back to what checkout() checked; the lines that differed are noted."""
        if not getattr(self, "cfg_path", None):
            return
        with open(self.cfg_path) as f:
            now = f.read()
        if now == self.cfg_good:
            return
        changed = sorted(set(now.splitlines()) ^ set(self.cfg_good.splitlines()))
        with open(self.cfg_path, "w") as f:
            f.write(self.cfg_good)
        self.note(f"{when} changed the repo cache's config; put back", "; ".join(ln.strip() for ln in changed)[:200])

    def with_fetch_retry(self, cmd, timeout):
        """Run a git command that may fetch (a partial clone's checkout fetches the blobs it lacks); a refused
        or broken fetch is tried again after FETCH_RETRY_PAUSES, and one that fails every time stops the run."""
        for pause in (0,) + FETCH_RETRY_PAUSES:
            if pause:
                self.note("fetch refused; trying again", f"in {pause}s")
                time.sleep(pause)
            code, out = sh(cmd, self.run_dir, self.env, timeout)
            if code == 0:
                return out
            if not FETCH_REFUSED.search(out):
                break
        raise Stop(f"{' '.join(cmd)}: exit {code}: {tail(out, 300)}")

    def reset(self):
        sh(["git", "-C", self.wt, "reset", "-q", "--hard", self.base], self.run_dir, self.env, 60, check=True)
        sh(["git", "-C", self.wt, "clean", "-fdq"], self.run_dir, self.env, 60, check=True)  # not -x: node_modules stays

    # 1 · the verdict
    def verdict(self, evidence=None, prior=None, acceptance_gap=False):
        ticket = os.path.join(self.run_dir, "ticket.json")
        with open(ticket, "w") as f:
            json.dump({"id": self.tid, "repo": self.repo.split("/")[1], "level": "L0-real", "source_url": self.issue.get("url", ""),
                       "description": (self.issue.get("title") or "") + "\n\n" + (self.issue.get("body") or ""),
                       "oracle_test_file": "", "code_files": []}, f)
        args = [os.path.join(BAKEOFF, "triage_run.sh"), ticket, "pi"] + ([evidence, prior] if evidence else [])
        env = dict(self.env, ACCEPTANCE_GAP="1" if acceptance_gap else "0", PI_TOOLS=READ_ONLY_TOOLS)
        results = os.path.join(self.root, "results")
        started = time.time() - 1
        code, out = sh(args, BAKEOFF, env, self.a.episode_timeout + 120)
        # the leaf names its directory from the prior verdict's kickback_round, which the model writes:
        # take the verdict this call wrote, not the one the round number predicts
        fresh = []
        for d in os.listdir(results) if os.path.isdir(results) else []:
            f = os.path.join(results, d, "pi", "triage.json")
            if d.startswith(f"{self.tid}-triage") and os.path.exists(f) and os.path.getmtime(f) >= started:
                fresh.append((os.path.getmtime(f), f))
        if not fresh:
            raise Stop(f"the verdict step gave no verdict (exit {code}): {tail(out, 300)}")
        path = max(fresh)[1]
        try:
            with open(path) as f:
                v = json.load(f)
        except (OSError, ValueError):
            raise Stop(f"the verdict step's verdict is not JSON (exit {code}): {tail(out, 300)}")
        kb = re.search(r"-kb(\d+)$", os.path.basename(os.path.dirname(os.path.dirname(path))))
        self.kb_round = int(kb.group(1)) if kb else 0
        return v, path

    # 3 · one fix attempt and its gates
    def sessions(self):
        """{session file: size} of pi's saved sessions in this run (all steps share PI_CODING_AGENT_DIR)."""
        out = {}
        for f in glob.glob(os.path.join(self.run_dir, "pi", "sessions", "*", "*.jsonl")):
            try:
                out[f] = os.path.getsize(f)
            except OSError:
                pass
        return out

    def attempt(self, manifest, resume=None):
        """One fix attempt. `resume` = (session file, the previous round's diff, the feedback): the worktree is put
        back as that round left it and its session goes on with the feedback as the next message, where a new session
        would explore the repo again from nothing (44-99 calls a session on 2026-11-15/16; fleet-eval-data
        projects/bugfix-cost, the advisor's review). If the diff does not apply, a new session from the manifest."""
        with open(manifest) as f:
            desc = json.load(f)["description"]
        note = "\n\nIMPORTANT: write a failing regression test FIRST (repro-first), then fix. Both must be in your final diff."
        self.reset()
        env, resumed_from = self.env, None
        if resume:
            session, patch, feedback = resume
            ok = True
            if patch.strip():
                pf = os.path.join(self.run_dir, "resume.patch")
                with open(pf, "w") as f:
                    f.write(patch)
                code, out = sh(["git", "-C", self.wt, "apply", "--index", "--binary", pf], self.run_dir, self.env, 60)
                ok = code == 0
                if not ok:
                    self.note("resume", f"the previous round's diff does not apply ({tail(out, 120)}): a new session")
                    self.reset()
            if ok:
                desc = feedback + "\n\nThe worktree is as your previous attempt left it." + note
                env, resumed_from = dict(self.env, PI_RESUME_SESSION=session), patch
            else:
                desc += note
        else:
            desc += note
        harness = os.path.join(BAKEOFF, "harnesses", self.a.harness + ".sh")
        if not re.fullmatch(r"[a-z0-9-]+", self.a.harness) or not os.path.exists(harness):
            raise Stop(f"no harness {self.a.harness!r} in {os.path.dirname(harness)}")
        desc = cap_bytes(desc, REVIEW_PROMPT_MAX)   # one argv string to pi: Linux caps it at 128 KiB
        before = self.sessions()
        code, stream = sh([harness, self.wt, desc], BAKEOFF, env, self.a.episode_timeout)
        after = self.sessions()
        grown = [f for f, n in after.items() if before.get(f) != n]
        self.last_session = max(grown, key=os.path.getmtime) if grown else None
        turns, out_tok = usage(stream)
        sh(["git", "-C", self.wt, "add", "-A"], self.run_dir, self.env, 60, check=True)
        # -z: names raw, never git's quoted form for a non-ASCII path; --no-renames: both sides of a rename
        # are listed, so a file moved out of an out-of-scope path is still seen
        _, names = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", "--no-renames", "-z", self.base],
                      self.run_dir, self.env, 60, check=True)
        files = [x for x in names.split("\0") if x.strip() and not x.startswith(".flotilla-suite")]
        _, self.last_patch = sh(["git", "-C", self.wt, "diff", "--cached", "--binary", self.base, "--", ".",
                                 ":(exclude).flotilla-suite.json"], self.run_dir, self.env, 60, check=True)
        stats = (turns, out_tok)
        if resumed_from is not None and self.last_patch == resumed_from:   # the tree still holds the last round's change
            return "the fix changed nothing since its last round", files, stream, stats
        if code == 124 and not files:
            return "the fix episode ran out of time with no change", files, stream, stats
        if not files:
            return "the fix changed nothing", files, stream, stats
        pytest = self.a.test_format == "pytest"
        bad = []
        for x in files:
            if OUT_OF_SCOPE.search(x) or any(re.search(o, x) for o in self.a.out_of_scope):
                bad.append(x)
            elif pytest and x.startswith('"'):   # git's quoted form of an odd name: fail closed
                bad.append(f"{x} (a quoted path name: it cannot be checked against the scope)")
            elif pytest and self.pytest_scope(x):
                bad.append(f"{x} ({self.pytest_scope(x)})")
        # under pytest the scope comes first: a file pytest imports must be named, not hidden behind "no test"
        if bad and pytest:
            return f"out of scope: the diff touches {', '.join(bad)}", files, stream, stats
        # the related gates run what is left: a round that deletes the test its change breaks (or moves it out
        # of a test path: --no-renames lists that as a delete) would pass them
        _, gone = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", "--no-renames", "-z", "--diff-filter=D", self.base],
                     self.run_dir, self.env, 60, check=True)
        deleted = [x for x in gone.split("\0") if x.strip() and TEST_FILE.search(x)]
        if deleted:
            return f"out of scope: a fix may not delete a test: {', '.join(deleted)}", files, stream, stats
        tests = [x for x in files if TEST_FILE.search(x) and os.path.exists(os.path.join(self.wt, x))]
        if pytest:   # only .py files go to pytest; a conftest or config is never a test
            tests = [x for x in tests if x.endswith(".py") and not PYTEST_OUT_OF_SCOPE.search(x)]
        if not tests:
            return "repro-first violated: no test in the diff", files, stream, stats
        if bad:
            return f"out of scope: the diff touches {', '.join(bad)}", files, stream, stats
        return self.gates(files, tests), files, stream, stats

    def pytest_scope(self, x):
        """Why a changed file may not be part of a pytest fix ("" when it may): an ALLOWLIST, a .py under
        --source-root or a test_*.py / *_test.py under --test-root; every other changed file anywhere (config,
        data, docs, helpers) is out. Python that pytest imports outside a test is code that runs with the
        worker's environment: the repo root is on sys.path (cwd under `python -m pytest`, and PYTHONPATH), so a
        top-level json.py or pytest_socket.py shadows the installed module, a tests/ __init__.py runs at
        package collection, a helper module when a test imports it."""
        if PYTEST_OUT_OF_SCOPE.search(x):
            return "pytest config, collection or install file"
        src, tst = self.a.source_root.rstrip("/") + "/", self.a.test_root.rstrip("/") + "/"
        if x.endswith(".py") and x.startswith(src):
            return ""
        if x.startswith(tst) and x.endswith(".py"):
            if re.fullmatch(r"test_[^/]*\.py|[^/]*_test\.py", os.path.basename(x)):
                return ""
            return (f"only test files (test_*.py, *_test.py) may change under {self.a.test_root}: "
                    "__init__.py and helper modules run at import")
        if x.endswith(".py"):
            return (f"a .py outside {self.a.source_root} and {self.a.test_root} is imported by pytest and may shadow "
                    "an installed module")
        return (f"only a .py under {self.a.source_root} or a test_*.py / *_test.py under {self.a.test_root} may change: "
                "config, data and other files are a person's")

    def gates(self, files, tests):
        """RFC-0010 §9: repro (the tests fail on the base, pass with the fix) and related (no failure the base
        did not have, in the tests related to the changed source files). Leaves the tree as the fix made it."""
        patch = os.path.join(self.run_dir, "fix.patch")
        _, d = sh(["git", "-C", self.wt, "diff", "--cached", "--binary", "--no-renames", self.base], self.run_dir, self.env, 60, check=True)
        with open(patch, "w") as f:
            f.write(d)
        src = [x for x in files if not TEST_FILE.search(x)]
        t = self.a.test_timeout

        def tree(with_source):
            self.reset()
            args = ["git", "-C", self.wt, "apply", "--binary"]
            if not with_source:
                args += [f"--include={x}" for x in tests]
            sh(args + [patch], self.run_dir, self.env, 60, check=True)

        tree(with_source=False)
        base = self.tests(self.a.test_cmd, tests)
        base_src = [x for x in src if os.path.exists(os.path.join(self.wt, x))]
        base_rel = self.related(base_src, say=False)
        tree(with_source=True)
        fixed = self.tests(self.a.test_cmd, tests)
        rel = self.related(src, say=True)
        self.unloaded_on_base = sorted(base.unloaded)
        for x in tests:
            if x not in base.unloaded and not any(f.startswith(x + "::") for f in base.fails):
                return f"repro: {x} passes on the base without the fix: it does not reproduce the bug"
        # RFC-0010 §9: "a repro test must load and fail on the base". A file that cannot load there shows no
        # behaviour; it is noted, and at least one other test must load and fail.
        if all(x in base.unloaded for x in tests):
            return (f"repro: no test loads and fails on the base ({', '.join(sorted(tests))} does not load there): "
                    "nothing shows the bug before the fix")
        if fixed.unloaded or fixed.fails:
            return f"the fix's own tests fail with the fix: {tail('; '.join(sorted(fixed.unloaded | fixed.fails)), 300)}"
        if self.a.related == "pytest-dirs" and self.rel_considered and not self.rel_targets:
            return (f"related: no changed source file under {self.a.source_root} maps to a test directory under "
                    f"{self.a.test_root} ({', '.join(self.mirror_hint(x) for x in self.rel_unmapped)}): no related test ran, "
                    "so nothing shows the change breaks none")
        new = sorted((rel.fails | rel.unloaded) - (base_rel.fails | base_rel.unloaded))
        if new:
            return f"{len(new)} new failures in the related tests: {tail('; '.join(new), 300)}"
        self.checked = f"{len(tests)} test files fail on the base and pass with the fix; {len(rel.ran)} related files, none newly failing"
        if self.rel_unmapped:
            self.checked += f"; no mirrored test directory for {', '.join(self.rel_unmapped)}"
        return ""

    def tests(self, cmd, files):
        return run_tests(self.wt, cmd, files, self.env, self.a.test_timeout, self.a.test_format)

    def mirror(self, src):
        """pytest-dirs: (the existing mirrored test paths, the changed files under the source root with none,
        the changed files under the source root). src/<root>/a/b.py -> <test-root>/a/; a module straight under
        the source root -> <test-root>/test_<module>.py; a non-.py file has no mirror."""
        root, troot = self.a.source_root.rstrip("/") + "/", self.a.test_root.rstrip("/")
        targets, unmapped, considered = set(), [], [x for x in src if x.startswith(root)]
        for x in considered:
            rel = x[len(root):]
            t = None
            if x.endswith(".py"):
                parent = os.path.dirname(rel)
                t = os.path.join(troot, parent) if parent else os.path.join(troot, "test_" + os.path.basename(rel))
            if t and os.path.exists(os.path.join(self.wt, t)):
                targets.add(t)
            else:
                unmapped.append(x)
        return sorted(t for t in targets if not any(t.startswith(o + "/") for o in targets)), unmapped, considered

    def mirror_path(self, x):
        """Where the tests of source file x belong: its mirror under --test-root ("" for a non-.py file)."""
        if not x.endswith(".py"):
            return ""
        rel = x[len(self.a.source_root.rstrip("/") + "/"):]
        parent = os.path.dirname(rel)
        troot = self.a.test_root.rstrip("/")
        return os.path.join(troot, parent) + "/" if parent else os.path.join(troot, "test_" + os.path.basename(rel))

    def mirror_hint(self, x):
        return f"{x}: its tests belong in {self.mirror_path(x)}" if self.mirror_path(x) else f"{x}: a non-.py file has no test mirror"

    def related(self, src, say):
        """The tests related to the changed source files. vitest: --related-cmd on them. pytest-dirs: --test-cmd
        on the mirrored test directories (see mirror); a file with no mirror is noted and left out, never
        widened to the whole suite, and when none maps the gate fails (gates). say: the final tree's call."""
        if not src:
            return Tests(set(), set(), set())
        if self.a.related != "pytest-dirs":
            return run_tests(self.wt, self.a.related_cmd, src, self.env, self.a.test_timeout, self.a.test_format)
        targets, unmapped, considered = self.mirror(src)
        if say:
            self.rel_targets, self.rel_unmapped, self.rel_considered = targets, unmapped, considered
            for x in unmapped:
                self.note("related", f"no mirrored test directory for {x}: skipped")
            if targets:
                self.note("related", ", ".join(targets))
        if not targets:
            return Tests(set(), set(), set())
        return self.tests(self.a.test_cmd, targets)

    def diff(self):
        sh(["git", "-C", self.wt, "add", "-A"], self.run_dir, self.env, 60, check=True)
        _, d = sh(["git", "-C", self.wt, "diff", "--cached", "--no-renames", self.base, "--", ".", ":(exclude).flotilla-suite.json"],
                  self.run_dir, self.env, 60, check=True)
        return d

    def review_prompt(self, diff, task="", summary=""):
        """The review's whole input: the issue as filed, the diff, and the files the fix touched as they are after it.
        With --review-tools none the reviewer judges from this alone, in one call: on 2026-11-15/16 a review explored
        the repo for 5 to 38 calls on glm-5.2, the fleet's dearest model (fleet-eval-data projects/bugfix-cost, C4).
        pi takes the prompt as one argument, which Linux caps at 128 KiB (MAX_ARG_STRLEN): the parts are cut to fit
        REVIEW_PROMPT_MAX, the diff first, then the issue, then the files, each cut said where it is."""
        head = (f"Review this automated fix for {self.repo}#{self.n} ({self.issue.get('title', '')}).\n\n")
        ask = ("\n\nGive a concise review: correctness risks, edge cases the fix misses, test adequacy, and whether it fixes "
               "the issue as filed rather than hiding its symptom. Ask for changes only for a fault you can show from what "
               "is above, and name its class: " + ", ".join(KICK_CLASSES) + ". Write anything else (what could not be "
               "checked from here, style, follow-ups) as notes and approve. End with one line: VERDICT: approve, or "
               "VERDICT: request-changes CLASS: <one of the classes>.")
        if self.a.review_tools == "none":
            ask = "\n\nYou have no tools: judge from what is above." + ask
        room = REVIEW_PROMPT_MAX - len((head + ask).encode())

        def cut(text, n):
            b = text.encode()
            return text if len(b) <= n else b[:max(n - 80, 0)].decode(errors="ignore") + f"\n[... cut: {len(b) - n + 80} more bytes]"

        parts = []
        gates = getattr(self, "checked", "") or ""
        if gates:
            parts.append(("THE GATES (run by the chain on this diff): ", gates + "\n\n"))
            room -= len(gates.encode()) + 60
        parts.append(("DIFF:\n", cut(diff, min(room, 40000))))
        room -= len(parts[-1][1].encode()) + 20
        if task:
            parts.append(("\n\nTHE TASK THE FIX WAS GIVEN (the verdict step's analysis):\n", cut(task, min(room, 6000))))
            room -= len(parts[-1][1].encode()) + 70
        if summary:
            parts.append(("\n\nTHE FIX AGENT'S OWN SUMMARY:\n", cut(summary, min(room, 3000))))
            room -= len(parts[-1][1].encode()) + 40
        body = self.issue.get("body") or ""
        parts.append(("\n\nTHE ISSUE AS FILED:\n", cut(body, min(room, 8000))))
        room -= len(parts[-1][1].encode()) + 30
        _, names = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", "--no-renames", "-z", "--diff-filter=d", self.base],
                      self.run_dir, self.env, 60)
        files = []
        for x in [n for n in names.split("\0") if n.strip() and not n.startswith(".flotilla-suite")]:
            if room < 200:
                files.append(f"=== {x} === [not included: no room left]")
                continue
            path = os.path.join(self.wt, x)
            # only a regular file, read no further than the room left: a fix that adds a symlink to the routine's key
            # file (/run/flotilla/model-*.key) must not put the key in a prompt the provider sees, and a FIFO or
            # /dev/zero must not block or fill the run (homelab's review of the series, 2026-11-16)
            inside = os.path.realpath(path).startswith(os.path.realpath(self.wt) + os.sep)
            if os.path.islink(path) or not inside or not os.path.isfile(path):
                files.append(f"=== {x} === [not included: not a regular file]")
                room -= len(x) + 60
                continue
            try:
                with open(path, "rb") as f:
                    text = f.read(max(room, 0) + 1).decode("utf-8", errors="replace")
            except OSError:
                continue
            piece = cut(text, room - len(x) - 20)
            files.append(f"=== {x} ===\n{piece}")
            room -= len(piece.encode()) + len(x) + 20
        if files:
            parts.append(("\n\nTHE CHANGED FILES, AFTER THE FIX:\n", "\n".join(files)))
        return head + "".join(a + b for a, b in parts) + ask

    def review(self, diff, task="", summary=""):
        prompt = self.review_prompt(diff, task, summary)
        rules = ["--no-context-files", "--append-system-prompt", self.env["BUGFIX_PI_RULES"]] if self.env.get("BUGFIX_PI_RULES") else []
        if self.a.review_tools == "none" and rules:   # the rules are about using tools; a review without them has none
            rules = ["--no-context-files"]
        tools = ["--no-tools"] if self.a.review_tools == "none" else ["--tools", READ_ONLY_TOOLS]
        code, stream = sh(["pi", "-p", "--mode", "json", "--provider", self.provider, "--model", self.reviewer,
                           *tools, *rules, prompt],
                          self.wt, self.env, self.a.review_timeout)
        text = assistant_text(stream)
        if text:
            return text
        err = stream_error(stream)
        return f"(no review: {tail(err, 300)})" if err else f"(no review: the reviewer episode ended {code} with no text)"

    def run(self):
        self.kept = None   # the last gate-clean round the review sent back: (rnd, diff, review, checked, unloaded)
        try:
            return self.rounds()
        except (Stop, OSError, subprocess.SubprocessError, ValueError) as e:
            if not self.kept:
                raise
            # a good fix is not thrown away because a later round failed (a gate, the verdict, or an error a
            # round raised): the operator gets the last clean one with the review's requested changes in it,
            # and the reason the later rounds ended
            rnd, d, review, checked, unloaded = self.kept
            where = ""
            if not isinstance(e, Stop):   # an error a round raised may be a bug: keep where it came from
                where = " | " + tail(" | ".join(traceback.format_exc().strip().splitlines()[-20:]), 1500)
            self.note("kept", f"round {rnd}'s clean fix; the run ended ({type(e).__name__}): {tail(str(e), 300)}{where}")
            return self.plan(rnd, d, review, checked, unloaded,
                             unresolved="the later rounds did not resolve what the review asked for")

    def rounds(self):
        self.checkout()
        # 0 · step one (RFC-0013 §1): can the bug happen on the base at all? Deterministic, before any model
        if self.a.step_one:
            decision, evidence = stepone.check((self.issue.get("title") or "") + "\n\n" + (self.issue.get("body") or ""),
                                               self.wt, self.base)
            self.note("step one", evidence.removeprefix("step one: "))
            if decision == "close":
                raise NotReproducible(evidence)
        # 1 · the verdict, on the base
        v, vpath = self.verdict()
        self.note("verdict", v.get("verdict", "none"))
        if v.get("verdict") != "actionable":
            raise Stop(self.not_actionable(v))
        manifest = os.path.join(self.triaged, f"{self.tid}-triaged.json")
        pins, prior, reviewed_back, resume = [], vpath, 0, None
        self.last_session, self.last_patch = None, ""
        for rnd in range(self.a.kickback_max + 1):
            if not os.path.exists(manifest):
                raise Stop(f"stuck: the verdict step wrote no triaged manifest for round {rnd}")
            reason, files, stream, (turns, out_tok) = self.attempt(manifest, resume)
            resume = None
            from_review = False
            if not reason:
                d = self.diff()
                if len(d) > DIFF_MAX:
                    raise Stop(f"the clean fix's diff is {len(d)} characters, more than the {DIFF_MAX} a plan may carry")
                with open(manifest) as mf:
                    task = json.load(mf).get("description") or ""
                review = self.review(d, task, assistant_text(stream or ""))
                self.note("review", tail(review.splitlines()[-1] if review else "", 80))
                if review.startswith("(no review"):   # a failed review is not an approval: the plan says so
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review, unresolved="the review did not run: " + tail(review, 200))
                if not any(VERDICT_LINE.match(ln) for ln in review.splitlines()):
                    # text without a verdict (a cut-off or timed-out review) is not an approval either
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review, unresolved="the review gave no verdict: " + tail(" ".join(review.split()), 200))
                # the review saw what the gates did not on 3 of phase A's 4 plans: request-changes sends the fix
                # back while the budget lasts; after that the operator sees the plan with the review in it
                if review_requests_changes(review) and review_class(review) not in KICK_CLASSES:
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review, unresolved=(
                        f"the review asked for changes it cannot show from the diff (class {review_class(review) or 'none'}), "
                        "so the fix was not sent back"))
                if review_requests_changes(review) and reviewed_back < self.a.review_kickback_max \
                        and rnd < self.a.kickback_max:
                    reviewed_back += 1
                    self.kept = (rnd, d, review, self.checked, self.unloaded_on_base)
                    from_review = True
                    # one line: the reason goes into result.tsv and the chain's log; the fix step gets the whole review
                    reason = "the review requested changes: " + tail(" ".join(review.split()), 400)
                    self.note(f"round {rnd}", reason)
                    manifest = self.review_manifest(manifest, rnd, d, review)
                    if self.a.resume and self.last_session:
                        resume = (self.last_session, self.last_patch,
                                  f"A REVIEW OF YOUR FIX ASKED FOR CHANGES. Address what it says that holds; if a point is "
                                  f"wrong, keep your fix and say why in the test or a comment.\n\nTHE REVIEW:\n{review[:12000]}")
                    continue
                else:
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review, unresolved=(
                        "the review's kick-back budget was spent, so nothing changed after it"
                        if review_requests_changes(review) else ""))
            self.note(f"round {rnd}", reason)
            if rnd == self.a.kickback_max:
                raise Stop(f"stuck: kick-back limit ({self.a.kickback_max}) reached; last: {reason}")
            # the kick-back evidence, as chain.go writes it for the leaves
            ev = os.path.join(self.root, "results", f"{self.tid}-r{rnd}")
            os.makedirs(ev, exist_ok=True)
            touched = [x for x in files if not TEST_FILE.search(x)]
            with open(os.path.join(ev, "touched.txt"), "w") as f:
                f.write("".join(x + "\n" for x in touched))
            with open(os.path.join(ev, "harness.json"), "w") as f:
                f.write(stream or "")
            with open(os.path.join(ev, "result.tsv"), "w") as f:
                f.write(f"{self.tid}\tpi\tFAIL ({reason})\t0\t0\t{turns}\t{out_tok}\tno\t0\n")
            gap = next((p for p in pins if p in touched), "")
            if gap:
                self.note("acceptance gap", f"fixed at the pin {gap} and still failing")
            elif self.a.advisor and not from_review:   # a review's objection is its own evidence: no pin needed
                code, out = sh([os.path.join(BAKEOFF, "advisor_run.sh"), os.path.join(self.run_dir, "ticket.json"), ev],
                               BAKEOFF, dict(self.env, PI_TOOLS=READ_ONLY_TOOLS), self.a.episode_timeout)
                try:
                    with open(os.path.join(ev, "advisor.json")) as f:
                        pin = json.load(f).get("file") or ""
                except (OSError, ValueError):
                    pin = ""
                self.note("advisor", pin or f"no usable pin (exit {code})")
                if pin:
                    pins.append(pin)
            kv, kpath = self.verdict(ev, prior, bool(gap))
            if gap and kv.get("verdict") == "actionable":  # acceptance mode may not re-pin (chain.go's downgrade)
                kv = dict(kv, verdict="needs-info")
                self.note("downgrade", "an acceptance gap's verdict came back actionable: needs-info")
            if kv.get("verdict") != "actionable":
                raise Stop(self.not_actionable(kv, after=f"after round {rnd}"))
            manifest = os.path.join(self.triaged, f"{self.tid}-triaged-kb{self.kb_round}.json")
            prior = kpath
            if self.a.resume and self.last_session and os.path.exists(manifest):
                try:
                    with open(manifest) as f:
                        task = json.load(f).get("description") or ""
                    resume = (self.last_session, self.last_patch,
                              f"YOUR FIX DID NOT PASS ITS GATES: {reason}\n\nTHE TASK, AS THE VERDICT STEP NOW SEES IT:\n{task}")
                except (OSError, ValueError, AttributeError) as e:   # the next round starts a new session instead
                    self.note("resume", f"the renewed task could not be read ({type(e).__name__}): a new session")

    def review_manifest(self, manifest, rnd, d, review):
        """The next fix's instructions after a review's request-changes: the same task, the whole review and the
        diff it judged (the next attempt starts from the base again). No new verdict: it re-ran ~30 calls of
        exploration to re-judge actionability, which the review did not dispute, and the review reached the fix
        step only as 400 characters of result.tsv through it (fleet-eval-data projects/bugfix-cost, the advisor's
        review: a likely reason #553's later rounds changed nothing)."""
        with open(manifest) as f:
            m = json.load(f)
        m["description"] = (m.get("description") or "") + (
            f"\n\nA REVIEW OF YOUR PREVIOUS ATTEMPT (round {rnd}) ASKED FOR CHANGES. Address what it says that holds; "
            f"if a point is wrong, keep your fix and say why in the test or a comment.\n\nTHE REVIEW:\n{review[:12000]}"
            f"\n\nTHE DIFF IT REVIEWED (the worktree is back at the base):\n{d[:16000]}")
        out = os.path.join(self.triaged, f"{self.tid}-triaged-review{rnd}.json")
        with open(out, "w") as f:
            json.dump(m, f)
        return out

    def not_actionable(self, v, after=""):
        verdict = v.get("verdict") or "none"
        missing = v.get("missing") or v.get("Missing") or []
        why = "; ".join(str(m) for m in missing) if isinstance(missing, list) else str(missing)
        head = {"needs-info": "needs info from the reporter", "reject": "rejected as not a bug",
                "rejected": "rejected as not a bug"}.get(verdict, f"no usable verdict ({verdict})")
        return " ".join(x for x in [head, after, (": " + why) if why else ""] if x).replace(" :", ":")

    def plan(self, rnd, d, review, checked=None, unloaded=None, unresolved=""):
        checked = self.checked if checked is None else checked
        unloaded = self.unloaded_on_base if unloaded is None else unloaded
        title = f"fix #{self.n}: {self.issue.get('title', '')}"[:TITLE_MAX]
        flag = (f"unresolved: the review did not run; round {rnd}'s clean fix kept"
                if unresolved.startswith("the review did not run") else
                f"unresolved: the review gave no verdict; round {rnd}'s clean fix kept"
                if unresolved.startswith("the review gave no verdict") else
                f"unresolved: the review raised points it cannot show from the diff; round {rnd}'s clean fix kept"
                if unresolved.startswith("the review asked for changes it cannot show") else
                f"unresolved: the review asked for changes; round {rnd}'s clean fix kept")
        head = f"**{flag}**: {unresolved} (see the review below).\n\n" if unresolved else ""
        body = "\n".join([
            f"An automated fix for {self.repo}#{self.n} by flotilla's bugfix fleet (RFC-0010), in beta evaluation.",
            "", f"Gates (base {self.base[:12]}, round {rnd}): {checked}; nothing in CI, hooks, config or the lockfile. "
            "The whole suite is this PR's CI.",
            *([f"Not loadable on the base (they import what the fix adds): {', '.join(unloaded)}."]
              if unloaded else []),
            "", "The chain:", *[f"- {x}" for x in self.log],
            "", "Review (fleet reviewer, comment only):", tail(review, 3000),
            "", "The operator merges, always: the fleet never does."])
        return {"outcome": "plan", "action": "github.open_pr",
                "detail": f"#{self.n}: " + (f"{flag}; " if unresolved else "") + f"a clean fix in round {rnd} at {self.base[:12]}",
                # the flag goes on after the cut: tail() keeps the end, and it must never cut the flag away
                "params": {"repo": self.repo, "base": self.base, "title": title,
                           "body": head + tail(body, BODY_MAX - len(head)), "diff": d}}


def parse(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--worker-model", required=True, help="an alias of FLOTILLA_MODELS")
    ap.add_argument("--advisor-model", required=True)
    ap.add_argument("--reviewer-model", required=True)
    ap.add_argument("--setup", default="npm ci --no-audit --no-fund && (npm run --if-present i18n:compile)")
    ap.add_argument("--test-cmd", default="npx vitest run --reporter=json --outputFile=%s", help="%%s: the report; the files follow")
    ap.add_argument("--related-cmd", default="npx vitest related --run --reporter=json --outputFile=%s")
    ap.add_argument("--test-format", choices=("vitest", "pytest"), default="vitest",
                    help="the shape of the report --test-cmd writes: vitest's JSON, or pytest-json-report's")
    ap.add_argument("--related", choices=("vitest", "pytest-dirs"), default="vitest",
                    help="vitest: --related-cmd on the changed source files; pytest-dirs: --test-cmd on the test "
                         "directories that mirror them (--source-root, --test-root)")
    ap.add_argument("--source-root", default="", help="pytest-dirs: the source tree the mirror starts at, e.g. src/pkg")
    ap.add_argument("--test-root", default="", help="pytest-dirs: the test tree that mirrors it, e.g. tests/unit/pkg")
    ap.add_argument("--out-of-scope", action="append", default=[], help="a regex of paths a fix may not touch")
    ap.add_argument("--base", default="", help="the commit to fix (default: main), for replays (phase B)")
    ap.add_argument("--harness", default="pi", help="harnesses/<name>.sh (phase B arms)")
    ap.add_argument("--exclude-path", action="append", default=[], help="a tree of the repo not to check out (sparse)")
    ap.add_argument("--blob-limit", default="256k", help="blobs up to this size come with the clone (git --filter=blob:limit)")
    ap.add_argument("--upstream", action="append", default=[], metavar="LITELLM_MODEL=OPENROUTER_MODEL",
                    help="the model LiteLLM routes an alias's model to, for pi's catalog (pi_config)")
    # a kick-back roughly doubles a case's cost (a new fix session, and before it a new verdict): 2 and 1 bound it
    ap.add_argument("--no-step-one", dest="step_one", action="store_false",
                    help="skip RFC-0013 §1's check that the failing code is on the base (bugfix/stepone.py)")
    ap.add_argument("--no-resume", dest="resume", action="store_false",
                    help="a kick-back starts a new fix session (before C10) instead of resuming the previous round's")
    ap.add_argument("--kickback-max", type=int, default=2)
    ap.add_argument("--review-kickback-max", type=int, default=1,
                    help="times the review's request-changes sends a gate-clean fix back (within --kickback-max)")
    ap.add_argument("--no-advisor", dest="advisor", action="store_false")
    ap.add_argument("--episode-timeout", type=int, default=1200)
    ap.add_argument("--test-timeout", type=int, default=900)
    ap.add_argument("--setup-timeout", type=int, default=1200)
    ap.add_argument("--review-timeout", type=int, default=600)
    ap.add_argument("--review-tools", choices=("none", "read"), default="none",
                    help="none: the reviewer judges from the issue, the diff and the changed files it is given, in one "
                         "call; read: it also gets pi's read tools and explores (the behaviour before C4)")
    ap.add_argument("--scratch", default=os.environ.get("BUGFIX_SCRATCH", "/scratch"))
    ap.add_argument("--pi-rules", default=os.path.join(HERE, "fleet_rules.md"),
                    help="the rule file every pi step gets instead of the repo's AGENTS.md (--no-context-files); '' keeps "
                         "pi's default of loading the repo's AGENTS.md")
    ap.add_argument("--spend-file", default="", help="the routine's spend_file, relative to HOME: the run writes its cost "
                    "there (the gateway's bill for its keys; bugfix/spend.py)")
    ap.add_argument("--spend-poll", type=float, default=5.0, help="seconds between the spend reads after the run")
    ap.add_argument("--spend-settle", type=float, default=60.0, help="most seconds to wait for the spend to settle")
    ap.add_argument("--spend-stable", type=float, default=20.0,
                    help="seconds the spend must hold still to count as settled (LiteLLM records calls late)")
    a = ap.parse_args(argv)
    if (a.related == "pytest-dirs" or a.test_format == "pytest") and not (a.source_root and a.test_root):
        ap.error("--related pytest-dirs and --test-format pytest need --source-root and --test-root (the scope rules use them)")
    return a


def main(argv=None, environ=None):
    environ = dict(os.environ if environ is None else environ)
    a = parse(argv)
    case = json.loads(environ["FLOTILLA_CASE"])
    run_dir = os.path.join(a.scratch, "runs", f"{case.get('id', 'case')}-{int(time.time())}")
    os.makedirs(run_dir)
    chain = None
    meter = None
    terminated = []

    def on_term(signum, frame):
        terminated.append(signum)
        raise Terminated()

    # before the first spend reads: a SIGTERM during them still leaves a number (no model has run: 0)
    old_term = signal.signal(signal.SIGTERM, on_term)
    if a.spend_file:
        try:
            cfg = json.loads(environ.get("FLOTILLA_MODELS") or "{}")
        except ValueError:
            cfg = {}
        try:
            meter = spend.Meter(spend.keys_of(cfg, {a.worker_model, a.advisor_model, a.reviewer_model}, environ),
                                a.spend_poll, a.spend_settle, stable_s=a.spend_stable)
        except Terminated:
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            spend.write(os.path.join(environ.get("HOME", "/"), a.spend_file), 0.0)
            shutil.rmtree(run_dir, ignore_errors=True)
            raise
    try:
        chain = Chain(a, case, run_dir, environ)
        out = chain.run()
    except Stop as e:
        out = {"outcome": "escalate", "detail": str(e)}
    except pi_config.ConfigError as e:
        out = {"outcome": "escalate", "detail": f"config: {e}"}
    except NotReproducible as e:
        out = {"outcome": "not_reproducible", "detail": str(e)}
    finally:
        # from here a SIGTERM (the worker's, at the routine's timeout; it SIGKILLs 10 s later) must not cut the report
        # short: it only hurries the spend read, here or already under way
        def on_late_term(signum, frame):
            terminated.append(signum)
            if meter is not None:
                meter.hurry()

        signal.signal(signal.SIGTERM, on_late_term)
        if terminated and meter is not None:
            meter.hurry()
        if meter is not None:   # before the run directory, which holds pi's sessions, is deleted
            # pi's figure first, so a SIGKILL during the gateway read still leaves a number; then the bill over it
            spend.write(os.path.join(environ.get("HOME", "/"), a.spend_file), spend.pi_cost(os.path.join(run_dir, "pi")))
            usd, source, why = meter.cost(os.path.join(run_dir, "pi"))
            spend.write(os.path.join(environ.get("HOME", "/"), a.spend_file), usd)
            print(f"BUGFIX SPEND case={case.get('id')} usd={usd:.6f} source={source}" + (f" ({why})" if why else ""))
        if chain is not None:
            chain.restore_config("the run")
        shutil.rmtree(run_dir, ignore_errors=True)
        shutil.rmtree(os.path.join(a.scratch, "venvs", os.path.basename(run_dir)), ignore_errors=True)
        if chain is not None and getattr(chain, "cache", None):
            sh(["git", "-C", chain.cache, "worktree", "prune"], a.scratch, environ, 60)
        if not terminated:
            signal.signal(signal.SIGTERM, old_term)
    with open(environ["FLOTILLA_PLAN_FILE"], "w") as f:
        json.dump(out, f)
    print(f"BUGFIX FIX case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}")
    return out


if __name__ == "__main__":
    main()
