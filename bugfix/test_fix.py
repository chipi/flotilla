"""python3 bugfix/test_fix.py: bugfix/fix's chain (RFC-0010 §2) on fake leaves, a fake pi, a fake suite
and a local git remote. The models are replaced, not the flow: the verdicts and fixes each leaf gives
are scripted per call, and the chain's steps, gates and limits are what is tested."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fix  # noqa: E402

MODELS = {"endpoints": {"gateway-bugfix": {"base_url": "http://127.0.0.1:9/v1", "kind": "litellm", "key_file": "/dev/null"}},
          "aliases": {"bugfix-flash": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-flash"},
                      "bugfix-reviewer": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-reviewer"}}}

# Each fake leaf pops its next scripted answer from a file in FAKE_STATE and appends what it was called with.
POP = textwrap.dedent('''\
    pop() { f="$FAKE_STATE/$1"; head -n1 "$f"; tail -n +2 "$f" > "$f.n"; mv "$f.n" "$f"; }
    ''')

TRIAGE = POP + textwrap.dedent('''\
    BUG_JSON="$1"; EV="${3:-}"; PRIOR="${4:-}"
    ID=$(jq -r .id "$BUG_JSON")
    V=$(pop verdicts)
    IMG=no; [ -e "$BAKEOFF_WT/static/images/x.png" ] && IMG=yes
    echo "triage $V ev=${EV:+yes} gap=$ACCEPTANCE_GAP model=$TRIAGE_MODEL provider=$PI_PROVIDER img=$IMG tools=${PI_TOOLS:-all} rows=$( [ -n "$EV" ] && wc -l < "$EV/result.tsv" | tr -d " " || echo 0)" >> "$FAKE_STATE/calls"
    KB=0; [ -n "$EV" ] && KB=$(( $(jq -r '.kickback_round // 0' "$PRIOR") + 1 ))
    SUF=""; [ "$KB" -gt 0 ] && SUF="-kb$KB"
    OUT="$BAKEOFF_ROOT/results/${ID}-triage${SUF}/pi"; mkdir -p "$OUT"
    printf '{"verdict": "%s", "kickback_round": %s, "missing": ["the expected value"], "problem": {}}' "$V" "$KB" > "$OUT/triage.json"
    if [ "$V" = actionable ]; then
      mkdir -p "$BAKEOFF_TRIAGED_DIR"
      printf '{"id": "%s", "description": "make value() return 2"}' "$ID" > "$BAKEOFF_TRIAGED_DIR/${ID}-triaged${SUF}.json"
    fi
    git -C "$BAKEOFF_WT" reset -q --hard "$TRIAGE_BASE"
    ''')

PI_HARNESS = POP + textwrap.dedent('''\
    WT="$1"; F=$(pop fixes)
    echo "fix $F model=$PI_MODEL tools=${PI_TOOLS:-all}" >> "$FAKE_STATE/calls"
    cd "$WT"
    case "$F" in
      good)     echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js ;;
      notest)   echo 'module.exports = () => 2;' > src/a.js ;;
      breaks)   echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js; echo 'module.exports = () => 2;' > src/b.js ;;
      norepro)  echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/b.js 1' > src/a.test.js ;;
      halffix)  echo 'module.exports = () => 3;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js ;;
      newmod)   echo 'module.exports = () => 3;' > src/c.js; printf 'import src/c.js\nexpect src/c.js 3\n' > src/c.test.js ;;
      goodnewmod) echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js
                echo 'module.exports = () => 3;' > src/c.js; printf 'import src/c.js\nexpect src/c.js 3\n' > src/c.test.js ;;
      lockfile) echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js; echo '{}' > package-lock.json ;;
      workflow) echo 'expect src/a.js 2' > src/a.test.js; mkdir -p .github/workflows; echo on: push > .github/workflows/x.yml ;;
      pygood)   echo 'def value(): return 2' > src/podcast_scraper/pkg/mod.py
                echo 'expect src/podcast_scraper/pkg/mod.py 2' > tests/unit/podcast_scraper/pkg/test_mod.py ;;
      pybreaks) echo 'def value(): return 2' > src/podcast_scraper/pkg/mod.py; echo 'def value(): return 2' > src/podcast_scraper/pkg/other.py
                echo 'expect src/podcast_scraper/pkg/mod.py 2' > tests/unit/podcast_scraper/pkg/test_mod.py ;;
      pylone)   echo 'def value(): return 2' > src/podcast_scraper/lone/z.py
                echo 'expect src/podcast_scraper/lone/z.py 2' > tests/unit/podcast_scraper/test_z.py ;;
      pynested) echo 'def value(): return 2' > src/podcast_scraper/pkg/mod.py; echo 'def value(): return 1  # x' > src/podcast_scraper/pkg/sub/s.py
                echo 'expect src/podcast_scraper/pkg/mod.py 2' > tests/unit/podcast_scraper/pkg/test_mod.py ;;
      pyskip)   echo 'def value(): return 2' > src/podcast_scraper/pkg/mod.py; printf 'SKIP\\nexpect src/podcast_scraper/pkg/mod.py 2\\n' > tests/unit/podcast_scraper/pkg/test_mod.py ;;
      pyconf)   echo 'def value(): return 2' > src/podcast_scraper/pkg/mod.py
                echo 'expect src/podcast_scraper/pkg/mod.py 2' > tests/unit/podcast_scraper/pkg/test_mod.py; echo '[project]' > pyproject.toml ;;
      nothing)  ;;
    esac
    printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"output":7}}}\\n'
    ''')

ADVISOR = textwrap.dedent('''\
    echo "advisor tools=${PI_TOOLS:-all}" >> "$FAKE_STATE/calls"
    printf '{"file": "src/a.js", "function": "value", "decoy": "src/b.js", "rationale": "it computes it"}' > "$2/advisor.json"
    ''')

# the reviewer is pi itself, on PATH
PI_BIN = POP + textwrap.dedent('''\
    T=all; prev=; for a in "$@"; do [ "$prev" = --tools ] && T="$a"; prev="$a"; done
    R=approve; [ -s "$FAKE_STATE/reviews" ] && R=$(pop reviews)
    echo "review model=$6 tools=$T verdict=$R" >> "$FAKE_STATE/calls"
    printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The test only checks a constant.\\\\nVERDICT: %s"}]}}\\n' "$R"
    ''')

# a vitest-shaped runner. `run REPORT FILES`: those test files; `related REPORT SOURCES`: every *.test.js
# that names one of the sources. A test file's lines: "import F" (a missing F: the file fails to load),
# "expect F N" (passes when F returns N), "FAIL" (always fails).
SUITE = textwrap.dedent('''\
    import json, os, sys
    mode, report, args = sys.argv[1], sys.argv[2], sys.argv[3:]
    def all_tests():
        for root, _, files in os.walk("."):
            if ".git" not in root:
                for f in files:
                    if f.endswith(".test.js"):
                        yield os.path.relpath(os.path.join(root, f))
    if mode == "run":
        files = args
    else:
        files = [t for t in all_tests() if any(a in open(t).read() for a in args)]
    res = []
    for f in files:
        p = os.path.abspath(f)
        lines = open(p).read().splitlines()
        if any(ln.startswith("import ") and not os.path.exists(ln.split()[1]) for ln in lines):
            res.append({"name": p, "status": "failed", "assertionResults": []})
            continue
        st = "passed"
        for ln in lines:
            if ln == "FAIL":
                st = "failed"
            elif ln.startswith("expect "):
                _, target, n = ln.split()
                if not os.path.exists(target) or f"=> {n};" not in open(target).read():
                    st = "failed"
        res.append({"name": p, "status": st, "assertionResults": [{"status": st, "fullName": f}]})
    json.dump({"testResults": res}, open(report, "w"))
    ''')


# a pytest-json-report-shaped runner: `PYSUITE REPORT ARGS`, the args test files or directories (every
# test_*.py below). A test file's lines are SUITE's, plus "SKIP" (every test in it is skipped, as a
# pytest.importorskip that found nothing does). A file that fails to load is a failed collector.
PYSUITE = textwrap.dedent('''\
    import json, os, sys
    report, args = sys.argv[1], sys.argv[2:]
    files = []
    for a in args:
        if os.path.isdir(a):
            for root, _, fs in os.walk(a):
                files += [os.path.join(root, f) for f in sorted(fs) if f.startswith("test_") and f.endswith(".py")]
        else:
            files.append(a)
    tests, collectors = [], [{"nodeid": "", "outcome": "passed", "result": []}]
    if not args:   # pytest with no path argument runs the whole suite: fail loudly, a gate must never do that
        tests.append({"nodeid": "WHOLE_SUITE::test_0", "outcome": "failed"})
    for f in sorted(set(files)):
        lines = open(f).read().splitlines()
        if any(ln.startswith("import ") and not os.path.exists(ln.split()[1]) for ln in lines):
            collectors.append({"nodeid": f, "outcome": "failed", "longrepr": "ImportError"})
            continue
        collectors.append({"nodeid": f, "outcome": "passed", "result": []})
        for i, ln in enumerate(l for l in lines if l == "FAIL" or l.startswith("expect ")):
            ok = True
            if ln == "FAIL":
                ok = False
            else:
                _, target, n = ln.split()
                ok = os.path.exists(target) and f"return {n}" in open(target).read()
            out = "skipped" if "SKIP" in lines else ("passed" if ok else "failed")
            tests.append({"nodeid": f"{f}::test_{i}", "outcome": out})
    json.dump({"tests": tests, "collectors": collectors}, open(report, "w"))
    ''')


def write(path, text, mode=0o755):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write("#!/usr/bin/env bash\nset -eu\n" + text if path.endswith(".sh") or path.endswith("/pi") else text)
    os.chmod(path, mode)


def git(*a, cwd):
    subprocess.run(["git", *a], cwd=cwd, check=True, capture_output=True)


class Fixture:
    def __init__(self, verdicts, fixes, py=False):
        self.py = py
        self.repo = "chipi/podcast_scraper" if py else "chipi/orrery"
        self.d = tempfile.mkdtemp()
        b = os.path.join(self.d, "bakeoff")
        write(os.path.join(b, "triage_run.sh"), TRIAGE)
        write(os.path.join(b, "advisor_run.sh"), ADVISOR)
        write(os.path.join(b, "harnesses", "pi.sh"), PI_HARNESS)
        write(os.path.join(self.d, "bin", "pi"), PI_BIN)
        self.suite = os.path.join(self.d, "suite.py")
        write(self.suite, PYSUITE if py else SUITE, 0o644)
        self.state = os.path.join(self.d, "state")
        os.makedirs(self.state)
        for name, items in (("verdicts", verdicts), ("fixes", fixes)):
            with open(os.path.join(self.state, name), "w") as f:
                f.write("".join(x + "\n" for x in items))
        # the remote: chipi/orrery with one passing test and one already red on main
        src = os.path.join(self.d, "src")
        os.makedirs(os.path.join(src, "src"))
        os.makedirs(os.path.join(src, "static", "images"))
        files = (("src/a.js", "module.exports = () => 1;\n"), ("src/b.js", "module.exports = () => 1;\n"),
                 ("keep.test.js", "expect src/b.js 1\n"), ("red.test.js", "expect src/a.js 9\n"),
                 ("static/images/x.png", "a big image\n"))
        if py:
            # podcast_scraper's shape: src/<pkg>/<a>/<b>.py, tests mirrored under tests/unit/<pkg>/<a>/
            pkg, tests = "src/podcast_scraper", "tests/unit/podcast_scraper"
            files = ((f"{pkg}/pkg/mod.py", "def value(): return 1\n"), (f"{pkg}/pkg/other.py", "def value(): return 1\n"),
                     (f"{pkg}/lone/z.py", "def value(): return 1\n"), (f"{pkg}/pkg/sub/s.py", "def value(): return 1\n"),
                     (f"{tests}/pkg/sub/test_s.py", f"expect {pkg}/pkg/sub/s.py 1\n"),
                     (f"{tests}/pkg/test_keep.py", f"expect {pkg}/pkg/other.py 1\n"),
                     (f"{tests}/pkg/test_red.py", f"expect {pkg}/pkg/mod.py 9\n"),
                     (f"{tests}/test_top.py", f"expect {pkg}/pkg/other.py 1\n"))
        for rel, text in files:
            os.makedirs(os.path.dirname(os.path.join(src, rel)), exist_ok=True)
            with open(os.path.join(src, rel), "w") as f:
                f.write(text)
        git("init", "-q", "-b", "main", cwd=src)
        git("-c", "user.email=t@t", "-c", "user.name=t", "add", "-A", cwd=src)
        git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base", cwd=src)
        remote = os.path.join(self.d, "remotes", *self.repo.split("/"))
        os.makedirs(os.path.dirname(remote))
        git("clone", "-q", "--bare", src, remote, cwd=self.d)
        git("config", "uploadpack.allowfilter", "true", cwd=remote)  # GitHub allows --filter; a local bare repo must be told
        self.base = subprocess.run(["git", "-C", remote, "rev-parse", "main"], capture_output=True, text=True).stdout.strip()
        self.bakeoff, self.remotes = b, "file://" + os.path.join(self.d, "remotes")  # file://: --depth and --filter work
        self.scratch = os.path.join(self.d, "scratch")

    def run(self, *extra):
        plan = os.path.join(self.d, "plan.json")
        env = dict(os.environ, FLOTILLA_PLAN_FILE=plan, FLOTILLA_MODELS=json.dumps(MODELS), FAKE_STATE=self.state, GIT_ALLOW_PROTOCOL="file",
                   GIT_REMOTE_BASE=self.remotes, PATH=os.path.join(self.d, "bin") + os.pathsep + os.environ["PATH"],
                   GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_CASE=json.dumps({"id": "case-20261115-abc", "kind": "code_bug", "target": f"repo:{self.repo}/issues/12",
                                             "data": {"issue": {"repo": self.repo, "number": 12, "title": "value is 1",
                                                                "body": "value() returns 1, want 2", "url": "u"}}}))
        old = fix.BAKEOFF
        fix.BAKEOFF = self.bakeoff
        try:
            cmds = (["--test-cmd", f"python3 {self.suite} %s", "--test-format", "pytest", "--related", "pytest-dirs",
                     "--source-root", "src/podcast_scraper", "--test-root", "tests/unit/podcast_scraper"] if self.py else
                    ["--test-cmd", f"python3 {self.suite} run %s", "--related-cmd", f"python3 {self.suite} related %s"])
            out = fix.main(["--repo", self.repo, "--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer",
                            "--reviewer-model", "bugfix-reviewer", "--setup", "true", *cmds,
                            "--scratch", self.scratch, *extra], environ=env)
        finally:
            fix.BAKEOFF = old
        with open(plan) as f:
            return json.load(f), out

    def calls(self):
        with open(os.path.join(self.state, "calls")) as f:
            return [ln.split()[0] for ln in f if ln.strip()]

    def close(self):
        shutil.rmtree(self.d, ignore_errors=True)


class Streams(unittest.TestCase):
    def test_the_last_message_s_text_or_error(self):
        ok = '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"VERDICT: approve"}],"stopReason":"stop"}}'
        bad = ('{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error",'
               '"errorMessage":"400: UnsupportedParamsError reasoning_effort"}}')
        self.assertEqual((fix.assistant_text(ok), fix.stream_error(ok)), ("VERDICT: approve", ""))
        self.assertEqual((fix.assistant_text(bad), fix.stream_error(bad)), ("", "400: UnsupportedParamsError reasoning_effort"))


class Timeouts(unittest.TestCase):
    def test_a_timeout_kills_the_whole_group(self):
        """A grandchild (pi under bash pi.sh, vitest under npx) dies with the timeout."""
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        marker = os.path.join(d, "alive")
        code, _ = fix.sh(f"(sleep 3; touch {marker}) & wait", d, timeout=1)
        self.assertEqual(code, 124)
        import time
        time.sleep(3.5)
        self.assertFalse(os.path.exists(marker), "the grandchild outlived the timeout")
        self.assertEqual(fix.sh(["true"], d, timeout=5), (0, ""))


class Errors(unittest.TestCase):
    def test_npm_s_error_lines_not_its_notice(self):
        out = ("added 1 package\nnpm error code 1\nnpm error path /wt/node_modules/canvas\n"
               "npm error gyp ERR! stack Error: getaddrinfo EAI_AGAIN nodejs.org\n"
               "npm notice New major version of npm available! 10.9.9 -> 12.2.2\n")
        got = fix.errors_of(out)
        self.assertIn("npm error path /wt/node_modules/canvas", got)
        self.assertIn("nodejs.org", got)
        self.assertNotIn("New major version", got)
        self.assertEqual(fix.errors_of("all fine\nthe end"), "all fine\nthe end", "no error line: the tail")


def pyrep(tests=(), collectors=()):
    return {"tests": [{"nodeid": n, "outcome": o} for n, o in tests],
            "collectors": [{"nodeid": n, "outcome": o} for n, o in collectors]}


class PytestReport(unittest.TestCase):
    """pytest-json-report's shape read into the one internal per-file result vitest's gives."""

    def test_passed_failed_collector_error_and_all_skipped(self):
        rep = pyrep(
            tests=[("tests/a/test_ok.py::test_1", "passed"), ("tests/a/test_ok.py::test_2[x-1]", "passed"),
                   ("tests/a/test_bad.py::TestC::test_1", "failed"), ("tests/a/test_bad.py::test_2", "passed"),
                   ("tests/a/test_err.py::test_1", "error"),
                   ("tests/a/test_skip.py::test_1", "skipped"), ("tests/a/test_skip.py::test_2", "skipped"),
                   ("tests/a/test_mixed.py::test_1", "skipped"), ("tests/a/test_mixed.py::test_2", "passed"),
                   ("tests/a/test_xf.py::test_1", "xfailed")],
            collectors=[("", "passed"), ("tests/a/test_ok.py", "passed"), ("tests/a/test_load.py", "failed"),
                        ("tests/a/test_modskip.py", "skipped")])
        t = fix.parse_pytest(rep, [])
        self.assertEqual(t.fails, {"tests/a/test_bad.py::TestC::test_1", "tests/a/test_err.py::test_1"})
        self.assertEqual(t.unloaded, {"tests/a/test_load.py", "tests/a/test_skip.py", "tests/a/test_modskip.py", "tests/a/test_xf.py"})
        self.assertEqual(t.ran, {"tests/a/test_ok.py", "tests/a/test_bad.py", "tests/a/test_err.py", "tests/a/test_mixed.py"})

    def test_a_file_whose_collector_failed_did_not_run(self):
        rep = pyrep(tests=[("tests/test_b.py::test_1", "passed")], collectors=[("tests/test_b.py", "failed")])
        t = fix.parse_pytest(rep, [])
        self.assertEqual((t.unloaded, t.ran), ({"tests/test_b.py"}, set()))

    def test_all_skipped_is_not_a_pass(self):
        t = fix.parse_pytest(pyrep(tests=[("tests/test_i.py::test_1", "skipped")]), ["tests/test_i.py"])
        self.assertEqual((t.unloaded, t.ran, t.fails), ({"tests/test_i.py"}, set(), set()))

    def test_a_requested_file_with_no_result_did_not_run(self):
        t = fix.parse_pytest(pyrep(tests=[("tests/test_a.py::test_1", "passed")]), ["tests/test_a.py", "tests/test_gone.py", "tests/dir"])
        self.assertEqual(t.unloaded, {"tests/test_gone.py"}, "a directory argument names no file; a missing file is unloaded")

    def test_a_collection_error_without_a_file_is_named(self):
        t = fix.parse_pytest(pyrep(collectors=[("", "failed")]), [])
        self.assertEqual(t.unloaded, {"(collection)"})


class Chain(unittest.TestCase):
    def fixture(self, verdicts, fixes):
        f = Fixture(verdicts, fixes)
        self.addCleanup(f.close)
        return f

    def test_a_clean_fix_is_a_pr_plan(self):
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run()
        self.assertEqual((plan["outcome"], plan["action"]), ("plan", "github.open_pr"), plan)
        p = plan["params"]
        self.assertEqual((p["repo"], p["base"]), ("chipi/orrery", f.base))
        self.assertEqual(p["title"], "fix #12: value is 1")
        self.assertIn("+++ b/src/a.test.js", p["diff"])
        self.assertIn("+module.exports = () => 2;", p["diff"])
        self.assertNotIn("flotilla-suite", p["diff"])
        self.assertIn("1 test files fail on the base and pass with the fix", p["body"])
        self.assertIn("none newly failing", p["body"], "red.test.js fails on the base too: not a new failure")
        self.assertIn("VERDICT: approve", p["body"])
        self.assertEqual(f.calls(), ["triage", "fix", "review"])
        self.assertEqual(os.listdir(os.path.join(f.scratch, "runs")), [], "the run's directory is deleted")
        cache = os.path.join(f.scratch, "repos", "chipi__orrery.git")
        wl = subprocess.run(["git", "-C", cache, "worktree", "list"], capture_output=True, text=True).stdout.splitlines()
        self.assertEqual(len(wl), 1, f"the cache stays and the run's worktree is gone: {wl}")
        # a second case reuses the cache: it fetches what is new, not the whole repo again
        with open(os.path.join(f.state, "verdicts"), "w") as fh:
            fh.write("needs-info\n")
        plan2, _ = f.run()
        self.assertEqual(plan2["outcome"], "escalate")
        self.assertTrue(os.path.isdir(cache))

    def test_excluded_paths_are_not_checked_out_and_not_in_the_diff(self):
        """orrery's images are left out of the checkout (--exclude-path); their absence is not a deletion."""
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run("--exclude-path", "static/images")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertNotIn("static/images", plan["params"]["diff"])
        with open(os.path.join(f.state, "calls")) as fh:
            self.assertIn("img=no", fh.read())
        g = self.fixture(["needs-info"], [])
        g.run()
        with open(os.path.join(g.state, "calls")) as fh:
            self.assertIn("img=yes", fh.read(), "without --exclude-path the whole tree is checked out")

    def test_a_tampered_cache_is_refused_and_no_hook_runs(self):
        """The repo cache is shared between runs: a URL rewrite left in its config stops the next run, and a
        hook left in it does not run."""
        f = self.fixture(["needs-info", "needs-info"], [])
        f.run()
        cache = os.path.join(f.scratch, "repos", "chipi__orrery.git")
        hook = os.path.join(cache, ".git", "hooks", "post-checkout")
        os.makedirs(os.path.dirname(hook), exist_ok=True)
        marker = os.path.join(f.d, "hook-ran")
        with open(hook, "w") as fh:
            fh.write(f"#!/bin/sh\ntouch {marker}\n")
        os.chmod(hook, 0o755)
        subprocess.run(["git", "-C", cache, "config", "url.https://evil.example/.insteadOf", "https://github.com/"], check=True)
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("the repo cache's config was changed", plan["detail"])
        subprocess.run(["git", "-C", cache, "config", "--unset", "url.https://evil.example/.insteadOf"], check=True)
        os.makedirs(os.path.dirname(hook), exist_ok=True)
        with open(hook, "w") as fh:
            fh.write(f"#!/bin/sh\ntouch {marker}\n")
        os.chmod(hook, 0o755)
        with open(os.path.join(f.state, "verdicts"), "w") as fh:
            fh.write("needs-info\n")
        f.run()
        self.assertFalse(os.path.exists(marker), "a hook in the shared cache ran")

    def test_the_repo_s_setup_writing_the_cache_config_is_put_back(self):
        """orrery's `prepare` writes core.hooksPath into the shared cache's config on every npm ci: the run puts
        the config back, so the next run is not refused (8 escalations on the X3, 2026-11-15). The fix's own
        change made after the setup is put back too, when the run ends."""
        f = self.fixture(["needs-info", "needs-info"], [])
        plan, _ = f.run("--setup", "git config core.hooksPath .husky")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertNotIn("config was changed", plan["detail"])
        cache = os.path.join(f.scratch, "repos", "chipi__orrery.git")
        got = subprocess.run(["git", "-C", cache, "config", "--local", "--get", "core.hooksPath"], capture_output=True, text=True)
        self.assertEqual(got.returncode, 1, f"core.hooksPath stayed in the cache: {got.stdout!r}")
        plan2, _ = f.run("--setup", "git config core.hooksPath .husky")
        self.assertIn("needs info", plan2["detail"], "the second run is not refused by the cache check")

    def test_a_refused_fetch_is_tried_again(self):
        """GitHub refusing the checkout's blob fetch (anonymous, in a burst) is tried again; a refusal every
        time still stops the run, and another failure is not retried."""
        f = self.fixture(["needs-info", "needs-info", "needs-info"], [])
        real = subprocess.run(["sh", "-c", "command -v git"], capture_output=True, text=True).stdout.strip()
        count = os.path.join(f.d, "checkouts")
        wrapper = os.path.join(f.d, "bin", "git")
        os.makedirs(os.path.dirname(wrapper), exist_ok=True)

        def fail(times, msg):
            with open(wrapper, "w") as fh:
                fh.write(f"""#!/bin/sh
case "$*" in *"checkout -q --detach HEAD"*)
  n=$(cat {count} 2>/dev/null || echo 0); n=$((n+1)); echo $n > {count}
  if [ $n -le {times} ]; then echo "fatal: {msg}" >&2; exit 128; fi;;
esac
exec {real} "$@"
""")
            os.chmod(wrapper, 0o755)
            if os.path.exists(count):
                os.remove(count)
        old = fix.FETCH_RETRY_PAUSES
        fix.FETCH_RETRY_PAUSES = (0, 0)
        try:
            fail(2, "could not read Username for 'https://github.com': No such device or address")
            plan, _ = f.run()
            self.assertIn("needs info", plan["detail"], "refused twice, then fetched: the run goes on")
            fail(3, "could not read Username for 'https://github.com': No such device or address")
            plan, _ = f.run()
            self.assertIn("could not read Username", plan["detail"], "refused every time: the run stops")
            fail(1, "invalid reference: HEAD")
            plan, _ = f.run()
            with open(count) as fh:
                self.assertEqual(fh.read().strip(), "1", "a failure that is not a refused fetch is not retried")
        finally:
            fix.FETCH_RETRY_PAUSES = old
            os.remove(wrapper)

    def test_not_actionable_escalates_with_the_reason(self):
        f = self.fixture(["needs-info"], [])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("needs info from the reporter: the expected value", plan["detail"])
        self.assertEqual(f.calls(), ["triage"])

    def test_no_test_kicks_back_to_the_advisor_and_round_two_ships(self):
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("round 0: repro-first violated: no test in the diff", plan["params"]["body"])
        self.assertIn("advisor: src/a.js", plan["params"]["body"])
        self.assertEqual(f.calls(), ["triage", "fix", "advisor", "triage", "fix", "review"])

    def test_the_review_can_send_a_fix_back(self):
        """Phase B: the review asked for changes on the 3 wrong or partial plans of 4 and could not stop
        any. request-changes now kicks the fix back (the review is the evidence, no advisor), at most
        --review-kickback-max times; after that the plan goes to the operator with the review in it."""
        f = self.fixture(["actionable", "actionable"], ["good", "good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\napprove\n")
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(f.calls(), ["triage", "fix", "review", "triage", "fix", "review"])
        self.assertIn("round 0: the review requested changes: The test only checks a constant.", plan["params"]["body"])
        self.assertIn("VERDICT: approve", plan["params"]["body"])
        with open(os.path.join(f.state, "calls")) as fh:
            kicked = [ln for ln in fh.read().splitlines() if ln.startswith("triage") and "ev=yes" in ln]
        self.assertEqual(len(kicked), 1)
        self.assertIn("rows=1", kicked[0], "the kick-back evidence the verdict step reads stays one row")
        # always request-changes: two kick-backs, then the plan carries the review's objection
        g = self.fixture(["actionable"] * 3, ["good"] * 3)
        with open(os.path.join(g.state, "reviews"), "w") as fh:
            fh.write("request-changes\n" * 3)
        plan, _ = g.run("--review-kickback-max", "2")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(g.calls().count("review"), 3)
        self.assertIn("VERDICT: request-changes", plan["params"]["body"])
        self.assertIn("unresolved: the review asked for changes", plan["detail"])
        self.assertIn("unresolved: the review asked for changes", plan["params"]["body"].splitlines()[0])
        # no kick-backs left in the round budget: the plan as before
        h = self.fixture(["actionable"], ["good"])
        with open(os.path.join(h.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        plan, _ = h.run("--kickback-max", "0")
        self.assertEqual((plan["outcome"], h.calls()), ("plan", ["triage", "fix", "review"]), plan)
        self.assertIn("unresolved: the review asked for changes", plan["detail"])

    def test_a_sent_back_round_that_fails_keeps_the_last_clean_fix(self):
        """Round 0 is gate-clean and the review asks for changes; the round that follows fails a gate or its
        verdict is not actionable. The run returns round 0's plan with the review attached, not an escalation."""
        f = self.fixture(["actionable", "actionable", "needs-info"], ["good", "notest"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        plan, _ = f.run()
        self.assertEqual((plan["outcome"], plan["action"]), ("plan", "github.open_pr"), plan)
        self.assertIn("+module.exports = () => 2;", plan["params"]["diff"])
        self.assertIn("VERDICT: request-changes", plan["params"]["body"])
        self.assertIn("round 0: the review requested changes", plan["params"]["body"])
        self.assertIn("round 1: repro-first violated", plan["params"]["body"])
        self.assertIn("a clean fix in round 0", plan["detail"])
        # the verdict after the review's kick-back is not actionable: the same
        e = self.fixture(["actionable", "needs-info"], ["good"])
        with open(os.path.join(e.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        plan, _ = e.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertNotIn("Traceback", plan["params"]["body"])      # a Stop is a reason, not a bug
        # the limit reached by a failing round keeps it too
        g = self.fixture(["actionable"], ["good", "notest"])
        with open(os.path.join(g.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        plan, _ = g.run("--kickback-max", "1")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("a clean fix in round 0", plan["detail"])
        # no round was ever gate-clean: still an escalation with no plan
        h = self.fixture(["actionable", "needs-info"], ["notest"])
        plan, _ = h.run()
        self.assertEqual(plan["outcome"], "escalate", plan)

    def _later_round_raises(self, exc):
        """Round 0 is clean and the review sends it back; round 1's attempt raises `exc`."""
        f = self.fixture(["actionable", "actionable"], ["good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        real, calls = fix.Chain.attempt, []

        def attempt(chain, manifest):
            calls.append(manifest)
            if len(calls) > 1:
                raise exc
            return real(chain, manifest)
        fix.Chain.attempt = attempt
        try:
            return f.run()[0]
        finally:
            fix.Chain.attempt = real

    def test_a_later_round_that_raises_keeps_the_last_clean_fix(self):
        for exc in (OSError("disk full"), subprocess.SubprocessError("boom"), ValueError("bad json")):
            plan = self._later_round_raises(exc)
            self.assertEqual((plan["outcome"], plan["action"]), ("plan", "github.open_pr"), (exc, plan))
            self.assertIn("+module.exports = () => 2;", plan["params"]["diff"])
            self.assertIn(type(exc).__name__, plan["params"]["body"])
            self.assertIn("Traceback", plan["params"]["body"], exc)    # a bug's traceback is not hidden
            self.assertIn("in attempt", plan["params"]["body"], exc)
        # not a failure a round can have: it still ends the run
        with self.assertRaises(KeyboardInterrupt):
            self._later_round_raises(KeyboardInterrupt())

    def test_a_kept_plan_says_the_review_s_changes_are_unresolved(self):
        plan = self._later_round_raises(OSError("disk full"))
        self.assertIn("unresolved: the review asked for changes", plan["detail"])
        self.assertIn("a clean fix in round 0", plan["detail"])
        first = plan["params"]["body"].splitlines()[0]
        self.assertIn("unresolved: the review asked for changes", first)
        self.assertIn("round 0", first)
        # a plan whose review approved says no such thing
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run()
        self.assertNotIn("unresolved", plan["detail"])
        self.assertNotIn("unresolved", plan["params"]["body"])

    def test_a_quoted_instruction_is_not_a_request_for_changes(self):
        asks = fix.review_requests_changes
        for quoted in ('I was told to end with "VERDICT: request-changes" or approve.',
                       "Reply `VERDICT: request-changes` only if it is broken.",
                       "> the prompt says: VERDICT: request-changes",
                       "It reads fine.\n```\nend with VERDICT: request-changes\n```\nNothing to change.",
                       "I was told to end with “VERDICT: request-changes” or approve."):
            self.assertFalse(asks(quoted), quoted)
        self.assertTrue(asks("It is broken, so VERDICT: request-changes"))
        self.assertTrue(asks('Quoting "fine" aside, VERDICT: request-changes'))

    def test_the_review_decides_on_its_last_verdict_line(self):
        f = self.fixture(["actionable"], ["good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write(r'approve (I rejected \"VERDICT: request-changes\")' + "\n")
        plan, _ = f.run()
        self.assertEqual((plan["outcome"], f.calls()), ("plan", ["triage", "fix", "review"]), plan)
        asks = fix.review_requests_changes
        self.assertFalse(asks("End with VERDICT: request-changes or approve.\nVERDICT: approve"))
        self.assertTrue(asks("VERDICT: approve\nOn reflection:\n**VERDICT: request-changes**"))
        self.assertFalse(asks("VERDICT: request-changes\nVERDICT: approve"))
        self.assertTrue(asks("no verdict line, but VERDICT: request-changes mid-line"))
        self.assertFalse(asks("looks fine"))

    def test_only_the_fixer_may_change_the_tree(self):
        """SR-0001 H8: the verdict, the advisor and the review read; only the fix step gets pi's default
        tools (the advisor once started a dev server). Read-only is a tool allowlist, not a prompt."""
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        f.run()
        with open(os.path.join(f.state, "calls")) as fh:
            raw = [ln for ln in fh.read().splitlines() if ln.strip()]
        lines = [ln.split() for ln in raw]
        tools = {ln[0]: [w for w in ln if w.startswith("tools=")] for ln in lines}
        for step in ("triage", "advisor", "review"):
            self.assertEqual(tools[step], ["tools=read,grep,find,ls"], step)
        self.assertEqual(tools["fix"], ["tools=all"])
        for leaf in ("triage_run.sh", "advisor_run.sh"):
            with open(os.path.join(fix.BAKEOFF, leaf)) as fh:
                self.assertIn('${PI_TOOLS:+--tools "$PI_TOOLS"}', fh.read(), leaf)

    def test_a_new_failure_kicks_back_and_needs_info_escalates(self):
        """The fix breaks src/b.js, which keep.test.js (related, passing on the base) checks."""
        f = self.fixture(["actionable", "needs-info"], ["breaks"])
        plan, out = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("after round 0", plan["detail"])
        with open(os.path.join(f.state, "calls")) as fh:
            self.assertNotIn("review", fh.read())

    def test_the_acceptance_transition(self):
        """A fix at the advisor's pin that still fails is an acceptance gap: no second advisor, and a verdict
        that comes back actionable is forced to needs-info (chain.go, measured 2026-07-26)."""
        f = self.fixture(["actionable", "actionable", "actionable"], ["notest", "notest"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("needs info", plan["detail"])
        self.assertEqual(f.calls(), ["triage", "fix", "advisor", "triage", "fix", "triage"])
        with open(os.path.join(f.state, "calls")) as fh:
            self.assertIn("gap=1", fh.read().splitlines()[-1])

    def test_scope_of_the_tests(self):
        """RFC-0010 §9: a test that passes on the base does not reproduce; a new failure in a related test
        stops the fix; a lockfile is out of scope. "A repro test must load and fail on the base": a test that
        cannot load on the base (it imports what the fix adds) shows no behaviour, so it is noted, and the
        fix needs at least one other test that loads and fails there (phase B: 2 of the first 4 plans
        passed the gates and were wrong)."""
        cases = {"norepro": "repro: src/a.test.js passes on the base without the fix",
                 "breaks": "new failures in the related tests: keep.test.js",
                 "lockfile": "out of scope: the diff touches package-lock.json",
                 "halffix": "the fix's own tests fail with the fix: src/a.test.js",
                 "newmod": "repro: no test loads and fails on the base (src/c.test.js does not load there)"}
        for fix_, want in cases.items():
            f = self.fixture(["actionable"], [fix_])
            plan, _ = f.run("--kickback-max", "0")
            self.assertEqual(plan["outcome"], "escalate", fix_)
            self.assertIn(want, plan["detail"], fix_)
        f = self.fixture(["actionable"], ["goodnewmod"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("Not loadable on the base (they import what the fix adds): src/c.test.js", plan["params"]["body"])

    def test_base_and_harness_seams(self):
        """Phase B's seams: --base fixes a given commit; --harness picks harnesses/<name>.sh."""
        f = self.fixture(["actionable"], ["good"])
        shutil.copy(os.path.join(f.bakeoff, "harnesses", "pi.sh"), os.path.join(f.bakeoff, "harnesses", "other.sh"))
        plan, _ = f.run("--base", f.base, "--harness", "other")
        self.assertEqual((plan["outcome"], plan["params"]["base"]), ("plan", f.base), plan)
        g = self.fixture(["actionable"], ["good"])
        plan, _ = g.run("--harness", "missing")
        self.assertEqual(plan["outcome"], "escalate", "a harness that is not there")

    def test_the_kickback_limit(self):
        f = self.fixture(["actionable", "actionable"], ["notest", "nothing"])
        plan, _ = f.run("--kickback-max", "1", "--no-advisor")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("stuck: kick-back limit (1) reached; last: the fix changed nothing", plan["detail"])
        self.assertEqual(f.calls(), ["triage", "fix", "triage", "fix"])

    def test_out_of_scope(self):
        f = self.fixture(["actionable"], ["workflow"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("last: out of scope: the diff touches .github/workflows/x.yml", plan["detail"])

    def test_a_case_without_an_issue(self):
        f = self.fixture([], [])
        old = os.environ.get("FLOTILLA_CASE")
        plan_path = os.path.join(f.d, "p.json")
        out = fix.main(["--repo", "chipi/orrery", "--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer",
                        "--reviewer-model", "bugfix-reviewer", "--scratch", f.scratch],
                       environ={"FLOTILLA_CASE": json.dumps({"id": "c", "data": {}}), "FLOTILLA_PLAN_FILE": plan_path,
                                "FLOTILLA_MODELS": json.dumps(MODELS), "PATH": os.environ["PATH"]})
        self.assertEqual(out["outcome"], "escalate")
        self.assertIn("no issue", out["detail"])
        self.assertEqual(old, os.environ.get("FLOTILLA_CASE"))


class PythonRepo(unittest.TestCase):
    """The chain on a Python-shaped repo: --test-format pytest, --related pytest-dirs."""
    PYCONF = ("--out-of-scope", r"^(pyproject\.toml|Makefile|(tests/)?conftest\.py)$")

    def fixture(self, verdicts, fixes):
        f = Fixture(verdicts, fixes, py=True)
        self.addCleanup(f.close)
        return f

    def test_a_clean_fix_goes_through_repro_the_base_check_related_and_the_gates(self):
        f = self.fixture(["actionable"], ["pygood"])
        plan, _ = f.run(*self.PYCONF)
        self.assertEqual((plan["outcome"], plan["action"]), ("plan", "github.open_pr"), plan)
        p = plan["params"]
        self.assertEqual(p["repo"], "chipi/podcast_scraper")
        self.assertIn("1 test files fail on the base and pass with the fix", p["body"])
        self.assertIn("4 related files, none newly failing", p["body"], "test_keep, test_red, sub/test_s and the fix's test_mod under pkg/")
        self.assertIn("related: tests/unit/podcast_scraper/pkg", p["body"])
        self.assertNotIn("flotilla-suite", p["diff"])

    def test_a_new_failure_in_the_mirrored_directory_stops_the_fix(self):
        f = self.fixture(["actionable"], ["pybreaks"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("new failures in the related tests: tests/unit/podcast_scraper/pkg/test_keep.py", plan["detail"])

    def test_no_mirrored_directory_says_so_and_does_not_run_the_suite(self):
        f = self.fixture(["actionable"], ["pylone"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("related: no mirrored test directory for src/podcast_scraper/lone/z.py: skipped", plan["params"]["body"])
        self.assertIn("0 related files", plan["params"]["body"])

    def test_a_nested_mirrored_directory_is_run_once_under_its_parent(self):
        f = self.fixture(["actionable"], ["pynested"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        body = plan["params"]["body"]
        self.assertIn("- related: tests/unit/podcast_scraper/pkg\n", body)
        self.assertIn("4 related files", body)

    def test_a_fix_whose_only_test_is_skipped_does_not_reproduce(self):
        f = self.fixture(["actionable"], ["pyskip"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("repro: no test loads and fails on the base", plan["detail"])

    def test_the_profile_s_out_of_scope_paths(self):
        f = self.fixture(["actionable"], ["pyconf"])
        plan, _ = f.run("--kickback-max", "0", *self.PYCONF)
        self.assertIn("out of scope: the diff touches pyproject.toml", plan["detail"])


if __name__ == "__main__":
    unittest.main()
