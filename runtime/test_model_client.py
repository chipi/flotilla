"""python3 runtime/test_model_client.py: the model client (RFC-0002 §3), offline, with a stand-in transport.

The golden-request tests drive the triage fleet's own _call (signal-fleet/mvp/triage.py) and the client
with the same inputs, and require the same bytes on the wire: §9 step 2 swaps one for the other.
"""
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
MVP = os.path.join(HERE, "..", "signal-fleet", "mvp")
os.environ.setdefault("SF_OBSERV_DISABLED", "1")
os.environ.setdefault("HOME", tempfile.mkdtemp())

import model_client as mc  # noqa: E402

CONFIG = {
    "endpoints": {
        "gateway": {"base_url": "http://litellm:4000/v1/", "key_file": "/k", "kind": "litellm",
                    "defaults": {"tools": "native", "json": "object", "cost": "reported"}},
        "plain": {"base_url": "http://vllm:8000/v1", "defaults": {"tools": "json", "json": "none", "cost": "table"}},
    },
    "aliases": {
        "triage-default": {"endpoint": "gateway", "model": "fleet-triage-pro"},
        "local-dgx": {"endpoint": "gateway", "model": "NVFP4/Qwen3", "tools": "none", "cost": "zero"},
        "plain-model": {"endpoint": "plain", "model": "qwen"},
        "no-cost": {"endpoint": "gateway", "model": "m", "cost": "unknown"},
    },
    "prices": {"plain-model": {"input_per_mtok": 0.14, "output_per_mtok": 0.28}},
}


class Transport:
    def __init__(self, *answers):
        self.answers, self.sent = list(answers), []

    def __call__(self, url, data, headers, timeout):
        self.sent.append({"url": url, "data": data, "headers": headers, "timeout": timeout})
        a = self.answers.pop(0)
        if isinstance(a, Exception):
            raise a
        return a


def ok(content='{"action":"decide"}', usage=None, headers=None, tool_calls=None, model="fleet-triage-pro"):
    msg = {"content": content}
    if tool_calls:
        msg["tool_calls"] = tool_calls
    return 200, headers or {}, json.dumps({"choices": [{"message": msg}], "model": model,
                                           "usage": usage if usage is not None else {"prompt_tokens": 10, "completion_tokens": 5}})


def client(*answers):
    t = Transport(*answers)
    return mc.Client(mc.load(CONFIG), transport=t, read_key=lambda p: "sk-test" if p else "", clock=iter(range(100)).__next__), t


class Load(unittest.TestCase):
    def test_alias_overrides_endpoint_defaults(self):
        a = mc.load(CONFIG)
        self.assertEqual((a["triage-default"].tools, a["triage-default"].json_mode, a["triage-default"].cost),
                         ("native", "object", "reported"))
        self.assertEqual((a["local-dgx"].tools, a["local-dgx"].cost), ("none", "zero"))
        self.assertEqual(a["triage-default"].base_url, "http://litellm:4000/v1")

    def test_refuses_bad_config(self):
        for name, bad in {
            "unknown endpoint": {"endpoints": {}, "aliases": {"x": {"endpoint": "nope", "model": "m"}}},
            "bad capability": {"endpoints": CONFIG["endpoints"], "aliases": {"x": {"endpoint": "gateway", "model": "m", "tools": "maybe"}}},
            "table without a price": {"endpoints": CONFIG["endpoints"], "aliases": {"x": {"endpoint": "plain", "model": "m"}}},
            "no model": {"endpoints": CONFIG["endpoints"], "aliases": {"x": {"endpoint": "gateway"}}},
        }.items():
            with self.subTest(name), self.assertRaises(mc.ConfigError):
                mc.load(bad)


class Golden(unittest.TestCase):
    """The client's request is byte-identical to triage._call's for the same inputs."""

    @classmethod
    def setUpClass(cls):
        sys.path.insert(0, MVP)
        import config as sfconfig
        import triage
        cls.triage, cls.sfconfig = triage, sfconfig

    def triage_request(self, url, messages, trace_id, step):
        got = {}

        def post(u, body, headers=None, timeout=60):
            got["data"], got["url"], got["headers"] = json.dumps(body).encode(), u, headers
            return {"choices": [{"message": {"content": "{}"}}], "usage": {}}
        with mock.patch.object(self.triage, "post_json", post), \
                mock.patch.object(self.sfconfig, "OPENROUTER_URL", url), \
                mock.patch.object(self.sfconfig, "OPENROUTER_KEY", "sk-test"), \
                mock.patch.object(self.sfconfig, "TRIAGE_MODEL", "fleet-triage-pro"):
            self.triage._call(messages, trace_id=trace_id, step=step)
        return got

    MESSAGES = [{"role": "system", "content": "playbook ü"}, {"role": "user", "content": "SIGNAL:\n{\"a\": 1}"}]

    def test_through_the_gateway_with_a_trace(self):
        t = self.triage_request("http://litellm:4000/v1/chat/completions", self.MESSAGES, "abc123", 2)
        c, tr = client(ok())
        c.chat("triage-default", self.MESSAGES, trace_id="abc123", generation_name="triage-turn-3")
        self.assertEqual(tr.sent[0]["data"], t["data"], "same bytes on the wire")
        self.assertEqual(tr.sent[0]["url"], t["url"])
        self.assertEqual(tr.sent[0]["headers"]["Authorization"], t["headers"]["Authorization"])
        self.assertEqual(tr.sent[0]["timeout"], 90)

    def test_direct_without_metadata(self):
        # OpenRouter direct: triage sends no metadata (via_gateway() is false); a non-LiteLLM endpoint neither
        t = self.triage_request("https://openrouter.ai/api/v1/chat/completions", self.MESSAGES, "abc123", 0)
        aliases = mc.load({"endpoints": {"or": {"base_url": "https://openrouter.ai/api/v1", "kind": "openai",
                                                "defaults": {"json": "object", "cost": "reported", "tools": "json"}}},
                           "aliases": {"a": {"endpoint": "or", "model": "fleet-triage-pro"}}})
        tr = Transport(ok())
        mc.Client(aliases, transport=tr).chat("a", self.MESSAGES, trace_id="abc123", generation_name="triage-turn-1")
        self.assertEqual(tr.sent[0]["data"], t["data"])


class Chat(unittest.TestCase):
    def test_record_of_a_call(self):
        c, tr = client(ok(usage={"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.0012},
                          headers={"x-litellm-model-id": "dep-1", "x-litellm-response-cost": "0.9"}))
        r = c.chat("triage-default", [{"role": "user", "content": "hi"}])
        self.assertEqual(r["request"], tr.sent[0]["data"].decode(), "the request exactly as sent")
        self.assertEqual((r["content"], r["model"], r["model_id"], r["status"]), ('{"action":"decide"}', "fleet-triage-pro", "dep-1", 200))
        self.assertEqual((r["cost_usd"], r["cost_source"]), (0.0012, "reported:body"), "the body's cost first")
        self.assertNotIn("sk-test", json.dumps(r), "the key is never recorded")

    def test_cost_header_when_the_body_has_none(self):
        c, _ = client(ok(headers={"x-litellm-response-cost": "9e-07"}))
        self.assertEqual(c.chat("triage-default", [])["cost_usd"], 9e-07)

    def test_reported_but_absent_is_unknown_not_zero(self):
        c, _ = client(ok())
        r = c.chat("triage-default", [])
        self.assertEqual((r["cost_usd"], r["cost_source"]), (None, "unknown"))

    def test_zero_table_unknown(self):
        c, _ = client(ok(), ok(), ok())
        self.assertEqual(c.chat("local-dgx", [])["cost_usd"], 0.0)
        r = c.chat("plain-model", [])
        self.assertAlmostEqual(r["cost_usd"], (10 * 0.14 + 5 * 0.28) / 1e6)
        self.assertEqual(c.chat("no-cost", [])["cost_source"], "unknown")

    def test_typed_errors(self):
        cases = [((401, {}, "no"), mc.AuthOrBilling), ((402, {}, "pay"), mc.AuthOrBilling), ((403, {}, "no"), mc.AuthOrBilling),
                 ((429, {}, "slow"), mc.Transient), ((503, {}, "busy"), mc.Transient), ((400, {}, "bad"), mc.RequestRejected),
                 (TimeoutError("timed out"), mc.Transient), ((200, {}, "<html>"), mc.Transient),
                 (ok(content=""), mc.EmptyCompletion), (ok(content=None), mc.EmptyCompletion)]
        for answer, err in cases:
            with self.subTest(answer=str(answer)[:40]):
                c, _ = client(answer)
                with self.assertRaises(err) as cm:
                    c.chat("triage-default", [])
                self.assertIn("request", cm.exception.record)

    def test_tools(self):
        tools = [{"type": "function", "function": {"name": "metric", "parameters": {}}}]
        call = [{"id": "1", "type": "function", "function": {"name": "metric", "arguments": "{}"}}]
        c, tr = client(ok(content=None, tool_calls=call), ok())
        r = c.chat("triage-default", [], tools=tools)
        self.assertEqual(json.loads(tr.sent[0]["data"])["tools"], tools)
        self.assertEqual(r["tool_calls"], call, "a tool call with no content is an answer, not an empty completion")
        c.chat("plain-model", [], tools=tools)
        self.assertNotIn("tools", json.loads(tr.sent[1]["data"]), "json protocol: tools are the caller's prompt")
        with self.assertRaises(mc.ConfigError):
            c.chat("local-dgx", [], tools=tools)

    def test_json_modes(self):
        c, tr = client(ok(), ok())
        c.chat("plain-model", [])
        self.assertNotIn("response_format", json.loads(tr.sent[0]["data"]))
        c.chat("triage-default", [])
        self.assertEqual(json.loads(tr.sent[1]["data"])["response_format"], {"type": "json_object"})

    def test_reasoning_stripped(self):
        c, _ = client(ok(content="<think>hmm {not this}</think>\n{\"a\":1}"))
        r = c.chat("triage-default", [])
        self.assertEqual(r["content"], '{"a":1}')
        self.assertIn("<think>", r["reply"], "the raw reply is kept for replay")

    def test_no_key_file_no_auth_header(self):
        c, tr = client(ok())
        c.chat("plain-model", [])
        self.assertNotIn("Authorization", tr.sent[0]["headers"])


if __name__ == "__main__":
    unittest.main(verbosity=1)
