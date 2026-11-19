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
  GET  /healthz   {ok, worker, version, running, mode}   (mode: live, or shadow)
  GET  /livez     {ok} without a token: the container healthcheck

A live worker also keeps the state snapshots a shadow starts from (v2 step 2, docs/wip/v2-design.md section 4):
  POST /v1/snapshots {run_id, paths}       copies the routine's state paths (relative to HOME) into <snapshot dir>/<run_id>/
                  (mode 0700, never through a symlink, at most SNAPSHOT_MAX_BYTES and SNAPSHOT_MAX_FILES) and answers
                  {run_id, bytes, files, skipped, missing}; made whole before it is visible, 409 when one exists
  POST /v1/snapshots/<run_id>/delete       removes it ({ok, existed}); a second delete is fine
  GET  /v1/snapshots/<run_id>              the snapshot as a tar.gz (an export of a pinned input)
  POST /v1/commit {repo, branch}           the head of the branch as the repository's API says it ({sha}), read as a routine would
                  read it (GITHUB_API from the worker's own env files, through the same proxy); a live worker only. flotillad
                  asks it at dispatch for a routine that names a repo, and hands that one commit to the live run and its shadows
A shadow worker (FLOTILLA_WORKER_MODE=shadow) runs shadow runs only and a live worker runs none (409 on a mismatch); it reads
the snapshot directory and copies the run's snapshot_of into its own HOME before the command starts.

Every request needs `Authorization: Bearer <the worker token>`; nothing else on the network can
start a run. One run per routine at a time (a second start of a running routine is 409).

A run's environment is built, never inherited: the worker's own environment (which holds its token)
does not reach the routine.
  1. PATH, HOME, LANG;
  2. every KEY=VAL of the env files in FLOTILLA_ENV_FILES, then FLOTILLA_READ_ENV_FILES (the credentials that can only read) and
     FLOTILLA_WRITE_ENV_FILES (the ones that can write), colon-separated, read per run, so a rotated secret is picked up
     without a restart. A shadow worker loads the read files only, refuses to start with FLOTILLA_ENV_FILES set or any write
     file present, and refuses a shadow run whose built environment holds a key of a write file or of FLOTILLA_WRITE_KEYS;
  3. every ROUTINE_<KEY> of the worker's environment, as <KEY> (this installation's overrides, e.g. the
     service URLs inside the container network);
  4. the request's env (FLEETD_STAGE, FLEETD_CYCLE_ID, FLEETD_BUDGET_LEFT, TRACEPARENT);
  5. FLOTILLA_API_TOKEN_FILE, when the request has an api_token: the run's own token file;
  6. for a shadow run: FLOTILLA_SHADOW=1 and FLEETD_STAGE=shadow, whatever the request said.
"""
import hmac
import json
import os
import re
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse
import urllib.request
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

WORKER = os.environ.get("FLOTILLA_WORKER", "worker")
VERSION = os.environ.get("FLOTILLA_VERSION", "dev")
CODE_ROOT = os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla")
KILL_GRACE_S = 10
TAIL = 4000
MAX_BODY = 1 << 20
MODE_LIVE, MODE_SHADOW = "live", "shadow"
SNAPSHOT_DIR = os.environ.get("FLOTILLA_SNAPSHOT_DIR", "/snapshots")
SNAPSHOT_MAX_BYTES = int(os.environ.get("FLOTILLA_SNAPSHOT_MAX_BYTES", 256 << 20))
SNAPSHOT_MAX_FILES = int(os.environ.get("FLOTILLA_SNAPSHOT_MAX_FILES", 50000))
SHADOW_NICE = 10   # a shadow run's priority (`nice -n`): below the live run's, so it cannot slow it

_running = {}
_cancels = {}   # run id -> the Event a cancel sets; present from the run's acceptance to its answer
_marks = {}     # run id -> the fields every log line of that run carries (a shadow run's), while it is in flight
_lock = threading.Lock()


class Refused(Exception):
    """A request this worker will not carry out as asked (409): the wrong mode for it, a write credential where a shadow
    runs, a snapshot that is not there or already is."""


class TooBig(Exception):
    """A snapshot or a state copy over its caps (413)."""


def worker_mode(environ=None):
    environ = os.environ if environ is None else environ
    mode = environ.get("FLOTILLA_WORKER_MODE") or MODE_LIVE
    if mode not in (MODE_LIVE, MODE_SHADOW):
        raise ValueError(f"FLOTILLA_WORKER_MODE={mode!r}: live (the default) or shadow")
    return mode


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


def env_paths(environ, name):
    return [p for p in environ.get(name, "").split(":") if p]


def build_env(request_env, environ=None):
    environ = os.environ if environ is None else environ
    env = {k: environ[k] for k in ("PATH", "HOME", "LANG", "FLOTILLA_VERSION", "FLOTILLA_WORKER") if k in environ}
    if worker_mode(environ) == MODE_SHADOW:
        if environ.get("FLOTILLA_ENV_FILES"):
            raise Refused("a shadow worker is configured with FLOTILLA_ENV_FILES, which does not say which credentials write: "
                          "split it into FLOTILLA_READ_ENV_FILES and FLOTILLA_WRITE_ENV_FILES and give this worker the read ones")
        files = env_paths(environ, "FLOTILLA_READ_ENV_FILES")
    else:
        files = (env_paths(environ, "FLOTILLA_ENV_FILES") + env_paths(environ, "FLOTILLA_READ_ENV_FILES")
                 + env_paths(environ, "FLOTILLA_WRITE_ENV_FILES"))
    for path in files:
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


def write_keys(environ=None):
    """The names of the credentials that can write: the keys of FLOTILLA_WRITE_ENV_FILES (files that are there) and the keys
    FLOTILLA_WRITE_KEYS lists, comma-separated. Names only, never values."""
    environ = os.environ if environ is None else environ
    keys = {k.strip() for k in environ.get("FLOTILLA_WRITE_KEYS", "").split(",") if k.strip()}
    for path in env_paths(environ, "FLOTILLA_WRITE_ENV_FILES"):
        if os.path.exists(path):
            keys |= set(read_env_file(path))
    return keys


def check_mode(req, environ=None):
    """A shadow run only on a shadow worker, and nothing else there: a live worker running a shadow would hand it the live
    credentials, a shadow worker running a live run would let it act with none. (A remote host agent that predates this field
    would drop it and run a shadow as live; flotillad refuses a remote shadow unless /healthz says mode.)"""
    mode = worker_mode(environ)
    if req.get("shadow") and mode != MODE_SHADOW:
        raise Refused("this worker is not a shadow worker: it does not run a shadow run")
    if not req.get("shadow") and mode == MODE_SHADOW:
        raise Refused("this is a shadow worker: it runs shadow runs only")


def check_no_write_credential(env, environ=None):
    leaked = sorted(write_keys(environ) & set(env))
    if leaked:
        raise Refused("a shadow run's environment holds write credentials (" + ", ".join(leaked) + "): refused")


def safe_id(run_id):
    return "".join(c if c.isalnum() or c in "._-" else "_" for c in str(run_id))[:120]


def snapshot_path(run_id, snapshot_dir=None):
    sid = safe_id(run_id)
    if not sid or sid.strip(".") == "":
        raise ValueError("a snapshot needs a run id")
    return os.path.join(SNAPSHOT_DIR if snapshot_dir is None else snapshot_dir, sid)


def copy_bounded(src, dst, budget):
    """Copy a file or a tree (src) to dst: regular files only, never through a symlink (a link is skipped and counted),
    directories 0700 and files 0600, and at most budget["bytes"] bytes and budget["files"] files in all, counted across calls
    (TooBig past it). Updates the budget's totals; returns nothing."""
    if os.path.islink(src) or not (os.path.isdir(src) or os.path.isfile(src)):
        budget["skipped"] += 1
        return
    if os.path.isfile(src):
        _copy_file(src, dst, budget)
        return
    os.makedirs(dst, mode=0o700, exist_ok=True)
    for root, dirs, files in os.walk(src, followlinks=False):
        rel = os.path.relpath(root, src)
        out = dst if rel == "." else os.path.join(dst, rel)
        os.makedirs(out, mode=0o700, exist_ok=True)
        for d in list(dirs):
            if os.path.islink(os.path.join(root, d)):
                budget["skipped"] += 1
                dirs.remove(d)
        for f in files:
            _copy_file(os.path.join(root, f), os.path.join(out, f), budget)


def _copy_file(src, dst, budget):
    try:
        fd = os.open(src, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    except OSError:
        budget["skipped"] += 1   # a symlink (O_NOFOLLOW), or a file that went away
        return
    with os.fdopen(fd, "rb") as fin:
        if not stat.S_ISREG(os.fstat(fin.fileno()).st_mode):
            budget["skipped"] += 1
            return
        if budget["files"] + 1 > SNAPSHOT_MAX_FILES:
            raise TooBig(f"more than {SNAPSHOT_MAX_FILES} files")
        budget["files"] += 1
        out = os.open(dst, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(out, "wb") as fout:
            while True:
                chunk = fin.read(1 << 20)
                if not chunk:
                    break
                budget["bytes"] += len(chunk)
                if budget["bytes"] > SNAPSHOT_MAX_BYTES:
                    raise TooBig(f"more than {SNAPSHOT_MAX_BYTES} bytes")
                fout.write(chunk)


def new_budget():
    return {"bytes": 0, "files": 0, "skipped": 0}


def clean_state_path(p):
    if not isinstance(p, str) or not p or p.startswith("/") or "\x00" in p:
        raise ValueError(f"a state path is relative to HOME, got {p!r}")
    parts = [x for x in p.split("/") if x not in ("", ".")]
    if not parts or ".." in parts:
        raise ValueError(f"a state path stays inside HOME and names something below it, got {p!r}")
    return "/".join(parts)


def make_snapshot(run_id, paths, environ=None, snapshot_dir=None):
    """Copy the routine's state paths (under HOME) into <snapshot dir>/<run_id>/, whole or not at all. A live worker only."""
    environ = os.environ if environ is None else environ
    if worker_mode(environ) != MODE_LIVE:
        raise Refused("a shadow worker takes no snapshots: it only reads them")
    if not isinstance(paths, list) or not paths:
        raise ValueError("paths: a list of state paths, at least one")
    paths = [clean_state_path(p) for p in paths]
    home = environ.get("HOME") or "/"
    final = snapshot_path(run_id, snapshot_dir)
    if os.path.exists(final):
        raise Refused(f"a snapshot of {run_id} already exists")
    tmp = final + ".partial"
    shutil.rmtree(tmp, ignore_errors=True)
    os.makedirs(tmp, mode=0o700)
    budget, missing = new_budget(), []
    try:
        for p in paths:
            src = resolve(home, p)
            if not os.path.lexists(src):
                missing.append(p)
                continue
            dst = os.path.join(tmp, p)
            os.makedirs(os.path.dirname(dst), mode=0o700, exist_ok=True)
            copy_bounded(src, dst, budget)
        os.rename(tmp, final)
    except BaseException:
        shutil.rmtree(tmp, ignore_errors=True)
        raise
    return {"run_id": run_id, "bytes": budget["bytes"], "files": budget["files"], "skipped": budget["skipped"], "missing": missing}


def delete_snapshot(run_id, snapshot_dir=None):
    final = snapshot_path(run_id, snapshot_dir)
    existed = os.path.exists(final)
    shutil.rmtree(final, ignore_errors=True)
    shutil.rmtree(final + ".partial", ignore_errors=True)
    return existed


def tar_snapshot(run_id, snapshot_dir=None):
    """The snapshot as a tar.gz in a temporary file, rewound; None when there is none."""
    final = snapshot_path(run_id, snapshot_dir)
    if not os.path.isdir(final):
        return None
    out = tempfile.TemporaryFile()
    with tarfile.open(fileobj=out, mode="w:gz") as tf:
        tf.add(final, arcname=".", recursive=True)
    out.seek(0)
    return out


REPO_RE = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")
BRANCH_RE = re.compile(r"^[A-Za-z0-9._/-]{1,100}$")
SHA_RE = re.compile(r"^[0-9a-f]{40}([0-9a-f]{24})?$")


def resolve_commit(repo, branch, environ=None, opener=None):
    """The commit a branch points at, from the repository's API (GITHUB_API, default https://api.github.com), read with the
    environment and proxy a live routine has. Names only a plain repository and branch; the answer must be a commit id."""
    environ = os.environ if environ is None else environ
    if worker_mode(environ) != MODE_LIVE:
        raise Refused("a shadow worker resolves no commits: flotillad asks the live worker")
    if not isinstance(repo, str) or not REPO_RE.match(repo):
        raise ValueError("repo is owner/name")
    if not isinstance(branch, str) or not BRANCH_RE.match(branch) or ".." in branch:
        raise ValueError("branch is a plain branch name")
    env = build_env({}, environ)
    api = (env.get("GITHUB_API") or "https://api.github.com").rstrip("/")
    url = f"{api}/repos/{repo}/commits/{urllib.parse.quote(branch, safe='/')}"
    if opener is None:
        proxies = {k.lower().split("_")[0]: env[k] for k in ("HTTP_PROXY", "HTTPS_PROXY") if env.get(k)}
        opener = urllib.request.build_opener(urllib.request.ProxyHandler(proxies)).open
    req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json", "User-Agent": "flotilla-worker"})
    with opener(req, timeout=15) as r:
        sha = json.load(r).get("sha")
    if not isinstance(sha, str) or not SHA_RE.match(sha):
        raise OSError("the repository's API gave no commit id")
    return sha


def restore_state(req, env, environ=None, snapshot_dir=None):
    """A shadow run's state: for each of the routine's state paths, remove what is in this worker's own HOME and copy the
    live run's snapshot of it (req snapshot_of) in. Bounded by the snapshot caps, never through a symlink, never outside HOME."""
    paths = req.get("state") or []
    if not paths:
        return
    sid = req.get("snapshot_of")
    if not sid:
        raise Refused("a shadow run with state paths needs snapshot_of: the live run whose snapshot it starts from")
    src_root = snapshot_path(sid, snapshot_dir)
    if not os.path.isdir(src_root):
        raise Refused(f"no snapshot of {sid} here (deleted, never taken, or not on this host)")
    home = env.get("HOME") or (environ or os.environ).get("HOME") or "/"
    budget = new_budget()
    for p in [clean_state_path(x) for x in paths]:
        dst = resolve(home, p)
        if dst == os.path.realpath(home):
            raise ValueError(f"state path {p!r} is the whole of HOME")
        if os.path.islink(os.path.join(home, p)):
            os.remove(os.path.join(home, p))
        elif os.path.isdir(dst):
            shutil.rmtree(dst)
        elif os.path.lexists(dst):
            os.remove(dst)
        src = os.path.join(src_root, p)
        if not os.path.lexists(src):
            continue   # the live routine had nothing there: the shadow starts with nothing
        os.makedirs(os.path.dirname(dst), mode=0o700, exist_ok=True)
        copy_bounded(resolve(src_root, p), dst, budget)


TOKEN_RE = re.compile(r"^[A-Za-z0-9_-]{16,256}$")


def token_path(env, run_id):
    """Where a run's own token file is: its temp directory (the calls file's), one per run. The file is mode 0600 but the same uid
    as every routine on this worker: another routine running at the same time can read it, as it can read the worker's own
    secrets. That is why the token is the run's (it names the run's routine and version, and ends with the run) and not a master."""
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


def prepare_env(req, environ=None):
    """The run's built environment, or a Refused. Side-effect free: the HTTP handler calls it before accepting the run, so a
    refusal is a 409 and not a started run that failed."""
    environ = os.environ if environ is None else environ
    check_mode(req, environ)
    env = build_env(req.get("env"), environ)
    if req.get("shadow"):
        check_no_write_credential(env, environ)
        env["FLOTILLA_SHADOW"], env["FLEETD_STAGE"] = "1", "shadow"
        if not shutil.which("nice", path=env.get("PATH")):
            raise Refused("nice is not on this worker's PATH: a shadow runs at a lower priority than the live run, or not at all")
        if req.get("state"):
            sid = req.get("snapshot_of")
            if not sid:
                raise Refused("a shadow run with state paths needs snapshot_of: the live run whose snapshot it starts from")
            if not os.path.isdir(snapshot_path(sid)):
                raise Refused(f"no snapshot of {sid} here (deleted, never taken, or not on this host)")
    return env


def run_job(req, environ=None, cancel=None):
    environ = os.environ if environ is None else environ
    env = prepare_env(req, environ)
    marks = run_marks(req)
    if marks:
        _marks[req["run_id"]] = marks
    token_file = ""
    if req.get("api_token"):
        token_file = token_path(env, req["run_id"])
        write_run_token(token_file, req["api_token"])
        env["FLOTILLA_API_TOKEN_FILE"] = token_file   # over a ROUTINE_ value: the run's own token, not the worker's
    try:
        if req.get("shadow"):
            restore_state(req, env, environ)
        return _run_job(req, env, cancel, shadow=bool(req.get("shadow")))
    finally:
        if marks:
            _marks.pop(req["run_id"], None)
        if token_file:
            try:
                os.remove(token_file)
            except OSError:
                pass


def _run_job(req, env, cancel, shadow=False):
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
    argv = ["sh", "-c", req["command"]]
    if shadow:
        # `nice` as a prefix, not a preexec_fn: a Python callback between fork and exec in a threaded server can deadlock the child
        argv = [shutil.which("nice", path=env.get("PATH")) or "nice", "-n", str(SHADOW_NICE)] + argv
    proc = subprocess.Popen(argv, cwd=workdir, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
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
        if self.path.startswith("/v1/snapshots/"):
            return self._snapshot_get(self.path[len("/v1/snapshots/"):])
        if self.path != "/healthz":
            return self._send(404, {"error": "not found"})
        with _lock:
            running = sorted(_running)
        self._send(200, {"ok": True, "worker": WORKER, "version": VERSION, "running": running, "mode": worker_mode()})

    def _snapshot_get(self, run_id):
        if worker_mode() != MODE_LIVE:
            return self._send(409, {"error": "a shadow worker keeps no snapshots"})
        try:
            f = tar_snapshot(run_id) if run_id and "/" not in run_id else None
        except ValueError as e:
            return self._send(400, {"error": str(e)})
        if f is None:
            return self._send(404, {"error": "no such snapshot here"})
        with f:
            size = f.seek(0, os.SEEK_END)
            f.seek(0)
            self._status = 200
            self.send_response(200)
            self.send_header("Content-Type", "application/gzip")
            self.send_header("Content-Length", str(size))
            self.end_headers()
            shutil.copyfileobj(f, self.wfile)

    def _commit_post(self):
        n = int(self.headers.get("Content-Length") or 0)
        try:
            if n <= 0 or n > MAX_BODY:
                raise ValueError("body required, at most 1 MiB")
            body = json.loads(self.rfile.read(n))
            if not isinstance(body, dict):
                raise ValueError("a JSON object: {repo, branch}")
            sha = resolve_commit(body.get("repo"), body.get("branch"))
            return self._send(200, {"sha": sha})
        except Refused as e:
            return self._send(409, {"error": str(e)})
        except (ValueError, json.JSONDecodeError) as e:
            return self._send(400, {"error": str(e)})
        except OSError as e:
            log("commit not resolved", level="warning", error=str(e))
            return self._send(502, {"error": f"the repository's API: {e}"})

    def _snapshots_post(self, path):
        n = int(self.headers.get("Content-Length") or 0)
        try:
            if path.endswith("/delete"):
                run_id = path[len("/v1/snapshots/"):-len("/delete")]
                if not run_id or "/" in run_id:
                    raise ValueError("a snapshot is named by its run id")
                if worker_mode() != MODE_LIVE:
                    raise Refused("a shadow worker keeps no snapshots")
                existed = delete_snapshot(run_id)
                log("snapshot deleted", run_id=run_id, existed=existed)
                return self._send(200, {"ok": True, "run_id": run_id, "existed": existed})
            if path != "/v1/snapshots":
                return self._send(404, {"error": "not found"})
            if n <= 0 or n > MAX_BODY:
                return self._send(400, {"error": "body required, at most 1 MiB"})
            body = json.loads(self.rfile.read(n))
            if not isinstance(body, dict) or not isinstance(body.get("run_id"), str) or not body["run_id"]:
                raise ValueError("run_id (the live run) is required")
            res = make_snapshot(body["run_id"], body.get("paths"))
            log("snapshot taken", run_id=body["run_id"], bytes=res["bytes"], files=res["files"], skipped=res["skipped"], missing=len(res["missing"]))
            return self._send(200, res)
        except Refused as e:
            return self._send(409, {"error": str(e)})
        except TooBig as e:
            log("snapshot refused: too big", level="warning", error=str(e))
            return self._send(413, {"error": f"the state is too big to snapshot: {e}"})
        except (ValueError, json.JSONDecodeError) as e:
            return self._send(400, {"error": str(e)})
        except OSError as e:
            log("snapshot failed", level="error", error=str(e))
            return self._send(500, {"error": f"snapshot: {e.strerror or e}"})

    def do_POST(self):
        if not self._authorized():
            return self._send(401, {"error": "unauthorized"})
        if self.path.startswith("/v1/snapshots"):
            return self._snapshots_post(self.path)
        if self.path == "/v1/commit":
            return self._commit_post()
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
            if "snapshot_of" in req and not (isinstance(req["snapshot_of"], str) and req["snapshot_of"]):
                raise ValueError("snapshot_of must be a run id")
            if "state" in req:
                if not isinstance(req["state"], list):
                    raise ValueError("state must be a list of paths")
                req["state"] = [clean_state_path(p) for p in req["state"]]
            if "input_key" in req and not isinstance(req["input_key"], str):
                raise ValueError("input_key must be a string")
            resolve(CODE_ROOT, req.get("workdir"))
        except (ValueError, json.JSONDecodeError) as e:
            return self._send(400, {"error": str(e)})
        try:
            prepare_env(req)   # a refusal is a 409 before anything starts
        except Refused as e:
            log("run refused", level="warning", run_id=req["run_id"], routine=req["routine"], error=str(e), **run_marks(req))
            return self._send(409, {"error": str(e)})
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


def check_start(environ=None):
    """What a worker refuses to start with: a bad mode, and for a shadow worker any way to reach a write credential."""
    environ = os.environ if environ is None else environ
    if worker_mode(environ) == MODE_SHADOW:
        if environ.get("FLOTILLA_ENV_FILES"):
            raise Refused("a shadow worker with FLOTILLA_ENV_FILES set: split it into FLOTILLA_READ_ENV_FILES and "
                          "FLOTILLA_WRITE_ENV_FILES, and give a shadow worker the read ones only")
        present = [p for p in env_paths(environ, "FLOTILLA_WRITE_ENV_FILES") if os.path.exists(p)]
        if present:
            raise Refused("a shadow worker can read a write credential file (" + ", ".join(present) + "): it must not")


def main():
    if len(worker_token()) < 32:
        sys.exit("no worker token of at least 32 characters (FLOTILLA_WORKER_TOKEN_FILE or "
                 "FLOTILLA_WORKER_TOKEN): refusing to serve without auth")
    try:
        check_start()
    except (Refused, ValueError) as e:
        sys.exit(f"refusing to serve: {e}")
    addr = os.environ.get("FLOTILLA_WORKER_LISTEN", "0.0.0.0:8081")
    host, port = addr.rsplit(":", 1)
    srv = ThreadingHTTPServer((host, int(port)), Handler)
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=srv.shutdown, daemon=True).start())
    log("worker listening", addr=addr, version=VERSION, mode=worker_mode())
    srv.serve_forever()


if __name__ == "__main__":
    main()
