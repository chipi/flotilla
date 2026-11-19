"""python3 runtime/test_adapter.py: the harness layer (runtime/adapter.py and the adapters, runtime/harness_<name>.py) as a
platform part: it stands on runtime/ alone, whichever fleet's image carries it (v2-design.md section 3), serves a `tools`
value and never a station name, and its capability table is the generated fleets/harnesses.json. The bugfix chain's use of
it is tested in bugfix/test_harness.py and bugfix/test_episodes.py.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
import unittest.mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import adapter  # noqa: E402
import harness_pi  # noqa: E402
from fakeh import check_contract, install_fake, request  # noqa: E402


def run_isolated(code):
    """`code` in a fresh interpreter whose only import path is runtime/ (and the standard library), from an empty cwd:
    what a worker image that carries runtime/ and no fleet code has."""
    with tempfile.TemporaryDirectory() as cwd:
        env = {"PATH": os.environ.get("PATH", ""), "PYTHONPATH": HERE, "HOME": cwd, "PYTHONDONTWRITEBYTECODE": "1"}
        p = subprocess.run([sys.executable, "-c", code], cwd=cwd, env=env, capture_output=True, text=True, timeout=60)
    return p.returncode, p.stdout, p.stderr


class Standalone(unittest.TestCase):
    def test_the_layer_loads_with_runtime_alone_and_pulls_in_no_fleet_code(self):
        code = ("import sys, json, adapter\n"
                "pi = adapter.get('pi')\n"
                "print(json.dumps({'name': pi.name, 'bugfix': sorted(m for m in sys.modules if m in "
                "('fix', 'callrec_pi', 'harness_prompts', 'declared_prompts', 'pi_config', 'spend', 'stepone', 'pi_review'))}))\n")
        rc, out, err = run_isolated(code)
        self.assertEqual(rc, 0, err)
        self.assertEqual(json.loads(out), {"name": "pi", "bugfix": []})

    def test_the_adapters_are_the_runtime_files_named_harness_name(self):
        names = adapter.names()
        self.assertIn("pi", names)
        for n in names:
            self.assertTrue(os.path.exists(os.path.join(HERE, f"harness_{n}.py")), n)

    def test_the_shared_module_is_not_the_in_process_loop(self):
        """runtime/harness.py (triage's loop, `import harness` in signal-fleet and test_hooks) keeps its name; the
        adapter layer is `adapter`, so neither shadows the other."""
        import harness
        self.assertTrue(hasattr(harness, "run_json_step"))
        self.assertFalse(hasattr(adapter, "run_json_step"))
        self.assertTrue(hasattr(adapter, "execute"))
        self.assertFalse(hasattr(harness, "execute"))


class Images(unittest.TestCase):
    def test_the_bugfix_worker_image_carries_runtime(self):
        """bugfix/fix.py imports the layer from ../runtime: /opt/flotilla/runtime beside /opt/flotilla/bugfix in the image."""
        with open(os.path.join(HERE, "..", "deploy", "docker", "worker-bugfix.Dockerfile")) as f:
            lines = [ln.strip() for ln in f]
        self.assertIn("COPY runtime /opt/flotilla/runtime", lines)
        self.assertIn("COPY bugfix /opt/flotilla/bugfix", lines)


class Types(unittest.TestCase):
    def test_an_episode_survives_its_json(self):
        ep = adapter.Episode(harness="x", station="fix", session_id="s", answer="done", cost_est=0.5, notes=["n"],
                             turns=[adapter.Turn(n=1, model="m", text="t", tools=[{"name": "read", "arg": "a", "ok": True}],
                                                 usage=adapter.usage_dict(1, 2), cost_est=0.5, stop="stop")],
                             usage_total=adapter.usage_dict(1, 2))
        self.assertEqual(adapter.Episode.from_dict(json.loads(json.dumps(ep.to_dict()))), ep)

    def test_the_request_has_no_role_and_the_station_is_a_free_label(self):
        names = set(adapter.Request.__dataclass_fields__)
        self.assertNotIn("role", names)
        self.assertNotIn("rules_path", names)
        self.assertNotIn("readonly", names)
        self.assertFalse(hasattr(adapter, "ROLES"))
        self.assertFalse(hasattr(adapter.Harness, "roles"))
        for label in ("fix", "decide", "x_1", "anything at all"):
            self.assertEqual(request(label).station, label)

    def test_a_version_1_episode_reads_back_with_its_role_as_the_station(self):
        v1 = {"harness": "pi", "role": "review", "session_id": "s", "turns": [], "answer": "a", "error": "", "exit_reason": "completed",
              "usage_total": None, "cost_est": None, "raw_path": "", "notes": []}
        self.assertEqual(adapter.Episode.from_dict(v1).station, "review")
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        v2 = {k: v for k, v in dict(v1, station="x", version=2).items() if k != "role"}
        for version, doc in ((1, dict(v1, version=1)), (2, v2)):
            path = os.path.join(d, f"v{version}.json")
            with open(path, "w") as f:
                json.dump(doc, f)
            self.assertEqual(adapter.read_episode(path)["station"], "review" if version == 1 else "x")
        with open(os.path.join(d, "v3.json"), "w") as f:
            json.dump(dict(v1, version=3), f)
        with self.assertRaises(ValueError):
            adapter.read_episode(os.path.join(d, "v3.json"))

    def test_the_platform_s_episode_document_is_version_2_and_says_station(self):
        ep = adapter.Episode(harness="x", station="decide")
        doc = adapter.episode_doc(ep, "alias", 3, 0, "ref")
        self.assertEqual((doc["version"], doc["station"], "role" in doc), (adapter.EPISODE_VERSION, "decide", False))
        self.assertEqual(adapter.EPISODE_VERSION, 2)

    def test_usage_is_summed_over_the_turns_that_report_it(self):
        t = [adapter.Turn(n=1, usage=adapter.usage_dict(1, 2, 3, 4, 5)), adapter.Turn(n=2), adapter.Turn(n=3, usage=adapter.usage_dict(10, 20))]
        self.assertEqual(adapter.sum_usage(t), {"input": 11, "output": 22, "cache_read": 3, "cache_write": 4, "reasoning": 5})
        self.assertIsNone(adapter.sum_usage([adapter.Turn(n=1)]))
        self.assertIsNone(adapter.sum_cost([adapter.Turn(n=1)]))
        self.assertEqual(adapter.sum_cost([adapter.Turn(n=1, cost_est=0.25), adapter.Turn(n=2, cost_est=0.5)]), 0.75)

    def test_a_route_is_read_from_the_models_section(self):
        cfg = {"endpoints": {"gw": {"base_url": "http://h/v1", "key_file": "/k"}},
               "aliases": {"a": {"endpoint": "gw", "model": "m1"}}}
        r = adapter.route(cfg, "a", {"x": 1})
        self.assertEqual((r.alias, r.model, r.base_url, r.endpoint, r.key_file, r.key_env, r.meta),
                         ("a", "m1", "http://h/v1", "gw", "/k", "", {"x": 1}))
        with self.assertRaises(adapter.HarnessError):
            adapter.route(cfg, "nope")
        with self.assertRaises(adapter.HarnessError):
            adapter.route({"aliases": {"a": {"endpoint": "gone", "model": "m"}}}, "a")

    def test_the_key_comes_from_the_environment_or_the_file_never_elsewhere(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        kf = os.path.join(d, "key")
        with open(kf, "w") as f:
            f.write("sk-file\n")
        self.assertEqual(adapter.read_key(adapter.ModelRoute("a", "m", "u", key_file=kf), {}), "sk-file")
        self.assertEqual(adapter.read_key(adapter.ModelRoute("a", "m", "u", key_file=kf, key_env="K"), {"K": "sk-env"}), "sk-env")
        self.assertEqual(adapter.read_key(adapter.ModelRoute("a", "m", "u", key_file=kf, key_env="K"), {}), "sk-file")
        self.assertEqual(adapter.read_key(adapter.ModelRoute("a", "m", "u", key_file=os.path.join(d, "none")), {}), "")
        self.assertEqual(adapter.read_key(adapter.ModelRoute("a", "m", "u"), {}), "")


class Registry(unittest.TestCase):
    def test_pi_is_registered_and_an_unknown_name_is_refused(self):
        self.assertIs(adapter.get("pi"), harness_pi.PI)
        self.assertIn("pi", adapter.names())
        for bad in ("nope", "", "Pi", "../pi", "prompts", "cli"):
            with self.assertRaises(adapter.HarnessError, msg=bad):
                adapter.get(bad)

    def test_a_name_and_the_tools_values_are_checked_at_registration(self):
        class Bad(adapter.Harness):
            name, tools_served = "Bad Name", frozenset({"all"})
        with self.assertRaises(adapter.HarnessError):
            adapter.register(Bad())

        class Odd(adapter.Harness):
            name, tools_served = "odd", frozenset({"all", "dream"})
        with self.assertRaises(adapter.HarnessError):
            adapter.register(Odd())

    def test_the_fake_is_there_only_while_a_test_installs_it(self):
        self.assertNotIn("fakeh", adapter.names())
        install_fake(self)
        self.assertIs(adapter.get("fakeh").name, "fakeh")


class Capabilities(unittest.TestCase):
    DOC = os.path.join(HERE, "HARNESS_SEAMS.md")
    JSON = os.path.join(HERE, "..", "fleets", "harnesses.json")

    def table(self):
        with open(self.DOC) as f:
            text = f.read()
        body = text.split("<!-- capabilities:begin -->")[1].split("<!-- capabilities:end -->")[0]
        return [ln for ln in body.strip().splitlines() if ln.startswith("|")]

    def test_pi_declares_each_capability_and_a_new_one_defaults_to_no(self):
        got = adapter.capabilities(harness_pi.PI)
        self.assertEqual(sorted(got), sorted(adapter.CAPABILITIES))
        self.assertEqual(got, {"can_resume": True, "resume_after_kill": True, "enforces_readonly": True, "supports_hooks": False,
                               "reports_cost": True, "reports_usage": True, "supports_turn_cap": True, "native_system_prompt": True,
                               "prompt_via_stdin": True})
        self.assertEqual(harness_pi.PI.tools_served, frozenset({"all", "read", "none"}))
        self.assertFalse(any(adapter.capabilities(adapter.Harness()).values()), "nothing is claimed by default")
        self.assertEqual(adapter.Harness().tools_served, frozenset(), "and no tools value is served by default")

    def test_a_step_that_asks_for_what_the_harness_lacks_is_refused_with_the_reason(self):
        pi = harness_pi.PI
        self.assertEqual(adapter.missing(pi, {"can_resume": True, "enforces_readonly": True, "tools": ["all", "read"]}), [])
        self.assertEqual(adapter.missing(pi, {"supports_hooks": True}),
                         [f"harness pi does not declare supports_hooks: {adapter.CAPABILITIES['supports_hooks']}"])
        self.assertEqual(adapter.missing(pi, {"supports_hooks": False}), [], "not asking is not a need")
        self.assertEqual(len(adapter.missing(pi, {"prompt_via_stdin": True, "supports_hooks": True})), 1)
        self.assertIn("is not a capability", adapter.missing(pi, {"telepathy": True})[0])

        class Mini(adapter.Harness):
            name, tools_served = "mini", frozenset({"all"})
        self.assertEqual(adapter.missing(Mini(), {"tools": ["all", "read", "none"]}),
                         ["harness mini does not serve tools=read", "harness mini does not serve tools=none"])

    def test_the_seams_doc_table_is_the_adapters(self):
        rows = self.table()
        self.assertEqual([c.strip() for c in rows[0].strip("|").split("|")], ["harness", "tools_served"] + list(adapter.CAPABILITIES))
        self.assertEqual(set(rows[1].strip("|").split("|")), {" --- "}, "a markdown separator row")
        documented = {r.strip("|").split("|")[0].strip(): r for r in rows[2:]}
        for name in adapter.names():
            self.assertEqual(documented[name], adapter.capability_row(adapter.get(name)), name)
        self.assertEqual(set(documented) - set(adapter.names()), set(), "a row for a harness that is not there")

    def test_the_seams_doc_names_every_seam_and_every_capability(self):
        with open(self.DOC) as f:
            text = f.read()
        for n in range(1, 12):
            self.assertRegex(text, rf"(?m)^### S{n} ")
        for k in adapter.CAPABILITIES:
            self.assertIn(k, text)

    def test_fleets_harnesses_json_is_what_the_adapters_declare(self):
        """flotillad checks a version's stations against this file: it is generated, never edited. Regenerate with
        `python3 runtime/adapter.py capabilities --write`."""
        with open(self.JSON) as f:
            on_disk = f.read()
        self.assertEqual(on_disk, adapter.capabilities_json(), "fleets/harnesses.json is stale: python3 runtime/adapter.py capabilities --write")
        doc = json.loads(on_disk)
        self.assertEqual(doc["version"], 1)
        self.assertEqual(set(doc["harnesses"]), set(adapter.names()) | {"flotilla"})
        pi = doc["harnesses"]["pi"]
        self.assertEqual(pi["tools_served"], ["all", "read", "none"])
        self.assertEqual({k: pi[k] for k in adapter.CAPABILITIES}, adapter.capabilities(harness_pi.PI))

    def test_the_generator_runs_as_a_script_and_prints_the_same_document(self):
        p = subprocess.run([sys.executable, os.path.join(HERE, "adapter.py"), "capabilities"], capture_output=True, text=True, timeout=60)
        self.assertEqual((p.returncode, p.stdout), (0, adapter.capabilities_json()), p.stderr)
        p = subprocess.run([sys.executable, os.path.join(HERE, "adapter.py"), "nonsense"], capture_output=True, text=True, timeout=60)
        self.assertEqual(p.returncode, 2)

    def test_the_in_process_harness_serves_all_and_none_and_has_hooks(self):
        flo = adapter.capabilities_doc()["harnesses"]["flotilla"]
        self.assertEqual(flo["tools_served"], ["all", "none"], "read has no meaning for tools the runtime serves")
        self.assertTrue(flo["supports_hooks"])
        self.assertEqual(set(flo) - {"tools_served"}, set(adapter.CAPABILITIES), "every capability is declared, none left to a default")


class Runner(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)

    def test_a_timeout_kills_the_whole_group_and_keeps_what_came_out(self):
        marker = os.path.join(self.d, "alive")
        code, out = adapter.run_process(f"echo first; (sleep 3; touch {marker}) & wait", self.d, timeout=1)
        self.assertEqual((code, out), (adapter.TIMED_OUT, "first\n"))
        time.sleep(3.5)
        self.assertFalse(os.path.exists(marker), "the grandchild outlived the timeout")

    def test_check_raises_on_a_timeout_and_on_a_failure(self):
        with self.assertRaisesRegex(adapter.ProcessFailed, "no result in 1s"):
            adapter.run_process("sleep 5", self.d, timeout=1, check=True)
        with self.assertRaisesRegex(adapter.ProcessFailed, "exit 3: boom"):
            adapter.run_process("echo boom; exit 3", self.d, timeout=5, check=True)

    def test_the_prompt_goes_to_stdin_and_stdin_is_closed(self):
        cmd = [sys.executable, "-c", "import sys; print(sys.stdin.read().upper())"]
        for kw in ({}, {"tee_path": os.path.join(self.d, "s")}):
            self.assertEqual(adapter.run_process(cmd, self.d, timeout=10, stdin="hello " * 40000, **kw)[1].strip(), ("HELLO " * 40000).strip())

    def test_the_turn_cap_kills_the_group_and_the_tee_holds_the_stream(self):
        tee = os.path.join(self.d, "episodes", "s.stream")
        script = "for i in 1 2 3 4 5; do echo \"turn $i\"; done; sleep 30; echo late"
        t0 = time.time()
        code, out = adapter.run_process(["sh", "-c", script], self.d, timeout=60, max_turns=3,
                                        turn_ended=lambda line: line.startswith("turn"), tee_path=tee)
        self.assertEqual(code, adapter.TURN_CAPPED)
        self.assertLess(time.time() - t0, 15)
        self.assertNotIn("late", out)
        with open(tee) as f:
            self.assertEqual(f.read(), out, "what the run printed is what the file holds")
        self.assertGreaterEqual(out.count("turn"), 3)

    def test_a_killed_run_leaves_its_stream(self):
        tee = os.path.join(self.d, "s")
        code, out = adapter.run_process(["sh", "-c", "echo a; sleep 30"], self.d, timeout=1, tee_path=tee)
        self.assertEqual((code, out), (adapter.TIMED_OUT, "a\n"))
        with open(tee) as f:
            self.assertEqual(f.read(), "a\n")

    def test_a_cap_needs_the_harness_s_own_turn_test(self):
        with self.assertRaises(ValueError):
            adapter.run_process(["true"], self.d, max_turns=3)

    def test_a_turn_test_that_raises_does_not_stop_the_reading(self):
        def bad(line):
            raise RuntimeError("x")
        code, out = adapter.run_process(["sh", "-c", "seq 1 5000"], self.d, timeout=30, max_turns=3, turn_ended=bad)
        self.assertEqual((code, out.count("\n")), (0, 5000))


class PiServesEveryToolsValue(unittest.TestCase):
    """The generic pi adapter: any station, any `tools` value, on the platform's own configuration."""

    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)

    def req(self, **kw):
        kw.setdefault("run_dir", self.d)
        kw.setdefault("workdir", os.path.join(self.d, "wt"))
        kw.setdefault("prompt", "THE PROMPT")
        return request(**kw)

    def test_each_tools_value_gets_its_own_switches_and_the_prompt_is_on_stdin(self):
        base = ["pi", "-p", "--mode", "json", "--provider", "flotilla-gateway-bugfix", "--model", "fleet-bugfix-flash"]
        for tools, want in (("all", []), ("read", ["--tools", "read,grep,find,ls"]), ("none", ["--no-tools"])):
            inv = harness_pi.PI.build(self.req(tools=tools))
            with self.subTest(tools):
                self.assertEqual(inv.argv, base + want + ["--no-context-files"])
                self.assertEqual(inv.stdin, "THE PROMPT", "the prompt is on stdin")
                self.assertNotIn("THE PROMPT", " ".join(inv.argv), "and not in argv")
                self.assertEqual(inv.cwd, os.path.join(self.d, "wt"))

    def test_the_system_text_is_appended_and_the_workspace_s_own_instructions_are_never_read(self):
        sys_text = os.path.join(self.d, "system.md")
        for tools in ("all", "read", "none"):
            inv = harness_pi.PI.build(self.req(tools=tools, system_path=sys_text))
            self.assertEqual(inv.argv[inv.argv.index("--append-system-prompt"):][:2], ["--append-system-prompt", sys_text], tools)
            self.assertIn("--no-context-files", inv.argv, tools)
            self.assertNotIn("--append-system-prompt", harness_pi.PI.build(self.req(tools=tools)).argv, "none given, none appended")
            self.assertIn("--no-context-files", harness_pi.PI.build(self.req(tools=tools)).argv)

    def test_the_session_store_and_the_model_configuration_are_under_the_run_directory(self):
        inv = harness_pi.PI.build(self.req())
        self.assertEqual(inv.env, {"PI_CODING_AGENT_DIR": os.path.join(self.d, "pi")})
        self.assertEqual(list(inv.files), ["pi/models.json"])
        cfg = json.loads(inv.files["pi/models.json"])
        p = cfg["providers"]["flotilla-gateway-bugfix"]
        self.assertEqual((p["baseUrl"], p["api"], p["models"]), ("http://127.0.0.1:9/v1", "openai-completions", [{"id": "fleet-bugfix-flash"}]))

    def test_the_key_is_named_in_the_configuration_and_written_nowhere(self):
        for kw, want in (({"key_file": "/run/secrets/k"}, "!cat /run/secrets/k"), ({"key_env": "GW_KEY"}, "$GW_KEY"), ({}, "none")):
            route = adapter.ModelRoute(alias="a", model="m", base_url="http://gw/v1", endpoint="gw", **kw)
            inv = harness_pi.PI.build(self.req(model=route))
            blob = json.dumps(inv.argv) + json.dumps(inv.env) + inv.stdin + json.dumps(inv.files)
            self.assertEqual(json.loads(inv.files["pi/models.json"])["providers"]["flotilla-gw"]["apiKey"], want)
            self.assertNotIn("sk-", blob)

    def test_the_value_of_a_key_never_reaches_argv_env_stdin_or_a_file(self):
        kf = os.path.join(self.d, "key")
        with open(kf, "w") as f:
            f.write("sk-" + "do-not-leak-0001\n")
        route = adapter.ModelRoute(alias="a", model="m", base_url="http://gw/v1", endpoint="gw", key_file=kf)
        inv = harness_pi.PI.build(self.req(model=route))
        self.assertNotIn("do-not-leak", json.dumps([inv.argv, inv.env, inv.stdin, inv.files, inv.notes]))

    def test_phone_home_is_off_without_the_image_setting_it(self):
        self.assertEqual(harness_pi.PI.phone_home_env(), {"PI_OFFLINE": "1", "PI_SKIP_VERSION_CHECK": "1"})

    def test_the_station_label_is_never_read_to_decide_anything(self):
        for tools in ("all", "read", "none"):
            a = harness_pi.PI.build(self.req(station="fix", tools=tools))
            for label in ("review", "decide", "", "repro-2"):
                self.assertEqual(harness_pi.PI.build(self.req(station=label, tools=tools)), a, (label, tools))

    def test_a_resume_continues_the_saved_session_and_trims_the_line_a_kill_cut(self):
        sdir = os.path.join(self.d, "pi", "sessions", "--wt--")
        os.makedirs(sdir)
        f = os.path.join(sdir, "2026-11-19T10-00-00_abc-123.jsonl")
        with open(f, "w") as fh:
            fh.write('{"type":"session","id":"abc-123"}\n{"type":"message","x":1}\n{"type":"mess')
        inv = harness_pi.PI.build(self.req(resume="abc-123"))
        self.assertEqual(inv.argv[inv.argv.index("--session"):][:2], ["--session", f])
        with open(f) as fh:
            self.assertEqual(fh.read(), '{"type":"session","id":"abc-123"}\n{"type":"message","x":1}\n', "whole JSON lines only")
        self.assertEqual(harness_pi.PI.build(self.req(resume=f)).argv[-2:], ["--session", f], "a path inside the run directory works too")
        self.assertNotIn("--session", harness_pi.PI.build(self.req()).argv)

    def test_a_resume_pi_cannot_find_or_a_path_outside_the_run_is_refused_not_started_fresh(self):
        for ref in ("no-such-session", "/etc/passwd", "../../etc/passwd"):
            with self.assertRaises(adapter.HarnessError, msg=ref):
                harness_pi.PI.build(self.req(resume=ref))

    def test_the_stream_is_read_into_an_episode_whose_station_the_runner_sets(self):
        with open(os.path.join(HERE, "..", "bugfix", "testdata", "pi", "stream_ok.jsonl")) as f:
            ep = harness_pi.PI.parse(f.read(), {})
        check_contract(self, ep, harness_pi.PI)
        self.assertEqual((ep.station, ep.session_id, ep.answer), ("", "s-ok", "Fixed: value() now returns 2."))


class Execute(unittest.TestCase):
    """adapter.execute: build, write the files, run, read the outputs, parse; over the fake harness."""

    def setUp(self):
        self.h, self.cli = install_fake(self)
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.state = os.path.join(self.d, "state")
        os.makedirs(self.state)
        self.env = dict(os.environ, FAKE_STATE=self.state)

    def script(self, *steps):
        with open(os.path.join(self.state, "fakeh_script"), "w") as f:
            f.write("".join(json.dumps(s) + "\n" for s in steps))

    def calls(self):
        with open(os.path.join(self.state, "fakeh_calls")) as f:
            return [json.loads(x) for x in f]

    def req(self, **kw):
        return request(run_dir=self.d, workdir=self.d, **kw)

    def test_a_run_gives_the_episode_its_files_and_its_stream(self):
        self.script({"turns": [{"text": "a", "usage": {"input": 5, "output": 2}, "cost": 0.5}, {"text": "b"}], "answer": "all done"})
        ep, code, stream = adapter.execute(self.h, self.req(prompt="the prompt", episode="001-fix", system_path="/r.md"), self.env)
        self.assertEqual((code, ep.harness, ep.station, ep.answer, ep.session_id, ep.exit_reason), (0, "fakeh", "fix", "all done", "fakeh-sess-1", "completed"))
        self.assertEqual([t.text for t in ep.turns], ["a", "b"])
        self.assertEqual((ep.usage_total["output"], ep.cost_est), (2, 0.5))
        self.assertEqual(ep.notes, ["fakeh: rules not delivered"])
        self.assertEqual(ep.raw_path, "episodes/001-fix.stream")
        with open(os.path.join(self.d, ep.raw_path)) as f:
            self.assertEqual(f.read(), stream)
        cfg = os.path.join(self.d, "fakeh", "config.json")
        self.assertEqual(oct(os.stat(cfg).st_mode & 0o777), "0o600")
        c = self.calls()[0]
        self.assertEqual((c["stdin"], c["argv"][-2:]), ("the prompt", ["--tools", "all"]), "the prompt is on stdin, not in argv")
        check_contract(self, ep, self.h)

    def test_the_station_is_any_string_and_comes_back_on_the_episode(self):
        for label in ("decide", "a b", ""):
            ep, _, _ = adapter.execute(self.h, self.req(station=label), self.env)
            self.assertEqual(ep.station, label)

    def test_the_key_is_in_the_child_s_environment_only(self):
        kf = os.path.join(self.d, "key")
        with open(kf, "w") as f:
            f.write("sk-secret\n")
        req = self.req()
        req.model.key_file = kf
        adapter.execute(self.h, req, self.env)
        c = self.calls()[0]
        self.assertEqual(c["key"], "sk-secret")
        self.assertNotIn("sk-secret", json.dumps(c["argv"]) + c["stdin"])

    def test_a_resume_passes_the_session_back(self):
        self.script({}, {"answer": "again"})
        ep1, _, _ = adapter.execute(self.h, self.req(), self.env)
        ep2, _, _ = adapter.execute(self.h, self.req(resume=self.h.resume_ref(ep1)), self.env)
        self.assertEqual((ep1.session_id, ep2.session_id, ep2.answer), ("fakeh-sess-1", "fakeh-sess-1", "again"))
        self.assertEqual(self.calls()[1]["argv"][-2:], ["--resume", "fakeh-sess-1"])

    def test_the_turn_cap_ends_the_run_and_the_episode_says_so(self):
        self.script({"turns": [{"text": str(i)} for i in range(6)], "sleep": 30})
        t0 = time.time()
        ep, code, _ = adapter.execute(self.h, self.req(max_turns=3, episode="002-fix"), self.env)
        self.assertEqual((code, ep.exit_reason), (adapter.TURN_CAPPED, "turn_cap"))
        self.assertLess(time.time() - t0, 15)
        self.assertGreaterEqual(len(ep.turns), 3)
        self.assertTrue(os.path.exists(os.path.join(self.d, "episodes", "002-fix.stream")))

    def test_a_timeout_and_a_crash_are_named(self):
        self.script({"turns": [{"text": "x"}], "sleep": 30}, {"exit": 3})
        ep, code, _ = adapter.execute(self.h, self.req(timeout=1), self.env)
        self.assertEqual((code, ep.exit_reason, ep.turns[0].text), (adapter.TIMED_OUT, "timeout", "x"))
        ep, code, _ = adapter.execute(self.h, self.req(), self.env)
        self.assertEqual((code, ep.exit_reason, ep.error), (3, "error", "no final event"))

    def test_a_tools_value_or_a_resume_the_harness_cannot_serve_is_refused_before_anything_runs(self):
        self.h.tools_served = frozenset({"all"})
        with self.assertRaisesRegex(adapter.HarnessError, "does not serve tools=read"):
            adapter.execute(self.h, self.req(tools="read"), self.env)
        self.h.can_resume = False
        with self.assertRaises(adapter.HarnessError):
            adapter.execute(self.h, self.req(resume="s"), self.env)
        self.assertFalse(os.path.exists(os.path.join(self.state, "fakeh_calls")))

    def test_a_file_outside_the_run_directory_is_refused(self):
        from fakeh import FakeH

        class Evil(FakeH):
            def build(self, req):
                inv = super().build(req)
                inv.files = {"../escape": "x"}
                return inv
        with self.assertRaises(adapter.HarnessError):
            adapter.execute(Evil(self.cli), self.req(), self.env)
        self.assertFalse(os.path.exists(os.path.join(os.path.dirname(self.d), "escape")))

    def test_outputs_the_run_writes_reach_parse(self):
        from fakeh import FakeH
        seen = {}

        class Out(FakeH):
            def build(self, req):
                inv = super().build(req)
                inv.outputs = ["fakeh/config.json", "fakeh/missing.json"]
                return inv

            def parse(self, stream_text, files):
                seen.update(files)
                return super().parse(stream_text, files)
        adapter.execute(Out(self.cli), self.req(), self.env)
        self.assertEqual(json.loads(seen["fakeh/config.json"]), {"alias": "bugfix-flash"})
        self.assertEqual(seen["fakeh/missing.json"], "")

    def test_pi_s_stream_is_not_written_beside_its_session_files(self):
        self.assertFalse(harness_pi.PI.tees)
        self.assertTrue(self.h.tees)


class ReadOnlyStationsAreChecked(unittest.TestCase):
    """S6, the runner's side: a harness that cannot stop a read station writing is asked in the prompt, and the workspace is
    compared with itself after the station; a change is a fatal stop."""

    def setUp(self):
        self.h, self.cli = install_fake(self)
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.wt = os.path.join(self.d, "wt")
        os.makedirs(os.path.join(self.wt, "src"))
        with open(os.path.join(self.wt, "src", "a.js"), "w") as f:
            f.write("module.exports = () => 1;\n")
        self.state = os.path.join(self.d, "state")
        os.makedirs(self.state)
        self.env = dict(os.environ, FAKE_STATE=self.state)
        self.run_dir = os.path.join(self.d, "run")
        os.makedirs(self.run_dir)

    def script(self, *steps):
        with open(os.path.join(self.state, "fakeh_script"), "w") as f:
            f.write("".join(json.dumps(s) + "\n" for s in steps))

    def go(self, tools, h=None, **kw):
        return adapter.execute(h or self.h, request(station="check", tools=tools, run_dir=self.run_dir, workdir=self.wt, **kw), self.env)

    def test_a_write_by_a_read_or_none_station_on_a_harness_that_cannot_enforce_it_is_a_fatal_stop(self):
        for tools in ("read", "none"):
            self.script({"files": {"src/a.js": "module.exports = () => 2;\n", "src/new.js": "x"}})
            with self.assertRaises(adapter.ReadOnlyViolation) as cm:
                self.go(tools)
            e = cm.exception
            self.assertEqual(e.changed, ["src/a.js", "src/new.js"], tools)
            self.assertIn(f"tools={tools}", str(e))
            self.assertIn("src/a.js", str(e))
            self.assertIn("cannot enforce it", str(e))
            self.assertEqual((e.episode.harness, e.episode.station), ("fakeh", "check"), "the run's episode is kept with the stop")
            self.assertIsInstance(e, adapter.HarnessError)
            with open(os.path.join(self.wt, "src", "new.js"), "w") as f:   # reset for the next round
                f.write("")
            os.remove(os.path.join(self.wt, "src", "new.js"))
            with open(os.path.join(self.wt, "src", "a.js"), "w") as f:
                f.write("module.exports = () => 1;\n")

    def test_a_removed_file_is_a_change_too(self):
        self.script({"remove": ["src/a.js"]})
        with self.assertRaises(adapter.ReadOnlyViolation) as cm:
            self.go("read")
        self.assertEqual(cm.exception.changed, ["src/a.js"])

    def test_a_station_that_only_reads_passes_and_the_workspace_is_untouched(self):
        self.script({"answer": "looked"})
        ep, code, _ = self.go("read")
        self.assertEqual((code, ep.answer), (0, "looked"))

    def test_a_station_with_all_tools_may_write_and_is_not_checked(self):
        self.script({"files": {"src/a.js": "module.exports = () => 2;\n"}})
        ep, code, _ = self.go("all")
        self.assertEqual(code, 0)
        with open(os.path.join(self.wt, "src", "a.js")) as f:
            self.assertIn("=> 2", f.read())

    def test_a_harness_that_enforces_it_is_not_digested_the_switches_are_its_word(self):
        from fakeh import FakeH

        class Enforcing(FakeH):
            enforces_readonly = True
        self.script({"files": {"src/a.js": "module.exports = () => 3;\n"}})
        with unittest.mock.patch.object(adapter, "workspace_digest", side_effect=AssertionError("digested")):
            ep, code, _ = self.go("read", h=Enforcing(self.cli))
        self.assertEqual(code, 0)

    def test_what_the_harness_keeps_under_the_run_directory_and_in_git_is_not_the_workspace(self):
        digest = adapter.workspace_digest(self.wt)
        os.makedirs(os.path.join(self.wt, ".git"))
        with open(os.path.join(self.wt, ".git", "index"), "w") as f:
            f.write("x")
        inside = os.path.join(self.wt, "run")
        os.makedirs(inside)
        with open(os.path.join(inside, "sessions"), "w") as f:
            f.write("x")
        self.assertEqual(adapter.workspace_digest(self.wt, exclude=[inside]), digest)
        self.assertNotEqual(adapter.workspace_digest(self.wt), digest, "without the exclusion the run folder is a change")

    def test_a_symlink_is_digested_by_its_target_not_followed(self):
        os.symlink("src/a.js", os.path.join(self.wt, "link"))
        before = adapter.workspace_digest(self.wt)
        os.remove(os.path.join(self.wt, "link"))
        os.symlink("elsewhere", os.path.join(self.wt, "link"))
        self.assertEqual(adapter.digest_changes(before, adapter.workspace_digest(self.wt)), ["link"])


if __name__ == "__main__":
    unittest.main()
