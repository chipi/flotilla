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


def tool(name, args=None, id=None):
    t = {"type": "toolCall", "name": name, "arguments": args if args is not None else {}}
    if id:
        t["id"] = id
    return t


def result(id, is_error=False):
    return msg("toolResult", content=[{"type": "text", "text": "out"}], toolCallId=id, isError=is_error)


class FakeMeter:
    """spend.Meter's episode(): the scripted bill of each episode, in order (None: not measurable, an exception: raised)."""

    def __init__(self, *bills):
        self.bills, self.asked = list(bills), 0

    def episode(self):
        self.asked += 1
        b = self.bills.pop(0)
        if isinstance(b, BaseException):
            raise b
        return b


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
    def test_one_line_per_assistant_turn_with_tools_step_and_finish_and_never_pi_s_cost(self):
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
                         (1, [{"name": "bash", "arg": ""}, {"name": "read", "arg": ""}], "tool_calls", 100, 20, None, ""))
        self.assertEqual((b["step"], b["tools"], b["finish_reason"], b["cost_usd"], b["cost_source"]), (2, [], "stop", None, ""))
        self.assertNotIn("0.01", json.dumps(got), "pi's catalog price of the turn is nowhere on its line")
        self.assertNotIn("0.02", json.dumps(got))
        self.assertNotIn("pi", [c["cost_source"] for c in got])
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
                         ("429 slow down", "error", None, None, ""))
        self.assertIsNone(got[1]["error"])

    def test_empty_garbled_and_foreign_lines_are_skipped(self):
        p = self.session("s.jsonl", ["", "not json", "[1, 2]", '{"type":"message"}', '{"type":"message","message":"x"}',
                                      json.dumps({"type": "model_change"}), msg(usage=usage(1, 2, 0.5), content=[tool("read"), {"type": "toolCall"}, "x"])])
        got = callrec_pi.session_lines(p, "advisor", "a")
        self.assertEqual([(c["step"], c["tools"]) for c in got], [(1, [{"name": "read", "arg": ""}])])
        self.assertEqual(callrec_pi.session_lines(os.path.join(self.d, "missing.jsonl"), "coder", "a"), [])

    def test_the_episode_is_the_file_name_without_a_header_id_and_start_skips_what_was_recorded(self):
        p = self.session("2026-11-17_xyz.jsonl", [msg(usage=usage(1, 1, 0)), msg(usage=usage(2, 2, 0)), msg(usage=usage(3, 3, 0))], header=False)
        got = callrec_pi.session_lines(p, "coder", "a", start=2)
        self.assertEqual([(c["step"], c["episode"], c["tokens_in"]) for c in got], [(3, "2026-11-17_xyz", 3)])


class ToolDetail(Sessions):
    def test_a_tool_call_carries_its_main_argument_and_whether_it_worked(self):
        p = self.session("s.jsonl", [
            msg(usage=usage(10, 2, 0), stop="toolUse", content=[
                tool("bash", {"command": "grep -n foo src/x.ts"}, "c1"), tool("read", {"path": "src/x.ts", "offset": 3}, "c2"),
                tool("grep", {"path": "src", "pattern": "needle"}, "c3"), tool("custom", {"n": 7}, "c4"), tool("noargs", None, "c5")]),
            result("c1"), result("c2", True), result("c3", False),
        ])
        (c,) = callrec_pi.session_lines(p, "coder", "a")
        self.assertEqual(c["tools"], [{"name": "bash", "arg": "grep -n foo src/x.ts", "ok": True},
                                      {"name": "read", "arg": "src/x.ts", "ok": False},
                                      {"name": "grep", "arg": "needle", "ok": True},
                                      {"name": "custom", "arg": "7"},
                                      {"name": "noargs", "arg": ""}], "ok is left out when the session has no result for the call")

    def test_the_argument_is_scrubbed_on_one_line_and_cut_at_160_characters(self):
        cmd = ("fetch -H 'Authorization: Bearer abcdefghij1234567890' https://u:hunter2@host.example/x?token=sk-" + "a" * 24 + " "
               "&& API_KEY=topsecret ./run --password 'p w' ssh root@10.1.2.3 mail ada@example.com\nnext line")
        p = self.session("s.jsonl", [msg(usage=usage(1, 1, 0), content=[tool("bash", {"command": cmd}, "c1"),
                                                                        tool("bash", {"command": "echo " + "y" * 400}, "c2")])])
        (c,) = callrec_pi.session_lines(p, "coder", "a")
        arg = callrec_pi.clean_arg(cmd, 1000)   # whole: what is kept of the rest is not cut away by the 160
        self.assertTrue(c["tools"][0]["arg"].startswith(arg[:100]))
        for secret in ("abcdefghij1234567890", "hunter2", "sk-aaaa", "topsecret", "p w", "10.1.2.3", "ada@example.com"):
            self.assertNotIn(secret, arg)
        for kept in ("fetch", "https://", "host.example", "<ip>", "<email>", "API_KEY=<redacted>", "next line"):
            self.assertIn(kept, arg)
        self.assertNotIn("\n", arg)
        self.assertEqual(len(c["tools"][1]["arg"]), callrec_pi.ARG_MAX)
        self.assertTrue(c["tools"][1]["arg"].endswith("…"))

    def test_a_key_is_redacted_before_the_cut_so_a_cut_never_leaves_half_of_it(self):
        key = "sk-" + "A" * 40
        text = "x" * 150 + " " + key
        out = callrec_pi.clean_arg(text)
        self.assertLessEqual(len(out), 160)
        self.assertNotIn("sk-", out)
        self.assertNotIn("AAAA", out)

    def test_the_two_scrubbers_agree(self):
        """bugfix/ cannot import runtime/toolrec.py in its worker image, so it carries a copy of the rules."""
        sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "runtime"))
        import toolrec  # noqa: E402
        corpus = ["fetch -H 'Authorization: Bearer abcdefghij1234567890' https://u:pw@h/x", "API_KEY=abc OPENAI_TOKEN='x y' z=1",
                  '{"api_key": "abc", "name": "n"}', "git config user.email a.b@c.example", "ping 192.168.0.1 Chrome/141.0.0.0",
                  "ghp_" + "a" * 36 + " glsa_" + "b" * 20 + " AKIA" + "C" * 16, "--token abc123 --password=hunter2", "a" * 300,
                  "ls -la\n  src\ttests", ".".join(["eyJhbGciOiJIUzI1NiJ9", "eyJzdWIiOiIxMjM0NSJ9", "c2lnbmF0dXJl"])]
        for text in corpus:
            self.assertEqual(callrec_pi.clean_arg(text), toolrec.clean_arg(text), text)
        for args in ({"command": "x", "path": "p"}, {"path": "p", "pattern": "q"}, {"zzz": "z", "a": 1}, {"a": 4, "b": "s"}, {"a": True}, {}, None, "s"):
            self.assertEqual(callrec_pi.primary_arg(args), toolrec.primary_arg(args), args)
        self.assertEqual(callrec_pi.ARG_MAX, toolrec.ARG_MAX)


class Apportion(unittest.TestCase):
    def lines(self, *tokens):
        return [{"tokens_in": i, "tokens_out": o, "cost_usd": 9.9, "cost_source": "pi"} for i, o in tokens]

    def test_the_shares_follow_the_tokens_and_sum_to_the_episode_s_bill(self):
        ls = self.lines((100, 20), (300, 80), (50, 0))
        callrec_pi.apportion(ls, 0.0137)
        self.assertEqual({c["cost_source"] for c in ls}, {"gateway-share"})
        self.assertAlmostEqual(ls[0]["cost_usd"] / ls[1]["cost_usd"], 120 / 380, places=9)
        self.assertAlmostEqual(ls[0]["cost_usd"], 0.0137 * 120 / 550, places=12)
        self.assertAlmostEqual(sum(c["cost_usd"] for c in ls), 0.0137, places=12)

    def test_a_turn_with_no_token_count_gets_none_and_one_with_zero_tokens_gets_zero(self):
        ls = self.lines((10, 10), (0, 0)) + [{"tokens_in": None, "tokens_out": None}]
        callrec_pi.apportion(ls, 1.0)
        self.assertEqual((ls[0]["cost_usd"], ls[1]["cost_usd"], ls[1]["cost_source"]), (1.0, 0.0, "gateway-share"))
        self.assertEqual((ls[2]["cost_usd"], ls[2]["cost_source"]), (None, ""))

    def test_an_episode_whose_bill_was_not_measured_has_null_cost_and_an_empty_source(self):
        ls = self.lines((10, 10), (5, 5))
        callrec_pi.apportion(ls, None)
        self.assertEqual([(c["cost_usd"], c["cost_source"]) for c in ls], [(None, ""), (None, "")])

    def test_an_episode_with_no_tokens_to_share_it_by_has_null_cost(self):
        ls = self.lines((0, 0), (0, 0))
        callrec_pi.apportion(ls, 0.5)
        self.assertEqual([(c["cost_usd"], c["cost_source"]) for c in ls], [(None, ""), (None, "")])
        callrec_pi.apportion([], 0.5)   # no lines: nothing to do


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

    def test_each_episode_s_lines_share_that_episode_s_gateway_bill(self):
        meter = FakeMeter(0.10, 0.30, None)
        r = self.rec()
        r.meter = meter
        self.session("a.jsonl", [msg(usage=usage(100, 0, 9)), msg(usage=usage(100, 100, 9))])
        r.record("verdict", "w")
        time.sleep(0.01)
        self.session("b.jsonl", [msg(usage=usage(10, 0, 9))])
        r.record("coder", "w")
        self.session("b.jsonl", [msg(usage=usage(10, 0, 9))], header=False)
        r.record("reviewer", "w")
        got = self.calls(self.path)
        self.assertEqual([(c["role"], c["cost_source"]) for c in got],
                         [("verdict", "gateway-share"), ("verdict", "gateway-share"), ("coder", "gateway-share"), ("reviewer", "")])
        self.assertAlmostEqual(got[0]["cost_usd"] + got[1]["cost_usd"], 0.10, places=12)
        self.assertAlmostEqual(got[0]["cost_usd"], 0.10 / 3, places=12)
        self.assertAlmostEqual(got[2]["cost_usd"], 0.30, places=12, msg="one turn takes its episode's whole bill")
        self.assertIsNone(got[3]["cost_usd"], "an episode that could not be measured has no cost, not pi's 9")
        self.assertEqual(meter.asked, 3)
        self.assertTrue(r.measures)

    def test_without_a_meter_the_cost_is_null_never_pi_s(self):
        r = self.rec()
        self.session("a.jsonl", [msg(usage=usage(1, 1, 7.5))])
        r.record("coder", "w")
        (c,) = self.calls(self.path)
        self.assertEqual((c["cost_usd"], c["cost_source"]), (None, ""))
        self.assertFalse(r.measures)

    def test_a_meter_that_fails_costs_the_lines_their_cost_not_the_run(self):
        r = self.rec()
        r.meter = FakeMeter(RuntimeError("boom"))
        self.session("a.jsonl", [msg(usage=usage(1, 1, 7.5))])
        r.record("coder", "w")
        (c,) = self.calls(self.path)
        self.assertEqual((c["cost_usd"], c["cost_source"]), (None, ""))

    def test_a_sigterm_during_the_wait_still_writes_the_lines(self):
        class Terminated(BaseException):
            pass
        r = self.rec()
        r.meter = FakeMeter(Terminated())
        self.session("a.jsonl", [msg(usage=usage(1, 1, 7.5))])
        with self.assertRaises(Terminated):
            r.record("coder", "w")
        (c,) = self.calls(self.path)
        self.assertEqual((c["role"], c["cost_usd"]), ("coder", None))

    def test_a_step_is_a_line_of_kind_step_with_no_model_tokens_or_cost(self):
        r = self.rec()
        r.step("gates", "round 1: out of scope: the diff touches package.json")
        r.step("advisor", "pin: messages/en-US.json\nnext")
        r.step("harness", "x" * 900)
        a, b, c = self.calls(self.path)
        self.assertEqual((a["kind"], a["role"], a["detail"], a["episode_seq"], a["decision_key"]),
                         ("step", "gates", "round 1: out of scope: the diff touches package.json", 1, "case-1"))
        self.assertEqual((b["episode_seq"], b["detail"]), (2, "pin: messages/en-US.json next"))
        self.assertEqual((c["episode_seq"], len(c["detail"])), (3, callrec_pi.STEP_MAX))
        for s in (a, b, c):
            self.assertEqual((s["alias"], s["model"], s["tokens_in"], s["tokens_out"], s["cost_usd"], s["cost_source"], s["finish_reason"], s["tools"]),
                             ("", "", None, None, None, "", "", []))
        callrec_pi.Recorder(self.dir, {}, "k").step("gates", "no file")   # no variable: nothing written

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
