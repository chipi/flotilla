"""python3 ops/test_health.py: the health contract's reader (docs/wip/health-contract.md, flotilla.health/1): a body that is not the
contract is refused and a refused body is unknown, never up; a missing component or capability is unknown; the fetch keeps ranked-watch's
hardening (https only, whole-fetch deadline, size cap, no cross-host redirect, the URL never in a message) and reads a 503's body (H7)."""
import copy
import http.server
import json
import os
import sys
import threading
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import common  # noqa: E402
import health  # noqa: E402

NOW = "2026-11-28T10:00:00Z"


def comp(name, status="up", **extra):
    return dict({"name": name, "kind": "model", "status": status, "since": "2026-11-27T03:10:00Z", "reason": "", "latency_ms": None,
                 "checked_at": NOW}, **extra)


def body(**over):
    b = {"contract": "flotilla.health/1", "status": "down", "checked_at": NOW, "version": "sha-1234567",
         "components": [comp("model-prod", "down", reason="connection refused"), comp("asr"), comp("database")],
         "capabilities": {"scheduled-run": {"ready": False, "needs": ["model-prod", "asr", "database"], "blocked_by": ["model-prod"]}}}
    b.update(over)
    return b


class Refused(unittest.TestCase):
    def refused(self, b):
        with self.assertRaises(health.Refused):
            health.parse(b)

    def test_a_body_that_is_not_the_contract_is_refused(self):
        self.refused(body(contract="flotilla.health/2"))
        self.refused({k: v for k, v in body().items() if k != "contract"})
        self.refused(["not", "an", "object"])
        self.refused(body(status="fine"))

    def test_a_wrong_shape_is_refused(self):
        self.refused(body(components={"model-prod": "down"}))
        self.refused(body(components=[comp("asr", "sleeping")]))
        self.refused(body(components=[comp("Model Prod")]))
        self.refused(body(components=[comp("asr"), comp("asr")]))
        self.refused(body(components=["asr"]))
        self.refused(body(capabilities=[]))
        self.refused(body(capabilities={"scheduled-run": {"ready": "yes", "needs": [], "blocked_by": []}}))
        self.refused(body(capabilities={"scheduled-run": {"ready": True, "needs": "asr", "blocked_by": []}}))
        self.refused(body(capabilities={"scheduled-run": {"ready": True, "needs": ["asr"]}}))

    def test_ready_with_something_blocking_it_contradicts_itself(self):
        self.refused(body(capabilities={"scheduled-run": {"ready": True, "needs": ["asr"], "blocked_by": ["asr"]}}))

    def test_a_refused_body_is_never_read_as_up(self):
        def fetch(url):
            return {"contract": "other/1"}
        env = {"SOURCE_HEALTH_EXAMPLE": "https://health.example/ready"}
        with self.assertRaises(health.Unreadable):
            health.read("health_example", environ=env, fetch=fetch)
        self.assertIsNone(health.fact_body("health_example", environ=env, fetch=fetch))

    def test_text_that_is_not_json_is_refused(self):
        with self.assertRaises(health.Refused):
            health.parse_bytes(b"<html>ok</html>")
        self.assertEqual(health.parse_bytes(json.dumps(body()).encode())["contract"], health.CONTRACT)


class Accessors(unittest.TestCase):
    def setUp(self):
        self.b = health.parse(body())

    def test_a_components_status(self):
        self.assertEqual(health.component_status(self.b, "model-prod"), "down")
        self.assertEqual(health.component_status(self.b, "asr"), "up")

    def test_a_missing_component_is_unknown_and_so_is_one_that_says_so(self):
        self.assertEqual(health.component_status(self.b, "nope"), "unknown")
        b = health.parse(body(components=[comp("asr", "unknown", reason="check timed out")]))
        self.assertEqual(health.component_status(b, "asr"), "unknown")
        self.assertFalse(health.component_up(b, "asr"))
        self.assertFalse(health.component_up(b, "nope"))

    def test_busy_is_up(self):
        # H11: a single-flight server busy with a long job reports up with busy in its reason; the status decides, never the words
        b = health.parse(body(components=[comp("model-prod", "up", reason="busy: one job running")]))
        self.assertEqual(health.component_status(b, "model-prod"), "up")
        self.assertTrue(health.component_up(b, "model-prod"))

    def test_degraded_serves_but_is_not_up(self):
        b = health.parse(body(components=[comp("asr", "degraded", reason="fallback")]))
        self.assertEqual(health.component_status(b, "asr"), "degraded")
        self.assertFalse(health.component_up(b, "asr"))

    def test_capability_ready_is_true_false_or_none(self):
        self.assertIs(health.capability_ready(self.b, "scheduled-run"), False)
        self.assertEqual(health.blocked_by(self.b, "scheduled-run"), ["model-prod"])
        ok = health.parse(body(components=[comp("model-prod"), comp("asr"), comp("database")],
                               capabilities={"scheduled-run": {"ready": True, "needs": ["model-prod", "asr", "database"], "blocked_by": []}}))
        self.assertIs(health.capability_ready(ok, "scheduled-run"), True)
        self.assertIsNone(health.capability_ready(self.b, "nope"))
        self.assertEqual(health.blocked_by(self.b, "nope"), [])

    def test_ready_that_a_need_contradicts_is_not_trusted(self):
        caps = {"scheduled-run": {"ready": True, "needs": ["model-prod", "asr"], "blocked_by": []}}
        down = health.parse(body(capabilities=caps))                       # model-prod reads down in components
        self.assertIs(health.capability_ready(down, "scheduled-run"), False)
        gone = health.parse(body(components=[comp("asr")], capabilities=caps))   # model-prod is not in components at all
        self.assertIsNone(health.capability_ready(gone, "scheduled-run"))

    def test_since_is_the_producers(self):
        self.assertEqual(health.component_since(self.b, "model-prod"), "2026-11-27T03:10:00Z")
        self.assertIsNone(health.component_since(self.b, "nope"))


def serve(handler):
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


class Fetch(unittest.TestCase):
    def setUp(self):
        for k in ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "all_proxy"):
            os.environ.pop(k, None)
        os.environ["no_proxy"] = "127.0.0.1"

    def url(self, srv, path="/health/ready"):
        return f"http://127.0.0.1:{srv.server_port}{path}"

    def test_a_503_with_the_body_is_read_not_refused(self):
        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                raw = json.dumps(body()).encode()
                self.send_response(503)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *a):
                pass
        srv = serve(H)
        got = health.fetch(self.url(srv))
        srv.shutdown()
        self.assertEqual(got["contract"], health.CONTRACT)
        self.assertIs(health.capability_ready(got, "scheduled-run"), False)

    def test_a_503_without_the_contract_is_unreadable_and_names_no_url(self):
        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(503)
                self.send_header("Content-Length", "2")
                self.end_headers()
                self.wfile.write(b"no")

            def log_message(self, *a):
                pass
        srv = serve(H)
        url = self.url(srv, "/secret-token-path")
        with self.assertRaises(health.Unreadable) as cm:
            health.fetch(url)
        srv.shutdown()
        self.assertNotIn("secret-token-path", str(cm.exception))
        self.assertNotIn("127.0.0.1", str(cm.exception))

    def test_a_body_over_the_cap_is_refused(self):
        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", str(health.MAX_BYTES + 10))
                self.end_headers()
                try:
                    self.wfile.write(b" " * (health.MAX_BYTES + 10))
                except OSError:
                    pass

            def log_message(self, *a):
                pass
        srv = serve(H)
        with self.assertRaises(health.Unreadable):
            health.fetch(self.url(srv))
        srv.shutdown()

    def test_a_server_that_never_finishes_is_cut_at_the_deadline(self):
        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", "100000")
                self.end_headers()
                for _ in range(40):
                    try:
                        self.wfile.write(b" ")
                        self.wfile.flush()
                    except OSError:
                        return
                    time.sleep(0.1)

            def log_message(self, *a):
                pass
        srv = serve(H)
        t0 = time.monotonic()
        with self.assertRaises(health.Unreadable):
            health.fetch(self.url(srv), timeout=5, deadline=0.5)
        srv.shutdown()
        self.assertLess(time.monotonic() - t0, 3)

    def test_a_redirect_to_another_host_is_refused(self):
        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(302)
                self.send_header("Location", "http://localhost:1/elsewhere")
                self.end_headers()

            def log_message(self, *a):
                pass
        srv = serve(H)
        with self.assertRaises(health.Unreadable) as cm:
            health.fetch(self.url(srv))
        srv.shutdown()
        self.assertNotIn("elsewhere", str(cm.exception))

    def test_a_refused_connection_is_unreadable(self):
        with self.assertRaises(health.Unreadable):
            health.fetch("http://127.0.0.1:9/health/ready")


class Source(unittest.TestCase):
    def test_https_only_unless_the_host_is_named(self):
        env = {"SOURCE_HEALTH_EXAMPLE": "http://health.example/ready"}
        with self.assertRaises(common.ConfigError):
            health.source_url("health_example", env)
        self.assertEqual(health.source_url("health_example", dict(env, HEALTH_ALLOW_HTTP="health.example")), "http://health.example/ready")
        self.assertEqual(health.source_url("health_example", {"SOURCE_HEALTH_EXAMPLE": "https://health.example/ready"}),
                         "https://health.example/ready")

    def test_an_unset_or_odd_source_is_a_config_error(self):
        for name, env in (("health_example", {}), ("Health", {}), ("health_example", {"SOURCE_HEALTH_EXAMPLE": "ftp://x/y"})):
            with self.assertRaises(common.ConfigError):
                health.source_url(name, env)

    def test_the_url_is_never_in_a_read_error(self):
        env = {"SOURCE_HEALTH_EXAMPLE": "https://health.example/ready?token=abc"}

        def boom(url):
            raise RuntimeError(url)
        with self.assertRaises(health.Unreadable) as cm:
            health.read("health_example", environ=env, fetch=boom)
        self.assertNotIn("token=abc", str(cm.exception))


if __name__ == "__main__":
    unittest.main()
