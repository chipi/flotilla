"""The e2e's stand-in MCP server (RFC-0014 §4.8): what n8n's MCP Server Trigger is to the reads worker. Streamable HTTP at /mcp: one JSON-RPC
message per POST, every answer a `text/event-stream` (as n8n answers), a session id from `initialize` that every later request must send
back (404 without it), and a bearer token read from /secrets/lab.token at each request (401 without it). Tools: get_page and get_log
(reads, which a station declares), get_log's result over 8192 characters (what an episode's tool record would have hashed), and delete_page,
a write the stations never declare: any call of it is the e2e's finding. Every request is a JSON line on stdout, which the e2e reads
(docker logs): the method, the tool and arguments, whether it carried a session and the right token, and the caller's address.

  python3 mcp_stub.py [port]
"""
import http.server
import json
import sys

TOKEN_FILE = "/secrets/lab.token"
LONG = ("0123456789" * 1200)   # 12000 characters: over the 8192 of the episode's tool record, under the station's cap
sessions = set()


def log(**kv):
    print(json.dumps(kv, sort_keys=True), flush=True)


def tool_result(name, args):
    if name == "get_page":
        return "page %s of the lab wiki" % args.get("id")
    if name == "get_log":
        return LONG
    if name == "delete_page":
        return "deleted page %s" % args.get("id")
    return None


class Mcp(http.server.BaseHTTPRequestHandler):
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

    def _answer(self, msg, body, headers=()):
        out = json.dumps(dict(jsonrpc="2.0", id=msg["id"], **body))
        self._send(200, ("event: message\ndata: %s\n\n" % out).encode(), "text/event-stream", headers)

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        msg = json.loads(self.rfile.read(n) or b"{}")
        method, params = msg.get("method"), msg.get("params") or {}
        try:
            with open(TOKEN_FILE) as f:
                want = f.read().strip()
        except OSError:
            want = ""
        authed = bool(want) and self.headers.get("Authorization") == "Bearer " + want
        sid = self.headers.get("Mcp-Session-Id")
        log(event="request", method=method, tool=params.get("name"), args=params.get("arguments"), authed=authed,
            session=bool(sid), client=self.client_address[0])
        if not authed:
            return self._send(401)
        if method != "initialize" and sid not in sessions:
            return self._send(404)
        if method == "notifications/initialized":
            return self._send(202)
        if method == "initialize":
            new = "sess-%d" % (len(sessions) + 1)
            sessions.add(new)
            return self._answer(msg, {"result": {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                                                 "serverInfo": {"name": "e2e-lab", "version": "1"}}}, [("Mcp-Session-Id", new)])
        if method == "tools/list":
            return self._answer(msg, {"result": {"tools": [{"name": t} for t in ("get_page", "get_log", "delete_page")]}})
        if method == "tools/call":
            text = tool_result(params.get("name"), params.get("arguments") or {})
            if text is None:
                return self._answer(msg, {"error": {"code": -32602, "message": "unknown tool"}})
            return self._answer(msg, {"result": {"content": [{"type": "text", "text": text}], "isError": False}})
        return self._answer(msg, {"error": {"code": -32601, "message": "no such method"}})


if __name__ == "__main__":
    http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1]) if len(sys.argv) > 1 else 8091), Mcp).serve_forever()
