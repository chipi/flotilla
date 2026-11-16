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
  4. a review of the clean diff (non-blocking), then the plan:
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
import json
import os
import re
import shlex
import shutil
import signal
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pi_config  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
BAKEOFF = os.environ.get("BUGFIX_BAKEOFF_DIR", os.path.join(os.path.dirname(HERE), "bugfix-fleet", "bakeoff"))
# SR-0001 H8: the steps that only read (verdict, advisor, review) get pi's read tools, no shell and no
# writes; "read only" in a prompt did not stop the advisor starting a dev server. The fix step keeps pi's own.
READ_ONLY_TOOLS = "read,grep,find,ls"
REQUEST_CHANGES = re.compile(r"VERDICT:\s*\**\s*request[- ]changes", re.I)
TEST_FILE = re.compile(r"\.(test|spec)\.[cm]?[tj]sx?$|(^|/)tests?/|(^|/)test_[^/]*\.py$")
# CI and hooks, and what `vitest related` cannot see: the repo's config, manifest and lockfile (the
# advisor's review, 2026-11-15); a spec adds its repo's own with --out-of-scope
OUT_OF_SCOPE = re.compile(r"^(\.github/|\.husky/|\.gitlab-ci|\.circleci/)|(^|/)(package\.json|package-lock\.json|"
                          r"npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|tsconfig[^/]*\.json|"
                          r"(vitest|vite|svelte|playwright)\.config\.[cm]?[tj]s)$")
DIFF_MAX = 16000   # the registry's max for github.open_pr's diff
BODY_MAX = 8000
TITLE_MAX = 200


class Stop(Exception):
    """Ends the chain with an outcome: escalate with a reason."""


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
    """One test run's result from a vitest-shaped JSON report: the failed tests (file::name), the files that
    failed to load (no assertions, a failed status), and the files that ran."""

    def __init__(self, fails, unloaded, ran):
        self.fails, self.unloaded, self.ran = fails, unloaded, ran


def run_tests(wt, cmd, files, env, timeout):
    """cmd with %s for the report and the files appended. A red run is data, not an error; a run that did
    not finish, or wrote no readable report, is an error (it names which)."""
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
        upstream = dict(x.split("=", 1) for x in a.upstream)
        cfg = pi_config.write(os.path.join(run_dir, "pi"), sorted({a.worker_model, a.advisor_model, a.reviewer_model}), environ, upstream)
        self.provider, self.worker = pi_config.pick(cfg, a.worker_model)
        _, self.advisor = pi_config.pick(cfg, a.advisor_model)
        _, self.reviewer = pi_config.pick(cfg, a.reviewer_model)
        self.env = dict(environ)
        self.env.update({"PI_CODING_AGENT_DIR": os.path.join(run_dir, "pi"), "PI_PROVIDER": self.provider,
                         "BAKEOFF_ROOT": self.root, "BAKEOFF_WT": self.wt, "BAKEOFF_TRIAGED_DIR": self.triaged,
                         "TRIAGE_MODEL": self.worker, "ADVISOR_MODEL": self.advisor,
                         "PI_MODEL": self.worker, "npm_config_cache": os.path.join(a.scratch, "npm-cache"),
                         "HOME": run_dir,
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
    def attempt(self, manifest):
        with open(manifest) as f:
            desc = json.load(f)["description"]
        desc += "\n\nIMPORTANT: write a failing regression test FIRST (repro-first), then fix. Both must be in your final diff."
        self.reset()
        harness = os.path.join(BAKEOFF, "harnesses", self.a.harness + ".sh")
        if not re.fullmatch(r"[a-z0-9-]+", self.a.harness) or not os.path.exists(harness):
            raise Stop(f"no harness {self.a.harness!r} in {os.path.dirname(harness)}")
        code, stream = sh([harness, self.wt, desc], BAKEOFF, self.env, self.a.episode_timeout)
        turns, out_tok = usage(stream)
        sh(["git", "-C", self.wt, "add", "-A"], self.run_dir, self.env, 60, check=True)
        _, names = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", self.base], self.run_dir, self.env, 60, check=True)
        files = [x for x in names.splitlines() if x.strip() and not x.startswith(".flotilla-suite")]
        stats = (turns, out_tok)
        if code == 124 and not files:
            return "the fix episode ran out of time with no change", files, stream, stats
        if not files:
            return "the fix changed nothing", files, stream, stats
        tests = [x for x in files if TEST_FILE.search(x) and os.path.exists(os.path.join(self.wt, x))]
        if not tests:
            return "repro-first violated: no test in the diff", files, stream, stats
        bad = [x for x in files if OUT_OF_SCOPE.search(x) or any(re.search(o, x) for o in self.a.out_of_scope)]
        if bad:
            return f"out of scope: the diff touches {', '.join(bad)}", files, stream, stats
        return self.gates(files, tests), files, stream, stats

    def gates(self, files, tests):
        """RFC-0010 §9: repro (the tests fail on the base, pass with the fix) and related (no failure the base
        did not have, in the tests related to the changed source files). Leaves the tree as the fix made it."""
        patch = os.path.join(self.run_dir, "fix.patch")
        _, d = sh(["git", "-C", self.wt, "diff", "--cached", "--binary", self.base], self.run_dir, self.env, 60, check=True)
        with open(patch, "w") as f:
            f.write(d)
        src = [x for x in files if not TEST_FILE.search(x)]
        t, rt = self.a.test_timeout, self.a.related_cmd

        def tree(with_source):
            self.reset()
            args = ["git", "-C", self.wt, "apply", "--binary"]
            if not with_source:
                args += [f"--include={x}" for x in tests]
            sh(args + [patch], self.run_dir, self.env, 60, check=True)

        tree(with_source=False)
        base = run_tests(self.wt, self.a.test_cmd, tests, self.env, t)
        base_src = [x for x in src if os.path.exists(os.path.join(self.wt, x))]
        base_rel = run_tests(self.wt, rt, base_src, self.env, t) if base_src else Tests(set(), set(), set())
        tree(with_source=True)
        fixed = run_tests(self.wt, self.a.test_cmd, tests, self.env, t)
        rel = run_tests(self.wt, rt, src, self.env, t) if src else Tests(set(), set(), set())
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
        new = sorted((rel.fails | rel.unloaded) - (base_rel.fails | base_rel.unloaded))
        if new:
            return f"{len(new)} new failures in the related tests: {tail('; '.join(new), 300)}"
        self.checked = f"{len(tests)} test files fail on the base and pass with the fix; {len(rel.ran)} related files, none newly failing"
        return ""

    def diff(self):
        sh(["git", "-C", self.wt, "add", "-A"], self.run_dir, self.env, 60, check=True)
        _, d = sh(["git", "-C", self.wt, "diff", "--cached", self.base, "--", ".", ":(exclude).flotilla-suite.json"],
                  self.run_dir, self.env, 60, check=True)
        return d

    def review(self, diff):
        prompt = (f"Review this automated fix for {self.repo}#{self.n} ({self.issue.get('title', '')}). Read only: do not edit any "
                  f"file.\n\nDIFF:\n{diff}\n\nGive a concise review: correctness risks, edge cases the fix misses, test adequacy. "
                  "End with VERDICT: approve or VERDICT: request-changes.")
        code, stream = sh(["pi", "-p", "--mode", "json", "--provider", self.provider, "--model", self.reviewer,
                           "--tools", READ_ONLY_TOOLS, prompt],
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
        except Stop as e:
            if not self.kept:
                raise
            # a good fix is not thrown away because a later round failed: the operator gets the last clean one
            # with the review's requested changes in it, and the reason the later rounds ended
            rnd, d, review, checked, unloaded = self.kept
            self.note("kept", f"round {rnd}'s clean fix; the run ended: {tail(str(e), 300)}")
            return self.plan(rnd, d, review, checked, unloaded)

    def rounds(self):
        self.checkout()
        # 1 · the verdict, on the base
        v, vpath = self.verdict()
        self.note("verdict", v.get("verdict", "none"))
        if v.get("verdict") != "actionable":
            raise Stop(self.not_actionable(v))
        manifest = os.path.join(self.triaged, f"{self.tid}-triaged.json")
        pins, prior, reviewed_back = [], vpath, 0
        for rnd in range(self.a.kickback_max + 1):
            if not os.path.exists(manifest):
                raise Stop(f"stuck: the verdict step wrote no triaged manifest for round {rnd}")
            reason, files, stream, (turns, out_tok) = self.attempt(manifest)
            from_review = False
            if not reason:
                d = self.diff()
                if len(d) > DIFF_MAX:
                    raise Stop(f"the clean fix's diff is {len(d)} characters, more than the {DIFF_MAX} a plan may carry")
                review = self.review(d)
                self.note("review", tail(review.splitlines()[-1] if review else "", 80))
                # the review saw what the gates did not on 3 of phase A's 4 plans: request-changes sends the fix
                # back while the budget lasts; after that the operator sees the plan with the review in it
                if REQUEST_CHANGES.search(review or "") and reviewed_back < self.a.review_kickback_max \
                        and rnd < self.a.kickback_max:
                    reviewed_back += 1
                    self.kept = (rnd, d, review, self.checked, self.unloaded_on_base)
                    from_review = True
                    # one line: the reason goes into result.tsv, the leaves' kick-back evidence
                    reason = "the review requested changes: " + tail(" ".join(review.split()), 400)
                else:
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review)
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

    def not_actionable(self, v, after=""):
        verdict = v.get("verdict") or "none"
        missing = v.get("missing") or v.get("Missing") or []
        why = "; ".join(str(m) for m in missing) if isinstance(missing, list) else str(missing)
        head = {"needs-info": "needs info from the reporter", "reject": "rejected as not a bug",
                "rejected": "rejected as not a bug"}.get(verdict, f"no usable verdict ({verdict})")
        return " ".join(x for x in [head, after, (": " + why) if why else ""] if x).replace(" :", ":")

    def plan(self, rnd, d, review, checked=None, unloaded=None):
        checked = self.checked if checked is None else checked
        unloaded = self.unloaded_on_base if unloaded is None else unloaded
        title = f"fix #{self.n}: {self.issue.get('title', '')}"[:TITLE_MAX]
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
                "detail": f"#{self.n}: a clean fix in round {rnd} at {self.base[:12]}",
                "params": {"repo": self.repo, "base": self.base, "title": title, "body": tail(body, BODY_MAX), "diff": d}}


def parse(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--worker-model", required=True, help="an alias of FLOTILLA_MODELS")
    ap.add_argument("--advisor-model", required=True)
    ap.add_argument("--reviewer-model", required=True)
    ap.add_argument("--setup", default="npm ci --no-audit --no-fund && (npm run --if-present i18n:compile)")
    ap.add_argument("--test-cmd", default="npx vitest run --reporter=json --outputFile=%s", help="%%s: the report; the files follow")
    ap.add_argument("--related-cmd", default="npx vitest related --run --reporter=json --outputFile=%s")
    ap.add_argument("--out-of-scope", action="append", default=[], help="a regex of paths a fix may not touch")
    ap.add_argument("--base", default="", help="the commit to fix (default: main), for replays (phase B)")
    ap.add_argument("--harness", default="pi", help="harnesses/<name>.sh (phase B arms)")
    ap.add_argument("--exclude-path", action="append", default=[], help="a tree of the repo not to check out (sparse)")
    ap.add_argument("--blob-limit", default="256k", help="blobs up to this size come with the clone (git --filter=blob:limit)")
    ap.add_argument("--upstream", action="append", default=[], metavar="LITELLM_MODEL=OPENROUTER_MODEL",
                    help="the model LiteLLM routes an alias's model to, for pi's catalog (pi_config)")
    ap.add_argument("--kickback-max", type=int, default=3)
    ap.add_argument("--review-kickback-max", type=int, default=2,
                    help="times the review's request-changes sends a gate-clean fix back (within --kickback-max)")
    ap.add_argument("--no-advisor", dest="advisor", action="store_false")
    ap.add_argument("--episode-timeout", type=int, default=1200)
    ap.add_argument("--test-timeout", type=int, default=900)
    ap.add_argument("--setup-timeout", type=int, default=1200)
    ap.add_argument("--review-timeout", type=int, default=600)
    ap.add_argument("--scratch", default=os.environ.get("BUGFIX_SCRATCH", "/scratch"))
    return ap.parse_args(argv)


def main(argv=None, environ=None):
    environ = dict(os.environ if environ is None else environ)
    a = parse(argv)
    case = json.loads(environ["FLOTILLA_CASE"])
    run_dir = os.path.join(a.scratch, "runs", f"{case.get('id', 'case')}-{int(time.time())}")
    os.makedirs(run_dir)
    chain = None
    try:
        chain = Chain(a, case, run_dir, environ)
        out = chain.run()
    except Stop as e:
        out = {"outcome": "escalate", "detail": str(e)}
    except pi_config.ConfigError as e:
        out = {"outcome": "escalate", "detail": f"config: {e}"}
    finally:
        if chain is not None:
            chain.restore_config("the run")
        shutil.rmtree(run_dir, ignore_errors=True)
        if chain is not None and getattr(chain, "cache", None):
            sh(["git", "-C", chain.cache, "worktree", "prune"], a.scratch, environ, 60)
    with open(environ["FLOTILLA_PLAN_FILE"], "w") as f:
        json.dump(out, f)
    print(f"BUGFIX FIX case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}")
    return out


if __name__ == "__main__":
    main()
