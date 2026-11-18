"""python3 bugfix/test_pi_golden.py: pi's behaviour in the bugfix chain, pinned (the multi-harness plan, step 0).

The chain's pi paths must stay byte-identical while other harnesses are added around them. Two goldens, recorded
from the code as it was before any of that work and compared on every run since:

  testdata/golden_pi_commands.json   what each leaf (verdict, fix, repro, advisor) and the reviewer's `pi` were
                                     called with, in order, for one scripted chain per path: argv, cwd and the
                                     environment the leaves read, and the plan the chain ended with
  testdata/golden_pi_parse.json      what the chain reads out of pi's streams and sessions: assistant_text,
                                     stream_error, usage, and the calls-file lines of a session

A change that moves one of these is a change to pi's behaviour: replay it, then regenerate on purpose
(GOLDEN_UPDATE=1 python3 bugfix/test_pi_golden.py) and say so in the commit.
"""
import contextlib
import io
import json
import os
import re
import subprocess
import sys
import tempfile
import shutil
import unittest
import unittest.mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import callrec_pi  # noqa: E402
import fix  # noqa: E402
import spend  # noqa: E402
import test_fix as tf  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "testdata")
COMMANDS = os.path.join(DATA, "golden_pi_commands.json")
PARSE = os.path.join(DATA, "golden_pi_parse.json")
UPDATE = bool(os.environ.get("GOLDEN_UPDATE"))

# the environment the leaves and the real pi.sh read (omitted when unset: unset and empty differ)
ENV_KEYS = ("PI_PROVIDER", "PI_MODEL", "PI_TOOLS", "PI_RESUME_SESSION", "BUGFIX_PI_RULES", "PI_CODING_AGENT_DIR",
            "TRIAGE_MODEL", "ADVISOR_MODEL", "ACCEPTANCE_GAP", "TRIAGE_BASE", "HOME", "BAKEOFF_ROOT", "BAKEOFF_WT",
            "BAKEOFF_TRIAGED_DIR")

RECORDER = """\
import json, os, sys
keys = %r
rec = {"leaf": sys.argv[1], "argv": sys.argv[2:], "cwd": os.getcwd(),
       "env": {k: os.environ[k] for k in keys if k in os.environ}}
with open(os.path.join(os.environ["FAKE_STATE"], "golden.jsonl"), "a") as f:
    f.write(json.dumps(rec) + "\\n")
""" % (ENV_KEYS,)

# name: (verdicts, fixes, extra state files, chain arguments)
SCENARIOS = {
    "fresh_clean_fix": (["actionable"], ["good"], {}, []),
    "gate_kickback_resumes_the_session": (["actionable", "actionable"], ["notest", "good"], {}, []),
    "gate_kickback_no_resume": (["actionable", "actionable"], ["notest", "good"], {}, ["--no-resume"]),
    "review_kickback_resumes_the_session": (["actionable"], ["good", "good"], {"reviews": ["request-changes", "approve"]}, []),
    "reproducer_then_fix": (["actionable"], ["srconly"], {"repros": ["good"]}, []),
    "review_tools_read": (["actionable"], ["good"], {}, ["--review-tools", "read"]),
    "rules_given_and_review_reads": (["actionable"], ["good"], {}, ["--pi-rules", "/rules/fleet.md", "--review-tools", "read"]),
    "rules_given_and_review_has_no_tools": (["actionable"], ["good"], {}, ["--pi-rules", "/rules/fleet.md"]),
    "no_rules": (["actionable"], ["good"], {}, ["--pi-rules", ""]),
    "fix_turn_cap": (["actionable", "needs-info"], ["chatty"], {}, ["--fix-max-turns", "3", "--no-advisor"]),
    "reproducer_turn_cap": (["actionable"], [], {"repros": ["chatty"]}, ["--repro-max-turns", "3"]),
    "fix_timeout": (["actionable"], ["hang"], {}, ["--episode-timeout", "2", "--kickback-max", "0"]),
}
WITH_CALLS = ("gate_kickback_resumes_the_session",)


def wrap(path, name):
    """The fake at path, moved to path.orig and replaced by a wrapper that records what it was called with and then
    runs it: the leaf's argv and environment are what the golden is made of."""
    os.rename(path, path + ".orig")
    tf.write(path, f'python3 "$REC" {name} "$@"\nexec bash "{path}.orig" "$@"\n')


def instrument(f):
    rec = os.path.join(f.d, "rec.py")
    with open(rec, "w") as fh:
        fh.write(RECORDER)
    for rel, name in (("triage_run.sh", "verdict"), ("advisor_run.sh", "advisor"), ("harnesses/pi.sh", "fix"),
                      ("harnesses/repro.sh", "repro")):
        wrap(os.path.join(f.bakeoff, rel), name)
    wrap(os.path.join(f.d, "bin", "pi"), "review")
    return rec


def normalizer(f):
    subs = [(os.path.realpath(f.d), "<D>"), (f.d, "<D>"), (f.base, "<BASE>"), (f.base[:12], "<BASE12>"),
            (os.path.dirname(os.path.abspath(fix.__file__)), "<BUGFIX>")]

    def norm(x):
        if isinstance(x, str):
            for a, b in subs:
                x = x.replace(a, b)
            x = re.sub(r"(s-[a-z]+)-\d+\.jsonl", r"\1-N.jsonl", x)
            return re.sub(r"(sess-[a-z]+-)\d+", r"\1N", x)
        if isinstance(x, list):
            return [norm(v) for v in x]
        if isinstance(x, dict):
            return {k: norm(v) for k, v in x.items()}
        return x
    return norm


def run_scenario(name):
    verdicts, fixes, state, args = SCENARIOS[name]
    f = tf.Fixture(verdicts, fixes)
    try:
        for fname, items in state.items():
            with open(os.path.join(f.state, fname), "w") as fh:
                fh.write("".join(x + "\n" for x in items))
        f.repro = "repros" in state
        rec = instrument(f)
        calls = os.path.join(f.d, "calls.jsonl")
        env = {"REC": rec}
        if name in WITH_CALLS:
            env.update(FAKE_SESS="1", FLOTILLA_CALLS_FILE=calls)
        with unittest.mock.patch.dict(os.environ, env), contextlib.redirect_stdout(io.StringIO()):
            plan, _ = f.run(*args)
        with open(os.path.join(f.state, "golden.jsonl")) as fh:
            invocations = [json.loads(x) for x in fh]
        norm = normalizer(f)
        p = plan.get("params") or {}
        out = {"plan": {k: plan.get(k) for k in ("outcome", "action", "detail")} | {"title": p.get("title"), "body": p.get("body")},
               "invocations": invocations}
        if name in WITH_CALLS:
            with open(calls) as fh:
                lines = [json.loads(x) for x in fh]
            for c in lines:
                c.pop("at", None)
            out["calls"] = lines
        return norm(out)
    finally:
        f.close()


def load(path):
    with open(path) as fh:
        return json.load(fh)


def save(path, doc):
    with open(path, "w") as fh:
        json.dump(doc, fh, indent=1, sort_keys=True)
        fh.write("\n")


class Commands(unittest.TestCase):
    def test_each_path_calls_pi_as_it_always_did(self):
        got = {name: run_scenario(name) for name in SCENARIOS}
        if UPDATE:
            save(COMMANDS, got)
        want = load(COMMANDS)
        self.assertEqual(sorted(got), sorted(want), "a scenario was added or removed: regenerate on purpose")
        for name in SCENARIOS:
            with self.subTest(name):
                self.assertEqual(got[name], want[name])

    def test_a_scenario_is_the_same_twice(self):
        """The goldens hold nothing that changes from run to run (paths, shas, random session names)."""
        self.assertEqual(run_scenario("review_kickback_resumes_the_session"), run_scenario("review_kickback_resumes_the_session"))

    def test_the_scenarios_cover_what_the_plan_names(self):
        want = load(COMMANDS)
        leaves = {inv["leaf"] for sc in want.values() for inv in sc["invocations"]}
        self.assertEqual(leaves, {"verdict", "fix", "repro", "advisor", "review"})
        resumed = [sc for sc in want.values() if any(inv["env"].get("PI_RESUME_SESSION") for inv in sc["invocations"])]
        self.assertEqual(len(resumed), 2, "a gate kick-back and a review kick-back resume")
        reviews = {tuple(a for a in inv["argv"] if a in ("--no-tools", "--tools", "--no-context-files", "--append-system-prompt"))
                   for sc in want.values() for inv in sc["invocations"] if inv["leaf"] == "review"}
        self.assertEqual(reviews, {("--no-tools", "--no-context-files"), ("--tools", "--no-context-files", "--append-system-prompt"),
                                   ("--no-tools",)})


class RealLeaves(unittest.TestCase):
    """harnesses/pi.sh and repro.sh themselves, run by bash with a fake `pi` on PATH that records its argv: the
    prompts the chain's fix and reproducer steps send."""

    CASES = (("fix_fresh_with_rules", "pi.sh", {"BUGFIX_PI_RULES": "/r.md"}),
             ("fix_fresh_without_rules", "pi.sh", {}),
             ("fix_resume_with_rules", "pi.sh", {"BUGFIX_PI_RULES": "/r.md", "PI_RESUME_SESSION": "/s/one.jsonl"}),
             ("fix_resume_without_rules", "pi.sh", {"PI_RESUME_SESSION": "/s/one.jsonl"}),
             ("repro_with_rules", "repro.sh", {"BUGFIX_PI_RULES": "/r.md"}),
             ("repro_without_rules", "repro.sh", {}))
    DESC = "THE ISSUE AS FILED:\nvalue() returns 1, want 2\n\nTHE TASK:\nmake value() return 2 ($HOME `x` \"q\" \\n)"

    def record(self, script, extra):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        tf.write(os.path.join(d, "bin", "pi"),
                 'python3 -c \'import json,os,sys; json.dump({"argv": sys.argv[1:], "cwd": os.getcwd(), "stdin_is_null": '
                 'os.fstat(0).st_rdev == os.stat("/dev/null").st_rdev}, open(os.environ["OUT"], "w"))\' "$@"\n')
        out = os.path.join(d, "out.json")
        env = {k: v for k, v in os.environ.items() if not k.startswith(("PI_", "BUGFIX_"))}
        env.update(PATH=os.path.join(d, "bin") + os.pathsep + env["PATH"], OUT=out, PI_MODEL="m1", PI_PROVIDER="p1", **extra)
        bakeoff = os.path.join(os.path.dirname(HERE), "bugfix-fleet", "bakeoff", "harnesses", script)
        wt = os.path.join(d, "wt")
        os.makedirs(wt)
        subprocess.run(["bash", bakeoff, wt, self.DESC], env=env, check=True, capture_output=True)
        got = load(out)
        got["cwd"] = got["cwd"].replace(os.path.realpath(d), "<D>")
        return got

    def test_the_fix_and_repro_leaves_send_what_they_always_did(self):
        got = {name: self.record(script, extra) for name, script, extra in self.CASES}
        if UPDATE:
            doc = load(PARSE) if os.path.exists(PARSE) else {}
            doc["leaves"] = got
            save(PARSE, doc)
        want = load(PARSE)["leaves"]
        for name in got:
            with self.subTest(name):
                self.assertEqual(got[name], want[name])


def stream(name):
    with open(os.path.join(DATA, "pi", name)) as fh:
        return fh.read()


class Parsing(unittest.TestCase):
    STREAMS = ("stream_ok.jsonl", "stream_error.jsonl", "stream_error_bare.jsonl", "stream_capped.jsonl")

    def pi_cost(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        os.makedirs(os.path.join(d, "sessions", "--wt--"))
        shutil.copy(os.path.join(DATA, "pi", "session.jsonl"), os.path.join(d, "sessions", "--wt--", "s.jsonl"))
        return spend.pi_cost(d)

    def parsed(self):
        out = {}
        for name in self.STREAMS:
            s = stream(name)
            out[name] = {"assistant_text": fix.assistant_text(s), "stream_error": fix.stream_error(s), "usage": list(fix.usage(s))}
        lines = callrec_pi.session_lines(os.path.join(DATA, "pi", "session.jsonl"), "coder", "bugfix-flash", 3, "case-1",
                                         now="2026-11-18T10:00:09Z")
        callrec_pi.apportion(lines, 0.9)
        out["session.jsonl"] = {"lines": lines,
                                "resumed_after_one": callrec_pi.session_lines(os.path.join(DATA, "pi", "session.jsonl"), "coder", "x", 3,
                                                                              "case-1", start=1, now="2026-11-18T10:00:09Z"),
                                "pi_cost": self.pi_cost()}
        return out

    def test_what_the_chain_reads_out_of_pi_s_streams_and_sessions(self):
        got = self.parsed()
        if UPDATE:
            doc = load(PARSE) if os.path.exists(PARSE) else {}
            doc["streams"] = got
            save(PARSE, doc)
        want = load(PARSE)["streams"]
        self.assertEqual(sorted(got), sorted(want))
        for name in got:
            with self.subTest(name):
                self.assertEqual(got[name], want[name])

    def test_the_golden_values_say_what_they_should(self):
        """Read by hand once, so a regenerated golden that went wrong is not simply believed."""
        want = load(PARSE)["streams"]
        self.assertEqual(want["stream_ok.jsonl"], {"assistant_text": "Fixed: value() now returns 2.", "stream_error": "",
                                                    "usage": [3, 45]})
        self.assertEqual(want["stream_error.jsonl"]["stream_error"], "400: UnsupportedParamsError reasoning_effort")
        self.assertEqual(want["stream_error.jsonl"]["assistant_text"], "Trying.")
        self.assertEqual(want["stream_error_bare.jsonl"]["stream_error"], "error")
        self.assertEqual((want["stream_capped.jsonl"]["assistant_text"], want["stream_capped.jsonl"]["usage"]), ("three", [3, 3]))
        lines = want["session.jsonl"]["lines"]
        self.assertEqual([(c["step"], c["episode"], c["episode_seq"], c["tokens_in"], c["tokens_out"]) for c in lines],
                         [(1, "sess-golden", 3, 100, 20), (2, "sess-golden", 3, 200, 60), (3, "sess-golden", 3, None, None)])
        self.assertEqual(lines[0]["tools"], [{"name": "bash", "arg": "cat src/a.js", "ok": True},
                                             {"name": "read", "arg": "src/b.js", "ok": False}])
        self.assertAlmostEqual(sum(c["cost_usd"] or 0 for c in lines), 0.9)
        self.assertEqual(want["session.jsonl"]["pi_cost"], 0.5)
        self.assertEqual(len(want["session.jsonl"]["resumed_after_one"]), 2)


if __name__ == "__main__":
    unittest.main()
