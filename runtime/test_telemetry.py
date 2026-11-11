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


@unittest.skipUnless(SDK, "no OpenTelemetry SDK here; deploy/e2e/run.sh runs this in the worker image")
class WithSDK(unittest.TestCase):
    def test_model_call_span_under_the_run(self):
        exp = InMemorySpanExporter()
        env = {"FLOTILLA_ROUTINE": "platform/heartbeat", "FLOTILLA_ENVIRONMENT": "e2e", "FLOTILLA_RUN_ID": "r1",
               "TRACEPARENT": f"00-{TRACE_ID}-{PARENT}-01"}
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


if __name__ == "__main__":
    unittest.main(verbosity=2)
