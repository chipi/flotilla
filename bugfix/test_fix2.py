"""python3 bugfix/test_fix2.py: bugfix/fix2.py, the chain of a version that declares stations, beside bugfix/fix.py.

What the stations say and the flags may not; the model route the adapter is given (pi's catalog entry survives: bugfix/pi_config.py's
context windows and reply-limit floor); the pi calls of the three stations as the differential harness (bugfix/differential.py) sees
them against the chain as it was before the switch. test_fix.py and test_pi_golden.py keep pinning fix.py."""
import hashlib
import json
import os
import shutil
import sys
import tempfile
import unittest
import unittest.mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import fix  # noqa: E402
import fix2  # noqa: E402
import adapter  # noqa: E402  (fix2 puts runtime/ on the path)
import harness_pi  # noqa: E402
import pi_config  # noqa: E402
import stations  # noqa: E402
import test_fix as tf  # noqa: E402

CATALOG = {"or/flash": {"contextWindow": 1000000, "maxTokens": 4096, "cost": {"input": 0.1, "output": 0.4}},
           "or/reviewer": {"contextWindow": 200000, "maxTokens": 64000}}
UPSTREAM = {"fleet-bugfix-flash": "or/flash", "fleet-bugfix-reviewer": "or/reviewer"}


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


class World:
    """A code root with the version's texts, the stations document flotillad would hand a run of it, and a Chain on them."""

    def __init__(self, tc, **limits):
        self.tmp = tempfile.mkdtemp()
        tc.addCleanup(shutil.rmtree, self.tmp, True)
        self.code = os.path.join(self.tmp, "code")
        d = os.path.join(self.code, "fleets", "bugfix", "fix", "v2")
        os.makedirs(d)
        texts = {"rules.md": "# Rules\nKeep it small.\n", "agent.md": "---\nname: repro\n---\n---\n# reproducer\nWrite one test.\n"}
        for name, text in texts.items():
            with open(os.path.join(d, name), "w") as f:
                f.write(text)
        ref = lambda n: {"path": f"fix/v2/{n}", "sha256": sha(texts[n])}  # noqa: E731
        base = {"max_output_tokens": 32768}
        self.doc = {"schema_version": 1, "version_limits": {"kickback_max": 3},
                    "stations": {"fix": {"harness": "pi", "model": "bugfix-flash", "tools": "all", "texts": {"rules": ref("rules.md")},
                                         "limits": dict(base, timeout="20m", max_turns=90)},
                                 "repro": {"harness": "pi", "model": "bugfix-flash", "tools": "all",
                                           "texts": {"rules": ref("rules.md"), "agent": ref("agent.md")},
                                           "limits": dict(base, timeout="20m", max_turns=40)},
                                 "review": {"harness": "pi", "model": "bugfix-reviewer", "tools": "none", "texts": {"rules": ref("rules.md")},
                                            "limits": dict(base, timeout="10m")}}}
        for k, v in limits.items():
            self.doc["stations"]["fix"]["limits"][k] = v
        self.environ = {"FLOTILLA_MODELS": json.dumps(tf.MODELS), "FLOTILLA_STATIONS": json.dumps(self.doc),
                        "FLOTILLA_CODE_ROOT": self.code, "FLOTILLA_ROUTINE": "bugfix/fix"}
        self.st = stations.load(self.environ)
        self.case = {"id": "case-1", "data": {"issue": {"repo": "example/shop", "number": 12, "title": "t", "body": "b"}}}

    def args(self, *extra):
        return fix2.parse(["--repo", "example/shop", "--scratch", self.tmp, "--upstream", "fleet-bugfix-flash=or/flash",
                           "--upstream", "fleet-bugfix-reviewer=or/reviewer", *extra], self.st)

    def chain(self, *extra):
        run_dir = os.path.join(self.tmp, "runs", "case-1")
        os.makedirs(run_dir)
        with unittest.mock.patch.object(pi_config, "catalog", return_value=CATALOG):
            return fix2.Chain(self.args(*extra), self.case, run_dir, dict(self.environ))


class Parsing(unittest.TestCase):
    def test_the_stations_say_what_the_v1_flags_said(self):
        a = World(self).args()
        self.assertEqual((a.worker_model, a.advisor_model, a.reviewer_model, a.review_tools), ("bugfix-flash", "bugfix-reviewer",
                                                                                                "bugfix-reviewer", "none"))
        self.assertEqual((a.episode_timeout, a.review_timeout, a.fix_max_turns, a.repro_max_turns, a.min_output), (1200, 600, 90, 40, 32768))
        self.assertEqual(a.kickback_max, 3, "the version's own limit")
        self.assertEqual((a.repro_harness, a.review_harness), (fix2.ADAPTER_KEY, fix2.ADAPTER_KEY))

    def test_a_model_a_text_or_a_limit_in_a_flag_is_refused(self):
        w = World(self)
        for flag in ("--worker-model", "--reviewer-model", "--pi-rules", "--review-tools", "--episode-timeout", "--fix-max-turns",
                     "--kickback-max", "--min-output", "--harness"):
            with self.subTest(flag):
                with self.assertRaisesRegex(stations.StationsError, "no model, text or limit in a flag"):
                    w.args(flag, "x")
        with self.assertRaisesRegex(stations.StationsError, "no model, text or limit in a flag"):
            w.args("--kickback-max=1")

    def test_a_missing_station_or_one_the_chain_cannot_run_is_refused_before_any_model_call(self):
        w = World(self)
        for name in ("fix", "repro", "review"):
            doc = json.loads(json.dumps(w.doc))
            del doc["stations"][name]
            with self.subTest(f"no {name}"), self.assertRaisesRegex(stations.StationsError, f"declares no station {name}"):
                fix2.parse(["--repo", "example/shop"], stations.Stations(doc))
        doc = json.loads(json.dumps(w.doc))
        del doc["stations"]["repro"]
        self.assertEqual(fix2.parse(["--repo", "example/shop", "--no-repro"], stations.Stations(doc)).repro, False,
                         "a run without a reproducer needs no repro station")
        for edit, why in ((("fix", "harness", "claude"), "serves pi"), (("review", "tools", "all"), "takes none or read"),
                          (("fix", "tools", "read"), "takes all"), (("fix", "model", ""), "names no model")):
            doc = json.loads(json.dumps(w.doc))
            doc["stations"][edit[0]][edit[1]] = edit[2]
            with self.subTest(edit), self.assertRaisesRegex(stations.StationsError, why):
                fix2.parse(["--repo", "example/shop"], stations.Stations(doc))

    def test_durations(self):
        self.assertEqual([fix2.seconds(x) for x in ("20m", "90s", "1h30m", 5, "2h")], [1200, 90, 5400, 5, 7200])
        for bad in ("", "soon", "5x", True):
            with self.assertRaises(stations.StationsError):
                fix2.seconds(bad)

    def test_a_text_that_is_not_what_flotillad_hashed_is_refused(self):
        w = World(self)
        with open(os.path.join(w.code, "fleets", "bugfix", "fix", "v2", "rules.md"), "w") as f:
            f.write("# Other rules\n")
        with self.assertRaisesRegex(fix2.declared_prompts.PromptError, "differs from the one flotillad checked"):
            fix2.pi_rules(w.args(), w.environ)
        with self.assertRaisesRegex(fix.Stop, "^prompt: "):
            w.chain()


class Route(unittest.TestCase):
    """The route a station is given carries pi's catalog entry for its model, so the models.json the adapter writes for the call is
    the entry bugfix/pi_config.py wrote for it: its context window, prices and the reply-limit floor."""

    def request(self, w, c, alias):
        return adapter.Request(prompt="p", system_path=None, model=c.route(alias), tools="all", resume=None, max_turns=0,
                               workdir=os.path.join(w.tmp, "wt"), run_dir=c.run_dir, timeout=60, station="fix")

    def test_the_models_json_the_adapter_writes_is_what_pi_config_wrote_for_the_route(self):
        w = World(self)
        c = w.chain()
        for alias in ("bugfix-flash", "bugfix-reviewer"):
            with self.subTest(alias):
                inv = harness_pi.Pi().build(self.request(w, c, alias))
                got = json.loads(inv.files["pi/models.json"])
                want = pi_config.models_json(tf.MODELS, [alias], CATALOG, UPSTREAM, 32768)
                self.assertEqual(got, want)
        flash = json.loads(harness_pi.Pi().build(self.request(w, c, "bugfix-flash")).files["pi/models.json"])
        entry = flash["providers"]["flotilla-gateway-bugfix"]["models"][0]
        self.assertEqual((entry["contextWindow"], entry["maxTokens"]), (1000000, 32768), "the catalog's window and the floor over its 4096")

    def test_the_run_keeps_the_one_models_json_every_leaf_needs(self):
        w = World(self)
        c = w.chain()
        with open(c.models_path) as f:
            self.assertEqual(json.load(f)["providers"]["flotilla-gateway-bugfix"]["models"][0]["id"], "fleet-bugfix-flash")
        self.assertEqual([m["id"] for m in c.models["providers"]["flotilla-gateway-bugfix"]["models"]],
                         ["fleet-bugfix-flash", "fleet-bugfix-reviewer"])


class Review(unittest.TestCase):
    def call(self, w, c, tools):
        """c.review with fix.sh replaced by a runner that records the models.json it finds and the argv it was given."""
        seen = []

        def run(cmd, cwd, env=None, timeout=None, **kw):
            with open(c.models_path) as f:
                seen.append((cmd, kw, json.load(f)))
            return 0, '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"VERDICT: approve"}]}}\n'
        c.a.review_tools = tools
        os.makedirs(c.wt, exist_ok=True)
        with unittest.mock.patch.object(fix, "sh", run), unittest.mock.patch.object(c, "review_prompt", return_value="THE REVIEW PROMPT"):
            answer = c.review("diff")
        return answer, seen

    def test_the_review_is_the_pi_adapter_s_call_and_the_runs_models_json_is_put_back(self):
        w = World(self)
        c = w.chain()
        answer, seen = self.call(w, c, "none")
        self.assertEqual(answer, "VERDICT: approve")
        (cmd, kw, during), = seen
        self.assertEqual(cmd[:2], ["pi", "-p"])
        self.assertEqual(kw["stdin"], "THE REVIEW PROMPT")
        self.assertNotIn("THE REVIEW PROMPT", cmd, "the prompt is on stdin, not in argv")
        self.assertEqual([m["id"] for m in during["providers"]["flotilla-gateway-bugfix"]["models"]], ["fleet-bugfix-reviewer"])
        with open(c.models_path) as f:
            self.assertEqual(f.read(), c.models_text, "the verdict and advisor leaves need every alias")

    def test_the_rules_are_given_to_a_review_that_has_tools_and_not_to_one_that_has_none(self):
        w = World(self)
        c = w.chain()
        rules = c.rules["review"]
        self.assertTrue(rules.endswith("rules.md"))
        for tools, given in (("none", False), ("read", True)):
            with self.subTest(tools):
                (cmd, _, _), = self.call(w, c, tools)[1]
                self.assertEqual("--append-system-prompt" in cmd, given)
                self.assertEqual(cmd[cmd.index("--append-system-prompt") + 1] if given else None, rules if given else None)
                self.assertIn("--no-context-files", cmd, "the repository's own rules are never read")
                self.assertEqual(("--no-tools" in cmd, "--tools" in cmd), (tools == "none", tools == "read"))

    def test_a_models_json_is_put_back_when_the_call_raises(self):
        w = World(self)
        c = w.chain()
        os.makedirs(c.wt, exist_ok=True)

        def run(*a, **k):
            raise fix.Stop("killed")
        with unittest.mock.patch.object(fix, "sh", run), unittest.mock.patch.object(c, "review_prompt", return_value="p"):
            with self.assertRaises(fix.Stop):
                c.review("diff")
        with open(c.models_path) as f:
            self.assertEqual(f.read(), c.models_text)


if __name__ == "__main__":
    unittest.main()
