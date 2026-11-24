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
                      "notools": {"endpoint": "gw", "model": "m-notools"},
                      "reasons": {"endpoint": "gw", "model": "m-reasons"}}}


class Endpoint:
    """Answers like an OpenAI endpoint: JSON for a JSON-mode call, a tool call when tools are offered (but
    not for m-notools), max_tokens honoured, and a model list on GET /models (listing: None serves none, "extra" adds a name)."""

    def __init__(self, listing=("m-good", "m-notools", "m-reasons"), status=200):
        self.sent, self.gets, self.listing, self.status = [], [], listing, status

    def __call__(self, url, data, headers, timeout):
        if data is None:
            self.gets.append(url)
            if self.listing is None or self.status != 200:
                return self.status if self.listing is not None else 404, {}, "{}"
            return 200, {}, json.dumps({"object": "list", "data": [{"id": m, "object": "model"} for m in self.listing]})
        body = json.loads(data)
        self.sent.append(body)
        msg = {"content": '{"ok": true}'}
        if body.get("tools") and body["model"] != "m-notools":
            msg = {"content": None, "tool_calls": [{"id": "1", "type": "function",
                                                    "function": {"name": "get_metric", "arguments": '{"name": "pg_up"}'}}]}
        out = min(body.get("max_tokens") or 20, 5)
        if body["model"] == "m-reasons" and body.get("max_tokens") == 5:
            msg = {"content": ""}  # a reasoning model: five tokens of thinking, no content
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
                               ("good", "unknown_model_not_offered"): True, ("notools", "json_object"): True,
                               ("notools", "tools"): False, ("notools", "max_tokens"): True,
                               ("notools", "unknown_model_not_offered"): True, ("reasons", "json_object"): True, ("reasons", "tools"): True,
                               ("reasons", "max_tokens"): True, ("reasons", "unknown_model_not_offered"): True})
        reasoning = [x for x in lines if x["alias"] == "reasons" and x["check"] == "max_tokens"][0]
        self.assertIn("all reasoning", reasoning["detail"])
        bad = [x for x in lines if not x["check_ok"]][0]
        self.assertIn("no get_metric call", bad["detail"])
        self.assertIn("CONTRACT FAIL: notools tools", out)
        self.assertEqual(float(spend), 0.0)
        for x in lines:
            self.assertEqual(x["alias"], x["alias"].strip())
            self.assertNotIn(x["alias"], ("main", "plain"), "a check line names the real alias")

    def run_with(self, endpoint):
        out = io.StringIO()
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        calls = os.path.join(d, "calls.jsonl")
        with contextlib.redirect_stdout(out):
            cr.main([], environ={"FLOTILLA_MODELS": json.dumps(MODELS), "FLOTILLA_CALLS_FILE": calls}, transport=endpoint)
        with open(calls) as f:
            return [json.loads(x) for x in f if "unknown_model_not_offered" in x]

    def test_the_gateway_is_never_called_with_a_model_that_does_not_exist(self):
        ep = Endpoint()
        self.run_with(ep)
        self.assertNotIn("no-such-model-contract-check", [b["model"] for b in ep.sent])
        self.assertEqual(ep.gets, ["http://gw/v1/models"] * 3)

    def test_an_endpoint_without_a_list_is_not_checked_not_failed(self):
        lines = self.run_with(Endpoint(listing=None))
        self.assertEqual([x["check_ok"] for x in lines], [True] * 3)
        self.assertIn("not checked: the endpoint lists no models", lines[0]["detail"])

    def test_the_fake_name_offered_fails(self):
        lines = self.run_with(Endpoint(listing=("m-good", "m-notools", "m-reasons", "no-such-model-contract-check")))
        self.assertEqual([x["check_ok"] for x in lines], [False] * 3)
        self.assertIn("is offered", lines[0]["detail"])

    def test_the_aliases_own_model_missing_fails(self):
        lines = {x["alias"]: x for x in self.run_with(Endpoint(listing=("m-good",)))}
        self.assertTrue(lines["good"]["check_ok"])
        self.assertFalse(lines["notools"]["check_ok"])
        self.assertIn("'m-notools' is not offered", lines["notools"]["detail"])

    def test_a_refused_list_fails(self):
        lines = self.run_with(Endpoint(status=401))
        self.assertEqual([x["check_ok"] for x in lines], [False] * 3)
        self.assertIn("AuthOrBilling", lines[0]["detail"])

    def test_a_span_per_check_call(self):
        """2026-11-16: a model-contract run made 8 calls and sent 1 span (the run's): the routine built its
        client with tracing off. Each check's call is now a model.chat span under the run's trace."""
        from test_model_client import Tracer
        tracer = Tracer()
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        with contextlib.redirect_stdout(io.StringIO()):
            cr.main([], environ={"FLOTILLA_MODELS": json.dumps(MODELS)}, transport=Endpoint(), tracer=tracer)
        names = {sp.name for sp in tracer.spans}
        self.assertGreaterEqual(len(tracer.spans), 9, "one span per model call: 3 aliases x 3 checks (the model list is no chat)")
        self.assertEqual(names, {"model.chat"})

    def test_checks_are_not_logged_twice(self):
        _, _, lines, _ = self.run_it()
        self.assertTrue(all("check" in x for x in lines), "the checks' own calls were also logged as ordinary calls")

    def test_no_models(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cr.main([], environ={}), 1)


if __name__ == "__main__":
    unittest.main()
