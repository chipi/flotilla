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
    echo "triage $V ev=${EV:+yes} gap=$ACCEPTANCE_GAP model=$TRIAGE_MODEL provider=$PI_PROVIDER img=$IMG" >> "$FAKE_STATE/calls"
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
    echo "fix $F model=$PI_MODEL" >> "$FAKE_STATE/calls"
    cd "$WT"
    case "$F" in
      good)     echo 'module.exports = () => 2;' > src/a.js; echo 'expect value 2' > src/a.test.js ;;
      notest)   echo 'module.exports = () => 2;' > src/a.js ;;
      breaks)   echo 'module.exports = () => 2;' > src/a.js; echo 'expect value 2' > src/a.test.js; echo FAIL > old.test.js ;;
      workflow) echo 'expect value 2' > src/a.test.js; mkdir -p .github/workflows; echo on: push > .github/workflows/x.yml ;;
      nothing)  ;;
    esac
    printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"output":7}}}\\n'
    ''')

ADVISOR = textwrap.dedent('''\
    echo "advisor" >> "$FAKE_STATE/calls"
    printf '{"file": "src/a.js", "function": "value", "decoy": "src/b.js", "rationale": "it computes it"}' > "$2/advisor.json"
    ''')

# the reviewer is pi itself, on PATH
PI_BIN = textwrap.dedent('''\
    echo "review model=$6" >> "$FAKE_STATE/calls"
    printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Fine.\\\\nVERDICT: approve"}]}}\\n'
    ''')

# a vitest-shaped report: every *.test.js is one test, failed when it says FAIL
SUITE = textwrap.dedent('''\
    import json, os, sys
    res = []
    for root, _, files in os.walk("."):
        if ".git" in root:
            continue
        for f in files:
            if f.endswith(".test.js"):
                p = os.path.abspath(os.path.join(root, f))
                st = "failed" if "FAIL" in open(p).read() else "passed"
                res.append({"name": p, "assertionResults": [{"status": st, "fullName": f}]})
    json.dump({"testResults": res}, open(sys.argv[1], "w"))
    ''')


def write(path, text, mode=0o755):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write("#!/usr/bin/env bash\nset -eu\n" + text if path.endswith(".sh") or path.endswith("/pi") else text)
    os.chmod(path, mode)


def git(*a, cwd):
    subprocess.run(["git", *a], cwd=cwd, check=True, capture_output=True)


class Fixture:
    def __init__(self, verdicts, fixes):
        self.d = tempfile.mkdtemp()
        b = os.path.join(self.d, "bakeoff")
        write(os.path.join(b, "triage_run.sh"), TRIAGE)
        write(os.path.join(b, "advisor_run.sh"), ADVISOR)
        write(os.path.join(b, "harnesses", "pi.sh"), PI_HARNESS)
        write(os.path.join(self.d, "bin", "pi"), PI_BIN)
        self.suite = os.path.join(self.d, "suite.py")
        write(self.suite, SUITE, 0o644)
        self.state = os.path.join(self.d, "state")
        os.makedirs(self.state)
        for name, items in (("verdicts", verdicts), ("fixes", fixes)):
            with open(os.path.join(self.state, name), "w") as f:
                f.write("".join(x + "\n" for x in items))
        # the remote: chipi/orrery with one passing test and one already red on main
        src = os.path.join(self.d, "src")
        os.makedirs(os.path.join(src, "src"))
        os.makedirs(os.path.join(src, "static", "images"))
        for rel, text in (("src/a.js", "module.exports = () => 1;\n"), ("old.test.js", "fine\n"), ("red.test.js", "FAIL\n"),
                          ("static/images/x.png", "a big image\n")):
            with open(os.path.join(src, rel), "w") as f:
                f.write(text)
        git("init", "-q", "-b", "main", cwd=src)
        git("-c", "user.email=t@t", "-c", "user.name=t", "add", "-A", cwd=src)
        git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base", cwd=src)
        remote = os.path.join(self.d, "remotes", "chipi", "orrery")
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
                   FLOTILLA_CASE=json.dumps({"id": "case-20261115-abc", "kind": "code_bug", "target": "repo:chipi/orrery/issues/12",
                                             "data": {"issue": {"repo": "chipi/orrery", "number": 12, "title": "value is 1",
                                                                "body": "value() returns 1, want 2", "url": "u"}}}))
        old = fix.BAKEOFF
        fix.BAKEOFF = self.bakeoff
        try:
            out = fix.main(["--repo", "chipi/orrery", "--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer",
                            "--reviewer-model", "bugfix-reviewer", "--setup", "true", "--suite-cmd", f"python3 {self.suite} %s",
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
        self.assertIn("1 failing on the base", p["body"], "red.test.js is the base's red set, not a new failure")
        self.assertIn("VERDICT: approve", p["body"])
        self.assertEqual(f.calls(), ["triage", "fix", "review"])
        self.assertEqual(os.listdir(os.path.join(f.scratch, "runs")), [], "the run's directory is deleted")

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

    def test_a_new_failure_kicks_back_and_needs_info_escalates(self):
        f = self.fixture(["actionable", "needs-info"], ["breaks"])
        plan, _ = f.run()
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


if __name__ == "__main__":
    unittest.main()
