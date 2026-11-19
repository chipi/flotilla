"""python3 runtime/test_offline_run.py: the offline entry point (runtime/offline_run.py): the child's environment is an allowlist (nothing of the
caller's, no flotillad address or token), the run's API is a stand-in that serves the input and records every write, the proxy refuses every
host but the models', --out is refused inside the state folder and the fleets tree, and recorded tool results answer the tools or fail the run."""
import json
import os
import sys
import tempfile
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import offline_run as off  # noqa: E402

CHILD = textwrap.dedent('''\
    import json, os, sys, urllib.request
    sys.path.insert(0, %r)
    out = os.environ["OFFLINE_TEST_OUT"] if "OFFLINE_TEST_OUT" in os.environ else sys.argv[1]
    seen = {"env": sorted(os.environ), "shadow": os.environ.get("FLOTILLA_SHADOW"), "home": os.environ.get("HOME"),
            "api": os.environ.get("FLOTILLA_API_URL", ""), "models": os.environ.get("FLOTILLA_MODELS", ""),
            "recorded": os.environ.get("FLOTILLA_RECORDED_TOOLS", "")}
    import ledger
    c = ledger.from_env()
    seen["filings"] = c.filings({"fingerprint": "a"})
    seen["entries"] = len(c.all_entries())
    seen["wrote"] = c.write_filing({"fingerprint": "new"})
    try:
        urllib.request.urlopen("http://evil.invalid/x", timeout=5)
        seen["evil"] = "reached"
    except Exception as e:
        seen["evil"] = type(e).__name__
    with open(os.path.join(sys.argv[1], "child.json"), "w") as f:
        json.dump(seen, f)
    if len(sys.argv) > 2 and sys.argv[2] == "miss":
        import harness
        cfg = {"tools": ["probe"], "limits": {"max_tool_calls": 1, "max_steps": 2},
               "quirks": {"bad_json_uses_turn": True, "empty_retry": True, "final_decision_call": True, "reply_echo_chars": 9, "decision_echo_chars": 9, "result_chars": 99}}
        replies = iter(['{"action":"probe","probe":"probe","probe_args":{"fp":"unrecorded"}}', '{"action":"decide","disposition":"file"}'])
        harness.run_json_step([], lambda m, s: (next(replies), {}), lambda n, a: sys.exit(77), cfg,
                              {"fatal": (OSError,), "defer": RuntimeError, "defer_prefix": ""}, station="decide", log=lambda *a: None)
''' % HERE)

ENVELOPE = {"schema_version": 1, "input_key": "k-1", "routine": "triage/cycle", "kind": "tick",
            "ledger": {"mark_filings": 2, "mark_entries": 1, "filings": [{"fingerprint": "a", "n": "1"}, {"fingerprint": "b", "n": "2"}],
                       "entries": [{"kind": "decision", "row": {"fingerprint": "a", "disposition": "file"}}]},
            "recorded_tool_results": {"decide": [{"tool": "probe", "args": {"fp": "known"}, "result": "r"}]}}


class World:
    def __init__(self, tc):
        self.tmp = tempfile.TemporaryDirectory()
        tc.addCleanup(self.tmp.cleanup)
        self.root = os.path.join(self.tmp.name, "code")
        os.makedirs(os.path.join(self.root, "w"))
        os.makedirs(os.path.join(self.root, "fleets"))
        with open(os.path.join(self.root, "w", "child.py"), "w") as f:
            f.write(CHILD)
        self.out = os.path.join(self.tmp.name, "out")
        self.models = {"endpoints": {"gw": {"base_url": "http://gateway.example:4000/v1"}}, "aliases": {"flash": {"endpoint": "gw", "model": "m"}}}
        self.caller = {"PATH": os.environ["PATH"], "FLOTILLA_CODE_ROOT": self.root, "FLOTILLA_API_URL": "http://127.0.0.1:1",
                       "FLOTILLA_API_TOKEN": "live-token", "GITHUB_TOKEN": "gh-secret", "LITELLM_KEY": "live-key", "FLOTILLA_ROUTINE_VERSION": "9"}

    def contract(self, *extra):
        return {"routine": "triage/cycle", "version": 2, "workdir": "w", "timeout": "1m",
                "command": f"{sys.executable} child.py {self.out} " + " ".join(extra),
                "env": {"FLOTILLA_ROUTINE": "triage/cycle", "FLOTILLA_ROUTINE_VERSION": "2", "FLOTILLA_STATIONS": '{"schema_version":1,"stations":{}}',
                        "FLOTILLA_API_TOKEN_FILE": "/live/token", "FLOTILLA_MODELS": "{}", "FLOTILLA_LIVE_KEY": "x", "FLOTILLA_ENVIRONMENT": "operations"}}

    def go(self, envelope=ENVELOPE, contract=None, forbid=(), out=None):
        return off.run(contract or self.contract(), envelope, self.models, out or self.out, forbid=forbid, caller_env=self.caller, code_root=self.root)

    def child(self):
        with open(os.path.join(self.out, "child.json")) as f:
            return json.load(f)

    def lines(self, name):
        with open(os.path.join(self.out, name)) as f:
            return [json.loads(x) for x in f]


class Offline(unittest.TestCase):
    def test_the_child_gets_an_allowlisted_environment_and_none_of_the_callers(self):
        w = World(self)
        self.assertEqual(w.go()["exit_code"], 0, open(os.path.join(w.out, "output.txt")).read())
        c = w.child()
        for leaked in ("FLOTILLA_API_TOKEN", "GITHUB_TOKEN", "LITELLM_KEY", "FLOTILLA_LIVE_KEY"):
            self.assertNotIn(leaked, c["env"])
        self.assertTrue(c["api"].startswith("http://127.0.0.1:") and c["api"] != "http://127.0.0.1:1", "the stand-in, not the caller's address")
        self.assertNotEqual(c["home"], os.environ.get("HOME"))
        self.assertEqual(c["shadow"], "1")
        self.assertEqual(json.loads(c["models"]), w.models, "the eval's own models config, not the contract's")
        self.assertIn("FLOTILLA_STATIONS", c["env"], "the contract's own variables arrive")
        self.assertNotIn("FLOTILLA_ROUTINE_VERSION=9", c["env"])

    def test_the_stand_in_serves_the_input_and_records_each_write_as_a_decision(self):
        w = World(self)
        w.go()
        c = w.child()
        self.assertEqual((c["filings"], c["entries"], c["wrote"]), ([{"fingerprint": "a", "n": "1"}], 1, 1))
        (d,) = w.lines("decisions.jsonl")
        self.assertEqual((d["method"], d["path"], d["body"]["row"]), ("POST", "/v1/ledger/filings", {"fingerprint": "new"}))

    def test_a_request_without_the_stand_in_token_is_refused(self):
        import urllib.error
        import urllib.request
        w = World(self)
        envelope = dict(ENVELOPE)
        api = off.StandIn(envelope, os.path.join(w.tmp.name, "d.jsonl"))
        import threading
        threading.Thread(target=api.serve_forever, daemon=True).start()
        self.addCleanup(api.shutdown)
        with self.assertRaises(urllib.error.HTTPError) as cm:
            urllib.request.urlopen(f"http://127.0.0.1:{api.server_address[1]}/v1/ledger/counts", timeout=5)
        self.assertEqual(cm.exception.code, 401)

    def test_the_proxy_refuses_every_host_but_the_models_and_keeps_what_it_refused(self):
        w = World(self)
        w.go()
        self.assertNotEqual(w.child()["evil"], "reached")
        self.assertTrue(any("evil.invalid" in r["target"] for r in w.lines("refused.jsonl")))
        env = off.build_env({}, "{}", w.out, "/h", "u", "t", "http://127.0.0.1:9", {"gateway.example"}, "", {})
        self.assertIn("gateway.example", env["NO_PROXY"], "a model host is reached directly, not through the refusing proxy")

    def test_recorded_tool_results_reach_the_child_as_a_file_and_not_when_the_input_has_none(self):
        w = World(self)
        w.go()
        with open(w.child()["recorded"]) as f:
            self.assertEqual(json.load(f), ENVELOPE["recorded_tool_results"])
        w2 = World(self)
        w2.go(envelope={k: v for k, v in ENVELOPE.items() if k != "recorded_tool_results"})
        self.assertEqual(w2.child()["recorded"], "")

    def test_a_tool_call_with_no_recorded_result_fails_the_run_and_is_never_sent_live(self):
        w = World(self)
        r = w.go(contract=w.contract("miss"))
        self.assertNotEqual(r["exit_code"], 0)
        self.assertIn("RecordedMiss", open(os.path.join(w.out, "output.txt")).read())
        self.assertNotEqual(r["exit_code"], 77, "the live tool was called")

    def test_out_inside_the_state_folder_or_the_fleets_tree_is_refused_and_nothing_is_written(self):
        w = World(self)
        state = os.path.join(w.tmp.name, "state")
        os.makedirs(state)
        for out, forbid in ((os.path.join(state, "x"), [state]), (state, [state]), (os.path.join(w.root, "fleets", "triage", "o"), [])):
            with self.assertRaises(off.Refused, msg=out):
                w.go(forbid=forbid, out=out)
            self.assertTrue(out == state or not os.path.exists(out))

    def test_an_input_that_is_not_an_envelope_is_refused(self):
        w = World(self)
        with self.assertRaisesRegex(off.Refused, "not a valid envelope"):
            w.go(envelope={"schema_version": 1})
        self.assertFalse(os.path.exists(w.out))

    def test_the_command_line_has_no_flag_for_a_flotillad_address_or_token(self):
        import io
        from contextlib import redirect_stderr
        for flag in ("--api-url", "--token", "--token-file", "--flotillad"):
            with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                off.main(["--contract", "c", "--input", "i", "--models", "m", "--out", "o", flag, "x"])

    def test_the_run_writes_its_result_and_the_files_the_caller_reads(self):
        w = World(self)
        w.go()
        for name in ("result.json", "output.txt", "calls.jsonl", "loaded.jsonl", "decisions.jsonl"):
            self.assertTrue(os.path.exists(os.path.join(w.out, name)), name)
        self.assertEqual(w.lines("result.json")[0]["input_key"], "k-1")


if __name__ == "__main__":
    unittest.main()
