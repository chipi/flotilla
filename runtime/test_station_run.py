"""python3 runtime/test_station_run.py: the station driver (runtime/station_run.py) over the fake harness: the prompt rendered from its
template and slots, the episode, its record and the calls lines kept, a session resumed by its reference, the gateway's cost, and a
read station that changed the workspace stopped with its episode on record."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import adapter  # noqa: E402
import station_run as sr  # noqa: E402
from fakeh import install_fake  # noqa: E402


def station(name="decide", tools="all", prompt="Decide {{what}}.", **kw):
    route = adapter.ModelRoute(alias="flash", model="fleet-flash", base_url="http://127.0.0.1:9/v1", endpoint="gw", key_file="/dev/null")
    return sr.Station(name=name, harness="fakeh", model=route, prompt=prompt, tools=tools, alias="flash", **kw)


class Render(unittest.TestCase):
    def test_slots_fill_the_template_and_a_space_inside_the_braces_is_fine(self):
        self.assertEqual(sr.render("a {{x}} b {{ y }} c {{x}}", {"x": 1, "y": "two"}), "a 1 b two c 1")
        self.assertEqual(sr.render("no slots", {}), "no slots")

    def test_a_slot_the_caller_did_not_give_is_an_error(self):
        with self.assertRaisesRegex(sr.StationError, "needs the slot.*y"):
            sr.render("{{x}} {{y}}", {"x": 1})

    def test_a_slot_the_text_does_not_use_is_an_error_too(self):
        with self.assertRaisesRegex(sr.StationError, "gave the slot.*z"):
            sr.render("{{x}}", {"x": 1, "z": 2})

    def test_text_ref_is_the_hash_and_the_length_in_bytes(self):
        r = sr.text_ref("system", "héllo")
        self.assertEqual((r["name"], r["bytes"]), ("system", 6))
        import hashlib
        self.assertEqual(r["sha256"], hashlib.sha256("héllo".encode()).hexdigest())


class RunStation(unittest.TestCase):
    def setUp(self):
        self.h, self.cli = install_fake(self)
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.run_dir, self.wt, self.state = (os.path.join(self.d, x) for x in ("run", "wt", "state"))
        for p in (self.run_dir, self.wt, self.state):
            os.makedirs(p)
        self.env = dict(os.environ, FAKE_STATE=self.state)
        self.calls = os.path.join(self.d, "calls.jsonl")

    def script(self, *steps):
        with open(os.path.join(self.state, "fakeh_script"), "w") as f:
            f.write("".join(json.dumps(s) + "\n" for s in steps))

    def fakeh_calls(self):
        with open(os.path.join(self.state, "fakeh_calls")) as f:
            return [json.loads(x) for x in f]

    def go(self, st, slots=None, **kw):
        return sr.run_station(st, {"what": "it"} if slots is None else slots, run_dir=self.run_dir, workdir=self.wt, base_env=self.env,
                              calls_path=self.calls, **kw)

    def test_the_prompt_is_rendered_and_goes_to_the_harness_on_stdin(self):
        self.script({"turns": [{"text": "a", "usage": {"input": 5, "output": 2}}], "answer": "chose x"})
        r = self.go(station(), seq=3)
        self.assertEqual(self.fakeh_calls()[0]["stdin"], "Decide it.")
        self.assertEqual((r.episode.station, r.episode.answer, r.code), ("decide", "chose x", 0))
        self.assertEqual(r.episode.turns[0].texts, [sr.text_ref("prompt", "Decide it.")], "the rendered text is on the first call")

    def test_a_slot_problem_fails_before_any_model_call(self):
        for slots in ({}, {"what": 1, "more": 2}):
            with self.assertRaises(sr.StationError):
                self.go(station(), slots)
        self.assertFalse(os.path.exists(os.path.join(self.state, "fakeh_calls")), "the harness was never started")
        self.assertFalse(os.path.exists(os.path.join(self.run_dir, "episodes")))

    def test_the_episode_the_record_and_the_calls_lines_are_kept_under_the_sequence(self):
        self.script({"turns": [{"text": "a", "usage": {"input": 5, "output": 2}, "tools": [{"name": "read", "arg": "x", "ok": True}]}]})
        r = self.go(station(), seq=3, version=2, spec_hash="0123456789ab")
        self.assertEqual((r.path, r.record_path), ("episodes/003-decide.json", "episodes/003-decide.record.json"))
        doc = adapter.read_episode(os.path.join(self.run_dir, r.path))
        self.assertEqual((doc["version"], doc["station"], doc["alias"], doc["seq"], doc["resume_ref"]), (2, "decide", "flash", 3, "fakeh-sess-1"))
        with open(os.path.join(self.run_dir, r.record_path)) as f:
            rec = json.load(f)
        self.assertEqual(adapter.validate_episode(rec), [])
        self.assertEqual((rec["seq"], rec["version"], rec["spec_hash"], rec["station"]), (3, 2, "0123456789ab", "decide"))
        with open(self.calls) as f:
            lines = [json.loads(x) for x in f]
        self.assertEqual(len(lines), 1)
        c = lines[0]
        self.assertEqual((c["station"], c["role"], c["harness"], c["episode"], c["episode_seq"], c["step"], c["alias"], c["tokens_in"], c["tokens_out"]),
                         ("decide", "", "fakeh", "fakeh-sess-1", 3, 1, "flash", 5, 2))
        self.assertEqual(c["tools"], [{"name": "read", "arg": "x", "ok": True}])
        self.assertEqual(c["texts"], [sr.text_ref("prompt", "Decide it.")])

    def test_a_calls_line_without_texts_has_no_texts_field(self):
        ep = adapter.Episode(harness="h", station="s", session_id="sid", turns=[adapter.Turn(n=1, model="m", usage=adapter.usage_dict(1, 2),
                                                                                              tools=[{"name": "bash", "arg": "ls", "ok": False, "args": {"command": "ls"},
                                                                                                      "result": "r", "duration_s": 1}])])
        (line,) = sr.calls_lines(ep, "alias", "001-s", 1, now="2026-01-01T00:00:00Z")
        self.assertEqual(line, {"at": "2026-01-01T00:00:00Z", "alias": "alias", "endpoint": None, "model": "m", "deployment": None,
                                "tokens_in": 1, "tokens_out": 2, "cost_usd": None, "cost_source": "", "latency_s": None, "status": None,
                                "error": None, "finish_reason": "", "step": 1, "decision_key": None, "role": "", "episode": "sid",
                                "episode_seq": 1, "tools": [{"name": "bash", "arg": "ls", "ok": False}], "harness": "h", "station": "s"})

    def test_a_session_is_resumed_by_the_reference_the_episode_left(self):
        self.script({}, {"answer": "again"})
        r1 = self.go(station())
        r2 = self.go(station(prompt="Follow up: {{what}}"), resume=r1.resume_ref, seq=2)
        self.assertEqual((r1.resume_ref, r2.episode.session_id, r2.episode.answer), ("fakeh-sess-1", "fakeh-sess-1", "again"))
        self.assertEqual(self.fakeh_calls()[1]["argv"][-2:], ["--resume", "fakeh-sess-1"])
        self.assertEqual(self.fakeh_calls()[1]["stdin"], "Follow up: it")

    def test_home_is_the_run_directory_whatever_the_caller_had(self):
        self.env["HOME"] = os.path.join(self.d, "worker-home")
        self.go(station())
        self.assertEqual(self.fakeh_calls()[0]["home"], self.run_dir)

    def test_the_cost_is_the_gateways_figure_over_the_run_when_a_meter_is_given(self):
        spend = iter([1.00, 1.25])
        r = self.go(station(), meter=lambda: next(spend))
        self.assertEqual(r.record["cost"], {"usd": 0.25, "source": "gateway"})
        r = self.go(station(), seq=2)
        self.assertEqual(r.record["cost"], {"usd": None, "source": "unknown"}, "no meter and no estimate: unknown, said so")

    def test_the_station_name_is_a_label_and_changes_nothing_but_the_records(self):
        self.go(station(name="alpha"), seq=1)
        self.go(station(name="zzz_other"), seq=2)
        a, b = self.fakeh_calls()
        self.assertEqual((a["argv"], a["stdin"]), (b["argv"], b["stdin"]))
        self.assertTrue(os.path.exists(os.path.join(self.run_dir, "episodes", "001-alpha.json")))
        self.assertTrue(os.path.exists(os.path.join(self.run_dir, "episodes", "002-zzz_other.json")))

    def test_a_tools_value_the_harness_does_not_serve_is_refused_before_it_runs(self):
        self.h.tools_served = frozenset({"all"})
        with self.assertRaises(adapter.HarnessError):
            self.go(station(tools="none"))
        self.assertFalse(os.path.exists(os.path.join(self.state, "fakeh_calls")))

    def test_an_episode_the_schema_refuses_is_an_error_and_nothing_is_written(self):
        class Bad(type(self.h)):
            def parse(inner, stream_text, files):
                ep = super().parse(stream_text, files)
                ep.exit_reason = "crashed"
                return ep
        adapter.register(Bad(self.cli))
        with self.assertRaisesRegex(sr.StationError, "schema refuses"):
            self.go(station())
        self.assertFalse(os.path.exists(os.path.join(self.run_dir, "episodes", "001-decide.json")))
        self.assertFalse(os.path.exists(self.calls))

    def test_a_read_station_that_changed_the_workspace_leaves_its_episode_and_record_then_stops(self):
        self.script({"files": {"x.txt": "changed"}})
        with self.assertRaises(adapter.ReadOnlyViolation) as cm:
            self.go(station(tools="read"))
        self.assertEqual(cm.exception.changed, ["x.txt"])
        for rel in ("001-decide.json", "001-decide.record.json"):
            self.assertTrue(os.path.exists(os.path.join(self.run_dir, "episodes", rel)), rel)
        self.assertTrue(os.path.exists(self.calls))


class Sigterm(unittest.TestCase):
    def test_sigterm_becomes_an_exit_so_the_harness_group_is_killed(self):
        code = ("import os, signal, sys; sys.path.insert(0, %r); import station_run; station_run.exit_on_sigterm(); "
                "os.kill(os.getpid(), signal.SIGTERM); import time; time.sleep(5)" % HERE)
        p = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, timeout=30)
        self.assertEqual(p.returncode, 143, p.stderr)


if __name__ == "__main__":
    unittest.main()
