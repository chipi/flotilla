"""runtime/offline_run.py: run a version's own command on a given input, offline (v2-design.md section 6, "The offline entry point").

  flotillad -stations fleets/<fleet>/<routine>/v<n>/ -models <models> > contract.json     # what flotillad would hand the run
  python3 runtime/offline_run.py --contract contract.json --input input.json --models models.json --out DIR [--forbid DIR ...]

It runs in the shadow worker's image, never in the caller's environment, and accepts no flotillad address or token:
  * the child's environment is built from an allowlist: the contract's own variables (minus any address, token or key), FLOTILLA_SHADOW=1,
    a fresh HOME, this run's calls and loaded files, a models config of its own (an eval spends on its own key, never a live or shadow key),
    and a few neutral variables. Nothing of the caller's environment is passed through;
  * the run's API is an in-process stand-in on loopback that serves the input envelope (the ledger as of its mark, from the envelope) and
    turns every would-be write into a decision record under --out;
  * a proxy of ours is the child's only route out: it refuses every host but the models config's, so a station's model calls go to the
    gateway and nothing else is reachable. That is the proxy and the credentials, not a kernel boundary: a process that ignores the proxy
    variables is stopped only by the deploy's network (the shadow worker's service network), which this file does not claim to replace;
  * given recorded tool results (the envelope's `recorded_tool_results`), a station answers its tools from them: a call with no recorded
    result fails the run and is never sent live (runtime/recorded.py).
--out is refused inside any --forbid folder (the state folder, the fleets tree) and inside the code root's fleets/.
It writes: calls.jsonl, loaded.jsonl, decisions.jsonl (the would-be writes), output.txt and result.json. Stdlib only.
"""
import argparse
import http.server
import json
import os
import socketserver
import subprocess
import sys
import tempfile
import threading
import urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import schemacheck  # noqa: E402

NEUTRAL = ("PATH", "LANG", "LC_ALL", "TZ", "PYTHONHASHSEED", "FLOTILLA_CODE_ROOT")
SECRETISH = ("TOKEN", "KEY", "SECRET", "PASSWORD", "API_URL", "CREDENTIAL")
STAND_IN_TOKEN = "offline-stand-in-token"


class Refused(Exception):
    pass


def refuse_out(out, forbid, code_root):
    real = os.path.realpath(out)
    for f in list(forbid) + [os.path.join(code_root, "fleets")]:
        root = os.path.realpath(f)
        if real == root or real.startswith(root + os.sep):
            raise Refused(f"--out {out} is inside {f}: an offline run writes outside the state folder and the fleets tree")


def model_hosts(models):
    hosts = set()
    for ep in (models.get("endpoints") or {}).values():
        h = urllib.parse.urlparse(ep.get("base_url", "")).hostname
        if h:
            hosts.add(h)
    return hosts


class StandIn(http.server.ThreadingHTTPServer):
    """The run's API: the input's ledger as of its mark, and a record of every write."""

    def __init__(self, envelope, decisions_path):
        self.env, self.path, self.lock, self.seq = envelope, decisions_path, threading.Lock(), 0
        super().__init__(("127.0.0.1", 0), self.Handler)
        self.daemon_threads = True

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def reply(self, code, doc):
            b = json.dumps(doc).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)

        def authed(self):
            return self.headers.get("Authorization") == f"Bearer {STAND_IN_TOKEN}"

        def do_GET(self):
            u = urllib.parse.urlparse(self.path)
            q = dict(urllib.parse.parse_qsl(u.query))
            led = self.server.env.get("ledger") or {}
            if not self.authed():
                return self.reply(401, {"error": "no token"})
            if u.path == "/v1/ledger/filings":
                keys = {k: v for k, v in q.items() if k not in ("ns",)}
                rows = [r for r in led.get("filings") or [] if not keys or any(r.get(k) == v for k, v in keys.items())]
                return self.reply(200, {"filings": [{"row": r} for r in rows]})
            if u.path == "/v1/ledger/entries":
                after, limit = int(q.get("after", 0)), int(q.get("limit", 5000))
                es = [dict(e["row"], kind=e["kind"], seq=i + 1) for i, e in enumerate(led.get("entries") or [])]
                return self.reply(200, {"entries": [e for e in es if e["seq"] > after][:limit]})
            if u.path == "/v1/ledger/counts":
                return self.reply(200, {"filings": len(led.get("filings") or []), "entries": len(led.get("entries") or [])})
            self.note("GET", u.path, None)
            self.reply(404, {"error": "not served offline", "path": u.path})

        def do_POST(self):
            u = urllib.parse.urlparse(self.path)
            n = int(self.headers.get("Content-Length") or 0)
            try:
                body = json.loads(self.rfile.read(n) or b"{}")
            except ValueError:
                body = None
            if not self.authed():
                return self.reply(401, {"error": "no token"})
            seq = self.note("POST", u.path, body)
            self.reply(200, {"seq": seq, "recorded": True})

        def note(self, method, path, body):
            srv = self.server
            with srv.lock:
                srv.seq += 1
                with open(srv.path, "a") as f:
                    f.write(json.dumps({"seq": srv.seq, "method": method, "path": path, "body": body}) + "\n")
                return srv.seq


class RefusingProxy(socketserver.ThreadingMixIn, http.server.HTTPServer):
    """The child's route out: a model host passes, everything else is refused and kept."""

    daemon_threads = True

    def __init__(self, allowed, refused_path):
        self.allowed, self.refused_path = allowed, refused_path
        super().__init__(("127.0.0.1", 0), self.Handler)

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def refuse(self):
            host = urllib.parse.urlparse(self.path if "//" in self.path else "//" + self.path).hostname or ""
            with open(self.server.refused_path, "a") as f:
                f.write(json.dumps({"method": self.command, "target": self.path}) + "\n")
            self.send_response(403)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return host
        do_GET = do_POST = do_PUT = do_HEAD = do_CONNECT = refuse   # a model host is reached by the child directly when it is on NO_PROXY


def build_env(contract_env, models_json, out, home, api_url, token_file, proxy_url, allowed_hosts, recorded_path, caller_env):
    env = {}
    for k, v in contract_env.items():
        if k.startswith("FLOTILLA_") and not any(s in k for s in SECRETISH) and k not in ("FLOTILLA_MODELS", "FLOTILLA_CALLS_FILE", "FLOTILLA_LOADED_FILE"):
            env[k] = v
    for k in NEUTRAL:
        if k in caller_env:
            env[k] = caller_env[k]
    env.update({"FLOTILLA_SHADOW": "1", "FLOTILLA_OFFLINE": "1", "FLOTILLA_MODELS": models_json, "HOME": home, "FLOTILLA_API_URL": api_url,
                "FLOTILLA_API_TOKEN_FILE": token_file, "FLOTILLA_CALLS_FILE": os.path.join(out, "calls.jsonl"),
                "FLOTILLA_LOADED_FILE": os.path.join(out, "loaded.jsonl"), "HTTP_PROXY": proxy_url, "HTTPS_PROXY": proxy_url,
                "NO_PROXY": ",".join(sorted({"127.0.0.1", "localhost"} | allowed_hosts))})
    if recorded_path:
        env["FLOTILLA_RECORDED_TOOLS"] = recorded_path
    return env


def run(contract, envelope, models, out, forbid=(), caller_env=None, code_root=None):
    caller_env = os.environ if caller_env is None else caller_env
    code_root = code_root or caller_env.get("FLOTILLA_CODE_ROOT") or os.path.join(HERE, "..")
    errs = schemacheck.validate(envelope, json.load(open(os.path.join(HERE, "input.schema.json"))))
    if errs:
        raise Refused("the input is not a valid envelope: " + "; ".join(errs[:3]))
    refuse_out(out, forbid, code_root)
    os.makedirs(out, exist_ok=True)
    home = tempfile.mkdtemp(prefix="home-", dir=out)
    token_file = os.path.join(out, ".token")
    fd = os.open(token_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(STAND_IN_TOKEN)
    recorded_path = ""
    if envelope.get("recorded_tool_results"):
        recorded_path = os.path.join(out, "recorded_tool_results.json")
        with open(recorded_path, "w") as f:
            json.dump(envelope["recorded_tool_results"], f)
    decisions, refused = os.path.join(out, "decisions.jsonl"), os.path.join(out, "refused.jsonl")
    api, proxy = StandIn(envelope, decisions), RefusingProxy(model_hosts(models), refused)
    for srv in (api, proxy):
        threading.Thread(target=srv.serve_forever, daemon=True).start()
    env = build_env(contract.get("env") or {}, json.dumps(models), out, home, f"http://127.0.0.1:{api.server_address[1]}", token_file,
                    f"http://127.0.0.1:{proxy.server_address[1]}", model_hosts(models), recorded_path, caller_env)
    cwd = os.path.join(code_root, contract.get("workdir") or ".")
    try:
        timeout = _seconds(contract.get("timeout") or "20m")
        p = subprocess.run(contract["command"], shell=True, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
        code, output = p.returncode, p.stdout + p.stderr
    except subprocess.TimeoutExpired as e:
        code, output = 124, (e.stdout or b"").decode() if isinstance(e.stdout, bytes) else (e.stdout or "")
    finally:
        api.shutdown()
        proxy.shutdown()
    with open(os.path.join(out, "output.txt"), "w") as f:
        f.write(output)
    for name in ("calls.jsonl", "loaded.jsonl", "decisions.jsonl", "refused.jsonl"):
        open(os.path.join(out, name), "a").close()
    result = {"exit_code": code, "routine": contract.get("routine"), "version": contract.get("version"), "input_key": envelope["input_key"]}
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(result, f)
    return result


def _seconds(s):
    n, unit = int(s[:-1]), s[-1]
    return n * {"s": 1, "m": 60, "h": 3600}[unit]


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--contract", required=True)
    ap.add_argument("--input", required=True)
    ap.add_argument("--models", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--forbid", action="append", default=[], help="a folder --out must not be inside (the state folder, the fleets tree)")
    a = ap.parse_args(argv)
    try:
        res = run(json.load(open(a.contract)), json.load(open(a.input)), json.load(open(a.models)), a.out, a.forbid)
    except Refused as e:
        print(f"offline_run: {e}", file=sys.stderr)
        return 2
    print(json.dumps(res))
    return 0 if res["exit_code"] == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
