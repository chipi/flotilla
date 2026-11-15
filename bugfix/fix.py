"""bugfix/fix: the acting routine for code_bug cases (RFC-0010 §2), fleetd's chain.go on the platform.

Started by flotillad with the case (FLOTILLA_CASE, the issue in its data). It runs the chain's steps in
chain.go's order, calling the bake-off's leaves through LiteLLM, and decides:

  1. verdict (triage_run.sh): actionable, or it escalates (needs-info, rejected: with the reason)
  2. the base: the repo at origin/main, its suite run once for the red set it already has
  3. up to 1 + kickback_max rounds of: a fix (harnesses/pi.sh) from the base, then the gates
       repro first: the diff adds or changes a test file
       the delta suite: no failure the base did not have
       the scope: no CI, workflow or hook file in the diff
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
import shutil
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pi_config  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
BAKEOFF = os.environ.get("BUGFIX_BAKEOFF_DIR", os.path.join(os.path.dirname(HERE), "bugfix-fleet", "bakeoff"))
TEST_FILE = re.compile(r"\.(test|spec)\.[cm]?[tj]sx?$|(^|/)tests?/|(^|/)test_[^/]*\.py$")
OUT_OF_SCOPE = re.compile(r"^(\.github/|\.husky/|\.gitlab-ci|\.circleci/)")
DIFF_MAX = 16000   # the registry's max for github.open_pr's diff
BODY_MAX = 8000
TITLE_MAX = 200


class Stop(Exception):
    """Ends the chain with an outcome: escalate with a reason."""


def tail(s, n):
    s = (s or "").strip()
    return s if len(s) <= n else "…" + s[-(n - 1):]


def sh(cmd, cwd, env=None, timeout=None, check=False):
    """Run a command; (exit code, combined output). A timeout is exit 124."""
    try:
        p = subprocess.run(cmd, cwd=cwd, env=env, timeout=timeout, capture_output=True, text=True,
                           shell=isinstance(cmd, str))
        out = (p.stdout or "") + (p.stderr or "")
        if check and p.returncode != 0:
            raise Stop(f"{cmd if isinstance(cmd, str) else ' '.join(cmd)}: exit {p.returncode}: {tail(out, 300)}")
        return p.returncode, out
    except subprocess.TimeoutExpired as e:
        out = (e.stdout or b"").decode("utf-8", "replace") if isinstance(e.stdout, bytes) else (e.stdout or "")
        if check:
            raise Stop(f"{cmd if isinstance(cmd, str) else ' '.join(cmd)}: no result in {timeout}s")
        return 124, out


ERR_LINE = re.compile(r"(npm (error|ERR!)|gyp ERR!|^error|Error:|ERR_)", re.I)


def errors_of(out, n=400):
    """The error lines of a tool's output, not its last lines: npm ends a failed install with an upgrade
    notice, which is all a tail showed (orrery #490 on the mini, 2026-11-15)."""
    lines = [ln.strip() for ln in (out or "").splitlines() if ERR_LINE.search(ln)]
    return tail(" | ".join(lines), n) if lines else tail(out, n)


def suite_failures(wt, suite_cmd, env, timeout):
    """The failed-test set (file::name) from the suite's JSON report (vitest's, as chain.go reads it). A red
    suite is data, not an error; a missing or unreadable report is an error."""
    report = os.path.join(wt, ".flotilla-suite.json")
    if os.path.exists(report):
        os.remove(report)
    sh(suite_cmd.replace("%s", report), wt, env, timeout)
    try:
        with open(report) as f:
            rep = json.load(f)
    except (OSError, ValueError) as e:
        raise Stop(f"the suite wrote no readable report ({e})")
    finally:
        if os.path.exists(report):
            os.remove(report)
    fails = set()
    for tr in rep.get("testResults") or []:
        for ar in tr.get("assertionResults") or []:
            if ar.get("status") == "failed":
                fails.add(os.path.relpath(tr.get("name", ""), wt) + "::" + ar.get("fullName", ""))
    return fails


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
                         "TRIAGE_BASE": "origin/main", "TRIAGE_MODEL": self.worker, "ADVISOR_MODEL": self.advisor,
                         "PI_MODEL": self.worker, "npm_config_cache": os.path.join(a.scratch, "npm-cache"),
                         "HOME": run_dir})
        self.env.pop("FLOTILLA_PLAN_FILE", None)  # the leaves write nothing of the run's own

    def note(self, step, detail=""):
        line = f"{step}: {detail}" if detail else step
        self.log.append(line)
        print(f"BUGFIX FIX #{self.n} {line}", flush=True)

    # 2 · the base
    def checkout(self):
        """main, one commit deep, its blobs fetched only for the paths checked out: orrery's history is 14 GB
        and its tree 6.8 GB, most of it images the suite does not read. --exclude-path leaves such trees out;
        a test that needs them fails on the base too, so it is in the base's red set, not a new failure."""
        remote = self.env.get("GIT_REMOTE_BASE", "https://github.com").rstrip("/") + "/" + self.repo
        sh(["git", "clone", "-q", "--depth", "1", "--filter=blob:none", "--no-checkout", "--branch", "main", remote, self.wt],
           self.run_dir, self.env, 1200, check=True)
        if self.a.exclude_path:
            sh(["git", "-C", self.wt, "sparse-checkout", "set", "--no-cone", "/*"] + [f"!/{x.strip('/')}/" for x in self.a.exclude_path],
               self.run_dir, self.env, 60, check=True)
        sh(["git", "-C", self.wt, "checkout", "-q", "--detach", "origin/main"], self.run_dir, self.env, 1200, check=True)
        _, base = sh(["git", "-C", self.wt, "rev-parse", "HEAD"], self.run_dir, self.env, 30, check=True)
        self.base = base.strip()
        self.note("base", self.base[:12])
        if self.a.setup:
            code, out = sh(self.a.setup, self.wt, self.env, self.a.setup_timeout)
            if code != 0:
                raise Stop(f"the repo's setup failed on the base (exit {code}): {errors_of(out)}")

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
        env = dict(self.env, ACCEPTANCE_GAP="1" if acceptance_gap else "0")
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
        code, stream = sh([os.path.join(BAKEOFF, "harnesses", "pi.sh"), self.wt, desc], BAKEOFF, self.env, self.a.episode_timeout)
        turns, out_tok = usage(stream)
        sh(["git", "-C", self.wt, "add", "-A"], self.run_dir, self.env, 60, check=True)
        _, names = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", self.base], self.run_dir, self.env, 60, check=True)
        files = [x for x in names.splitlines() if x.strip() and not x.startswith(".flotilla-suite")]
        if code == 124 and not files:
            return "the fix episode ran out of time with no change", files, stream, (turns, out_tok)
        if not files:
            return "the fix changed nothing", files, stream, (turns, out_tok)
        if not any(TEST_FILE.search(x) for x in files):
            return "repro-first violated: no test in the diff", files, stream, (turns, out_tok)
        bad = [x for x in files if OUT_OF_SCOPE.search(x)]
        if bad:
            return f"out of scope: the diff touches {', '.join(bad)}", files, stream, (turns, out_tok)
        sh(["git", "-C", self.wt, "reset", "-q"], self.run_dir, self.env, 60, check=True)
        after = suite_failures(self.wt, self.a.suite_cmd, self.env, self.a.suite_timeout)
        new = sorted(after - self.base_fails)
        if new:
            return f"{len(new)} new failures (delta gate): {tail('; '.join(new), 300)}", files, stream, (turns, out_tok)
        return "", files, stream, (turns, out_tok)

    def diff(self):
        sh(["git", "-C", self.wt, "add", "-A"], self.run_dir, self.env, 60, check=True)
        _, d = sh(["git", "-C", self.wt, "diff", "--cached", self.base, "--", ".", ":(exclude).flotilla-suite.json"],
                  self.run_dir, self.env, 60, check=True)
        return d

    def review(self, diff):
        prompt = (f"Review this automated fix for {self.repo}#{self.n} ({self.issue.get('title', '')}). Read only: do not edit any "
                  f"file.\n\nDIFF:\n{diff}\n\nGive a concise review: correctness risks, edge cases the fix misses, test adequacy. "
                  "End with VERDICT: approve or VERDICT: request-changes.")
        code, stream = sh(["pi", "-p", "--mode", "json", "--provider", self.provider, "--model", self.reviewer, prompt],
                          self.wt, self.env, self.a.review_timeout)
        text = assistant_text(stream)
        if text:
            return text
        err = stream_error(stream)
        return f"(no review: {tail(err, 300)})" if err else f"(no review: the reviewer episode ended {code} with no text)"

    def run(self):
        self.checkout()
        # 1 · the verdict, on the base
        v, vpath = self.verdict()
        self.note("verdict", v.get("verdict", "none"))
        if v.get("verdict") != "actionable":
            raise Stop(self.not_actionable(v))
        manifest = os.path.join(self.triaged, f"{self.tid}-triaged.json")
        self.reset()
        self.base_fails = suite_failures(self.wt, self.a.suite_cmd, self.env, self.a.suite_timeout)
        self.note("baseline", f"{len(self.base_fails)} failing on the base")
        pins, prior = [], vpath
        for rnd in range(self.a.kickback_max + 1):
            if not os.path.exists(manifest):
                raise Stop(f"stuck: the verdict step wrote no triaged manifest for round {rnd}")
            reason, files, stream, (turns, out_tok) = self.attempt(manifest)
            self.note(f"round {rnd}", reason or f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens)")
            if not reason:
                return self.plan(rnd)
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
            elif self.a.advisor:
                code, out = sh([os.path.join(BAKEOFF, "advisor_run.sh"), os.path.join(self.run_dir, "ticket.json"), ev],
                               BAKEOFF, self.env, self.a.episode_timeout)
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

    def plan(self, rnd):
        d = self.diff()
        if len(d) > DIFF_MAX:
            raise Stop(f"the clean fix's diff is {len(d)} characters, more than the {DIFF_MAX} a plan may carry")
        review = self.review(d)
        self.note("review", tail(review.splitlines()[-1] if review else "", 80))
        title = f"fix #{self.n}: {self.issue.get('title', '')}"[:TITLE_MAX]
        body = "\n".join([
            f"An automated fix for {self.repo}#{self.n} by flotilla's bugfix fleet (RFC-0010), in beta evaluation.",
            "", "Gates: repro first (a test in the diff), the suite with no failure the base did not have "
            f"({len(self.base_fails)} failing on the base {self.base[:12]}), nothing in CI or hooks. Round {rnd}.",
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
    ap.add_argument("--suite-cmd", default="npx vitest run --reporter=json --outputFile=%s")
    ap.add_argument("--exclude-path", action="append", default=[], help="a tree of the repo not to check out (sparse)")
    ap.add_argument("--upstream", action="append", default=[], metavar="LITELLM_MODEL=OPENROUTER_MODEL",
                    help="the model LiteLLM routes an alias's model to, for pi's catalog (pi_config)")
    ap.add_argument("--kickback-max", type=int, default=3)
    ap.add_argument("--no-advisor", dest="advisor", action="store_false")
    ap.add_argument("--episode-timeout", type=int, default=1200)
    ap.add_argument("--suite-timeout", type=int, default=1200)
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
    try:
        out = Chain(a, case, run_dir, environ).run()
    except Stop as e:
        out = {"outcome": "escalate", "detail": str(e)}
    except pi_config.ConfigError as e:
        out = {"outcome": "escalate", "detail": f"config: {e}"}
    finally:
        shutil.rmtree(run_dir, ignore_errors=True)
    with open(environ["FLOTILLA_PLAN_FILE"], "w") as f:
        json.dump(out, f)
    print(f"BUGFIX FIX case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}")
    return out


if __name__ == "__main__":
    main()
