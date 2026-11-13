"""A stand-in for a service the e2e stack reads or writes (VictoriaMetrics, VictoriaTraces, LiteLLM).
Answers every request with 200 (204 for writes) and prints one line per request to stdout, so the
e2e script can read what reached it from `docker logs`. As LiteLLM it also answers a chat completion
the way LiteLLM does (usage.cost, the deployment id header); as VictoriaTraces it says which span
names a trace export carries (the names are plain strings inside the protobuf).
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

NAME = sys.argv[1] if len(sys.argv) > 1 else "stub"


class H(BaseHTTPRequestHandler):
    def _done(self, code, body=b'{"ok":true}'):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        print(f"{NAME} GET {self.path}", flush=True)
        self._done(200)

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else b""
        print(f"{NAME} POST {self.path} {len(body)}b", flush=True)
        if self.path == "/api/v1/query":
            # As VictoriaMetrics, for the ops monitor: the backup's age, which the e2e sets (STUB_QUERY_VALUE,
            # seconds); the query itself is printed so the e2e can read what was asked.
            print(f"{NAME} QUERY {body.decode('utf-8', 'replace')}", flush=True)
            value = os.environ.get("STUB_QUERY_VALUE", "73800")
            out = json.dumps({"status": "success", "data": {"resultType": "vector",
                              "result": [{"metric": {}, "value": [0, value]}]}}).encode()
            self._done(200, out)
            return
        if self.path.endswith("/chat/completions"):
            auth = self.headers.get("Authorization", "")
            print(f"{NAME} CHAT auth={'bearer' if auth.startswith('Bearer ') and len(auth) > 7 else 'none'} "
                  f"model={json.loads(body).get('model')}", flush=True)
            out = json.dumps({"choices": [{"message": {"content": '{"ok": true}'}, "finish_reason": "stop"}],
                              "model": json.loads(body).get("model"),
                              "usage": {"prompt_tokens": 12, "completion_tokens": 4, "cost": 0.0000021}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("x-litellm-model-id", "e2e-deployment")
            self.send_header("Content-Length", str(len(out)))
            self.end_headers()
            self.wfile.write(out)
            return
        if self.path.startswith("/insert/opentelemetry"):
            for name in (b"model.chat", b"flotilla-platform"):
                if name in body:
                    print(f"{NAME} SPAN {name.decode()}", flush=True)
        if self.path.startswith("/api/v1/import/prometheus"):
            for line in body.decode("utf-8", "replace").splitlines():
                print(f"{NAME} SAMPLE {line}", flush=True)
        self._done(204, b"")

    def log_message(self, *a):
        pass


HTTPServer(("0.0.0.0", 8080), H).serve_forever()
