"""python3 runtime/test_contract_routine.py: platform/model-contract (G15) offline, with a stand-in endpoint."""
import contextlib
import io
import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import contract_routine as cr  # noqa: E402

MODELS = {"endpoints": {"gw": {"base_url": "http://gw/v1", "kind": "openai",
                               "defaults": {"tools": "native", "json": "object", "cost": "zero"}}},
          "aliases": {"good": {"endpoint": "gw", "model": "m-good"},
                      "notools": {"endpoint": "gw", "model": "m-notools"}}}


class Endpoint:
    """Answers like an OpenAI endpoint: JSON for a JSON-mode call, a tool call when tools are offered (but
    not for m-notools), max_tokens honoured, 404 for an unknown model."""

    def __init__(self):
        self.sent = []

    def __call__(self, url, data, headers, timeout):
        body = json.loads(data)
        self.sent.append(body)
        if body["model"] == "no-such-model-contract-check":
            return 404, {}, json.dumps({"error": {"message": "model not found"}})
        msg = {"content": '{"ok": true}'}
        if body.get("tools") and body["model"] != "m-notools":
            msg = {"content": None, "tool_calls": [{"id": "1", "type": "function",
                                                    "function": {"name": "get_metric", "arguments": '{"name": "pg_up"}'}}]}
        out = min(body.get("max_tokens") or 20, 5)
        return 200, {}, json.dumps({"choices": [{"message": msg, "finish_reason": "stop"}], "model": body["model"],
                                    "usage": {"prompt_tokens": 10, "completion_tokens": out}})


class Routine(unittest.TestCase):
    def run_it(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        calls, spend = os.path.join(d, "calls.jsonl"), os.path.join(d, "spend")
        env = {"FLOTILLA_MODELS": json.dumps(MODELS), "FLOTILLA_CALLS_FILE": calls}
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            rc = cr.main(["--spend-file", spend], environ=env, transport=Endpoint())
        with open(calls) as f:
            lines = [json.loads(x) for x in f]
        with open(spend) as f:
            spent = f.read()
        return rc, out.getvalue(), lines, spent

    def test_every_alias_every_check(self):
        rc, out, lines, spend = self.run_it()
        self.assertEqual(rc, 1, out)
        got = {(x["alias"], x["check"]): x["check_ok"] for x in lines}
        self.assertEqual(got, {("good", "json_object"): True, ("good", "tools"): True, ("good", "max_tokens"): True,
                               ("good", "unknown_model"): True, ("notools", "json_object"): True,
                               ("notools", "tools"): False, ("notools", "max_tokens"): True,
                               ("notools", "unknown_model"): True})
        bad = [x for x in lines if not x["check_ok"]][0]
        self.assertIn("no get_metric call", bad["detail"])
        self.assertIn("CONTRACT FAIL: notools tools", out)
        self.assertEqual(float(spend), 0.0)
        for x in lines:
            self.assertEqual(x["alias"], x["alias"].strip())
            self.assertNotIn(x["alias"], ("main", "plain", "bogus"), "a check line names the real alias")

    def test_checks_are_not_logged_twice(self):
        _, _, lines, _ = self.run_it()
        self.assertTrue(all("check" in x for x in lines), "the checks' own calls were also logged as ordinary calls")

    def test_no_models(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cr.main([], environ={}), 1)


if __name__ == "__main__":
    unittest.main()
