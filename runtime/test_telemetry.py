"""python3 runtime/test_telemetry.py: the routine's tracing (runtime/telemetry.py).

Without the OpenTelemetry SDK (a laptop, CI) only the no-op paths run and the SDK tests are skipped,
saying so. With it (the worker image: deploy/e2e/run.sh runs this file there) they run in full: the
model client's span is exported, under the run's trace from TRACEPARENT.
"""
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import model_client as mc  # noqa: E402
import telemetry  # noqa: E402

try:
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    SDK = True
except ImportError:
    SDK = False

TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
PARENT = "00f067aa0ba902b7"


class NoOp(unittest.TestCase):
    def test_no_url_no_session(self):
        s = telemetry.setup({})
        self.assertFalse(s.active)
        self.assertIn("FLOTILLA_OTLP_TRACES_URL", s.why)
        s.shutdown()

    @unittest.skipIf(SDK, "the SDK is installed here")
    def test_without_the_sdk(self):
        s = telemetry.setup({"FLOTILLA_OTLP_TRACES_URL": "http://victoriatraces:10428/x"})
        self.assertFalse(s.active)
        self.assertIn("not installed", s.why)


class RunMarks(unittest.TestCase):
    """What a run's spans carry: its routine's version, and for a shadow run flotilla.shadow=true (nothing on a live run)."""

    def test_a_live_run_has_a_version_and_no_shadow_mark(self):
        self.assertEqual(telemetry.run_marks({"FLOTILLA_ROUTINE_VERSION": "2"}), {"flotilla.version": 2})
        self.assertEqual(telemetry.run_marks({}), {})

    def test_a_shadow_run_is_marked(self):
        self.assertEqual(telemetry.run_marks({"FLOTILLA_SHADOW": "1", "FLOTILLA_ROUTINE_VERSION": "3"}),
                         {"flotilla.version": 3, "flotilla.shadow": True})
        self.assertEqual(telemetry.run_marks({"FLOTILLA_SHADOW": "1"}), {"flotilla.shadow": True})

    def test_only_one_means_shadow(self):
        for v in ("0", "true", "", "no"):
            self.assertNotIn("flotilla.shadow", telemetry.run_marks({"FLOTILLA_SHADOW": v}), v)

    def test_a_bad_version_is_left_out(self):
        self.assertEqual(telemetry.run_marks({"FLOTILLA_ROUTINE_VERSION": "x2"}), {})

    def test_the_resource_of_a_shadow_run_carries_the_mark_and_a_live_runs_does_not(self):
        env = {"FLOTILLA_ROUTINE": "triage/cycle", "FLOTILLA_ENVIRONMENT": "e2e", "FLOTILLA_RUN_ID": "r1", "FLOTILLA_ROUTINE_VERSION": "2"}
        live = telemetry.resource_attrs(env)
        shadow = telemetry.resource_attrs(dict(env, FLOTILLA_SHADOW="1"))
        self.assertEqual(live["flotilla.version"], 2)
        self.assertNotIn("flotilla.shadow", live)
        self.assertEqual((shadow["flotilla.version"], shadow["flotilla.shadow"]), (2, True))
        self.assertEqual((shadow["service.name"], shadow["flotilla.run_id"], shadow["flotilla.routine"]), ("flotilla-triage", "r1", "triage/cycle"))
        self.assertEqual({k: v for k, v in shadow.items() if k != "flotilla.shadow"}, live, "nothing else differs")


@unittest.skipUnless(SDK, "no OpenTelemetry SDK here; deploy/e2e/run.sh runs this in the worker image")
class WithSDK(unittest.TestCase):
    def test_model_call_span_under_the_run(self):
        exp = InMemorySpanExporter()
        env = {"FLOTILLA_ROUTINE": "platform/heartbeat", "FLOTILLA_ENVIRONMENT": "e2e", "FLOTILLA_RUN_ID": "r1",
               "FLOTILLA_ROUTINE_VERSION": "2", "FLOTILLA_SHADOW": "1", "TRACEPARENT": f"00-{TRACE_ID}-{PARENT}-01"}
        s = telemetry.setup(env, exporter=exp)
        self.assertTrue(s.active)
        cfg = {"endpoints": {"e": {"base_url": "http://x/v1", "kind": "litellm"}},
               "aliases": {"a": {"endpoint": "e", "model": "m", "cost": "reported"}}}

        def transport(url, data, headers, timeout):
            return 200, {"x-litellm-model-id": "dep-9"}, json.dumps(
                {"choices": [{"message": {"content": "{}"}, "finish_reason": "stop"}], "model": "m",
                 "usage": {"prompt_tokens": 3, "completion_tokens": 1, "cost": 0.0001}})
        mc.Client(mc.load(cfg), transport=transport).chat("a", [])
        s.shutdown()
        spans = exp.get_finished_spans()
        self.assertEqual([sp.name for sp in spans], ["model.chat"])
        sp = spans[0]
        self.assertEqual(format(sp.context.trace_id, "032x"), TRACE_ID, "the run's trace")
        self.assertEqual(format(sp.parent.span_id, "016x"), PARENT, "the run's span is the parent")
        self.assertEqual(sp.attributes["flotilla.model.deployment_id"], "dep-9")
        self.assertEqual(sp.attributes["gen_ai.usage.input_tokens"], 3)
        self.assertEqual(sp.resource.attributes["service.name"], "flotilla-platform")
        self.assertEqual(sp.resource.attributes["deployment.environment"], "e2e")
        self.assertEqual(sp.resource.attributes["flotilla.version"], 2, "the routine's version is on the run's spans")
        self.assertIs(sp.resource.attributes["flotilla.shadow"], True, "a shadow run's spans carry the mark")


if __name__ == "__main__":
    unittest.main(verbosity=2)
