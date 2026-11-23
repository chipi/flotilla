"""python3 runtime/test_mcp_client.py: the MCP client (runtime/mcp_client.py) against an in-process streamable HTTP server: the session
(initialize, the id, the 202 notification, the protocol header), JSON and SSE answers (n8n's are SSE), a 404 that starts a new session,
JSON-RPC and HTTP errors, the body cap, the timeout, and the bearer token read from a file at each call."""
import json
import os
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import mcp_client  # noqa: E402


class Stub:
    """A server with the behaviour a test switches: `sse` (answer as an event stream), `version`, `token` (required bearer), `delay`,
    `lose_session` (404 once, as after a restart), `tools` (name -> function(args) -> result object), `seen` (what it was sent)."""

    def __init__(self):
        self.sse, self.version, self.token, self.delay, self.lose_session = False, "2025-06-18", None, 0, False
        self.sid_n, self.sid, self.seen, self.tools, self.junk_before = 0, None, [], {}, True
        self.raw = None                      # a function(handler, message) that answers by itself
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), self._handler())
        self.httpd.daemon_threads = True
        self.httpd.handle_error = lambda *a: None
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}/mcp"
        threading.Thread(target=self.httpd.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()

    def methods(self):
        return [m["method"] for _, m in self.seen]

    def _handler(self):
        stub = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def _send(self, code, body=b"", ctype=None, headers=()):
                self.send_response(code)
                if ctype:
                    self.send_header("Content-Type", ctype)
                for k, v in headers:
                    self.send_header(k, v)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                msg = json.loads(self.rfile.read(n))
                stub.seen.append(({k.lower(): v for k, v in self.headers.items()}, msg))
                if stub.delay:
                    time.sleep(stub.delay)
                if stub.token is not None and self.headers.get("Authorization") != "Bearer " + stub.token:
                    return self._send(401)
                method = msg.get("method")
                if stub.raw:
                    return stub.raw(self, msg)
                if method != "initialize" and stub.sid and self.headers.get("Mcp-Session-Id") != stub.sid:
                    return self._send(404)
                if method != "initialize" and stub.lose_session:
                    stub.lose_session, stub.sid = False, None
                    return self._send(404)
                if method == "notifications/initialized":
                    return self._send(202)
                if method == "initialize":
                    stub.sid_n += 1
                    stub.sid = f"sess-{stub.sid_n}"
                    result = {"protocolVersion": stub.version, "capabilities": {"tools": {}}, "serverInfo": {"name": "stub", "version": "1"}}
                    return self.answer(msg, {"result": result}, [("Mcp-Session-Id", stub.sid)])
                if method == "tools/list":
                    return self.answer(msg, {"result": {"tools": [{"name": n} for n in stub.tools]}})
                if method == "tools/call":
                    fn = stub.tools.get(msg["params"]["name"])
                    if fn is None:
                        return self.answer(msg, {"error": {"code": -32602, "message": "unknown tool"}})
                    return self.answer(msg, {"result": fn(msg["params"]["arguments"])})
                return self.answer(msg, {"error": {"code": -32601, "message": "no such method"}})

            def answer(self, msg, body, headers=()):
                out = dict(jsonrpc="2.0", id=msg["id"], **body)
                if not stub.sse:
                    return self._send(200, json.dumps(out).encode(), "application/json", headers)
                events = ""
                if stub.junk_before:
                    events += ": a comment\nevent: message\ndata: " + json.dumps({"jsonrpc": "2.0", "method": "notifications/message", "params": {}}) + "\n\n"
                    events += "event: message\nid: 7\ndata: " + json.dumps({"jsonrpc": "2.0", "id": 9999, "result": {"other": True}}) + "\n\n"
                events += "event: message\nid: 8\r\ndata: " + json.dumps(out) + "\r\n\r\n"
                self._send(200, events.encode(), "text/event-stream", headers)

        return H


def token_file(text):
    d = tempfile.mkdtemp()
    p = os.path.join(d, "token")
    with open(p, "w") as f:
        f.write(text)
    return p


class Base(unittest.TestCase):
    def setUp(self):
        for k in ("HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"):
            os.environ.pop(k, None)
        os.environ["NO_PROXY"] = "127.0.0.1"
        self.stub = Stub()
        self.stub.tools = {"echo": lambda a: {"content": [{"type": "text", "text": "hi " + json.dumps(a)}], "isError": False}}
        self.addCleanup(self.stub.close)

    def client(self, **kw):
        return mcp_client.McpClient(self.stub.url, **kw)


class Session(Base):
    def test_initialize_then_the_initialized_notification_then_the_call_with_session_and_version(self):
        r = self.client().call_tool("echo", {"q": 1})
        self.assertEqual(mcp_client.text(r), 'hi {"q": 1}')
        self.assertEqual(self.stub.methods(), ["initialize", "notifications/initialized", "tools/call"])
        init, note, call = (h for h, _ in self.stub.seen)
        self.assertEqual(self.stub.seen[0][1]["params"]["protocolVersion"], "2025-06-18")
        self.assertNotIn("mcp-session-id", init)
        for h in (note, call):
            self.assertEqual(h.get("mcp-session-id"), "sess-1")
            self.assertEqual(h.get("mcp-protocol-version"), "2025-06-18")
        for h in (init, note, call):
            self.assertEqual(h["accept"], "application/json, text/event-stream")
        self.assertNotIn("id", self.stub.seen[1][1])        # a notification has no id

    def test_a_second_call_reuses_the_session(self):
        c = self.client()
        c.call_tool("echo", {})
        c.call_tool("echo", {})
        self.assertEqual(self.stub.methods().count("initialize"), 1)

    def test_an_older_accepted_version_is_used_afterwards(self):
        self.stub.version = "2025-03-26"
        c = self.client()
        c.call_tool("echo", {})
        self.assertEqual(self.stub.seen[-1][0].get("mcp-protocol-version"), "2025-03-26")

    def test_a_version_not_accepted_is_an_error(self):
        self.stub.version = "2024-11-05"
        with self.assertRaisesRegex(mcp_client.McpError, "speaks MCP '2024-11-05'"):
            self.client().call_tool("echo", {})

    def test_list_tools(self):
        self.assertEqual([t["name"] for t in self.client().list_tools()], ["echo"])

    def test_a_notification_answered_other_than_202_is_an_error(self):
        def raw(h, msg):
            if msg["method"] == "initialize":
                return h._send(200, json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"protocolVersion": "2025-06-18"}}).encode(), "application/json")
            return h._send(200)
        self.stub.raw = raw
        with self.assertRaisesRegex(mcp_client.McpError, "answered 200, not 202"):
            self.client().call_tool("echo", {})

    def test_a_failed_start_is_started_again_on_the_next_call(self):
        c = self.client()
        self.stub.raw = lambda h, msg: h._send(200) if msg["method"] == "notifications/initialized" else self.stub.raw_off(h, msg)
        self.stub.raw_off = lambda h, msg: h._send(200, json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"protocolVersion": "2025-06-18"}}).encode(), "application/json")
        with self.assertRaises(mcp_client.McpError):
            c.call_tool("echo", {})
        self.stub.raw, self.stub.seen = None, []
        self.assertEqual(mcp_client.text(c.call_tool("echo", {"a": 1})), 'hi {"a": 1}')
        self.assertEqual(self.stub.methods(), ["initialize", "notifications/initialized", "tools/call"])

    def test_a_server_without_a_session_id_works(self):
        def raw(h, msg):
            if msg["method"] == "notifications/initialized":
                return h._send(202)
            res = {"protocolVersion": "2025-06-18"} if msg["method"] == "initialize" else {"content": [{"type": "text", "text": "ok"}]}
            return h._send(200, json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": res}).encode(), "application/json")
        self.stub.raw = raw
        c = self.client()
        self.assertEqual(mcp_client.text(c.call_tool("echo", {})), "ok")
        c.call_tool("echo", {})
        self.assertEqual(self.stub.methods().count("initialize"), 1)
        self.assertNotIn("mcp-session-id", self.stub.seen[-1][0])


class Answers(Base):
    def test_an_sse_answer_with_a_notification_and_another_id_before_the_result(self):
        self.stub.sse = True
        r = self.client().call_tool("echo", {"q": "é"})
        self.assertEqual(mcp_client.text(r), 'hi {"q": "\\u00e9"}')
        self.assertEqual(self.stub.sid, "sess-1")

    def test_an_sse_answer_stops_at_the_matching_id_and_does_not_wait_for_the_stream_to_close(self):
        def raw(h, msg):
            if msg["method"] == "notifications/initialized":
                return h._send(202)
            res = {"protocolVersion": "2025-06-18"} if msg["method"] == "initialize" else {"content": [{"type": "text", "text": "done"}]}
            h.send_response(200)
            h.send_header("Content-Type", "text/event-stream; charset=utf-8")
            h.send_header("Mcp-Session-Id", "x")
            h.end_headers()
            h.wfile.write(b"data: " + json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": res}).encode() + b"\n\n")
            h.wfile.flush()
            time.sleep(2)                    # the stream stays open
            h.close_connection = True
        self.stub.raw = raw
        t0 = time.monotonic()
        self.assertEqual(mcp_client.text(self.client(timeout=10).call_tool("echo", {})), "done")
        self.assertLess(time.monotonic() - t0, 2.5)

    def test_an_sse_event_split_over_data_lines_is_joined(self):
        def raw(h, msg):
            if msg["method"] == "notifications/initialized":
                return h._send(202)
            res = {"protocolVersion": "2025-06-18"} if msg["method"] == "initialize" else {"content": [{"type": "text", "text": "multi"}]}
            doc = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": res}, indent=1).split("\n")
            h._send(200, ("".join("data: " + l + "\n" for l in doc) + "\n").encode(), "text/event-stream")
        self.stub.raw = raw
        self.assertEqual(mcp_client.text(self.client().call_tool("echo", {})), "multi")

    def test_a_stream_that_ends_without_the_answer_is_an_error(self):
        def raw(h, msg):
            if msg["method"] == "initialize":
                return h._send(200, b'data: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}\n\n', "text/event-stream")
            if msg["method"] == "notifications/initialized":
                return h._send(202)
            h._send(200, b"event: ping\n\n", "text/event-stream")
        self.stub.raw = raw
        with self.assertRaisesRegex(mcp_client.McpError, "ended without an answer"):
            self.client().call_tool("echo", {})

    def test_text_joins_blocks_and_falls_back_to_structured_content(self):
        self.assertEqual(mcp_client.text({"content": [{"type": "text", "text": "a"}, {"type": "image"}, {"type": "text", "text": "b"}]}), "a\nb")
        self.assertEqual(mcp_client.text({"content": [], "structuredContent": {"b": 1, "a": [2]}}), '{"a": [2], "b": 1}')

    def test_a_tool_error_is_a_result_with_iserror(self):
        self.stub.tools["bad"] = lambda a: {"content": [{"type": "text", "text": "no such page"}], "isError": True}
        r = self.client().call_tool("bad", {})
        self.assertTrue(r["isError"])
        self.assertEqual(mcp_client.text(r), "no such page")


class Failures(Base):
    def test_a_json_rpc_error(self):
        with self.assertRaisesRegex(mcp_client.McpError, r"tools/call: JSON-RPC error -32602: unknown tool"):
            self.client().call_tool("nope", {})

    def test_a_404_with_a_session_starts_a_new_one_once_and_repeats_the_call(self):
        c = self.client()
        c.call_tool("echo", {})
        self.stub.lose_session = True
        self.assertEqual(mcp_client.text(c.call_tool("echo", {"n": 2})), 'hi {"n": 2}')
        self.assertEqual(self.stub.methods(), ["initialize", "notifications/initialized", "tools/call", "tools/call",
                                               "initialize", "notifications/initialized", "tools/call"])
        self.assertEqual(self.stub.seen[-1][0].get("mcp-session-id"), "sess-2")

    def test_a_second_404_is_an_error(self):
        def raw(h, msg):
            if msg["method"] == "initialize":
                return h._send(200, json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"protocolVersion": "2025-06-18"}}).encode(),
                               "application/json", [("Mcp-Session-Id", "s")])
            if msg["method"] == "notifications/initialized":
                return h._send(202)
            h._send(404)
        self.stub.raw = raw
        with self.assertRaisesRegex(mcp_client.McpError, "dropped the session again"):
            self.client().call_tool("echo", {})
        self.assertEqual(self.stub.methods().count("initialize"), 2)

    def test_401_and_403_say_unauthorised(self):
        self.stub.token = "right"
        p = token_file("wrong\n")
        with self.assertRaisesRegex(mcp_client.McpError, r"^unauthorised \(401\)$"):
            self.client(token_file=p).call_tool("echo", {})
        self.stub.raw = lambda h, msg: h._send(403)
        self.stub.token = None
        with self.assertRaisesRegex(mcp_client.McpError, r"^unauthorised \(403\)$"):
            self.client().call_tool("echo", {})

    def test_other_http_statuses(self):
        self.stub.raw = lambda h, msg: h._send(500, b"boom")
        with self.assertRaisesRegex(mcp_client.McpError, r"^HTTP 500$"):
            self.client().call_tool("echo", {})

    def test_a_body_over_the_cap_is_refused(self):
        self.stub.tools["big"] = lambda a: {"content": [{"type": "text", "text": "x" * 5000}]}
        c = self.client(max_body=2000)
        with self.assertRaisesRegex(mcp_client.McpError, "over 2000 bytes"):
            c.call_tool("big", {})
        self.stub.sse = True
        with self.assertRaisesRegex(mcp_client.McpError, "over 2000 bytes"):
            self.client(max_body=2000).call_tool("big", {})

    def test_a_server_that_does_not_answer_in_time(self):
        self.stub.delay = 1.5
        t0 = time.monotonic()
        with self.assertRaisesRegex(mcp_client.McpError, "no answer in 0.3s"):
            self.client(timeout=0.3).call_tool("echo", {})
        self.assertLess(time.monotonic() - t0, 1.4)

    def test_a_server_that_is_down(self):
        self.stub.close()
        with self.assertRaisesRegex(mcp_client.McpError, "cannot reach the server"):
            self.client().call_tool("echo", {})
        self.stub = Stub()          # so tearDown's close has a live server
        self.addCleanup(self.stub.close)

    def test_a_url_that_is_not_http(self):
        with self.assertRaises(mcp_client.McpError):
            mcp_client.McpClient("file:///etc/passwd")

    def test_an_answer_that_is_not_json(self):
        self.stub.raw = lambda h, msg: h._send(200, b"<html>", "text/html")
        with self.assertRaisesRegex(mcp_client.McpError, "not JSON"):
            self.client().call_tool("echo", {})


class Token(Base):
    def test_the_bearer_token_comes_from_the_file_at_each_call(self):
        p = token_file("t-one\n")
        c = self.client(token_file=p)
        self.stub.token = "t-one"
        c.call_tool("echo", {})
        with open(p, "w") as f:
            f.write("t-two")
        self.stub.token = "t-two"
        c.call_tool("echo", {})                # a rotated token is picked up without a new client
        self.assertTrue(all(h.get("authorization") == "Bearer t-two" for h, _ in self.stub.seen[-1:]))

    def test_a_missing_token_file_fails_without_a_request_and_without_the_path_in_the_message(self):
        with self.assertRaises(mcp_client.McpError) as cm:
            self.client(token_file="/nonexistent/dir/token").call_tool("echo", {})
        self.assertIn("token file is unreadable", str(cm.exception))
        self.assertNotIn("/nonexistent", str(cm.exception))
        self.assertEqual(self.stub.seen, [])

    def test_an_empty_token_file_fails(self):
        with self.assertRaisesRegex(mcp_client.McpError, "token file is empty"):
            self.client(token_file=token_file("\n")).call_tool("echo", {})

    def test_no_token_file_sends_no_authorization(self):
        self.client().call_tool("echo", {})
        self.assertTrue(all("authorization" not in h for h, _ in self.stub.seen))


if __name__ == "__main__":
    unittest.main()
