"""python3 bugfix/test_fix2.py: bugfix/fix2.py, the chain of a version that declares stations, beside bugfix/fix.py.

What the stations say and the flags may not; the model route the adapter is given (pi's catalog entry survives: bugfix/pi_config.py's
context windows and reply-limit floor); the pi calls of the three stations as the differential harness (bugfix/differential.py) sees
them against the chain as it was before the switch. test_fix.py and test_pi_golden.py keep pinning fix.py."""
import contextlib
import hashlib
import io
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
import callrec_pi  # noqa: E402
import spend  # noqa: E402
import harness_pi  # noqa: E402
import harness_prompts  # noqa: E402
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

    def __init__(self, tc, no_rules=False, **limits):
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
        # distinct floors per station (the largest, 32768, is the run's) and distinct values for each of the version's limits, so a
        # station or a limit mixed up with another shows
        self.doc = {"schema_version": 1, "version_limits": {"kickback_max": 3, "review_kickback_max": 4, "test_timeout": 555,
                                                            "setup_timeout": 777},
                    "stations": {"fix": {"harness": "pi", "model": "bugfix-flash", "tools": "all", "texts": {"rules": ref("rules.md")},
                                         "limits": {"max_output_tokens": 16384, "timeout": "20m", "max_turns": 90}},
                                 "repro": {"harness": "pi", "model": "bugfix-flash", "tools": "all",
                                           "texts": {"rules": ref("rules.md"), "agent": ref("agent.md")},
                                           "limits": {"max_output_tokens": 32768, "timeout": "20m", "max_turns": 40}},
                                 "review": {"harness": "pi", "model": "bugfix-reviewer", "tools": "none", "texts": {"rules": ref("rules.md")},
                                            "limits": {"max_output_tokens": 8192, "timeout": "10m"}}}}
        for k, v in limits.items():
            self.doc["stations"]["fix"]["limits"][k] = v
        if no_rules:
            for st in self.doc["stations"].values():
                del st["texts"]["rules"]
        self.calls = os.path.join(self.tmp, "calls.jsonl")
        self.environ = {"FLOTILLA_MODELS": json.dumps(tf.MODELS), "FLOTILLA_STATIONS": json.dumps(self.doc),
                        "FLOTILLA_CODE_ROOT": self.code, "FLOTILLA_ROUTINE": "bugfix/fix", "FLOTILLA_CALLS_FILE": self.calls}
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
        self.assertEqual((a.kickback_max, a.review_kickback_max, a.test_timeout, a.setup_timeout), (3, 4, 555, 777),
                         "the version's own limits, each in its own flag")
        self.assertEqual((a.repro_harness, a.review_harness), (fix2.ADAPTER_KEY, fix2.ADAPTER_KEY))

    def test_the_reply_floor_is_the_largest_of_the_stations_the_run_calls(self):
        w = World(self)
        self.assertEqual(w.args().min_output, 32768, "the reproducer's, the largest of 16384, 32768 and 8192")
        self.assertEqual(w.args("--no-repro").min_output, 16384, "no reproducer: the fix's and the review's")

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
                seen.append((cmd, dict(kw, timeout=timeout), json.load(f)))
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
        self.assertEqual((kw["timeout"], kw["max_turns"]), (600, None), "the review station's 10m, and no turn cap")
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

    def test_without_rules_declared_the_repository_s_own_are_read_as_in_v1(self):
        w = World(self, no_rules=True)
        c = w.chain()
        for tools in ("none", "read"):
            with self.subTest(tools):
                (cmd, _, _), = self.call(w, c, tools)[1]
                self.assertNotIn("--no-context-files", cmd)
                self.assertNotIn("--append-system-prompt", cmd)

    def test_a_pi_that_cannot_be_started_raises_in_the_review_as_it_did_in_v1(self):
        w = World(self)
        c = w.chain()
        os.makedirs(c.wt, exist_ok=True)

        def run(*a, **k):
            raise FileNotFoundError(2, "No such file or directory", "pi")
        with unittest.mock.patch.object(fix, "sh", run), unittest.mock.patch.object(c, "review_prompt", return_value="p"):
            with self.assertRaises(FileNotFoundError):
                c.review("diff")

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


class Station(unittest.TestCase):
    """The fix's and the reproducer's calls: what the bash leaves sent, as the adapter sends it."""

    DESC = "THE ISSUE AS FILED:\nvalue() returns 1\n\nTHE TASK:\nmake value() return 2 ($HOME `x` \"q\")"

    def setUp(self):
        self.w = World(self)
        self.c = self.w.chain()
        os.makedirs(self.c.wt)
        self.session = os.path.join(self.c.run_dir, "pi", "sessions", "--wt--", "2026_s1.jsonl")
        os.makedirs(os.path.dirname(self.session))
        with open(self.session, "w") as f:
            f.write('{"type":"session","id":"s1"}\n{"type":"message","mess')

    def call(self, role, desc, env=None, timeout=1200, max_turns=0):
        """The command, keyword arguments and environment fix.sh was given by the step for a leaf, as the chain runs it."""
        seen = []

        def run(cmd, cwd, env=None, timeout=None, **kw):
            seen.append((cmd, cwd, env, timeout, kw))
            return 0, "STREAM"
        leaf = os.path.join(fix.BAKEOFF, "harnesses", "pi.sh" if role == "fix" else "repro.sh")
        with unittest.mock.patch.object(fix, "sh", run), self.c.leaves():
            got = fix.sh([leaf, self.c.wt, desc], fix.BAKEOFF, dict(self.c.env, **(env or {})), timeout, max_turns=max_turns)
        self.assertEqual(got, (0, "STREAM"))
        return seen[-1]

    def test_a_fresh_fix_is_the_prompt_template_on_stdin_with_pi_s_heap(self):
        cmd, cwd, env, timeout, kw = self.call("fix", self.DESC, max_turns=90)
        self.assertEqual((cwd, timeout, kw["max_turns"]), (self.c.wt, 1200, 90))
        self.assertEqual(kw["stdin"], harness_prompts.fix_prompt(self.DESC))
        self.assertNotIn("--session", cmd)
        self.assertEqual(env["NODE_OPTIONS"], "--max-old-space-size=4096")
        self.assertEqual(env["PI_CODING_AGENT_DIR"], os.path.join(self.c.run_dir, "pi"))
        self.assertEqual(cmd[cmd.index("--model") + 1], "fleet-bugfix-flash")

    def test_without_rules_declared_the_repository_s_own_are_read_as_in_v1(self):
        w = World(self, no_rules=True)
        c = w.chain()
        os.makedirs(c.wt)
        seen = []
        leaves = {"fix": "pi.sh", "repro": "repro.sh"}
        with unittest.mock.patch.object(fix, "sh", lambda cmd, *a, **k: seen.append(cmd) or (0, "")), c.leaves():
            for role, leaf in leaves.items():
                fix.sh([os.path.join(fix.BAKEOFF, "harnesses", leaf), c.wt, "d"], fix.BAKEOFF, dict(c.env), 60)
        self.assertEqual(len(seen), 2)
        for cmd in seen:
            self.assertNotIn("--no-context-files", cmd)
            self.assertNotIn("--append-system-prompt", cmd)

    def test_a_resumed_fix_continues_the_session_with_the_message_as_it_is(self):
        cmd, _, env, _, kw = self.call("fix", "YOUR FIX DID NOT PASS ITS GATES", env={"PI_RESUME_SESSION": self.session})
        self.assertEqual(cmd[cmd.index("--session") + 1], os.path.realpath(self.session))
        self.assertEqual(kw["stdin"], "YOUR FIX DID NOT PASS ITS GATES", "no template around a follow-up")
        self.assertEqual(env["NODE_OPTIONS"], "--max-old-space-size=4096")
        with open(self.session) as f:
            self.assertEqual(f.read(), '{"type":"session","id":"s1"}\n', "a line the kill cut short is dropped before pi reads it")

    def test_the_interrupted_run_continues_in_its_session_with_the_chain_s_message(self):
        """What Chain.attempt asks for after a kill (ADR-0019): the session it was killed in, and INTERRUPTED plus the frozen-test note."""
        note = "\n\nIMPORTANT: the failing test src/a.test.js reproduces the bug and is frozen."
        cmd, _, _, _, kw = self.call("fix", fix.INTERRUPTED + note, env={"PI_RESUME_SESSION": self.session})
        self.assertEqual(kw["stdin"], fix.INTERRUPTED + note)
        self.assertEqual(cmd[cmd.index("--session") + 1], os.path.realpath(self.session))

    def test_a_session_that_is_not_in_the_run_directory_is_no_session(self):
        with self.assertRaisesRegex(fix.Stop, "no saved session"):
            self.call("fix", "x", env={"PI_RESUME_SESSION": "/etc/passwd"})

    def test_the_reproducer_is_the_agent_text_then_the_issue(self):
        cmd, _, env, _, kw = self.call("repro", self.DESC, max_turns=40)
        self.assertEqual(kw["stdin"], harness_prompts.repro_prompt(harness_prompts.agent_def_body(
            "---\nname: repro\n---\n---\n# reproducer\nWrite one test.\n"), self.DESC))
        self.assertTrue(kw["stdin"].startswith("# reproducer\nWrite one test.\n\nTHE ISSUE AS FILED:"))
        self.assertEqual((kw["max_turns"], env["NODE_OPTIONS"]), (40, "--max-old-space-size=4096"))

    def test_with_rules_declared_fix_and_repro_never_read_the_repository_s_own(self):
        for role, leaf in (("fix", "pi.sh"), ("repro", "repro.sh")):
            cmd = self.call(role, self.DESC)[0]
            self.assertIn("--no-context-files", cmd, role)
            self.assertEqual(cmd[cmd.index("--append-system-prompt") + 1], self.c.rules[role])

    def test_a_station_has_its_own_timeout_and_rules(self):
        w = World(self)
        w.doc["stations"]["repro"]["limits"]["timeout"] = "7m"
        w.environ["FLOTILLA_STATIONS"] = json.dumps(w.doc)
        w.st = stations.load(w.environ)
        c = w.chain()
        os.makedirs(c.wt)
        seen = []
        with unittest.mock.patch.object(fix, "sh", lambda *a, **k: seen.append((a, k)) or (0, "")), c.leaves():
            fix.sh([os.path.join(fix.BAKEOFF, "harnesses", "repro.sh"), c.wt, "d"], fix.BAKEOFF, dict(c.env), 1200)
        (cmd, *_), kw = seen[-1]
        self.assertEqual(seen[-1][0][3], 420, "the repro station's 7m, not the fix station's 20m the step was given")
        self.assertEqual(cmd[cmd.index("--append-system-prompt") + 1], c.rules["repro"])

    def test_each_station_calls_its_own_model(self):
        w = World(self)
        w.doc["stations"]["repro"]["model"] = "bugfix-reviewer"
        w.environ["FLOTILLA_STATIONS"] = json.dumps(w.doc)
        w.st = stations.load(w.environ)
        c = w.chain()
        os.makedirs(c.wt)
        seen = []
        with unittest.mock.patch.object(fix, "sh", lambda cmd, *a, **k: seen.append(cmd) or (0, "")), c.leaves():
            for leaf in ("pi.sh", "repro.sh"):
                fix.sh([os.path.join(fix.BAKEOFF, "harnesses", leaf), c.wt, "d"], fix.BAKEOFF, dict(c.env), 60)
        self.assertEqual([cmd[cmd.index("--model") + 1] for cmd in seen], ["fleet-bugfix-flash", "fleet-bugfix-reviewer"])

    def test_a_pi_that_cannot_be_started_is_an_exit_code_as_the_bash_leaf_gave(self):
        """v1: `bash pi.sh` ran no pi and gave 127; the chain handles an exit code and not the OSError run_process raises."""
        for error, code in ((FileNotFoundError(2, "No such file or directory", "pi"), 127), (PermissionError(13, "Permission denied"), 126),
                            (UnicodeEncodeError("utf-8", "\ud800", 0, 1, "surrogates not allowed"), 1)):
            for role, leaf in (("fix", "pi.sh"), ("repro", "repro.sh")):
                def run(*a, **k):
                    raise error
                with self.subTest(type(error).__name__, role=role), unittest.mock.patch.object(fix, "sh", run), self.c.leaves():
                    got = fix.sh([os.path.join(fix.BAKEOFF, "harnesses", leaf), self.c.wt, self.DESC], fix.BAKEOFF, dict(self.c.env), 60)
                self.assertEqual(got[0], code)
                self.assertTrue(got[1].startswith("pi: "), got)
                with open(self.c.models_path) as f:
                    self.assertEqual(f.read(), self.c.models_text)

    def test_an_error_that_is_not_about_starting_pi_is_not_an_exit_code(self):
        def run(*a, **k):
            raise ValueError("max_turns needs turn_ended")
        with unittest.mock.patch.object(fix, "sh", run), self.c.leaves():
            with self.assertRaises(ValueError):
                fix.sh([os.path.join(fix.BAKEOFF, "harnesses", "pi.sh"), self.c.wt, self.DESC], fix.BAKEOFF, dict(self.c.env), 60)

    def test_what_is_not_a_leaf_is_run_as_it_was(self):
        seen = []
        with unittest.mock.patch.object(fix, "sh", lambda *a, **k: seen.append((a, k)) or (0, "out")), self.c.leaves():
            self.assertEqual(fix.sh(["git", "-C", self.c.wt, "status"], self.c.run_dir, None, 60, check=True), (0, "out"))
        self.assertEqual(seen[0][0][0], ["git", "-C", self.c.wt, "status"])
        self.assertTrue(seen[0][1]["check"])
        self.assertIsNot(fix.sh, None)
        self.assertEqual(fix.sh.__name__, "sh", "the swap ends with the step")


SESSION = os.path.join(HERE, "testdata", "pi", "session.jsonl")


def golden_session():
    """pi's saved session of testdata/pi/session.jsonl (two assistant messages, one bill of 0.5) and the stream pi printed for it."""
    with open(SESSION) as f:
        lines = [json.loads(x) for x in f][:-1]   # without its last, failed, message
    stream = "".join(json.dumps(x if x["type"] == "session" else {"type": "message_end", "message": x["message"]}) + "\n" for x in lines)
    return "".join(json.dumps(x) + "\n" for x in lines), stream


class Episodes(unittest.TestCase):
    """pi's episodes as version 2 files, and pi's calls and spend counted once: from its session files."""

    def setUp(self):
        self.w = World(self)
        self.c = self.w.chain()
        os.makedirs(self.c.wt)
        self.session, self.stream = golden_session()
        self.n = 0

    def run_station(self, station="fix", alias="bugfix-flash", resume=None):
        def run(cmd, cwd, env=None, timeout=None, **kw):
            self.n += 1
            d = os.path.join(self.c.run_dir, "pi", "sessions", "--wt--")
            os.makedirs(d, exist_ok=True)
            with open(os.path.join(d, f"s{self.n}.jsonl"), "w") as f:
                f.write(self.session)
            return 0, self.stream
        req = adapter.Request(station=station, prompt="p", system_path=None, model=self.c.route(alias), tools="all", resume=resume,
                              max_turns=0, workdir=self.c.wt, run_dir=self.c.run_dir, timeout=60)
        with unittest.mock.patch.object(fix, "sh", run):
            return self.c.execute(req)

    def episodes(self):
        d = os.path.join(self.c.run_dir, "episodes")
        return sorted(x for x in os.listdir(d)) if os.path.isdir(d) else []

    def test_a_station_leaves_a_version_2_episode_file_of_the_harness_pi(self):
        self.run_station("fix")
        self.run_station("review", "bugfix-reviewer")
        self.assertEqual(self.episodes(), ["001-fix.json", "001-fix.json.recorded", "002-review.json", "002-review.json.recorded"])
        doc = adapter.read_episode(os.path.join(self.c.run_dir, "episodes", "001-fix.json"))
        self.assertEqual((doc["version"], doc["harness"], doc["station"], doc["alias"], doc["seq"], doc["code"]),
                         (2, "pi", "fix", "bugfix-flash", 1, 0))
        self.assertEqual((doc["session_id"], doc["resume_ref"], doc["calls_role"], len(doc["turns"])), ("sess-golden", "sess-golden", "coder", 2))
        self.assertNotIn("role", doc, "version 1 says `role`; the platform's own document says `station`")
        self.assertEqual(adapter.read_episode(os.path.join(self.c.run_dir, "episodes", "002-review.json"))["alias"], "bugfix-reviewer")

    def test_the_spend_of_a_pi_run_is_its_sessions_and_not_also_its_episode_files(self):
        self.run_station("fix")
        run_dir, agent = self.c.run_dir, os.path.join(self.c.run_dir, "pi")
        with open(os.path.join(HERE, "testdata", "golden_pi_parse.json")) as f:
            v1 = json.load(f)["streams"]["session.jsonl"]["pi_cost"]
        with open(os.path.join(run_dir, "episodes", "001-fix.json")) as f:
            self.assertEqual(json.load(f)["cost_est"], 0.5, "the episode file holds the figure the sessions hold")
        self.assertEqual((spend.pi_cost(agent), spend.episode_cost(run_dir)), (v1, 0.0))
        self.assertEqual(spend.pi_cost(agent) + spend.episode_cost(run_dir), v1, "run_case's sum is the v1 figure")
        self.assertEqual(spend.pi_cost(agent, 0.5) + spend.episode_cost(run_dir, 0.0), 0.0, "a resumed run's baseline is the sessions'")

    def test_the_calls_of_a_pi_run_are_its_sessions_and_not_also_its_episode_files(self):
        self.run_station("fix")
        self.c.calls.record("coder", "bugfix-flash")
        self.c.calls.record_pending_episodes()
        with open(self.w.calls) as f:
            lines = [json.loads(x) for x in f]
        self.assertEqual([(c["role"], c["step"]) for c in lines if c.get("kind") != "step"], [("coder", 1), ("coder", 2)])

    def test_version_2_files_of_other_harnesses_are_read_by_the_spend_and_the_calls(self):
        """spend.episode_cost and callrec_pi accept version 2: a harness other than pi counts, and is recorded when no marker says it was."""
        ep = adapter.Episode(harness="fakeh", station="fix", turns=[adapter.Turn(n=1, model="m", usage=adapter.usage_dict(10, 2))],
                             cost_est=0.2)
        doc = dict(adapter.episode_doc(ep, "bugfix-flash", 1, 0, ""), calls_role="coder")
        adapter.write_episode(self.c.run_dir, "001-fix", doc)
        self.assertEqual(doc["version"], 2)
        self.assertEqual(spend.episode_cost(self.c.run_dir), 0.2)
        self.c.calls.record_pending_episodes()
        with open(self.w.calls) as f:
            self.assertEqual([(c["role"], c["harness"]) for c in map(json.loads, f)], [("coder", "fakeh")])

    def test_a_resumed_run_does_not_number_pi_s_episode_files_among_its_sessions(self):
        self.run_station("fix")
        ep = adapter.Episode(harness="fakeh", station="fix")
        adapter.write_episode(self.c.run_dir, "002-fix", dict(adapter.episode_doc(ep, "a", 2, 0, ""), calls_role="coder"))
        self.c.calls.skip_existing()
        names = sorted(os.path.basename(f) for f in self.c.calls.seq)
        self.assertEqual(names, ["001-fix.json", "002-fix.json", "s1.jsonl"])
        self.c.forget_pi_episodes()
        self.assertEqual(sorted(os.path.basename(f) for f in self.c.calls.seq), ["002-fix.json", "s1.jsonl"])
        self.assertEqual(adapter.last_episode(self.c.run_dir), 2, "the episode counter still goes on from the files")


class Resumption(unittest.TestCase):
    """ADR-0019 across the two chains: a checkpoint of one is not continued by the other (a session of one harness is not continued by
    another: the checkpoint names the harness of each role, fix.load_checkpoint compares them), and each continues its own."""

    CASE = "case-20261115-abc"

    def setUp(self):
        self.w = World(self)
        self.f = tf.Fixture([], [])
        self.addCleanup(self.f.close)
        self.home = os.path.join(self.f.d, "home")
        os.makedirs(self.home)

    def env(self, resume=None):
        f = self.f
        env = dict(os.environ, FLOTILLA_PLAN_FILE=os.path.join(f.d, "plan.json"), FAKE_STATE=f.state, GIT_ALLOW_PROTOCOL="file",
                   GIT_REMOTE_BASE=f.remotes, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_CASE=json.dumps({"id": self.CASE, "kind": "code_bug", "target": f"repo:{f.repo}/issues/12",
                                             "data": {"issue": {"repo": f.repo, "number": 12, "title": "value is 1", "body": "b", "url": "u"}}}),
                   HOME=self.home, **self.w.environ)
        if resume:
            env["FLOTILLA_RESUME"] = json.dumps(resume)
        return env

    def run_dir(self):
        return os.path.join(self.f.scratch, "runs", self.CASE)

    def start(self, chain_module, resume=None):
        """One run of the chain, whose loop is replaced by: check the base out and checkpoint it (as the real loop does), or note what
        it was restored from; a fresh run is then killed (Terminated), so its directory and checkpoint are left. Returns (what the chain
        was restored from, its output)."""
        got, out = [], io.StringIO()

        def rounds(chain):
            got.append(chain.restored)
            self.chain = chain
            if not chain.restored:
                chain.checkout()
                chain.save("checkout", None)
                raise fix.Terminated()
            return {"outcome": "escalate", "detail": "resumed"}
        common = ["--repo", self.f.repo, "--setup", "true", "--scratch", self.f.scratch, "--no-repro"]
        models = ["--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer"]
        with unittest.mock.patch.object(fix.Chain, "rounds", rounds), contextlib.redirect_stdout(out):
            try:
                if chain_module == "fix":
                    fix.main(common + models, environ=self.env(resume))
                else:
                    fix2.main(common, environ=self.env(resume))
            except fix.Terminated:
                pass
        return got, out.getvalue()

    def checkpoint(self):
        with open(os.path.join(self.run_dir(), fix.CHECKPOINT)) as f:
            return json.load(f)

    def test_the_checkpoints_name_the_harness_of_each_role(self):
        self.start("fix")
        self.assertEqual({k: self.checkpoint()[k] for k in fix.HARNESS_KEYS}, {"harness": "pi", "repro_harness": "pi", "review_harness": "pi"})
        self.start("fix2")
        self.assertEqual({k: self.checkpoint()[k] for k in fix.HARNESS_KEYS},
                         {"harness": "pi", "repro_harness": fix2.ADAPTER_KEY, "review_harness": fix2.ADAPTER_KEY})

    def test_a_v1_checkpoint_starts_fresh_under_fix2(self):
        self.start("fix")
        got, out = self.start("fix2", resume={"run_id": "r1", "spend_usd": 0})
        self.assertEqual(got, [None], "a fresh start")
        self.assertIn(f"no usable checkpoint (ValueError: it was made with repro_harness 'pi', this run's flags say '{fix2.ADAPTER_KEY}')", out)

    def test_a_fix2_checkpoint_starts_fresh_under_v1(self):
        self.start("fix2")
        got, out = self.start("fix", resume={"run_id": "r1", "spend_usd": 0})
        self.assertEqual(got, [None])
        self.assertIn(f"it was made with repro_harness '{fix2.ADAPTER_KEY}', this run's flags say 'pi'", out)

    def test_a_resumed_fix2_run_numbers_its_sessions_without_pi_s_episode_files(self):
        """Chain.adopt end to end: the earlier run left a pi session and, as fix2 keeps for each pi call, a version 2 episode file of
        it. The sessions are the record of pi's calls, so the episode file is not numbered among them (callrec_pi.skip_existing
        numbers by first appearance), whereas the file of a harness other than pi is."""
        self.start("fix2")
        run_dir = self.run_dir()
        session, _ = golden_session()
        os.makedirs(os.path.join(run_dir, "pi", "sessions", "--wt--"))
        with open(os.path.join(run_dir, "pi", "sessions", "--wt--", "s1.jsonl"), "w") as f:
            f.write(session)
        for n, harness in ((1, "pi"), (2, "fakeh")):
            ep = adapter.Episode(harness=harness, station="fix")
            adapter.write_episode(run_dir, f"00{n}-fix", dict(adapter.episode_doc(ep, "bugfix-flash", n, 0, ""), calls_role="coder"))
            open(os.path.join(run_dir, "episodes", f"00{n}-fix.json" + callrec_pi.RECORDED), "w").close()
        got, out = self.start("fix2", resume={"run_id": "r1", "spend_usd": 0})
        self.assertEqual([c["phase"] for c in got], ["checkout"], out)
        self.assertEqual(sorted(os.path.basename(f) for f in self.chain.calls.seq), ["002-fix.json", "s1.jsonl"])
        self.assertEqual(self.chain.episode_n, 2, "the episode counter goes on from the files")

    def test_each_chain_continues_its_own_checkpoint(self):
        for module in ("fix", "fix2"):
            with self.subTest(module):
                fresh = self.start(module)[0]
                self.assertEqual(fresh, [None])
                got, out = self.start(module, resume={"run_id": "r1", "spend_usd": 0})
                self.assertEqual([c["phase"] for c in got], ["checkout"], out)


class Example(unittest.TestCase):
    """fleets/bugfix/fix/v2, the version fix2.py is written for (flotillad -check and -stations read it: cmd/flotillad/bugfixv2_test.go)."""

    V2 = os.path.join(os.path.dirname(HERE), "fleets", "bugfix", "fix", "v2")

    def read(self, *path):
        with open(os.path.join(*path)) as f:
            return f.read()

    def test_its_texts_are_the_ones_the_v1_chain_reads(self):
        self.assertEqual(self.read(self.V2, "rules.md"), self.read(HERE, "fleet_rules.md"))
        self.assertEqual(self.read(self.V2, "rules.md"), self.read(os.path.dirname(HERE), "fleets", "bugfix", "fix", "v1", "rules.md"))
        self.assertEqual(self.read(self.V2, "repro.md"), self.read(os.path.dirname(HERE), "bugfix-fleet", "agents", "repro.md"))

    def test_its_stations_and_command_are_what_fix2_serves(self):
        text = self.read(self.V2, "spec.yaml")
        import re
        names = re.findall(r"^  (\w+):\n    harness: (\w+)\n", text, re.M)
        self.assertEqual(names, [(fix2.FIX, "pi"), (fix2.REPRO, "pi"), (fix2.REVIEW, "pi")])
        command = re.search(r"command: >-\n\s+(.*)\n", text).group(1)
        self.assertIn("bugfix/fix2.py", command)
        self.assertEqual([f for f in fix2.REFUSED_FLAGS if f in command], [], "no model, text or limit in the command's flags")
        self.assertIn("prompts: {rules: rules.md, agent: repro.md}", text, "the reproducer's agent text is the station's `agent`")
        self.assertIn("limits: {kickback_max: 2, review_kickback_max: 1}", text)


if __name__ == "__main__":
    unittest.main()
