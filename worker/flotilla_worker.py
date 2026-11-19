"""flotilla worker: runs a fleet's routines inside that fleet's container, for flotillad only.

The worker holds the fleet's secrets (its env files) and its code; flotillad holds neither. flotillad
says what to run and when; the worker runs it and reports. Python stdlib only.

  POST /v1/runs   {run_id, routine, command, workdir, timeout_s, spend_file, plan_file, env, api_token, shadow}  -> the
                  result, when done. api_token is this run's own token for flotillad's workers' listener: the worker writes it
                  to a file in the run's temp directory (mode 0600), names the file in FLOTILLA_API_TOKEN_FILE (over any
                  ROUTINE_ value) and removes it when the run ends; it is never in the run's environment or the worker's log.
  POST /v1/runs/<run_id>/cancel   {ok, run_id}: stops that run as its timeout does (SIGTERM to its process
                  group, SIGKILL after KILL_GRACE_S); the run's own request then answers with status
                  "cancelled" and its spend as usual. A second cancel of a run still ending is 200 again;
                  a run id this worker is not running (never started, or already answered) is 404.
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
  4. the request's env (FLEETD_STAGE, FLEETD_CYCLE_ID, FLEETD_BUDGET_LEFT, TRACEPARENT);
  5. FLOTILLA_API_TOKEN_FILE, when the request has an api_token: the run's own token file.
"""
import hmac
import json
import os
import re
import signal
import subprocess
import sys
import tempfile
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
_cancels = {}   # run id -> the Event a cancel sets; present from the run's acceptance to its answer
_marks = {}     # run id -> the fields every log line of that run carries (a shadow run's), while it is in flight
_lock = threading.Lock()


def run_marks(req):
    """What marks a run's log lines: nothing for a live run (its lines are as they always were); shadow=true and
    routine_version for a shadow run, so a log query can leave shadows out (a shadow is separated by this mark, never by a
    quieter level: its failures log as loudly as a live one's)."""
    if not req.get("shadow"):
        return {}
    out = {"shadow": True}
    v = str((req.get("env") or {}).get("FLOTILLA_ROUTINE_VERSION", ""))
    if v.isdigit():
        out["routine_version"] = int(v)
    return out


def log(msg, **fields):
    """One JSON line to stdout: Alloy collects container stdout into VictoriaLogs. A line about a shadow run in flight
    carries that run's marks."""
    fields = {**_marks.get(fields.get("run_id"), {}), **fields}
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
    return spend_report(path)[0]


def spend_report(path):
    """(USD, reported): the routine's spend report, read then truncated. `reported` is False when the routine
    names a spend file but left no number in it (missing, empty, unreadable): the run's spend is then 0 by
    default, not by report, and flotillad says so."""
    if not path:
        return 0.0, True
    try:
        with open(path) as f:
            raw = f.read().strip()
        open(path, "w").close()
        return (float(raw), True) if raw else (0.0, False)
    except (OSError, ValueError):
        return 0.0, False


CALLS_MAX = 5000  # calls-file lines returned per run (model calls and harness steps); a run that writes more reports the first CALLS_MAX and how many it dropped
LOADED_MAX = 200  # what a run read (runtime/loaded.py), per run


def calls_path(env, run_id, what="calls"):
    """The run's calls (or loaded) file, one per run: two runs on one worker never share it. In the temp
    directory: a worker's HOME can be read-only (the triage worker's is; only /tmp and its state volume
    are writable), and the calls were lost there until 2026-11-14."""
    safe = "".join(c if c.isalnum() or c in "._-" else "_" for c in run_id)[:120]
    return os.path.join(env.get("TMPDIR") or tempfile.gettempdir(), f".flotilla-{what}-{safe}.jsonl")


def read_calls(path, limit=None):
    """Consume the run's model calls (runtime/model_client.py writes one JSON line per call), or what it
    read (runtime/loaded.py), then remove the file. Lines that are not JSON objects are skipped."""
    return read_calls_counted(path, limit)[0]


def read_calls_counted(path, limit=None):
    """(the first `limit` lines of the file that are JSON objects, how many more there were), then the file removed."""
    limit = CALLS_MAX if limit is None else limit
    out, dropped = [], 0
    try:
        with open(path) as f:
            for line in f:
                try:
                    c = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(c, dict):
                    continue
                if len(out) < limit:
                    out.append(c)
                else:
                    dropped += 1
    except OSError:
        return out, dropped
    finally:
        try:
            os.remove(path)
        except OSError:
            pass
    return out, dropped


PLAN_MAX = 64 * 1024


def read_plan(path):
    """Consume what a routine on a case decided (pipeline v1 step 5): one JSON object, at most PLAN_MAX
    bytes, then removed. Returns (plan, error); a missing file is (None, "")."""
    if not path:
        return None, ""
    try:
        with open(path, "rb") as f:
            raw = f.read(PLAN_MAX + 1)
    except FileNotFoundError:
        return None, ""
    except OSError as e:
        return None, f"plan_file: {e}"
    finally:
        try:
            os.remove(path)
        except OSError:
            pass
    if len(raw) > PLAN_MAX:
        return None, f"plan_file: over {PLAN_MAX} bytes"
    try:
        plan = json.loads(raw)
    except ValueError as e:
        return None, f"plan_file: not JSON: {e}"
    if not isinstance(plan, dict):
        return None, "plan_file: not a JSON object"
    return plan, ""


def cancel_run(run_id):
    """Ask the run to stop; False when this worker is not running it."""
    with _lock:
        ev = _cancels.get(run_id)
    if ev is None:
        return False
    ev.set()
    return True


TOKEN_RE = re.compile(r"^[A-Za-z0-9_-]{16,256}$")


def token_path(env, run_id):
    """Where a run's own token file is: its temp directory (the calls file's), one per run."""
    safe = "".join(c if c.isalnum() or c in "._-" else "_" for c in run_id)[:120]
    return os.path.join(env.get("TMPDIR") or tempfile.gettempdir(), f".flotilla-token-{safe}")


def write_run_token(path, token):
    """The token file, mode 0600, made fresh: a stale file (a crashed run's) is removed first and a symlink in its place is
    never followed."""
    try:
        os.remove(path)
    except FileNotFoundError:
        pass
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(token)


def run_job(req, environ=None, cancel=None):
    environ = os.environ if environ is None else environ
    env = build_env(req.get("env"), environ)
    marks = run_marks(req)
    if marks:
        _marks[req["run_id"]] = marks
    token_file = ""
    if req.get("api_token"):
        token_file = token_path(env, req["run_id"])
        write_run_token(token_file, req["api_token"])
        env["FLOTILLA_API_TOKEN_FILE"] = token_file   # over a ROUTINE_ value: the run's own token, not the worker's
    try:
        return _run_job(req, env, cancel)
    finally:
        if marks:
            _marks.pop(req["run_id"], None)
        if token_file:
            try:
                os.remove(token_file)
            except OSError:
                pass


def _run_job(req, env, cancel):
    cancel = cancel or threading.Event()
    workdir = resolve(CODE_ROOT, req.get("workdir"))
    spend_path = resolve(env.get("HOME", "/"), req["spend_file"]) if req.get("spend_file") else ""
    plan_path = resolve(env.get("HOME", "/"), req["plan_file"]) if req.get("plan_file") else ""
    if plan_path:  # a plan left by an earlier run must never be read as this run's
        try:
            os.remove(plan_path)
        except FileNotFoundError:
            pass
        env["FLOTILLA_PLAN_FILE"] = plan_path
    calls, loaded = calls_path(env, req["run_id"]), calls_path(env, req["run_id"], "loaded")
    for f in (calls, loaded):
        try:
            os.remove(f)
        except OSError:
            pass
    env["FLOTILLA_CALLS_FILE"], env["FLOTILLA_LOADED_FILE"] = calls, loaded
    timeout = float(req["timeout_s"])
    start = time.monotonic()
    # so a run that gets SIGTERM can tell its own timeout (kill at the deadline) from a cancel or a restart
    env["FLOTILLA_RUN_DEADLINE"] = repr(time.time() + timeout)
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
    while True:
        try:
            proc.wait(timeout=0.2)
            break
        except subprocess.TimeoutExpired:
            pass
        if cancel.is_set():
            status = "cancelled"
            _kill_group(proc)
            break
        if time.monotonic() - start >= timeout:
            status = "timeout"
            _kill_group(proc)
            break
    reader.join(timeout=KILL_GRACE_S)
    if status == "ok" and proc.returncode != 0:
        status = "error"
    out = b"".join(chunks).decode("utf-8", "replace")
    spend = spend_report(spend_path)
    kept, dropped = read_calls_counted(calls)
    res = {"status": status, "exit_code": proc.returncode, "duration_s": round(time.monotonic() - start, 3),
           "spend_usd": spend[0], "spend_reported": spend[1], "output_tail": out[-TAIL:], "calls": kept,
           "loaded": read_calls(loaded, LOADED_MAX)}
    if dropped:
        res["calls_dropped"] = dropped
    plan, plan_error = read_plan(plan_path)
    if status == "ok":  # a failed run's plan is not a decision
        if plan is not None:
            res["plan"] = plan
        if plan_error:
            res["plan_error"] = plan_error
    return res


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
        if self.path.startswith("/v1/runs/") and self.path.endswith("/cancel"):
            run_id = self.path[len("/v1/runs/"):-len("/cancel")]
            if not run_id or "/" in run_id or not cancel_run(run_id):
                return self._send(404, {"error": "no such run here"})
            log("run cancel requested", run_id=run_id, level="warning")
            return self._send(200, {"ok": True, "run_id": run_id})
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
            if "api_token" in req and not (isinstance(req["api_token"], str) and TOKEN_RE.match(req["api_token"])):
                raise ValueError("api_token must be 16 to 256 characters of A-Z a-z 0-9 _ -")
            if "shadow" in req and not isinstance(req["shadow"], bool):
                raise ValueError("shadow must be true or false")
            resolve(CODE_ROOT, req.get("workdir"))
        except (ValueError, json.JSONDecodeError) as e:
            return self._send(400, {"error": str(e)})
        with _lock:
            if req["routine"] in _running:
                return self._send(409, {"error": f"{req['routine']} is already running ({_running[req['routine']]})"})
            _running[req["routine"]] = req["run_id"]
            cancel = _cancels[req["run_id"]] = threading.Event()
        marks = run_marks(req)
        log("run start", run_id=req["run_id"], routine=req["routine"], **marks)
        try:
            res = run_job(req, cancel=cancel)
        except Exception as e:  # noqa: BLE001 — report, never crash the server
            log("run failed to start", level="error", run_id=req["run_id"], routine=req["routine"], error=str(e), **marks)
            res = {"status": "error", "exit_code": None, "duration_s": 0, "spend_usd": 0, "output_tail": str(e)}
        finally:
            with _lock:
                _running.pop(req["routine"], None)
                _cancels.pop(req["run_id"], None)
        log("run end", run_id=req["run_id"], routine=req["routine"], status=res["status"], exit_code=res["exit_code"],
            duration_s=res["duration_s"], spend_usd=res["spend_usd"],
            level="info" if res["status"] == "ok" else "warning", **marks)
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
