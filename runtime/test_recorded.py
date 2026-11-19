"""python3 runtime/test_recorded.py: recorded tool results (runtime/recorded.py) and the flotilla loop answering from them (runtime/harness.py,
FLOTILLA_RECORDED_TOOLS): the normaliser, the match, a miss that fails the run and is never sent live, repeated keys, and the loop untouched
when the variable is not set."""
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import harness  # noqa: E402
import recorded  # noqa: E402


class Normalise(unittest.TestCase):
    def test_equal_arguments_in_any_spelling_have_one_key(self):
        a = recorded.key("s", "probe", {"b": 1, "a": ["x  y", {"k": 2.0}]})
        for same in ({"a": ["x y", {"k": 2}], "b": 1.0}, {"a": [" x\n y ", {"k": 2}], "b": 1}):
            self.assertEqual(recorded.key("s", "probe", same), a)

    def test_nfc_strings_and_the_whole_key_tell_apart_what_differs(self):
        self.assertEqual(recorded.key("s", "t", {"n": "é"}), recorded.key("s", "t", {"n": "é"}))
        base = recorded.key("s", "t", {"n": 1})
        for other in (recorded.key("s2", "t", {"n": 1}), recorded.key("s", "t2", {"n": 1}), recorded.key("s", "t", {"n": 2}), recorded.key("s", "t", {"n": 1.5})):
            self.assertNotEqual(other, base)

    def test_a_bool_is_not_an_integer_and_missing_arguments_are_empty(self):
        self.assertNotEqual(recorded.key("s", "t", {"n": True}), recorded.key("s", "t", {"n": 1}))
        self.assertEqual(recorded.key("s", "t", None), recorded.key("s", "t", {}))


class Replay(unittest.TestCase):
    REC = {"decide": [{"tool": "probe", "args": {"fp": "a"}, "result": {"n": 1}, "ok": True},
                      {"tool": "probe", "args": {"fp": "b"}, "result": "first"},
                      {"tool": "probe", "args": {"fp": "b"}, "result": "second"}]}

    def test_a_call_is_matched_by_station_tool_and_arguments_never_by_position(self):
        r = recorded.Replayer(self.REC)
        self.assertEqual(r.answer("decide", "probe", {"fp": "b"}), "first")
        self.assertEqual(r.answer("decide", "probe", {"fp": "a"}), {"n": 1}, "asked out of the recorded order")

    def test_a_call_with_no_recorded_result_fails_and_names_what_it_asked(self):
        r = recorded.Replayer(self.REC)
        for args in ({"fp": "zzz"}, {}):
            with self.assertRaisesRegex(recorded.RecordedMiss, "no recorded result for station 'decide' tool 'probe'"):
                r.answer("decide", "probe", args)
        with self.assertRaises(recorded.RecordedMiss):
            r.answer("other", "probe", {"fp": "a"})

    def test_a_key_recorded_once_answers_every_repeat_and_several_answer_in_order_with_the_last_repeating(self):
        r = recorded.Replayer(self.REC)
        self.assertEqual([r.answer("decide", "probe", {"fp": "a"}) for _ in range(3)], [{"n": 1}] * 3)
        self.assertEqual([r.answer("decide", "probe", {"fp": "b"}) for _ in range(4)], ["first", "second", "second", "second"])

    def test_a_wrapped_tool_never_calls_the_live_one(self):
        live = mock.Mock(return_value="LIVE")
        tool = recorded.Replayer(self.REC).wrap("decide", live)
        self.assertEqual(tool("probe", {"fp": "a"}), {"n": 1})
        with self.assertRaises(recorded.RecordedMiss):
            tool("probe", {"fp": "nope"})
        live.assert_not_called()

    def test_malformed_recordings_are_refused(self):
        for bad in ([], {"s": {}}, {"s": [{"args": {}, "result": 1}]}, {"s": [{"tool": "t", "args": {}}]}, {"s": [{"tool": "t", "args": [], "result": 1}]}):
            with self.assertRaises(ValueError, msg=bad):
                recorded.Replayer(bad)


CFG = {"tools": ["probe"], "limits": {"max_tool_calls": 2, "max_steps": 3},
       "quirks": {"bad_json_uses_turn": True, "empty_retry": True, "final_decision_call": True, "reply_echo_chars": 600, "decision_echo_chars": 400, "result_chars": 1800}}


class Down(Exception):
    pass


class Later(Exception):
    pass


ERR = {"fatal": (Down,), "defer": Later, "defer_prefix": "x: "}


def model(*replies):
    rs = list(replies)
    return lambda messages, step: (rs.pop(0), {})


class Loop(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.addCleanup(self.d.cleanup)
        self.path = os.path.join(self.d.name, "rec.json")
        with open(self.path, "w") as f:
            json.dump({"decide": [{"tool": "probe", "args": {"fp": "a"}, "result": "recorded A"}]}, f)

    def run_step(self, call, live, env):
        with mock.patch.dict(os.environ, env):
            return harness.run_json_step([], call, live, CFG, ERR, station="decide", log=lambda *a: None)

    def test_with_the_variable_the_loop_answers_probes_from_the_recordings(self):
        live = mock.Mock(return_value="LIVE")
        call = model('{"action":"probe","probe":"probe","probe_args":{"fp":"a"}}', '{"action":"decide","disposition":"file"}')
        r = self.run_step(call, live, {"FLOTILLA_RECORDED_TOOLS": self.path})
        self.assertEqual((r.decision["disposition"], r.trace[0]["result"]), ("file", "recorded A"))
        live.assert_not_called()

    def test_a_probe_with_no_recording_fails_the_run_and_the_live_tool_is_not_called(self):
        live = mock.Mock(return_value="LIVE")
        call = model('{"action":"probe","probe":"probe","probe_args":{"fp":"unrecorded"}}', '{"action":"decide","disposition":"file"}')
        with self.assertRaises(recorded.RecordedMiss):
            self.run_step(call, live, {"FLOTILLA_RECORDED_TOOLS": self.path})
        live.assert_not_called()

    def test_without_the_variable_the_live_tool_answers_and_recorded_is_not_even_imported(self):
        live = mock.Mock(return_value="LIVE")
        call = model('{"action":"probe","probe":"probe","probe_args":{"fp":"a"}}', '{"action":"decide","disposition":"file"}')
        env = {k: v for k, v in os.environ.items() if k != "FLOTILLA_RECORDED_TOOLS"}
        with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(recorded, "from_env", side_effect=AssertionError("touched")):
            r = harness.run_json_step([], call, live, CFG, ERR, log=lambda *a: None)
        self.assertEqual(r.trace[0]["result"], "LIVE")
        live.assert_called_once()


if __name__ == "__main__":
    unittest.main()
