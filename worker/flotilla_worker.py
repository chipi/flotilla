"""flotilla worker: runs a fleet's routines inside that fleet's container, for flotillad only.

The worker holds the fleet's secrets (its env files) and its code; flotillad holds neither. flotillad
says what to run and when; the worker runs it and reports. Python stdlib only.

  POST /v1/runs   {run_id, routine, command, workdir, timeout_s, spend_file, env}  -> the result, when done
  GET  /healthz   {ok, worker, version, running}
  GET  /livez     {ok} without a token: the container healthcheck

Every request needs `Authorization: Bearer <the worker token>`; nothing else on the network can
start a run. One run per routine at a time (a second start of a running routine is 409).

A run's environment is built, never inherited: the worker's own environment (which holds its token)
does not reach the routine.
  1. PATH, HOME, LANG;
  2. every KEY=VAL of the env files in FLOTILLA_ENV_FILES (colon-separated, read per run, so a
     rotated secret is picked up without a restart);
  3. every ROUTINE_<KEY> of the worker's environment, as <KEY> (this installation's overrides, e.g. the
     service URLs inside the container network);
  4. the request's env (FLEETD_STAGE, FLEETD_CYCLE_ID, FLEETD_BUDGET_LEFT, TRACEPARENT).
"""
import hmac
import json
import os
import signal
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

WORKER = os.environ.get("FLOTILLA_WORKER", "worker")
VERSION = os.environ.get("FLOTILLA_VERSION", "dev")
CODE_ROOT = os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla")
KILL_GRACE_S = 10
TAIL = 4000
MAX_BODY = 1 << 20

_running = {}
_lock = threading.Lock()


def log(msg, **fields):
    """One JSON line to stdout: Alloy collects container stdout into VictoriaLogs."""
    rec = {"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "level": fields.pop("level", "info"),
           "service": f"flotilla-worker-{WORKER}", "msg": msg, **fields}
    print(json.dumps(rec, ensure_ascii=False), flush=True)


def read_env_file(path):
    out = {}
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#"):
                    continue
                if line.startswith("export "):
                    line = line[len("export "):]
                if "=" in line:
                    k, v = line.split("=", 1)
                    out[k.strip()] = v.strip().strip('"').strip("'")
    except OSError as e:
        log("env file unreadable", level="error", path=path, error=str(e))
    return out


def build_env(request_env, environ=None):
    environ = os.environ if environ is None else environ
    env = {k: environ[k] for k in ("PATH", "HOME", "LANG", "FLOTILLA_VERSION", "FLOTILLA_WORKER") if k in environ}
    for path in filter(None, environ.get("FLOTILLA_ENV_FILES", "").split(":")):
        env.update(read_env_file(path))
    prefix = "ROUTINE_"
    for k, v in environ.items():
        if k.startswith(prefix) and len(k) > len(prefix):
            env[k[len(prefix):]] = v
    env.update({k: str(v) for k, v in (request_env or {}).items()})
    return env


def resolve(base, rel):
    """A path from a spec, relative to base; never outside it."""
    p = os.path.realpath(os.path.join(base, rel or "."))
    b = os.path.realpath(base)
    if p != b and not p.startswith(b + os.sep):
        raise ValueError(f"{rel!r} leaves {base}")
    return p


def read_spend(path):
    """Consume the routine's spend report (USD, one number), as fleetd did: read, then truncate."""
    if not path:
        return 0.0
    try:
        with open(path) as f:
            raw = f.read().strip()
        open(path, "w").close()
        return float(raw) if raw else 0.0
    except (OSError, ValueError):
        return 0.0


def run_job(req, environ=None):
    environ = os.environ if environ is None else environ
    env = build_env(req.get("env"), environ)
    workdir = resolve(CODE_ROOT, req.get("workdir"))
    spend_path = resolve(env.get("HOME", "/"), req["spend_file"]) if req.get("spend_file") else ""
    timeout = float(req["timeout_s"])
    start = time.monotonic()
    proc = subprocess.Popen(["sh", "-c", req["command"]], cwd=workdir, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, start_new_session=True)
    chunks = []

    def pump():
        for line in iter(proc.stdout.readline, b""):
            chunks.append(line)
            log("run output", run_id=req["run_id"], routine=req["routine"], line=line.decode("utf-8", "replace").rstrip())

    reader = threading.Thread(target=pump, daemon=True)
    reader.start()
    status = "ok"
    try:
        proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        status = "timeout"
        _kill_group(proc)
    reader.join(timeout=KILL_GRACE_S)
    if status == "ok" and proc.returncode != 0:
        status = "error"
    out = b"".join(chunks).decode("utf-8", "replace")
    return {"status": status, "exit_code": proc.returncode, "duration_s": round(time.monotonic() - start, 3),
            "spend_usd": read_spend(spend_path), "output_tail": out[-TAIL:]}


def _kill_group(proc):
    """SIGTERM the whole process group, SIGKILL whatever is left after the grace period."""
    try:
        os.killpg(proc.pid, signal.SIGTERM)
        proc.wait(timeout=KILL_GRACE_S)
    except subprocess.TimeoutExpired:
        pass
    except ProcessLookupError:
        return
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    proc.wait()


REQUIRED = {"run_id": str, "routine": str, "command": str, "timeout_s": (int, float)}


class Handler(BaseHTTPRequestHandler):
    server_version = "flotilla-worker"

    def log_message(self, fmt, *args):   # one JSON line per request instead of the default
        log("http", method=self.command, path=self.path, status=getattr(self, "_status", None))

    def _send(self, code, obj):
        self._status = code
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authorized(self):
        token = worker_token()
        got = self.headers.get("Authorization", "")
        return bool(token) and hmac.compare_digest(got.encode(), ("Bearer " + token).encode())

    def do_GET(self):
        if self.path == "/livez":   # the container healthcheck: up and serving; says nothing else
            return self._send(200, {"ok": True})
        if not self._authorized():
            return self._send(401, {"error": "unauthorized"})
        if self.path != "/healthz":
            return self._send(404, {"error": "not found"})
        with _lock:
            running = sorted(_running)
        self._send(200, {"ok": True, "worker": WORKER, "version": VERSION, "running": running})

    def do_POST(self):
        if not self._authorized():
            return self._send(401, {"error": "unauthorized"})
        if self.path != "/v1/runs":
            return self._send(404, {"error": "not found"})
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0 or n > MAX_BODY:
            return self._send(400, {"error": "body required, at most 1 MiB"})
        try:
            req = json.loads(self.rfile.read(n))
            for k, t in REQUIRED.items():
                if not isinstance(req.get(k), t):
                    raise ValueError(f"{k} missing or wrong type")
            if req["timeout_s"] <= 0:
                raise ValueError("timeout_s must be positive")
            resolve(CODE_ROOT, req.get("workdir"))
        except (ValueError, json.JSONDecodeError) as e:
            return self._send(400, {"error": str(e)})
        with _lock:
            if req["routine"] in _running:
                return self._send(409, {"error": f"{req['routine']} is already running ({_running[req['routine']]})"})
            _running[req["routine"]] = req["run_id"]
        log("run start", run_id=req["run_id"], routine=req["routine"])
        try:
            res = run_job(req)
        except Exception as e:  # noqa: BLE001 — report, never crash the server
            log("run failed to start", level="error", run_id=req["run_id"], routine=req["routine"], error=str(e))
            res = {"status": "error", "exit_code": None, "duration_s": 0, "spend_usd": 0, "output_tail": str(e)}
        finally:
            with _lock:
                _running.pop(req["routine"], None)
        log("run end", run_id=req["run_id"], routine=req["routine"], status=res["status"], exit_code=res["exit_code"],
            duration_s=res["duration_s"], spend_usd=res["spend_usd"],
            level="info" if res["status"] == "ok" else "warning")
        self._send(200, res)


def worker_token():
    """FLOTILLA_WORKER_TOKEN_FILE (a mounted secret, preferred) or FLOTILLA_WORKER_TOKEN."""
    path = os.environ.get("FLOTILLA_WORKER_TOKEN_FILE")
    if path:
        try:
            with open(path) as f:
                return f.read().strip()
        except OSError:
            return ""
    return os.environ.get("FLOTILLA_WORKER_TOKEN", "")


def main():
    if len(worker_token()) < 32:
        sys.exit("no worker token of at least 32 characters (FLOTILLA_WORKER_TOKEN_FILE or "
                 "FLOTILLA_WORKER_TOKEN): refusing to serve without auth")
    addr = os.environ.get("FLOTILLA_WORKER_LISTEN", "0.0.0.0:8081")
    host, port = addr.rsplit(":", 1)
    srv = ThreadingHTTPServer((host, int(port)), Handler)
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=srv.shutdown, daemon=True).start())
    log("worker listening", addr=addr, version=VERSION)
    srv.serve_forever()


if __name__ == "__main__":
    main()
