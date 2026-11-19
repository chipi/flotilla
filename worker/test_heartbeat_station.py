"""python3 worker/test_heartbeat_station.py: the heartbeat of a version that declares the probe station (fleets/platform/heartbeat/v2).

v1's model check (worker/heartbeat.py, an alias named by an environment variable) and v2's (worker/heartbeat_station.py, the alias, text and
limits of the probe station) send the same request: the bytes of the body and the call's timeout are compared. The environment is what
flotillad hands a run of the shipped v2: internal/sched/testdata/runenv_shipped.json, which a Go test pins to the loader.
"""
import contextlib
import io
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(ROOT, "runtime"))
import heartbeat  # noqa: E402
import heartbeat_station  # noqa: E402
import model_client  # noqa: E402
import stations  # noqa: E402

MODELS = {"endpoints": {"gateway": {"base_url": "http://litellm:4000/v1", "kind": "litellm", "key_env": "K",
                                    "defaults": {"tools": "native", "json": "object", "cost": "reported"}}},
          "aliases": {"triage-flash": {"endpoint": "gateway", "model": "fleet-triage-flash"}}}
REPLY = json.dumps({"choices": [{"message": {"content": "{\"ok\": true}"}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 9, "completion_tokens": 3},
                    "model": "fleet-triage-flash"})


def v2_env():
    with open(os.path.join(ROOT, "internal", "sched", "testdata", "runenv_shipped.json")) as f:
        env = json.load(f)["platform/heartbeat@v2"]
    return {"FLOTILLA_STATIONS": env["FLOTILLA_STATIONS"], "FLOTILLA_MODELS": json.dumps(MODELS), "K": "secret", "FLOTILLA_CODE_ROOT": ROOT}


class Sent:
    """A transport that records what the client sends and answers one canned reply."""

    def __init__(self):
        self.calls = []

    def __call__(self, url, data, headers, timeout):
        self.calls.append({"url": url, "data": data.decode(), "timeout": timeout, "auth": "Authorization" in headers})
        return 200, {}, REPLY


def run(step, env, calls_file, *args):
    sent = Sent()
    real = model_client.from_env
    with mock.patch.dict(os.environ, dict(env, FLOTILLA_CALLS_FILE=calls_file), clear=False), \
            mock.patch.object(model_client, "from_env", lambda environ=None, **kw: real(environ, transport=sent, **kw)), \
            contextlib.redirect_stdout(io.StringIO()) as out:
        ok = step(*args)
    return ok, sent.calls, out.getvalue()


class ProbeStation(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.calls = os.path.join(self.tmp.name, "calls.jsonl")

    def test_v2_sends_the_request_v1_sends(self):
        env = v2_env()
        ok1, v1, _ = run(heartbeat.model_step, {k: env[k] for k in ("FLOTILLA_MODELS", "K")}, self.calls + ".v1", "triage-flash")
        ok2, v2, out = run(heartbeat_station.model_step, env, self.calls, "ignored")
        self.assertTrue(ok1 and ok2, out)
        self.assertEqual(len(v1), 1)
        self.assertEqual(v1, v2)
        body = json.loads(v2[0]["data"])
        self.assertEqual((body["model"], body["max_tokens"], v2[0]["timeout"]), ("fleet-triage-flash", 256, 30))
        self.assertEqual(body["messages"], [{"role": "user", "content": 'Reply with the JSON {"ok": true}.'}])

    def test_the_call_is_recorded_with_its_station_and_text(self):
        ok, _, out = run(heartbeat_station.model_step, v2_env(), self.calls, "x")
        self.assertTrue(ok, out)
        with open(self.calls) as f:
            line = json.loads(f.readline())
        self.assertEqual((line["station"], line["alias"]), ("probe", "triage-flash"))
        t = line["texts"][0]
        self.assertEqual((t["name"], t["text"]), ("task", 'Reply with the JSON {"ok": true}.'))

    def test_a_text_that_is_not_the_one_flotillad_hashed_fails_the_check_before_a_call(self):
        env = v2_env()
        doc = json.loads(env["FLOTILLA_STATIONS"])
        doc["stations"]["probe"]["texts"]["task"]["sha256"] = "0" * 64
        env["FLOTILLA_STATIONS"] = json.dumps(doc)
        ok, sent, out = run(heartbeat_station.model_step, env, self.calls, "x")
        self.assertFalse(ok)
        self.assertEqual(sent, [])
        self.assertIn("differs from the one flotillad checked", out)

    def test_without_stations_the_entry_point_says_so(self):
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("FLOTILLA_STATIONS", None)
            with contextlib.redirect_stdout(io.StringIO()) as out:
                self.assertEqual(heartbeat_station.main(), 2)
        self.assertIn("FLOTILLA_STATIONS is not set", out.getvalue())

    def test_a_limit_the_station_lacks_is_refused(self):
        env = v2_env()
        doc = json.loads(env["FLOTILLA_STATIONS"])
        del doc["stations"]["probe"]["limits"]["max_output_tokens"]
        with self.assertRaisesRegex(stations.StationsError, "max_output_tokens"):
            heartbeat_station.station_request({"FLOTILLA_STATIONS": json.dumps(doc), "FLOTILLA_CODE_ROOT": ROOT})


if __name__ == "__main__":
    unittest.main(verbosity=1)
