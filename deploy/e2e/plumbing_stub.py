"""The e2e's stand-in plumbing tool (RFC-0014 §4.6): it answers plumbing.call's callbacks on the plumbing
network as the installation's n8n would, at /webhook/<name> (create_tasks, post_decisions, reply, post,
retract). Each call must be signed with that callback's secret (/secrets/<name>.secret) within five minutes,
as internal/hook verifies an inbound one; a call that is not gets 401. A send gets {"ref": "<name>-<delivery>"};
a status call gets {"done": true} with the same ref once that delivery was sent, {"done": false} before.
Every call is a JSON line on stdout, which the e2e reads (docker logs).

  python3 plumbing_stub.py [port]
"""
import hashlib
import hmac
import http.server
import json
import os
import sys
import time

SECRETS = "/secrets"
WINDOW_S = 300
sent = {}


def secret(name):
    path = os.path.join(SECRETS, name + ".secret")
    if not name.replace("_", "").isalnum() or not os.path.isfile(path):
        return None
    with open(path) as f:
        return f.read().strip().encode()


def log(**kv):
    print(json.dumps(kv, sort_keys=True), flush=True)


class Plumbing(http.server.BaseHTTPRequestHandler):
    def answer(self, code, doc=None):
        out = json.dumps(doc or {}).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def do_POST(self):
        name = self.path.removeprefix("/webhook/")
        body = self.rfile.read(min(int(self.headers.get("Content-Length", "0")), 65536))
        key = secret(name) if self.path.startswith("/webhook/") else None
        ts, dl, sig = (self.headers.get(h, "") for h in ("X-Flotilla-Timestamp", "X-Flotilla-Delivery", "X-Flotilla-Signature"))
        want = "sha256=" + hmac.new(key, f"{ts}.{dl}.".encode() + body, hashlib.sha256).hexdigest() if key else ""
        fresh = ts.isdigit() and abs(time.time() - int(ts)) <= WINDOW_S
        if not key or not fresh or not hmac.compare_digest(sig, want):
            log(event="refused", name=name, delivery=dl, why="no such callback" if not key else "stale" if not fresh else "signature")
            return self.answer(401)
        try:
            doc = json.loads(body)
        except ValueError:
            log(event="refused", name=name, delivery=dl, why="not JSON")
            return self.answer(400)
        if doc.get("mode") == "send":
            sent[dl] = f"{name}-{dl}"
            log(event="send", name=name, delivery=dl, action=doc.get("action"), case=doc.get("case_id"), params=doc.get("params"))
            return self.answer(200, {"ref": sent[dl]})
        if doc.get("mode") == "status":
            log(event="status", name=name, delivery=dl, done=dl in sent)
            return self.answer(200, {"done": True, "ref": sent[dl]} if dl in sent else {"done": False})
        log(event="refused", name=name, delivery=dl, why="mode")
        return self.answer(400)

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1]) if len(sys.argv) > 1 else 8090), Plumbing).serve_forever()
