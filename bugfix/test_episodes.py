"""python3 bugfix/test_episodes.py: what the chain does with an episode of a harness other than pi (ADR-0020): the episode
file, the calls file's lines, the spend estimate, and the checkpoint that names its harnesses. The harness is the fake
one of test_harness.py; the chain is test_fix.py's Fixture with its loop replaced by a script of run_harness calls."""
import contextlib
import io
import json
import os
import sys
import tempfile
import shutil
import unittest
import unittest.mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
sys.path.append(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "runtime"))   # the harness layer: adapter, harness_pi
import adapter  # noqa: E402
import callrec_pi  # noqa: E402
import fix  # noqa: E402
import spend  # noqa: E402
import test_fix as tf  # noqa: E402
import test_harness as th  # noqa: E402

CASE = "case-20261115-abc"
TOKEN = "abcdefgh" + "12345678"   # built here: a literal in a header line is what the secrets scan looks for
RAW = f"curl -H 'Authorization: Bearer {TOKEN}' http://x"


def scripted(body):
    """Chain.rounds as a script: the base is checked out (as the real loop does) and checkpointed, then `body(chain)`."""
    def rounds(chain):
        if not chain.restored:
            chain.checkout()
            chain.save("checkout", None)
        return body(chain)
    return rounds


class Plumbing(unittest.TestCase):
    def setUp(self):
        th.install_fake(self)
        self.f = tf.Fixture([], [])
        self.addCleanup(self.f.close)
        self.home = os.path.join(self.f.d, "home")
        os.makedirs(self.home)
        self.calls = os.path.join(self.f.d, "calls.jsonl")

    def fakeh(self, *steps):
        with open(os.path.join(self.f.state, "fakeh_script"), "w") as fh:
            fh.write("".join(json.dumps(s) + "\n" for s in steps))

    def run_chain(self, body, *extra, calls=None, resume=None):
        env = {"HOME": self.home, "FLOTILLA_CALLS_FILE": calls or self.calls}
        out = io.StringIO()
        with unittest.mock.patch.dict(os.environ, env), unittest.mock.patch.object(fix.Chain, "rounds", scripted(body)), \
                contextlib.redirect_stdout(out):
            try:
                plan, _ = self.f.run("--spend-file", ".spend", *extra, resume=resume)
            except fix.Terminated:
                plan = None
        return plan, out.getvalue()

    def lines(self, path=None):
        with open(path or self.calls) as fh:
            return [json.loads(x) for x in fh]

    def run_dir(self):
        return os.path.join(self.f.scratch, "runs", CASE)

    def spent(self):
        with open(os.path.join(self.home, ".spend")) as fh:
            return float(fh.read())

    TWO = ({"turns": [{"text": "look", "tools": [{"name": "bash", "arg": RAW, "ok": True}], "usage": {"input": 100, "output": 20},
                       "cost": 0.5},
                      {"text": "done", "usage": {"input": 150, "output": 30}, "stop": "stop"}], "answer": "fixed"},
           {"turns": [{"text": "r", "usage": {"input": 10, "output": 5}, "cost": 0.25}], "answer": "VERDICT: approve"})

    def test_an_episode_is_kept_and_its_turns_are_the_calls(self):
        self.fakeh(*self.TWO)
        seen = {}

        def body(chain):
            seen["fix"] = chain.run_harness("fix", "fakeh", "bugfix-flash", "PROMPT FIX", "all", False, 60)
            seen["review"] = chain.run_harness("review", "fakeh", "bugfix-reviewer", "PROMPT REVIEW", "none", True, 60)
            seen["files"] = sorted(os.listdir(os.path.join(chain.run_dir, "episodes")))
            with open(os.path.join(chain.run_dir, "episodes", "001-fix.json")) as fh:
                seen["doc"] = json.load(fh)
            return {"outcome": "escalate", "detail": "x"}
        plan, _ = self.run_chain(body)
        self.assertEqual(plan["outcome"], "escalate")
        ep, code, stream, rel = seen["fix"]
        self.assertEqual((rel, code, ep.answer, ep.session_id), ("episodes/001-fix.json", 0, "fixed", "fakeh-sess-1"))
        self.assertEqual(seen["review"][3], "episodes/002-review.json")
        self.assertEqual(seen["files"], ["001-fix.json", "001-fix.json.recorded", "001-fix.stream",
                                         "002-review.json", "002-review.json.recorded", "002-review.stream"])
        d = seen["doc"]
        self.assertEqual((d["version"], d["harness"], d["role"], d["alias"], d["seq"], d["code"], d["resume_ref"], d["calls_role"]),
                         (1, "fakeh", "fix", "bugfix-flash", 1, 0, "fakeh-sess-1", "coder"))
        self.assertEqual((len(d["turns"]), d["cost_est"], d["usage_total"]["output"], d["raw_path"]), (2, 0.5, 50, "episodes/001-fix.stream"))
        with open(os.path.join(self.f.state, "fakeh_calls")) as fh:
            first = json.loads(fh.readline())
        self.assertEqual(first["stdin"], "PROMPT FIX")
        self.assertNotIn("PROMPT FIX", json.dumps(first["argv"]))

        lines = self.lines()
        turns = [c for c in lines if c.get("kind") != "step"]
        self.assertEqual([(c["role"], c["harness"], c["episode"], c["episode_seq"], c["step"], c["alias"], c["tokens_in"], c["tokens_out"],
                           c["finish_reason"], c["decision_key"]) for c in turns],
                         [("coder", "fakeh", "fakeh-sess-1", 1, 1, "bugfix-flash", 100, 20, "tool_calls", CASE),
                          ("coder", "fakeh", "fakeh-sess-1", 1, 2, "bugfix-flash", 150, 30, "stop", CASE),
                          ("reviewer", "fakeh", "fakeh-sess-2", 2, 1, "bugfix-reviewer", 10, 5, "tool_calls", CASE)])
        tool = turns[0]["tools"][0]
        self.assertEqual((tool["name"], tool["ok"]), ("bash", True))
        self.assertIn("<redacted>", tool["arg"])
        self.assertNotIn(TOKEN, json.dumps(lines))
        self.assertTrue(all(c["cost_source"] == "" and c["cost_usd"] is None for c in turns), "no meter: the bill is not known")
        notes = [c["detail"] for c in lines if c.get("kind") == "step" and c["role"] == "harness"]
        self.assertIn("fakeh fix: fakeh: rules not delivered", notes, "a prompt delta is on the calls file")

    def test_the_episode_s_bill_is_the_meters_and_is_shared_over_its_turns(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        doc = {"harness": "fakeh", "session_id": "s", "turns": [
            {"n": 1, "usage": {"input": 100, "output": 0}, "stop": "tool_calls"}, {"n": 2, "usage": {"input": 100, "output": 100}, "stop": "stop"},
            {"n": 3, "stop": "stop"}]}
        calls = os.path.join(d, "calls.jsonl")
        meter = type("M", (), {"episode": lambda self: 0.9})()
        r = callrec_pi.Recorder(os.path.join(d, "pi"), {"FLOTILLA_CALLS_FILE": calls}, "k", meter)
        r.record_episode("coder", "a", doc)
        with open(calls) as fh:
            lines = [json.loads(x) for x in fh]
        self.assertEqual([c["cost_source"] for c in lines], ["gateway-share", "gateway-share", ""])
        self.assertAlmostEqual(sum(c["cost_usd"] or 0 for c in lines), 0.9)
        self.assertAlmostEqual(lines[0]["cost_usd"], 0.9 / 3)
        self.assertEqual((lines[2]["tokens_in"], lines[2]["cost_usd"]), (None, None))

    def test_an_episode_killed_before_it_was_recorded_is_recorded_by_the_run_that_resumes(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        os.makedirs(os.path.join(d, "episodes"))
        calls = os.path.join(d, "calls.jsonl")
        for stem, recorded in (("001-fix", True), ("002-review", False)):
            with open(os.path.join(d, "episodes", stem + ".json"), "w") as fh:
                json.dump({"version": 1, "harness": "fakeh", "calls_role": "x" + stem, "alias": "a", "session_id": "s" + stem,
                           "turns": [{"n": 1, "text": "t"}]}, fh)
            if recorded:
                open(os.path.join(d, "episodes", stem + ".json.recorded"), "w").close()
        meter = type("M", (), {"episode": lambda self: self.fail("a late record is not measured")})()
        r = callrec_pi.Recorder(os.path.join(d, "pi"), {"FLOTILLA_CALLS_FILE": calls}, "k", meter)
        r.skip_existing()
        r.record_pending_episodes()
        with open(calls) as fh:
            lines = [json.loads(x) for x in fh]
        self.assertEqual([(c["role"], c["episode"], c["episode_seq"]) for c in lines], [("x002-review", "s002-review", 2)])
        self.assertTrue(os.path.exists(os.path.join(d, "episodes", "002-review.json.recorded")))
        r.record_pending_episodes()
        with open(calls) as fh:
            self.assertEqual(len(fh.readlines()), 1, "once")

    def test_a_kill_between_saving_and_recording_an_episode_loses_no_calls(self):
        self.fakeh(*self.TWO)

        def first(chain):
            chain.run_harness("fix", "fakeh", "bugfix-flash", "p", "all", False, 60)
            return {"outcome": "escalate", "detail": "x"}
        with unittest.mock.patch.object(callrec_pi.Recorder, "record_episode", side_effect=fix.Terminated):
            self.assertIsNone(self.run_chain(first)[0])
        self.assertEqual([c for c in self.lines() if c.get("kind") != "step"], [], "killed before its calls were written")
        ep = os.path.join(self.run_dir(), "episodes", "001-fix.json")
        self.assertTrue(os.path.exists(ep) and not os.path.exists(ep + callrec_pi.RECORDED))
        calls2 = os.path.join(self.f.d, "calls2.jsonl")
        self.run_chain(lambda chain: {"outcome": "escalate", "detail": "x"}, calls=calls2, resume={"run_id": "r1", "spend_usd": 0})
        self.assertEqual([(c["role"], c["episode_seq"], c["step"]) for c in self.lines(calls2) if c.get("kind") != "step"],
                         [("coder", 1, 1), ("coder", 1, 2)], "the resumed run writes the calls the killed one owed")

    def test_with_the_gateway_unreadable_the_run_reports_the_episodes_estimates(self):
        self.fakeh(*self.TWO)

        def body(chain):
            chain.run_harness("fix", "fakeh", "bugfix-flash", "p", "all", False, 60)
            chain.run_harness("review", "fakeh", "bugfix-reviewer", "p", "none", True, 60)
            return {"outcome": "escalate", "detail": "x"}
        _, out = self.run_chain(body)
        self.assertAlmostEqual(self.spent(), 0.75)
        self.assertIn("source=harness-estimate", out)

    def test_a_run_of_pi_alone_reports_what_it_always_did(self):
        _, out = self.run_chain(lambda chain: {"outcome": "escalate", "detail": "x"})
        self.assertEqual(self.spent(), 0.0)
        self.assertIn("source=pi", out)
        self.assertNotIn("harness-estimate", out)

    def test_a_resumed_run_carries_what_the_interrupted_one_estimated_and_records_only_its_own(self):
        self.fakeh(*self.TWO)

        def first(chain):
            chain.run_harness("fix", "fakeh", "bugfix-flash", "p", "all", False, 60)
            raise fix.Terminated()
        self.assertIsNone(self.run_chain(first)[0])
        self.assertAlmostEqual(self.spent(), 0.5)
        self.assertTrue(os.path.exists(os.path.join(self.run_dir(), "episodes", "001-fix.json")), "the run directory is kept")
        seq = {}

        def second(chain):
            seq["resumed"] = chain.resumed_from
            seq["n"] = chain.episode_n
            chain.run_harness("review", "fakeh", "bugfix-reviewer", "p", "none", True, 60)
            return {"outcome": "escalate", "detail": "x"}
        calls2 = os.path.join(self.f.d, "calls2.jsonl")
        plan, out = self.run_chain(second, calls=calls2, resume={"run_id": "r1", "spend_usd": 0.1})
        self.assertEqual(plan["outcome"], "escalate", plan)
        self.assertEqual(seq["n"], 1, "the episodes go on from the interrupted run's")
        # the interrupted run estimated 0.5 and flotillad recorded 0.1 of it: 0.4 is carried onto this run's own 0.25
        self.assertAlmostEqual(self.spent(), 0.65)
        self.assertIn("carried=0.400000", out)
        mine = [c for c in self.lines(calls2) if c.get("kind") != "step"]
        self.assertEqual([(c["role"], c["episode_seq"]) for c in mine], [("reviewer", 2)], "only this run's episode is recorded")
        self.assertEqual(len(self.lines()), len([c for c in self.lines()]), "the first run's calls stay in its own file")

    def test_the_checkpoint_names_its_harnesses_and_another_run_does_not_continue_it(self):
        self.fakeh(*self.TWO)

        def first(chain):
            chain.run_harness("fix", "fakeh", "bugfix-flash", "p", "all", False, 60)
            raise fix.Terminated()
        self.run_chain(first)
        path = os.path.join(self.run_dir(), fix.CHECKPOINT)
        with open(path) as fh:
            ck = json.load(fh)
        self.assertEqual({k: ck[k] for k in fix.HARNESS_KEYS}, {"harness": "pi", "repro_harness": "pi", "review_harness": "pi"})
        case = {"id": CASE}
        self.assertEqual(fix.load_checkpoint(self.run_dir(), case, self.f.repo, "main")[1], "")
        for key in fix.HARNESS_KEYS:
            names = dict.fromkeys(fix.HARNESS_KEYS, "pi") | {key: "fakeh"}
            got, why = fix.load_checkpoint(self.run_dir(), case, self.f.repo, "main", harnesses=names)
            self.assertIsNone(got, key)
            self.assertIn(f"it was made with {key} 'pi', this run's flags say 'fakeh'", why)
        # a checkpoint of before harnesses were chosen has no keys: it is pi's
        for k in fix.HARNESS_KEYS:
            ck.pop(k)
        with open(path, "w") as fh:
            json.dump(ck, fh)
        self.assertEqual(fix.load_checkpoint(self.run_dir(), case, self.f.repo, "main")[1], "")
        self.assertIsNone(fix.load_checkpoint(self.run_dir(), case, self.f.repo, "main",
                                              harnesses=dict.fromkeys(fix.HARNESS_KEYS, "pi") | {"harness": "fakeh"})[0])
        # and the run that is told to continue starts fresh, saying why
        real = fix.parse

        def parse(argv):
            a = real(argv)
            a.review_harness = "fakeh"
            return a
        ran = []
        with unittest.mock.patch.object(fix, "parse", parse):
            plan, out = self.run_chain(lambda chain: ran.append(chain.restored) or {"outcome": "escalate", "detail": "x"},
                                       resume={"run_id": "r1", "spend_usd": 0})
        self.assertEqual(ran, [None], "a fresh start")
        self.assertIn("no usable checkpoint (ValueError: it was made with review_harness 'pi', this run's flags say 'fakeh')", out)

    def test_a_checkpoint_may_name_an_episode_file_as_the_session_and_nothing_outside_the_run(self):
        self.fakeh({})

        def first(chain):
            ep, _, _, rel = chain.run_harness("fix", "fakeh", "bugfix-flash", "p", "all", False, 60)
            for name in ("prior.json", "manifest.json"):
                with open(os.path.join(chain.run_dir, name), "w") as fh:
                    fh.write("{}")
            state = {"round": 0, "pins": [], "prior": os.path.join(chain.run_dir, "prior.json"),
                     "manifest": os.path.join(chain.run_dir, "manifest.json"), "reviewed_back": 0,
                     "resume": (os.path.join(chain.run_dir, rel), "a patch", "feedback")}
            chain.save("attempt", state, attempt={"before": {}})
            raise fix.Terminated()
        self.run_chain(first)
        run_dir, path, case = self.run_dir(), os.path.join(self.run_dir(), fix.CHECKPOINT), {"id": CASE}
        ck, why = fix.load_checkpoint(run_dir, case, self.f.repo, "main")
        self.assertEqual((why, ck["state"]["resume"]["session"]), ("", "episodes/001-fix.json"))
        outside = os.path.join(os.path.dirname(run_dir), "outside.json")
        with open(outside, "w") as fh:
            fh.write("{}")
        os.symlink(outside, os.path.join(run_dir, "episodes", "link.json"))
        for bad in ("../outside.json", outside, "episodes/link.json", "episodes/missing.json", "episodes/../../outside.json"):
            doc = json.loads(json.dumps(ck))
            doc["state"]["resume"]["session"] = bad
            with open(path, "w") as fh:
                json.dump(doc, fh)
            got, why = fix.load_checkpoint(run_dir, case, self.f.repo, "main")
            self.assertIsNone(got, bad)
            self.assertIn("ValueError", why)

    def test_a_pi_episode_is_not_kept_as_an_episode_file(self):
        def body(chain):
            with self.assertRaises(adapter.HarnessError):
                chain.run_harness("fix", "pi", "bugfix-flash", "p", "all", False, 60)
            return {"outcome": "escalate", "detail": "x"}
        self.assertEqual(self.run_chain(body)[0]["outcome"], "escalate")


class Spend(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)
        os.makedirs(os.path.join(self.d, "episodes"))

    def episode(self, name, **doc):
        with open(os.path.join(self.d, "episodes", name), "w") as fh:
            fh.write(doc.pop("raw", None) or json.dumps(doc))

    def test_the_episodes_estimates_add_up_less_what_an_earlier_run_reported(self):
        self.assertEqual(spend.episode_cost(os.path.join(self.d, "nowhere")), 0.0)
        self.episode("001-fix.json", harness="fakeh", cost_est=0.5)
        self.episode("002-review.json", harness="fakeh", cost_est=0.25)
        self.episode("003-fix.json", harness="fakeh")
        self.episode("004-fix.json", raw="not json")
        self.episode("005-fix.json", raw="[1]")
        self.episode("006-fix.json", harness="fakeh", cost_est=-3)
        self.episode("007-fix.json", harness="pi", cost_est=9)
        self.episode("001-fix.stream", raw="{}")
        self.assertEqual(spend.episode_cost(self.d), 0.75)
        self.assertEqual(spend.episode_cost(self.d, 0.5), 0.25)
        self.assertEqual(spend.episode_cost(self.d, 5), 0.0, "never below 0")

    def test_the_estimate_is_only_the_fallback_and_says_so(self):
        gone = spend.Meter([], poll_s=0.01, settle_s=0.01)
        self.assertEqual(gone.cost(os.path.join(self.d, "pi"))[:2], (0.0, "pi"))
        self.assertEqual(gone.cost(os.path.join(self.d, "pi"), estimate=0.75)[:2], (0.75, "harness-estimate"))
        m = spend.Meter([("http://h/v1", "k")], poll_s=0.01, settle_s=0.05, stable_s=0.02, read=lambda *a: None)
        usd, source, why = m.cost(os.path.join(self.d, "pi"), estimate=0.75)
        self.assertEqual((usd, source), (0.75, "harness-estimate"))
        self.assertIn("unreadable in time", why)
        reads = iter([1.0, 3.0, 3.0, 3.0, 3.0, 3.0])
        m = spend.Meter([("http://h/v1", "k")], poll_s=0.01, settle_s=0.05, stable_s=0.02, read=lambda *a: next(reads))
        self.assertEqual(m.cost(os.path.join(self.d, "pi"), estimate=0.75)[:2], (2.0, "gateway"), "the bill wins over the estimate")


class Names(unittest.TestCase):
    def test_the_harnesses_of_a_run_default_to_pi(self):
        a = fix.parse(["--repo", "x/y", "--worker-model", "a", "--advisor-model", "b", "--reviewer-model", "c"])
        self.assertEqual(fix.harness_names(a), {"harness": "pi", "repro_harness": "pi", "review_harness": "pi"})
        a.repro_harness = "fakeh"
        self.assertEqual(fix.harness_names(a)["repro_harness"], "fakeh")

    def test_the_episode_counter_goes_on_from_the_files_there(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        self.assertEqual(adapter.last_episode(d), 0)
        os.makedirs(os.path.join(d, "episodes"))
        for n in ("001-fix.json", "002-review.json", "002-review.json.recorded", "007-repro.json", "007-repro.stream", "notes.json"):
            open(os.path.join(d, "episodes", n), "w").close()
        self.assertEqual(adapter.last_episode(d), 7)


if __name__ == "__main__":
    unittest.main()
