#!/usr/bin/env python3
"""The e2e's old worker: answers /healthz and /livez as a worker built before run tokens did, with no `caps`.

  old_worker.py

flotillad under run_tokens: required refuses to start (and -check refuses) while a reachable worker's /healthz does not list run_token, naming it;
under accept it only says so. This is that worker, for the e2e's old-agent row (the caps-skip mutant breaks it).
"""
import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"ok": True, "worker": "old", "version": "e2e-local", "running": [], "mode": "live"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


HTTPServer(("0.0.0.0", 8081), H).serve_forever()
