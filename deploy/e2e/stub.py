"""A stand-in for a service the e2e stack reads or writes (VictoriaMetrics, VictoriaTraces, LiteLLM).
Answers every request with 200 (204 for writes) and prints one line per request to stdout, so the
e2e script can read what reached it from `docker logs`.
"""
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
        if self.path.startswith("/api/v1/import/prometheus"):
            for line in body.decode("utf-8", "replace").splitlines():
                print(f"{NAME} SAMPLE {line}", flush=True)
        self._done(204, b"")

    def log_message(self, *a):
        pass


HTTPServer(("0.0.0.0", 8080), H).serve_forever()
