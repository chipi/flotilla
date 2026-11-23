"""python3 runtime/test_reads.py: a station's live reads (runtime/reads.py) against an in-process MCP server (the stub of test_mcp_client.py):
the allowlist (a refused call never reaches the server), the per-run call cap, the cut over the cap with the full size recorded, credentials
masked once for the model and the record alike, the record line that a replay (runtime/recorded.py) serves as it is, fail-closed in a shadow
run, a read that cannot be recorded, and the loaded record."""
import json
import os
import stat
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import harness  # noqa: E402
import loaded  # noqa: E402
import reads  # noqa: E402
import recorded  # noqa: E402
from test_mcp_client import Stub, token_file  # noqa: E402

DOC = {"reads": [{"server": "wiki-docs", "tools": ["get_page", "search"], "max_calls": 3, "max_result_chars": 100, "timeout_s": 5}]}


def text_result(s, is_error=False):
    return {"content": [{"type": "text", "text": s}], "isError": is_error}


class Reads(unittest.TestCase):
    def setUp(self):
        for k in ("HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"):
            os.environ.pop(k, None)
        os.environ["NO_PROXY"] = "127.0.0.1"
        reads.reset()
        loaded._seen.clear()
        self.stub = Stub()
        self.addCleanup(self.stub.close)
        d = tempfile.mkdtemp()
        self.file, self.loaded = os.path.join(d, "reads.jsonl"), os.path.join(d, "loaded.jsonl")
        self.env = {"MCP_WIKI_DOCS_URL": self.stub.url, "FLOTILLA_READS_FILE": self.file, "FLOTILLA_LOADED_FILE": self.loaded}
        self.fallback_calls = []
        self.tool = reads.make_tool("answer", DOC, lambda n, a: self.fallback_calls.append(n) or "fallback " + n, env=self.env)
        self.stub.tools = {"get_page": lambda a: text_result("page " + str(a.get("id"))), "search": lambda a: text_result("found")}

    def lines(self):
        if not os.path.exists(self.file):
            return []
        with open(self.file) as f:
            return [json.loads(l) for l in f]


class Allowlist(Reads):
    def test_tools_for_names_each_declared_tool_with_its_server(self):
        self.assertEqual(reads.tools_for(DOC), ["wiki-docs.get_page", "wiki-docs.search"])
        self.assertEqual(reads.tools_for({}), [])
        self.assertEqual(reads.tools_for(None), [])

    def test_a_declared_tool_is_read_live_and_recorded(self):
        self.assertEqual(self.tool("wiki-docs.get_page", {"id": 7}), "page 7")
        self.assertEqual(self.stub.methods(), ["initialize", "notifications/initialized", "tools/call"])
        self.assertEqual(self.stub.seen[-1][1]["params"], {"name": "get_page", "arguments": {"id": 7}})   # the server's own name, not ours
        (line,) = self.lines()
        self.assertEqual({k: line[k] for k in ("station", "tool", "args", "result", "ok", "chars", "cut")},
                         {"station": "answer", "tool": "wiki-docs.get_page", "args": {"id": 7}, "result": "page 7", "ok": True, "chars": 6, "cut": False})
        self.assertRegex(line["at"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        self.assertIsInstance(line["duration_s"], float)

    def test_a_tool_the_server_has_and_the_station_did_not_declare_is_refused_and_the_server_sees_nothing(self):
        self.stub.tools["delete_page"] = lambda a: text_result("deleted")
        got = self.tool("wiki-docs.delete_page", {"id": 1})
        self.assertRegex(got, r"^<read refused: wiki-docs\.delete_page is not a tool this station declares for wiki-docs>$")
        self.assertEqual(self.stub.seen, [])
        self.assertEqual(self.lines(), [])
        self.assertEqual(self.fallback_calls, [])

    def test_other_names_go_to_the_fallback_tool(self):
        self.assertEqual(self.tool("probe", {"x": 1}), "fallback probe")
        self.assertEqual(self.tool("other-server.get_page", {}), "fallback other-server.get_page")
        self.assertEqual(self.tool("wiki-docs", {}), "fallback wiki-docs")       # no tool part: not a read name
        self.assertEqual(self.stub.seen, [])

    def test_arguments_that_are_not_an_object_are_refused(self):
        self.assertIn("<read refused:", self.tool("wiki-docs.search", ["q"]))
        self.assertEqual(self.stub.seen, [])

    def test_the_servers_address_comes_from_the_environment_or_the_read_is_refused(self):
        tool = reads.make_tool("answer", DOC, lambda n, a: "", env={"FLOTILLA_READS_FILE": self.file})
        self.assertIn("MCP_WIKI_DOCS_URL is not set", tool("wiki-docs.search", {}))
        self.assertEqual(self.lines(), [])

    def test_a_token_file_from_the_environment_is_sent_as_the_bearer(self):
        self.stub.token = "tok-123"
        self.env["MCP_WIKI_DOCS_TOKEN_FILE"] = token_file("tok-123\n")
        self.assertEqual(self.tool("wiki-docs.search", {}), "found")
        line = self.lines()[0]
        self.assertNotIn("tok-123", json.dumps(line))


class Caps(Reads):
    def test_the_call_cap_is_per_run_across_stations_and_a_refused_call_is_not_recorded(self):
        other = reads.make_tool("review", DOC, lambda n, a: "", env=self.env)
        self.assertEqual(self.tool("wiki-docs.search", {"q": 1}), "found")
        self.assertEqual(other("wiki-docs.search", {"q": 2}), "found")
        self.assertEqual(self.tool("wiki-docs.search", {"q": 3}), "found")
        self.assertEqual(other("wiki-docs.search", {"q": 4}), "<read refused: wiki-docs is at its 3 reads for this run>")
        self.assertEqual(self.tool("wiki-docs.search", {"q": 5}), "<read refused: wiki-docs is at its 3 reads for this run>")
        self.assertEqual(self.stub.methods().count("tools/call"), 3)
        self.assertEqual([l["station"] for l in self.lines()], ["answer", "review", "answer"])

    def test_a_result_over_the_cap_is_cut_visibly_and_its_full_size_is_recorded(self):
        self.stub.tools["get_page"] = lambda a: text_result("a" * 250)
        got = self.tool("wiki-docs.get_page", {"id": 1})
        self.assertEqual(got, "a" * 100 + "…[cut at 100 of 250 chars]")
        (line,) = self.lines()
        self.assertEqual((line["result"], line["chars"], line["cut"], line["ok"]), (got, 250, True, True))

    def test_a_result_at_the_cap_is_not_cut(self):
        self.stub.tools["get_page"] = lambda a: text_result("b" * 100)
        self.assertEqual(self.tool("wiki-docs.get_page", {}), "b" * 100)
        self.assertFalse(self.lines()[0]["cut"])

    def test_the_body_read_is_the_cap_plus_a_mebibyte(self):
        self.stub.tools["get_page"] = lambda a: text_result("c" * (2 << 20))
        got = self.tool("wiki-docs.get_page", {})
        self.assertRegex(got, r"^<read failed: the answer is over \d+ bytes>$")
        self.assertFalse(self.lines()[0]["ok"])


class Redaction(Reads):
    SECRET = "ghp_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4"

    def test_credentials_are_masked_once_and_the_model_and_the_record_hold_the_same_text(self):
        self.stub.tools["get_page"] = lambda a: text_result(f"token={self.SECRET} and Authorization: Bearer abcdefgh12345678 see https://u:pw@h/x")
        got = self.tool("wiki-docs.get_page", {})
        self.assertNotIn(self.SECRET, got)
        self.assertNotIn("abcdefgh12345678", got)
        self.assertNotIn("u:pw@", got)
        self.assertIn("<redacted>", got)
        self.assertEqual(self.lines()[0]["result"], got)

    def test_e_mail_addresses_and_ips_stay(self):
        self.stub.tools["get_page"] = lambda a: text_result("ask ana@example.org at 10.1.2.3")
        self.assertEqual(self.tool("wiki-docs.get_page", {}), "ask ana@example.org at 10.1.2.3")

    def test_an_error_text_is_masked_too(self):
        self.stub.tools["get_page"] = lambda a: text_result(f"bad credential {self.SECRET}", is_error=True)
        got = self.tool("wiki-docs.get_page", {})
        self.assertTrue(got.startswith("<read failed: bad credential "))
        self.assertNotIn(self.SECRET, got)
        line = self.lines()[0]
        self.assertEqual((line["result"], line["ok"]), (got, False))

    def test_a_credential_is_masked_before_the_cut(self):
        self.stub.tools["get_page"] = lambda a: text_result("x" * 90 + self.SECRET)
        got = self.tool("wiki-docs.get_page", {})
        self.assertNotIn("ghp_", got)


class Failures(Reads):
    def test_a_tool_error_is_a_failed_read_that_is_recorded(self):
        self.stub.tools["search"] = lambda a: text_result("no such page", is_error=True)
        self.assertEqual(self.tool("wiki-docs.search", {"q": "x"}), "<read failed: no such page>")
        line = self.lines()[0]
        self.assertEqual((line["ok"], line["result"]), (False, "<read failed: no such page>"))

    def test_a_server_failure_is_a_failed_read_that_is_recorded_and_never_raises(self):
        self.stub.raw = lambda h, msg: h._send(500)
        got = self.tool("wiki-docs.search", {})
        self.assertEqual(got, "<read failed: HTTP 500>")
        self.assertFalse(self.lines()[0]["ok"])

    def test_a_missing_token_file_is_a_failed_read(self):
        self.env["MCP_WIKI_DOCS_TOKEN_FILE"] = "/nonexistent/token"
        self.assertEqual(self.tool("wiki-docs.search", {}), "<read failed: the token file is unreadable (FileNotFoundError)>")
        self.assertEqual(self.stub.seen, [])

    def test_a_read_that_cannot_be_recorded_fails_and_serves_nothing(self):
        with mock.patch.object(reads.os, "write", side_effect=OSError("disk full")):
            got = self.tool("wiki-docs.search", {"q": "x"})
        self.assertEqual(got, "<read failed: the read could not be recorded (OSError)>")

    def test_without_a_reads_file_nothing_is_read(self):
        del self.env["FLOTILLA_READS_FILE"]
        self.assertIn("FLOTILLA_READS_FILE is not set", self.tool("wiki-docs.search", {}))
        self.assertEqual(self.stub.seen, [])

    def test_a_reads_file_that_cannot_be_opened_means_no_read(self):
        self.env["FLOTILLA_READS_FILE"] = "/nonexistent/dir/reads.jsonl"
        self.assertIn("reads file cannot be opened", self.tool("wiki-docs.search", {}))
        self.assertEqual(self.stub.seen, [])

    def test_the_reads_file_is_mode_600(self):
        self.tool("wiki-docs.search", {})
        self.assertEqual(stat.S_IMODE(os.stat(self.file).st_mode), 0o600)


class Shadow(Reads):
    def test_a_shadow_without_a_record_opens_no_connection(self):
        self.env["FLOTILLA_SHADOW"] = "1"
        got = self.tool("wiki-docs.get_page", {"id": 1})
        self.assertRegex(got, r"^<read refused: a shadow run without the live run's record opens no connection>$")
        self.assertEqual(self.stub.seen, [])
        self.assertEqual(self.lines(), [])

    def test_a_shadow_with_a_record_is_not_refused_here_because_the_harness_serves_the_record_first(self):
        self.env.update(FLOTILLA_SHADOW="1", FLOTILLA_RECORDED_TOOLS="/x")
        self.assertEqual(self.tool("wiki-docs.search", {}), "found")

    def test_a_live_run_is_not_a_shadow(self):
        self.env["FLOTILLA_SHADOW"] = "0"
        self.assertEqual(self.tool("wiki-docs.search", {}), "found")


class RecordAndReplay(Reads):
    def test_the_record_line_is_served_by_the_replayer_as_it_is(self):
        self.stub.tools["get_page"] = lambda a: text_result("z" * 250)
        first = self.tool("wiki-docs.get_page", {"id": 1, "q": "a  b"})
        self.tool("wiki-docs.search", {"q": "x"})
        by_station = {}
        for l in self.lines():
            by_station.setdefault(l["station"], []).append(l)
        rep = recorded.Replayer(by_station)
        self.assertEqual(rep.answer("answer", "wiki-docs.get_page", {"q": "a b", "id": 1.0}), first)       # the normalised key
        self.assertEqual(rep.answer("answer", "wiki-docs.search", {"q": "x"}), "found")
        with self.assertRaises(recorded.RecordedMiss):
            rep.answer("answer", "wiki-docs.get_page", {"id": 2})

    def test_the_harness_wrapper_serves_a_record_and_never_the_live_tool(self):
        self.tool("wiki-docs.search", {"q": "x"})
        n = len(self.stub.seen)
        with open(self.file) as f:
            rec = {"answer": [json.loads(l) for l in f]}
        wrapped = recorded.Replayer(rec).wrap("answer", self.tool)
        self.assertEqual(wrapped("wiki-docs.search", {"q": "x"}), "found")
        self.assertEqual(len(self.stub.seen), n)

    def test_each_read_is_noted_as_untrusted_in_the_loaded_file(self):
        self.tool("wiki-docs.search", {"q": "x"})
        self.tool("wiki-docs.search", {"q": "x"})         # the same item is kept once by loaded.record
        with open(self.loaded) as f:
            rows = [json.loads(l) for l in f]
        self.assertEqual(len(rows), 1)
        r = rows[0]
        self.assertEqual((r["kind"], r["trust"], r["title"], r["source"]), ("probe", "untrusted", "wiki-docs.search", "mcp:wiki-docs"))
        self.assertRegex(r["sha256"], r"^[0-9a-f]{64}$")

    def test_loaded_record_is_called_with_the_hash_of_what_the_model_saw(self):
        import hashlib
        with mock.patch.object(reads.loaded, "record") as rec:
            got = self.tool("wiki-docs.search", {})
        rec.assert_called_once_with("probe", "untrusted", "wiki-docs.search", "mcp:wiki-docs", hashlib.sha256(got.encode()).hexdigest(), environ=self.env)


CFG = {"tools": ["probe"], "limits": {"max_tool_calls": 2, "max_steps": 3},
       "quirks": {"bad_json_uses_turn": True, "empty_retry": True, "final_decision_call": True, "reply_echo_chars": 600,
                  "decision_echo_chars": 400, "result_chars": 1800}}


class Down(Exception):
    pass


class Later(Exception):
    pass


ERR = {"fatal": (Down,), "defer": Later, "defer_prefix": "x: "}
ASK_READ = '{"action":"probe","probe":"wiki-docs.get_page","probe_args":{"id":7}}'
ASK_PROBE = '{"action":"probe","probe":"probe","probe_args":{"fp":"a"}}'
DECIDE = '{"action":"decide","disposition":"file"}'


class InTheLoop(Reads):
    """The flotilla loop (runtime/harness.py) offers a station's read tools only when the station declares them."""

    def step(self, replies, station_doc, live, env=None):
        rs = list(replies)
        with mock.patch.dict(os.environ, dict(self.env, **(env or {}))):
            if not env:
                os.environ.pop("FLOTILLA_RECORDED_TOOLS", None)
            return harness.run_json_step([], lambda m, i: (rs.pop(0), {}), live, CFG, ERR, station="answer", reads=station_doc, log=lambda *a: None)

    def test_a_station_with_reads_gets_the_read_tools_and_the_fleets_own_stay_the_fallback(self):
        live = mock.Mock(return_value="LIVE")
        r = self.step([ASK_READ, ASK_PROBE, DECIDE], DOC, live)
        self.assertEqual([t["result"] for t in r.trace], ["page 7", "LIVE"])
        live.assert_called_once_with("probe", {"fp": "a"})
        (line,) = self.lines()
        self.assertEqual((line["station"], line["tool"], line["args"]), ("answer", "wiki-docs.get_page", {"id": 7}))

    def test_a_station_without_reads_is_unchanged_the_read_name_is_unknown_and_reads_is_never_built(self):
        for doc in (None, {}, {"reads": []}):
            live = mock.Mock(return_value="LIVE")
            with mock.patch.object(reads, "make_tool", side_effect=AssertionError("touched")):
                r = self.step([ASK_READ, ASK_PROBE, DECIDE], doc, live)
            self.assertEqual([t["result"] for t in r.trace], [harness.UNKNOWN_TOOL % "wiki-docs.get_page", "LIVE"])
        self.assertEqual((self.stub.seen, self.lines()), ([], []))

    def test_a_replay_of_a_station_with_reads_answers_from_the_record_and_never_connects(self):
        with open(os.path.join(os.path.dirname(self.file), "rec.json"), "w") as f:
            json.dump({"answer": [{"tool": "wiki-docs.get_page", "args": {"id": 7}, "result": "recorded page", "ok": True}]}, f)
        live = mock.Mock(return_value="LIVE")
        r = self.step([ASK_READ, DECIDE], DOC, live, env={"FLOTILLA_RECORDED_TOOLS": os.path.join(os.path.dirname(self.file), "rec.json")})
        self.assertEqual(r.trace[0]["result"], "recorded page")
        self.assertEqual((self.stub.seen, self.lines()), ([], []))
        live.assert_not_called()


class ShadowReplay(InTheLoop):
    """A shadow of a station with reads (RFC-0014 section 4.8): its reads are answered from the live run's record, a read the record
    lacks fails the run, and the fleet's own tools stay live. The record is the live run's own lines, keyed by the station's name."""

    def record_of(self, live_lines):
        by_station = {}
        for l in live_lines:
            by_station.setdefault(l["station"], []).append(l)
        path = os.path.join(os.path.dirname(self.file), "rec.json")
        with open(path, "w") as f:
            json.dump(by_station, f)
        return {"FLOTILLA_RECORDED_TOOLS": path, "FLOTILLA_SHADOW": "1"}

    def test_the_live_runs_lines_replay_under_the_same_station_name_and_the_server_is_never_reached(self):
        live = mock.Mock(return_value="LIVE")
        self.step([ASK_READ, DECIDE], DOC, live)                         # the live run: one read, recorded under "answer"
        env = self.record_of(self.lines())
        n = len(self.stub.seen)
        r = self.step([ASK_READ, DECIDE], DOC, live, env=env)
        self.assertEqual(r.trace[0]["result"], "page 7")
        self.assertEqual(len(self.stub.seen), n)

    def test_a_read_the_record_lacks_fails_the_run_and_nothing_is_sent(self):
        live = mock.Mock(return_value="LIVE")
        self.step([ASK_READ, DECIDE], DOC, live)
        env = self.record_of(self.lines())
        n = len(self.stub.seen)
        other = '{"action":"probe","probe":"wiki-docs.get_page","probe_args":{"id":8}}'
        with self.assertRaises(recorded.RecordedMiss):
            self.step([other, DECIDE], DOC, live, env=env)
        self.assertEqual(len(self.stub.seen), n)
        live.assert_not_called()

    def test_an_empty_record_misses_every_read(self):
        path = os.path.join(os.path.dirname(self.file), "empty.json")
        with open(path, "w") as f:
            json.dump({}, f)
        with self.assertRaises(recorded.RecordedMiss):
            self.step([ASK_READ, DECIDE], DOC, mock.Mock(), env={"FLOTILLA_RECORDED_TOOLS": path, "FLOTILLA_SHADOW": "1"})
        self.assertEqual(self.stub.seen, [])

    def test_the_fleets_own_tool_the_record_has_nothing_for_stays_live_in_a_station_with_reads(self):
        live = mock.Mock(return_value="LIVE")
        self.step([ASK_READ, DECIDE], DOC, live)
        env = self.record_of(self.lines())
        r = self.step([ASK_READ, ASK_PROBE, DECIDE], DOC, live, env=env)
        self.assertEqual([t["result"] for t in r.trace], ["page 7", "LIVE"])
        live.assert_called_once_with("probe", {"fp": "a"})

    def test_an_offline_run_of_a_station_with_reads_never_goes_live(self):
        live = mock.Mock(return_value="LIVE")
        self.step([ASK_READ, DECIDE], DOC, live)
        env = dict(self.record_of(self.lines()), FLOTILLA_OFFLINE="1")
        with self.assertRaises(recorded.RecordedMiss):
            self.step([ASK_READ, ASK_PROBE, DECIDE], DOC, live, env=env)
        live.assert_not_called()

    def test_a_station_without_reads_still_answers_every_tool_from_the_record(self):
        path = os.path.join(os.path.dirname(self.file), "none.json")
        with open(path, "w") as f:
            json.dump({"answer": []}, f)
        live = mock.Mock(return_value="LIVE")
        with self.assertRaises(recorded.RecordedMiss):
            self.step([ASK_PROBE, DECIDE], None, live, env={"FLOTILLA_RECORDED_TOOLS": path})
        live.assert_not_called()


if __name__ == "__main__":
    unittest.main()
