"""python3 bugfix/test_callrec_pi.py: the calls file lines of a bugfix run's pi episodes (bugfix/callrec_pi.py).
The sessions are synthetic, in the shape bugfix/spend.py reads."""
import json
import os
import sys
import tempfile
import time
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import callrec_pi  # noqa: E402


def msg(role="assistant", model="m-1", usage=None, content=None, stop=None, ts=None, **extra):
    m = {"role": role, "model": model, "content": content if content is not None else []}
    if usage is not None:
        m["usage"] = usage
    if stop:
        m["stopReason"] = stop
    m.update(extra)
    line = {"type": "message", "message": m}
    if ts:
        line["timestamp"] = ts
    return json.dumps(line)


def usage(i, o, cost):
    return {"input": i, "output": o, "cacheRead": 5, "totalTokens": i + o + 5, "cost": {"total": cost}}


def tool(name):
    return {"type": "toolCall", "name": name, "arguments": {}}


class Sessions(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.dir = os.path.join(self.d, "pi")

    def session(self, name, lines, header=True):
        p = os.path.join(self.dir, "sessions", "--wt--", name)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        fresh = not os.path.exists(p)
        with open(p, "a") as f:
            if header and fresh:
                f.write(json.dumps({"type": "session", "version": 3, "id": "abc-123"}) + "\n")
            f.write("".join(x + "\n" for x in lines))
        return p

    def calls(self, path):
        with open(path) as f:
            return [json.loads(x) for x in f]


class SessionLines(Sessions):
    def test_one_line_per_assistant_turn_with_tools_cost_step_and_finish(self):
        p = self.session("s.jsonl", [
            msg("user", content=[{"type": "text", "text": "fix it"}]),
            msg(usage=usage(100, 20, 0.01), stop="toolUse", ts="2026-11-17T10:00:01Z",
                content=[{"type": "text", "text": "looking"}, tool("bash"), tool("read")]),
            msg("toolResult", content=[{"type": "text", "text": "out"}]),
            msg(usage=usage(150, 30, 0.02), stop="stop", content=[{"type": "text", "text": "done"}]),
        ])
        got = callrec_pi.session_lines(p, "coder", "bugfix-flash", episode_seq=2, decision_key="case-1", now="2026-11-17T10:09:09Z")
        self.assertEqual(len(got), 2)
        a, b = got
        self.assertEqual((a["step"], a["tools"], a["finish_reason"], a["tokens_in"], a["tokens_out"], a["cost_usd"], a["cost_source"]),
                         (1, ["bash", "read"], "tool_calls", 100, 20, 0.01, "pi"))
        self.assertEqual((b["step"], b["tools"], b["finish_reason"], b["cost_usd"]), (2, [], "stop", 0.02))
        for c in got:
            self.assertEqual((c["role"], c["alias"], c["episode"], c["episode_seq"], c["decision_key"], c["model"]),
                             ("coder", "bugfix-flash", "abc-123", 2, "case-1", "m-1"))
        self.assertEqual((a["at"], b["at"]), ("2026-11-17T10:00:01Z", "2026-11-17T10:09:09Z"), "pi's time when it gave one")

    def test_finish_reasons_are_mapped_and_unknown_ones_kept(self):
        lines = [msg(stop=s, usage=usage(1, 1, 0)) for s in ("toolUse", "stop", "length", "aborted")] + [msg(usage=usage(1, 1, 0))]
        got = callrec_pi.session_lines(self.session("s.jsonl", lines), "reviewer", "a")
        self.assertEqual([c["finish_reason"] for c in got], ["tool_calls", "stop", "length", "aborted", ""])

    def test_an_error_turn_names_its_error_and_a_turn_without_usage_has_none(self):
        got = callrec_pi.session_lines(self.session("s.jsonl", [msg(stop="error", errorMessage="429 slow down"), msg()]), "verdict", "a")
        self.assertEqual((got[0]["error"], got[0]["finish_reason"], got[0]["tokens_in"], got[0]["cost_usd"], got[0]["cost_source"]),
                         ("429 slow down", "error", None, None, None))
        self.assertIsNone(got[1]["error"])

    def test_empty_garbled_and_foreign_lines_are_skipped(self):
        p = self.session("s.jsonl", ["", "not json", "[1, 2]", '{"type":"message"}', '{"type":"message","message":"x"}',
                                      json.dumps({"type": "model_change"}), msg(usage=usage(1, 2, 0.5), content=[tool("read"), {"type": "toolCall"}, "x"])])
        got = callrec_pi.session_lines(p, "advisor", "a")
        self.assertEqual([(c["step"], c["tools"]) for c in got], [(1, ["read"])])
        self.assertEqual(callrec_pi.session_lines(os.path.join(self.d, "missing.jsonl"), "coder", "a"), [])

    def test_the_episode_is_the_file_name_without_a_header_id_and_start_skips_what_was_recorded(self):
        p = self.session("2026-11-17_xyz.jsonl", [msg(usage=usage(1, 1, 0)), msg(usage=usage(2, 2, 0)), msg(usage=usage(3, 3, 0))], header=False)
        got = callrec_pi.session_lines(p, "coder", "a", start=2)
        self.assertEqual([(c["step"], c["episode"], c["tokens_in"]) for c in got], [(3, "2026-11-17_xyz", 3)])


class RecorderTest(Sessions):
    def rec(self, **env):
        self.path = os.path.join(self.d, "calls.jsonl")
        return callrec_pi.Recorder(self.dir, dict({"FLOTILLA_CALLS_FILE": self.path}, **env), "case-1")

    def test_each_episode_gets_its_role_and_order_and_a_resumed_one_only_its_new_turns(self):
        r = self.rec()
        a = self.session("a.jsonl", [msg(usage=usage(1, 1, 0.1))])
        r.record("verdict", "w")
        time.sleep(0.01)
        b = self.session("b.jsonl", [msg(usage=usage(2, 2, 0.1)), msg(usage=usage(3, 3, 0.1))])
        r.record("coder", "w")
        r.record("coder", "w")   # nothing new: nothing written
        os.utime(a, None)
        self.session("a.jsonl", [msg(usage=usage(4, 4, 0.1))], header=False)   # a resume goes on in its file
        r.record("coder", "w")
        self.assertEqual([(c["role"], c["episode_seq"], c["step"], c["tokens_in"]) for c in self.calls(self.path)],
                         [("verdict", 1, 1, 1), ("coder", 2, 1, 2), ("coder", 2, 2, 3), ("coder", 1, 2, 4)])
        self.assertTrue(os.path.exists(b))

    def test_without_the_variable_nothing_is_written(self):
        r = callrec_pi.Recorder(self.dir, {}, "case-1")
        self.session("a.jsonl", [msg(usage=usage(1, 1, 0.1))])
        r.record("coder", "w")
        self.assertEqual(os.listdir(self.d), ["pi"])

    def test_a_failure_to_write_never_fails_the_run(self):
        r = callrec_pi.Recorder(self.dir, {"FLOTILLA_CALLS_FILE": os.path.join(self.d, "no", "such", "dir", "calls.jsonl")}, "case-1")
        self.session("a.jsonl", [msg(usage=usage(1, 1, 0.1))])
        r.record("coder", "w")   # prints to stderr, raises nothing


if __name__ == "__main__":
    unittest.main()
