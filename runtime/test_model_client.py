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
        "gateway": {"base_url": "http://litellm:4000/v1/", "key_env": "OPENROUTER_API_KEY", "kind": "litellm",
                    "defaults": {"tools": "native", "json": "object", "cost": "reported"}},
        "plain": {"base_url": "http://vllm:8000/v1", "defaults": {"tools": "json", "json": "none", "cost": "table"}},
    },
    "aliases": {
        "triage-default": {"endpoint": "gateway", "model": "fleet-triage-pro"},
        "local-model": {"endpoint": "gateway", "model": "NVFP4/Qwen3", "tools": "none", "cost": "zero"},
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


def ok(content='{"action":"decide"}', usage=None, headers=None, tool_calls=None, model="fleet-triage-pro", finish_reason=None):
    msg = {"content": content}
    if tool_calls:
        msg["tool_calls"] = tool_calls
    choice = {"message": msg}
    if finish_reason:
        choice["finish_reason"] = finish_reason
    return 200, headers or {}, json.dumps({"choices": [choice], "model": model,
                                           "usage": usage if usage is not None else {"prompt_tokens": 10, "completion_tokens": 5}})


def client(*answers, tracer=None, environ=None):
    t = Transport(*answers)
    env = {"OPENROUTER_API_KEY": "sk-test"} if environ is None else environ
    return mc.Client(mc.load(CONFIG), transport=t, environ=env, clock=iter(range(100)).__next__, tracer=tracer), t


class Load(unittest.TestCase):
    def test_alias_overrides_endpoint_defaults(self):
        a = mc.load(CONFIG)
        self.assertEqual((a["triage-default"].tools, a["triage-default"].json_mode, a["triage-default"].cost),
                         ("native", "object", "reported"))
        self.assertEqual((a["local-model"].tools, a["local-model"].cost), ("none", "zero"))
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


class SharedFixtures(unittest.TestCase):
    """runtime/testdata/models-*.json: the rules flotillad's internal/models applies too (Go tests read them)."""

    def test_valid(self):
        with open(os.path.join(HERE, "testdata", "models-valid.json")) as f:
            a = mc.load(json.load(f))
        self.assertEqual(sorted(a), ["local-model", "plain-model", "triage-default"])

    def test_invalid(self):
        with open(os.path.join(HERE, "testdata", "models-invalid.json")) as f:
            cases = json.load(f)
        self.assertGreaterEqual(len(cases), 10)
        for name, cfg in cases.items():
            with self.subTest(name), self.assertRaises(mc.ConfigError):
                mc.load(cfg)


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
        c.chat("triage-default", self.MESSAGES, trace_id="abc123", generation_name="triage-turn-3",
               max_tokens=self.triage.REPLY_MAX_TOKENS)
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
        mc.Client(aliases, transport=tr, tracer=None).chat("a", self.MESSAGES, trace_id="abc123", generation_name="triage-turn-1",
                                                                  max_tokens=self.triage.REPLY_MAX_TOKENS)
        self.assertEqual(tr.sent[0]["data"], t["data"])


class ShadowTags(unittest.TestCase):
    """A shadow run (FLOTILLA_SHADOW=1) tags its generations in Langfuse; a live run's request is the bytes it always was."""

    MESSAGES = [{"role": "user", "content": "x"}]

    def sent(self, environ, **kw):
        c, tr = client(ok(), environ=dict({"OPENROUTER_API_KEY": "sk-test"}, **environ))
        c.chat("triage-default", self.MESSAGES, **kw)
        return json.loads(tr.sent[0]["data"])

    def test_a_shadow_run_tags_its_generation_in_the_trace(self):
        body = self.sent({"FLOTILLA_SHADOW": "1", "FLOTILLA_ROUTINE_VERSION": "2"}, trace_id="abc", generation_name="turn-1")
        self.assertEqual(body["metadata"], {"existing_trace_id": "abc", "generation_name": "turn-1",
                                            "tags": ["shadow", "routine_version:2"]})

    def test_a_shadow_run_is_tagged_even_without_a_trace(self):
        body = self.sent({"FLOTILLA_SHADOW": "1", "FLOTILLA_ROUTINE_VERSION": "3"}, generation_name="g")
        self.assertEqual(body["metadata"], {"tags": ["shadow", "routine_version:3"], "generation_name": "g"})
        body = self.sent({"FLOTILLA_SHADOW": "1"})
        self.assertEqual(body["metadata"], {"tags": ["shadow"]})

    def test_a_live_run_sends_what_it_always_did(self):
        live = self.sent({"FLOTILLA_ROUTINE_VERSION": "2"}, trace_id="abc", generation_name="turn-1")
        self.assertEqual(live["metadata"], {"existing_trace_id": "abc", "generation_name": "turn-1"})
        self.assertNotIn("metadata", self.sent({"FLOTILLA_ROUTINE_VERSION": "2"}))
        self.assertNotIn("tags", json.dumps(live))

    def test_only_one_means_shadow(self):
        for v in ("0", "true", "", "yes"):
            self.assertNotIn("metadata", self.sent({"FLOTILLA_SHADOW": v}), v)

    def test_a_direct_endpoint_gets_no_metadata_either_way(self):
        aliases = mc.load({"endpoints": {"or": {"base_url": "https://openrouter.ai/api/v1", "kind": "openai",
                                                "defaults": {"json": "object", "cost": "reported", "tools": "json"}}},
                           "aliases": {"a": {"endpoint": "or", "model": "m"}}})
        tr = Transport(ok())
        mc.Client(aliases, transport=tr, tracer=None, environ={"FLOTILLA_SHADOW": "1"}).chat("a", self.MESSAGES, trace_id="abc")
        self.assertNotIn("metadata", json.loads(tr.sent[0]["data"]))


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

    def test_calls_file_has_one_line_per_call(self):
        """FLOTILLA_CALLS_FILE (the worker's) gets each call: model, tokens, cost; a failed call too; no key."""
        import tempfile
        path = os.path.join(tempfile.mkdtemp(), "calls.jsonl")
        env = {"OPENROUTER_API_KEY": "sk-test", "FLOTILLA_CALLS_FILE": path}
        c, _ = client(ok(usage={"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.0012}, headers={"x-litellm-model-id": "dep-1"}),
                      (500, {}, "upstream down"), environ=env)
        c.chat("triage-default", [])
        with self.assertRaises(mc.Transient):
            c.chat("triage-default", [])
        with open(path) as f:
            lines = [json.loads(l) for l in f]
        self.assertEqual(len(lines), 2)
        self.assertEqual({k: lines[0][k] for k in ("alias", "model", "deployment", "tokens_in", "tokens_out", "cost_usd", "cost_source", "error")},
                         {"alias": "triage-default", "model": "fleet-triage-pro", "deployment": "dep-1", "tokens_in": 10, "tokens_out": 5,
                          "cost_usd": 0.0012, "cost_source": "reported:body", "error": None})
        self.assertEqual((lines[1]["error"], lines[1]["status"], lines[1]["tokens_in"]), ("Transient", 500, None))
        with open(path) as f:
            self.assertNotIn("sk-test", f.read())

    def test_calls_file_has_finish_reason_step_and_decision_key(self):
        """Why the model stopped, which turn of a harness step it was and what it served are in the line; none of
        them is sent to the endpoint. A call that gave none writes "" and null."""
        path = os.path.join(tempfile.mkdtemp(), "calls.jsonl")
        env = {"OPENROUTER_API_KEY": "sk-test", "FLOTILLA_CALLS_FILE": path}
        c, tr = client(ok(finish_reason="length"), ok(finish_reason="stop"), ok(), (500, {}, "down"), environ=env)
        c.chat("triage-default", [], step=2, decision_key="fp-1")
        c.chat_policy({"model": "triage-default"}, [], step=3)
        c.chat("triage-default", [])
        with self.assertRaises(mc.Transient):
            c.chat("triage-default", [], step=1, decision_key="fp-2")
        with open(path) as f:
            lines = [json.loads(l) for l in f]
        self.assertEqual([(l["finish_reason"], l["step"], l["decision_key"]) for l in lines],
                         [("length", 2, "fp-1"), ("stop", 3, None), ("", None, None), ("", 1, "fp-2")])
        for sent in tr.sent:
            self.assertNotIn(b"decision_key", sent["data"])
            self.assertNotIn(b'"step"', sent["data"])

    def test_calls_file_has_role_episode_and_tools(self):
        """Who called, in which session and in what order, and which tools the reply asked for (a list, or a
        function of the call's record) are in the line; a call that gave none writes "", "", null, []; a
        function that raises costs the line its tools, not the call."""
        path = os.path.join(tempfile.mkdtemp(), "calls.jsonl")
        env = {"OPENROUTER_API_KEY": "sk-test", "FLOTILLA_CALLS_FILE": path}
        c, tr = client(ok(), ok('{"action":"probe"}'), ok(), (500, {}, "down"), ok(), environ=env)
        c.chat("triage-default", [], role="coder", episode="ep-1", episode_seq=2, tool_names=["bash", "read"])
        c.chat("triage-default", [], role="triage", episode="fp-1", episode_seq=1,
               tool_names=lambda rec: ["probe:" + json.loads(rec["content"])["action"]])
        c.chat("triage-default", [])
        with self.assertRaises(mc.Transient):
            c.chat("triage-default", [], role="advisor", tool_names=lambda rec: [rec["content"]])
        c.chat("triage-default", [], tool_names=lambda rec: 1 / 0)
        with open(path) as f:
            lines = [json.loads(l) for l in f]
        self.assertEqual([(l["role"], l["episode"], l["episode_seq"], l["tools"]) for l in lines],
                         [("coder", "ep-1", 2, [{"name": "bash", "arg": ""}, {"name": "read", "arg": ""}]),
                          ("triage", "fp-1", 1, [{"name": "probe:probe", "arg": ""}]),
                          ("", "", None, []), ("advisor", "", None, []), ("", "", None, [])])
        for sent in tr.sent:
            for k in (b"role", b"episode", b"tools"):
                self.assertNotIn(k, sent["data"])

    def test_tool_entries_carry_a_clean_argument_and_the_result(self):
        """A tool given as a dict keeps its argument (redacted, cut at 160) and its ok; a nameless one is dropped."""
        path = os.path.join(tempfile.mkdtemp(), "calls.jsonl")
        env = {"OPENROUTER_API_KEY": "sk-test", "FLOTILLA_CALLS_FILE": path}
        c, _ = client(ok(), environ=env)
        c.chat("triage-default", [], tool_names=[{"name": "bash", "arg": "fetch -H 'Authorization: Bearer abcdefghij1234567890' x", "ok": False},
                                                 {"name": "read", "arg": "src/" + "a" * 300, "ok": True}, {"arg": "no name"}, "ls"])
        with open(path) as f:
            tools = json.loads(f.readline())["tools"]
        self.assertEqual([t["name"] for t in tools], ["bash", "read", "ls"])
        self.assertNotIn("abcdefghij", tools[0]["arg"])
        self.assertIs(tools[0]["ok"], False)
        self.assertEqual(len(tools[1]["arg"]), 160)
        self.assertNotIn("ok", tools[2])

    def test_calls_lines_are_golden(self):
        """The calls file's lines, byte for byte, for the three shapes a run writes today (a plain call, a harness turn
        with role, episode and tools, a failed call): runtime/testdata/calls_lines.golden.jsonl, captured before the
        stations work. A line of a run that declares no station must stay exactly this (v2-design.md section 7, steps 3
        and 4: a `station` and `texts` field appear only with stations). UPDATE_GOLDEN=1 rewrites it."""
        path = os.path.join(tempfile.mkdtemp(), "calls.jsonl")
        env = {"OPENROUTER_API_KEY": "sk-test", "FLOTILLA_CALLS_FILE": path}
        c, _ = client(ok(usage={"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.0012}, headers={"x-litellm-model-id": "dep-1"},
                         finish_reason="stop"),
                      ok('{"action":"probe"}', finish_reason="tool_calls"), (500, {}, "upstream down"), environ=env)
        with mock.patch.object(mc.time, "strftime", return_value="2026-01-01T00:00:00Z"):
            c.chat("triage-default", [], step=1, decision_key="fp-1")
            c.chat("triage-default", [], role="coder", episode="ep-1", episode_seq=2,
                   tool_names=[{"name": "bash", "arg": "ls -la", "ok": True}, "read"])
            with self.assertRaises(mc.Transient):
                c.chat("triage-default", [])
        with open(path) as f:
            got = f.read()
        golden = os.path.join(HERE, "testdata", "calls_lines.golden.jsonl")
        if os.environ.get("UPDATE_GOLDEN"):
            with open(golden, "w") as f:
                f.write(got)
        with open(golden) as f:
            self.assertEqual(got, f.read())

    def test_no_calls_file_without_the_variable(self):
        c, _ = client(ok())
        c.chat("triage-default", [])   # nothing to assert but that it does not fail: no variable, no file

    def test_zero_table_unknown(self):
        c, _ = client(ok(), ok(), ok())
        self.assertEqual(c.chat("local-model", [])["cost_usd"], 0.0)
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
            c.chat("local-model", [], tools=tools)

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


class Span:
    def __init__(self, name, attributes):
        self.name, self.attrs, self.exc, self.status = name, dict(attributes), None, None

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def set_attribute(self, k, v):
        self.attrs[k] = v

    def record_exception(self, e):
        self.exc = e

    def set_status(self, s):
        self.status = s


class Tracer:
    def __init__(self):
        self.spans = []

    def start_as_current_span(self, name, attributes=None):
        s = Span(name, attributes or {})
        self.spans.append(s)
        return s


class Production(unittest.TestCase):
    """The gaps closed after the first version (operator, 2026-11-09: production-ready, not a sketch)."""

    def test_key_from_env_or_file_and_typed_errors(self):
        c, tr = client(ok(), environ={})
        with self.assertRaises(mc.ConfigError) as cm:
            c.chat("triage-default", [])
        self.assertIn("OPENROUTER_API_KEY is not set", str(cm.exception))
        self.assertEqual(tr.sent, [], "nothing sent without the key")
        d = tempfile.mkdtemp()
        cfg = {"endpoints": {"e": {"base_url": "http://x/v1", "key_file": os.path.join(d, "k")}},
               "aliases": {"a": {"endpoint": "e", "model": "m"}}}
        tr = Transport(ok())
        c = mc.Client(mc.load(cfg), transport=tr, tracer=None)
        with self.assertRaises(mc.ConfigError):
            c.chat("a", [])                                     # the file is missing
        with open(os.path.join(d, "k"), "w") as f:
            f.write("sk-file\n")
        c.chat("a", [])
        self.assertEqual(tr.sent[0]["headers"]["Authorization"], "Bearer sk-file")
        with self.assertRaises(mc.ConfigError):
            mc.load({"endpoints": {"e": {"base_url": "u", "key_file": "f", "key_env": "V"}},
                     "aliases": {"a": {"endpoint": "e", "model": "m"}}})

    def test_unknown_alias(self):
        c, _ = client()
        with self.assertRaises(mc.ConfigError) as cm:
            c.chat("nope", [])
        self.assertIn("configured: local-model, no-cost, plain-model, triage-default", str(cm.exception))

    def test_generation_params_only_when_set(self):
        c, tr = client(ok(), ok())
        c.chat("triage-default", [])
        self.assertEqual(list(json.loads(tr.sent[0]["data"])), ["model", "messages", "response_format", "temperature"])
        c.chat("triage-default", [], max_tokens=800, seed=7, stop=["\n\n"])
        b = json.loads(tr.sent[1]["data"])
        self.assertEqual((b["max_tokens"], b["seed"], b["stop"]), (800, 7, ["\n\n"]))

    def test_usage_missing_is_flagged_and_never_priced(self):
        c, _ = client(ok(usage={}), ok(usage={}))
        r = c.chat("plain-model", [])
        self.assertFalse(r["usage_reported"])
        self.assertEqual((r["cost_usd"], r["cost_source"]), (None, "unknown"), "no tokens: no table price, not zero")
        self.assertTrue(c.chat("triage-default", []) is not None)

    def test_fallback_only_as_declared(self):
        c, _ = client((503, {}, "busy"), ok())
        r = c.chat_policy({"model": "triage-default", "fallback": ["plain-model"]}, [])
        self.assertTrue(r["fallback"])
        self.assertEqual((r["alias"], r["attempts"][0]["alias"], r["attempts"][0]["error"]),
                         ("plain-model", "triage-default", "Transient"))
        c, tr = client((401, {}, "no"), ok())
        with self.assertRaises(mc.AuthOrBilling) as cm:
            c.chat_policy({"model": "triage-default", "fallback": ["plain-model"]}, [])
        self.assertEqual(len(tr.sent), 1, "a rejected key is not a reason to fall back unless the policy says auth")
        self.assertEqual(cm.exception.record["attempts"][0]["error"], "AuthOrBilling")
        c, _ = client(ok())
        r = c.chat_policy({"model": "triage-default"}, [])
        self.assertEqual((r["fallback"], r["attempts"]), (False, []))
        with self.assertRaises(mc.ConfigError):
            client()[0].chat_policy({"model": "triage-default", "fallback_on": ["always"]}, [])

    def test_last_fallback_failure_raises(self):
        c, _ = client((503, {}, "busy"), (503, {}, "busy"))
        with self.assertRaises(mc.Transient) as cm:
            c.chat_policy({"model": "triage-default", "fallback": ["plain-model"]}, [])
        self.assertEqual([a["alias"] for a in cm.exception.record["attempts"]], ["triage-default", "plain-model"])

    def test_span_per_call(self):
        tracer = Tracer()
        c, _ = client(ok(usage={"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.01},
                         headers={"x-litellm-model-id": "dep-1"}), (503, {}, "busy"), tracer=tracer)
        c.chat("triage-default", [])
        with self.assertRaises(mc.Transient):
            c.chat("triage-default", [])
        ok_span, bad = tracer.spans
        self.assertEqual(ok_span.name, "model.chat")
        self.assertEqual((ok_span.attrs["gen_ai.request.model"], ok_span.attrs["gen_ai.usage.input_tokens"],
                          ok_span.attrs["flotilla.model.deployment_id"], ok_span.attrs["flotilla.model.cost_usd"]),
                         ("fleet-triage-pro", 10, "dep-1", 0.01))
        self.assertIsNone(ok_span.exc)
        self.assertEqual((bad.attrs["error.type"], bad.attrs["http.response.status_code"]), ("Transient", 503))
        self.assertIsInstance(bad.exc, mc.Transient)
        self.assertNotIn("sk-test", json.dumps(ok_span.attrs, default=str))

    def test_from_env(self):
        c = mc.from_env({"FLOTILLA_MODELS": json.dumps(CONFIG), "OPENROUTER_API_KEY": "sk-test"}, tracer=None)
        self.assertIn("triage-default", c.aliases)
        for env in ({}, {"FLOTILLA_MODELS": "{nope"}):
            with self.assertRaises(mc.ConfigError):
                mc.from_env(env)

    def test_oversized_response_and_proxy_env(self):
        # the real transport against a local server: a response over the cap is refused, not read into memory
        import http.server
        import threading
        big = b"x" * (mc.MAX_RESPONSE_BYTES + 10)

        class H(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                self.send_response(200)
                self.send_header("Content-Length", str(len(big)))
                self.end_headers()
                self.wfile.write(big)

            def log_message(self, *a):
                pass
        srv = http.server.HTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            cfg = {"endpoints": {"e": {"base_url": f"http://127.0.0.1:{srv.server_port}/v1"}},
                   "aliases": {"a": {"endpoint": "e", "model": "m"}}}
            with mock.patch.dict(os.environ, {"NO_PROXY": "127.0.0.1"}):
                with self.assertRaises(mc.Transient) as cm:
                    mc.Client(mc.load(cfg), tracer=None).chat("a", [])
            self.assertIn("over", cm.exception.record["error"])
        finally:
            srv.shutdown()


if __name__ == "__main__":
    unittest.main(verbosity=1)
