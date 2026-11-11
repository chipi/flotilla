"""python3 runtime/test_harness.py — the harness's own checks; triage's loop on it is proven by
signal-fleet/mvp/eval_harness_replay.py (old against new)."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import harness  # noqa: E402

CFG = {"limits": {"max_tool_calls": 1, "max_steps": 2},
       "quirks": {"bad_json_uses_turn": True, "empty_retry": True, "final_decision_call": True,
                  "reply_echo_chars": 600, "decision_echo_chars": 400, "result_chars": 1800}}


class Down(Exception):
    pass


class Later(Exception):
    pass


ERR = {"fatal": (Down,), "defer": Later, "defer_prefix": "call failed: "}


def run(replies, cfg=CFG, marks=None):
    rs = list(replies)

    def call(messages, step):
        r = rs.pop(0)
        if isinstance(r, Exception):
            raise r
        return r, {"prompt_tokens": 1, "completion_tokens": 1}
    return harness.run_json_step([{"role": "user", "content": "x"}], call, lambda n, a: f"result of {n}", cfg, ERR,
                                 on_quirk=(marks.append if marks is not None else None), log=lambda s: None)


class Harness(unittest.TestCase):
    def test_probe_then_decide(self):
        r = run(['{"action":"probe","probe":"p","probe_args":{}}', '{"action":"decide","disposition":"file"}'])
        self.assertEqual((r.decision, r.in_loop, len(r.trace), r.usage["prompt_tokens"]), ({"disposition": "file"}, True, 1, 2))

    def test_errors(self):
        with self.assertRaises(Down):
            run([Down("401")])
        with self.assertRaisesRegex(Later, "^call failed: boom$"):
            run([RuntimeError("boom")])
        with self.assertRaisesRegex(Later, "^empty completion on the last turn$"):
            run(["", ""])

    def test_quirk_marks(self):
        marks = []
        run(["", "not json", '{"action":"decide","disposition":"file"}'][:2], marks=marks)
        self.assertEqual(marks, ["empty_retry", "bad_json_uses_turn"])

    def test_refuses_what_it_does_not_implement(self):
        for change, msg in (({"max_steps": 3}, "max_steps"), ):
            with self.subTest(msg), self.assertRaisesRegex(ValueError, msg):
                run([], cfg=dict(CFG, limits=dict(CFG["limits"], **change)))
        with self.assertRaisesRegex(ValueError, "only true"):
            run([], cfg=dict(CFG, quirks=dict(CFG["quirks"], empty_retry=False)))


if __name__ == "__main__":
    unittest.main(verbosity=1)
