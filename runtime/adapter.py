"""runtime/adapter: the platform's coding harnesses behind one interface (ADR-0020, v2-design.md section 3).

A station that runs on a coding harness (pi, or another CLI that reads a prompt, works in a workspace and calls a model
through the installation's gateway) goes through this layer: the request a station makes, the command a harness turns
it into, the normalized episode a run leaves, the registry, and the shared runner (process group, stream tee, turn
cap, timeout). The caller reads only the Episode; a harness's own stream is kept beside it as the raw record. An
adapter is asked to serve a `tools` value (all, read, none), never a station name: `Request.station` is a label that
goes on the Episode and nowhere else, and the texts it delivers (the prompt, the system text) are the caller's.

The seams where a caller meets a harness (S1 to S11), who owns each, how pi satisfies it, the capability table and the
seam end-to-end every harness must pass are in runtime/HARNESS_SEAMS.md: the source of truth, changed first when a
seam changes. The capability table is generated from the adapters into fleets/harnesses.json
(`python3 runtime/adapter.py capabilities --write`), which flotillad checks a version's stations against.

HOW TO ADD A HARNESS

  1. One file, runtime/harness_<name>.py (lower case letters and digits, at most 16; stdlib only: the worker image has
     nothing else). It subclasses Harness, and ends with `adapter.register(<Class>())`. adapter.get("<name>")
     imports the file by its name; nothing else is edited. harness_prompts, harness_cli and harness_probe are not
     adapters.
  2. Class attributes: `name`; `tools_served`, the `tools` values it can serve (a subset of TOOLS); `can_resume`,
     whether a later call can continue a session (False: the caller starts a fresh episode); `enforces_readonly`,
     whether the harness itself stops a read station writing (not just being asked: a harness that cannot says False,
     and the runner checks the workspace after a read or none station instead, Request.workdir); `tees`, True unless
     the stream must not be written beside the run (pi, which keeps its own session files). The other capabilities
     (CAPABILITIES) are declared too, each True or False and never left to a default that flatters: a spec that asks
     for a capability the harness does not declare is rejected (missing(), and flotillad's -check against
     fleets/harnesses.json), and a test keeps that file and the table in runtime/HARNESS_SEAMS.md equal to the adapters.
  3. build(req) -> Invocation. The command, the environment it adds, and what the run must find first.
       - req.prompt is the caller's complete prompt: add no task text. A delta the harness cannot avoid (the system
         text delivered as a prompt prefix because it has no system-prompt flag, a submit protocol) goes in
         Invocation.notes; the caller records each note on the episode.
       - The prompt goes on stdin (Invocation.stdin), never in argv: no 128 KiB limit, nothing in `ps`. The CLI must
         not wait on an inherited stdin: give it the prompt or an empty string.
       - The key is never in argv or in a file of the repository: read_key(req.model, environ) at build time, set
         in Invocation.env only; or a config file that names where the CLI reads it at call time. A config file goes
         in Invocation.files (relative to req.run_dir, written mode 600, deleted with the run), never anywhere else.
       - HOME is already req.run_dir: the CLI's session store lands inside the run directory, where a checkpoint
         can keep it. req.workdir is the workspace and the cwd. req.tools is "all" | "read" | "none"; for "read" and
         "none" turn off writing and shell with the harness's own switches when it has them.
       - req.system_path is the system text to add to the harness's own (a file, hash-checked by the caller): deliver it
         as the harness's system prompt addition, else as a prefix to the prompt and say so in notes. Stop the harness
         reading the workspace's own AGENTS.md or CLAUDE.md (the version's texts replace them).
       - req.max_turns is the caller's cap: the runner counts turns from turn_ended and kills the process group. Set the
         harness's own cap one above it, as a backstop, when it has one.
       - req.resume, when set, is the resume_ref of an earlier Episode: build the resume invocation (the prompt is the
         next message). A resume the harness cannot honour must say so in build (HarnessError), never start a fresh
         session silently.
       - Invocation.outputs lists the files the run writes that parse() needs (relative to req.run_dir); the runner
         reads them after the run and hands their text to parse() as `files`.
  4. turn_ended(line) -> bool. Called on each stdout line as it arrives: True when that line ends one model call
     (the unit of a turn cap). Cheap, never raises. A harness whose stream has no such line (a trajectory file
     written at the end) returns False and relies on its own cap plus the timeout.
  5. parse(stream_text, files) -> Episode. Never raises: a stream that is empty, garbled, or cut off by a kill
     gives an Episode with `error` set (and as many turns as were whole). Turns are normalized (Turn): text, tools as
     [{"name", "arg", "ok"}] (ok left out when the harness has no result for the call), usage with all five keys
     (0 for a part the harness does not report; None for the whole dict when it reports none), the harness's own
     cost estimate in cost_est (never the bill: the gateway's meter is that). Episode.answer is the last non-empty
     assistant text, stripped. exit_reason is completed, turn_cap (the harness's own cap fired) or error; the
     runner sets timeout and turn_cap from the exit code it saw.
  6. resume_ref(ep) -> str. What the caller passes as req.resume next time: normally ep.session_id. The caller keeps
     the episode file, so the reference survives a kill of the run.
  7. phone_home_env() -> dict. The environment variables that switch off the CLI's update checks, telemetry and
     model-catalog fetches: nothing on the egress allowlist may be needed for a run.
  8. Tests, in runtime/test_adapter_<name>.py (add it to the Makefile's test-unit lines):
       - sample streams under runtime/testdata/ (one success, one capped, one error). A sample no real run produced
         has a first line {"_synthetic": true, ...}, and the test names say so, until the installation records real ones.
       - fakeh.check_contract(self, ep) on every parsed sample: turns numbered from 1, usage non-negative, answer a
         stripped str, exit_reason one of EXIT_REASONS.
       - argv/env tests per `tools` value: the key is in no argv and no file but Invocation.files; the prompt is on
         stdin; read and none carry the read-only switches; the system text reaches the harness; phone_home_env is
         set; the session store is under run_dir; a resume builds the resume invocation.
       - the seam end-to-end: runtime/test_seam_<name>.py, a subclass of seam_e2e.SeamSuite (test_seam_pi.py is the
         shape): the real CLI, run through the station driver against a scripted model endpoint, every seam of
         HARNESS_SEAMS.md asserted. runtime/test_seam_fakeh.py runs the same suite over the fake CLI in CI.

What a caller keeps of an episode: <run_dir>/episodes/<stem>.json, the normalized Episode with its station, alias and
order (episode_doc, schema "version": EPISODE_VERSION), and <stem>.stream, the harness's native stdout as the runner tees
it. A version 1 file (the bugfix chain's, which says `role` where a version 2 says `station`) reads back by
read_episode and is still what the chain writes: bugfix/fix.py's episode_file.
"""
import hashlib
import importlib
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
from dataclasses import asdict, dataclass, field
from typing import Optional

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import toolrec  # noqa: E402

TURN_CAPPED = 125   # the exit code of a run the chain's turn cap killed
TIMED_OUT = 124     # and of one its timeout killed
TOOLS = ("all", "read", "none")
EXIT_REASONS = ("completed", "turn_cap", "timeout", "error")
EPISODES = "episodes"   # under the run directory
EPISODE_VERSION = 2
READABLE_EPISODE_VERSIONS = (1, 2)   # a bugfix chain's file of an earlier deploy says `role`, not `station`
NAME_RE = re.compile(r"[a-z][a-z0-9]{0,15}")
NOT_ADAPTERS = {"prompts", "cli", "probe"}   # harness_<these>.py are not harnesses


class HarnessError(Exception):
    """A request a harness cannot serve (an unknown name, a `tools` value it does not serve, a resume it cannot honour)."""


class ProcessFailed(Exception):
    """run_process(check=True): the command timed out or exited non-zero."""


@dataclass
class ModelRoute:
    """One gateway alias as a harness needs it. `model` is the name the endpoint serves for the alias; `meta` holds
    what a harness may use beyond that (pi: {"pi_provider": ...}; context window and prices when known)."""
    alias: str
    model: str
    base_url: str
    endpoint: str = ""
    key_file: str = ""
    key_env: str = ""
    meta: dict = field(default_factory=dict)


@dataclass
class Request:
    """What one station asks of a harness. An adapter reads no field to decide what the station is: `station` is the
    caller's label for the Episode and the records."""
    prompt: str                     # the caller's own prompt, complete; adapters never add task text
    system_path: Optional[str]      # the system text to add (a file the caller has hash-checked), or None
    model: ModelRoute
    tools: str                      # "all" | "read" | "none"
    resume: Optional[str]           # a resume_ref from an earlier Episode
    max_turns: int                  # 0 = none; the caller's own cap
    workdir: str                    # the workspace and the cwd
    run_dir: str                    # sessions and config live under it (HOME is already run_dir)
    timeout: int
    episode: str = ""               # the stem of the episode's files under run_dir/episodes ("" = not kept)
    station: str = ""               # a free label (the version's own station name); recorded on the Episode, never read by an adapter


@dataclass
class Invocation:
    argv: list
    env: dict = field(default_factory=dict)      # added to the chain's environment, not a replacement
    stdin: Optional[str] = None                  # None: inherited (a caller's own legacy call only; adapters send the prompt)
    cwd: str = ""                                # "": the request's workdir
    files: dict = field(default_factory=dict)    # config files to write first (mode 600), relative to run_dir
    stream_path: str = ""                        # "": the runner's default under run_dir/episodes (tees only)
    outputs: list = field(default_factory=list)  # files the run writes that parse() reads, relative to run_dir
    notes: list = field(default_factory=list)    # prompt deltas and other recorded differences from the caller's text


@dataclass
class Turn:
    """One model call."""
    n: int
    model: Optional[str] = None
    text: str = ""
    tools: list = field(default_factory=list)      # [{"name", "arg", "ok"}] and, when the stream has them, "args", "result", "duration_s" (action())
    usage: Optional[dict] = None                   # usage_dict(); None when the harness reports none
    cost_est: Optional[float] = None               # the harness's own estimate, never authoritative
    stop: str = ""                                 # stop, tool_calls, length, error (OpenAI's finish reasons)
    error: Optional[str] = None
    time: Optional[str] = None                     # when the call ended (UTC, RFC 3339), when the stream says
    texts: list = field(default_factory=list)      # [{"name", "sha256", "bytes"}]: the rendered texts it read, set by the caller


@dataclass
class Episode:
    harness: str
    station: str = ""   # the Request's label
    session_id: Optional[str] = None
    turns: list = field(default_factory=list)
    answer: str = ""                # the last non-empty assistant text (what fix.assistant_text() gives for pi)
    error: str = ""
    exit_reason: str = "completed"  # one of EXIT_REASONS
    usage_total: Optional[dict] = None
    cost_est: Optional[float] = None
    raw_path: str = ""              # the native stream's path relative to the run directory, "" when not kept
    notes: list = field(default_factory=list)

    def to_dict(self):
        d = asdict(self)
        for t in d["turns"]:   # a turn that has no time or texts says nothing of them: the bugfix chain's files stay as they were
            if t.get("time") is None:
                t.pop("time", None)
            if not t.get("texts"):
                t.pop("texts", None)
        return d

    @classmethod
    def from_dict(cls, d):
        turns = [Turn(**t) for t in d.get("turns") or []]
        keys = ("harness", "station", "session_id", "answer", "error", "exit_reason", "usage_total", "cost_est", "raw_path", "notes")
        got = {k: d[k] for k in keys if k in d}
        if "station" not in got and "role" in d:   # a version 1 document
            got["station"] = d["role"]
        return cls(turns=turns, **got)


USAGE_KEYS = ("input", "output", "cache_read", "cache_write", "reasoning")


def usage_dict(input=0, output=0, cache_read=0, cache_write=0, reasoning=0):
    """A Turn's usage: all five keys, ints, 0 for a part the harness does not report."""
    return {"input": int(input or 0), "output": int(output or 0), "cache_read": int(cache_read or 0),
            "cache_write": int(cache_write or 0), "reasoning": int(reasoning or 0)}


def sum_usage(turns):
    """The episode's usage: the turns' summed, None when no turn reports any."""
    got = [t.usage for t in turns if t.usage]
    return {k: sum(u.get(k, 0) for u in got) for k in USAGE_KEYS} if got else None


def sum_cost(turns):
    got = [t.cost_est for t in turns if t.cost_est is not None]
    return sum(got) if got else None


# What a harness declares it can do, per seam (runtime/HARNESS_SEAMS.md, the capability table). A spec that needs one is
# checked against the declaration before any model call: missing().
CAPABILITIES = {
    "can_resume": "S7: a later call can continue a session (a follow-up and a resumed run keep the conversation)",
    "resume_after_kill": "S7: a session whose run was killed can be continued (ADR-0019)",
    "enforces_readonly": "S6: the harness itself stops a read station writing or running a shell, not only the prompt",
    "supports_hooks": "S6: the platform can run a hook before or after each tool call (a spec's `hooks:`)",
    "reports_cost": "S9: the stream carries the harness's own cost estimate (never the bill; labelled)",
    "reports_usage": "S4: the stream carries tokens per model call",
    "supports_turn_cap": "S5: the stream has a line per model call, so the runner can cap the turns live",
    "native_system_prompt": "S2: the system text reaches the model as system text, not as a prefix to the prompt",
    "prompt_via_stdin": "S1: the prompt is delivered on stdin (False: in argv, which Linux caps at 128 KiB)",
}


class Harness:
    """What an adapter is. See the top of this file. The capabilities default to False: an adapter says what it can."""
    name = ""
    tools_served = frozenset()   # the `tools` values (TOOLS) it serves
    can_resume = False
    resume_after_kill = False
    enforces_readonly = False
    supports_hooks = False
    reports_cost = False
    reports_usage = False
    supports_turn_cap = False
    native_system_prompt = False
    prompt_via_stdin = False
    tees = True

    def build(self, req):
        raise NotImplementedError

    def turn_ended(self, line):
        raise NotImplementedError

    def parse(self, stream_text, files):
        raise NotImplementedError

    def resume_ref(self, ep):
        return ep.session_id or ""

    def phone_home_env(self):
        return {}


def capabilities(h):
    """{capability: bool} as the harness declares them."""
    return {k: bool(getattr(h, k, False)) for k in CAPABILITIES}


def missing(h, wants):
    """The reasons harness `h` cannot do what a step asks (`wants`: {capability: True} for each it needs, and "tools": the
    `tools` values its stations use); [] when it can. For a spec's check, before any model call."""
    out = []
    for k, v in (wants or {}).items():
        if k == "tools":
            out += [f"harness {h.name} does not serve tools={t}" for t in v if t not in h.tools_served]
        elif k not in CAPABILITIES:
            out.append(f"{k!r} is not a capability a harness declares (known: {', '.join(sorted(CAPABILITIES))})")
        elif v and not getattr(h, k, False):
            out.append(f"harness {h.name} does not declare {k}: {CAPABILITIES[k]}")
    return out


def capability_row(h):
    """The harness's row of HARNESS_SEAMS.md's capability table, as markdown."""
    return "| " + h.name + " | " + ", ".join(t for t in TOOLS if t in h.tools_served) + " | " + " | ".join(
        "yes" if getattr(h, k, False) else "no" for k in CAPABILITIES) + " |"


# the episode record: runtime/episode.schema.json

SCHEMA_VERSION = 1
RESULT_CAP = 8192   # characters of a tool's arguments or result a record keeps; above it, a hash and a length, never a cut text
# The schemas every episode is validated against: (name, file). eval-harness's generic episode schema is pending; when its file
# exists, add one entry here (and its copy under testdata/), and flotilla's stays a superset of it.
SCHEMAS = [("flotilla", os.path.join(HERE, "episode.schema.json"))]


def _redact_tree(x):
    if isinstance(x, str):
        return toolrec.redact(x)
    if isinstance(x, dict):
        return {k: _redact_tree(v) for k, v in x.items()}
    if isinstance(x, list):
        return [_redact_tree(v) for v in x]
    return x


def bounded(key, value, cap=RESULT_CAP):
    """{key: value} with credentials, e-mail and IPv4 addresses masked, or, when its text is over `cap` characters,
    {key_sha256, key_bytes} of the masked text: a record holds a masked text or a hash and a length, never a cut text and
    never a long one."""
    masked = _redact_tree(value)
    text = masked if isinstance(masked, str) else json.dumps(masked, sort_keys=True, separators=(",", ":"))
    if len(text) <= cap:
        return {key: masked}
    raw = text.encode("utf-8")
    return {key + "_sha256": hashlib.sha256(raw).hexdigest(), key + "_bytes": len(raw)}


def action(name, args=None, result=None, duration_s=None, ok=None):
    """One tool call as a Turn.tools entry: `arg` is the main argument cleaned for a line, `args` the whole call and `result`
    the tool's answer, both masked and bounded. What the harness does not report stays out (an `ok` or a `duration_s` the
    stream has no word for is not made up)."""
    out = {"name": str(name), "arg": toolrec.clean_arg(toolrec.primary_arg(args)) if args else ""}
    if isinstance(args, dict) and args:
        out.update(bounded("args", args))
    if isinstance(result, str):
        out.update(bounded("result", result))
    if isinstance(duration_s, (int, float)) and not isinstance(duration_s, bool) and duration_s >= 0:
        out["duration_s"] = duration_s
    if isinstance(ok, bool):
        out["ok"] = ok
    return out


def episode_record(ep, seq=None, version=None, spec_hash=None, cost=None):
    """The Episode as the episode schema's document. `cost` is {usd, source} from the caller when it has the gateway's figure;
    else the harness's own estimate labelled as one, else the explicit unknown. What an adapter cannot supply is left out."""
    if cost is None:
        cost = {"usd": ep.cost_est, "source": "harness-estimate"} if ep.cost_est is not None else {"usd": None, "source": "unknown"}
    rec = {"schema_version": SCHEMA_VERSION, "harness": ep.harness, "station": ep.station, "exit_reason": ep.exit_reason,
           "cost": dict(cost), "turns": []}
    for k, v in (("seq", seq), ("version", version), ("spec_hash", spec_hash), ("session_id", ep.session_id or None),
                 ("error", ep.error or None), ("answer", toolrec.redact_credentials(ep.answer) if ep.answer else None),
                 ("notes", list(ep.notes) if ep.notes else None), ("raw_path", ep.raw_path or None),
                 ("usage", dict(ep.usage_total) if ep.usage_total else None)):
        if v is not None:
            rec[k] = v
    for t in ep.turns:
        r = {"n": t.n}
        for k, v in (("time", t.time), ("model", t.model), ("text", toolrec.redact_credentials(t.text) if t.text else None),
                     ("texts", [dict(x) for x in t.texts] if t.texts else None), ("usage", dict(t.usage) if t.usage else None),
                     ("cost_est", t.cost_est), ("stop", t.stop or None), ("error", t.error)):
            if v is not None:
                r[k] = v
        if t.tools:
            r["actions"] = [dict(x) for x in t.tools]
        rec["turns"].append(r)
    return rec


def validate_episode(record):
    """The errors of an episode record against every schema in SCHEMAS ([] when it is valid against all)."""
    import schemacheck
    errors = []
    for name, path in SCHEMAS:
        with open(path) as f:
            schema = json.load(f)
        errors += [f"{name}: {e}" for e in schemacheck.validate(record, schema)]
    return errors


def iso_utc(ms):
    """An epoch in milliseconds as RFC 3339 UTC, None for anything that is not one."""
    if isinstance(ms, bool) or not isinstance(ms, (int, float)) or ms <= 0:
        return None
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(ms / 1000)) + ".%03dZ" % (int(ms) % 1000)


# the registry

_REGISTRY = {}


def register(h):
    if not NAME_RE.fullmatch(h.name or ""):
        raise HarnessError(f"harness name {h.name!r}: lower case letters and digits, at most 16")
    bad = set(h.tools_served) - set(TOOLS)
    if bad:
        raise HarnessError(f"harness {h.name}: unknown tools values {sorted(bad)} (known: {', '.join(TOOLS)})")
    _REGISTRY[h.name] = h
    return h


def unregister(name):
    _REGISTRY.pop(name, None)


def get(name):
    """The registered harness `name`, importing runtime/harness_<name>.py first when it is not loaded yet."""
    if name not in _REGISTRY and NAME_RE.fullmatch(name or "") and name not in NOT_ADAPTERS \
            and os.path.exists(os.path.join(HERE, f"harness_{name}.py")):
        importlib.import_module(f"harness_{name}")
    if name not in _REGISTRY:
        raise HarnessError(f"no harness {name!r} (known: {', '.join(names()) or 'none'})")
    return _REGISTRY[name]


def names():
    found = {f[len("harness_"):-len(".py")] for f in os.listdir(HERE) if f.startswith("harness_") and f.endswith(".py")}
    return sorted((found - NOT_ADAPTERS) | set(_REGISTRY))


def route(cfg, alias, meta=None):
    """The ModelRoute of an alias of FLOTILLA_MODELS (parsed)."""
    a = (cfg.get("aliases") or {}).get(alias)
    if not a:
        raise HarnessError(f"alias {alias!r} is not in FLOTILLA_MODELS")
    ep = (cfg.get("endpoints") or {}).get(a.get("endpoint"))
    if not ep:
        raise HarnessError(f"alias {alias!r}: endpoint {a.get('endpoint')!r} is not in FLOTILLA_MODELS")
    return ModelRoute(alias=alias, model=a["model"], base_url=ep["base_url"], endpoint=a["endpoint"],
                      key_file=ep.get("key_file") or "", key_env=ep.get("key_env") or "", meta=dict(meta or {}))


def read_key(route, environ):
    """The endpoint's key: its key_env from `environ`, else the contents of its key_file; "" when it has none (an
    endpoint that takes no Authorization). For a child's environment only: never argv, never a file."""
    if route.key_env and environ.get(route.key_env):
        return environ[route.key_env]
    if route.key_file:
        try:
            with open(route.key_file) as f:
                return f.read().strip()
        except OSError:
            return ""
    return ""


# the shared runner

def _tail(s, n):
    s = (s or "").strip()
    return s if len(s) <= n else "…" + s[-(n - 1):]


def _kill(p):
    try:
        os.killpg(p.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def run_process(cmd, cwd, env=None, timeout=None, check=False, max_turns=None, turn_ended=None, tee_path=None, stdin=None):
    """Run a command in its own process group; (exit code, combined output). A timeout kills the whole group and is
    exit 124: subprocess.run's timeout kills only the direct child, and a timed-out pi or vitest under `bash pi.sh` or
    `sh -c npx` kept running (holding the CPUs, editing the worktree the next round resets) until the worker's own run
    limit (the advisor's review, 2026-11-15). With max_turns the output is read as it comes, and the group is killed
    once that many turns have ended (turn_ended(line) says which lines end one): exit TURN_CAPPED. With tee_path every
    line is also written there as it arrives, so a run that is killed leaves its stream. `stdin` is written to the
    command's stdin and closed; None leaves stdin as the caller's. check: a timeout or a non-zero exit raises
    ProcessFailed (not with max_turns or tee_path, which read as they go)."""
    if max_turns and turn_ended is None:
        raise ValueError("max_turns needs turn_ended")
    p = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                         shell=isinstance(cmd, str), start_new_session=True,
                         **({"stdin": subprocess.PIPE} if stdin is not None else {}))
    if max_turns or tee_path:
        return _sh_capped(p, timeout, max_turns, turn_ended, tee_path, stdin)
    try:
        out, _ = p.communicate(input=stdin, timeout=timeout)
    except subprocess.TimeoutExpired:
        _kill(p)
        out, _ = p.communicate()
        if check:
            raise ProcessFailed(f"{cmd if isinstance(cmd, str) else ' '.join(cmd)}: no result in {timeout}s")
        return TIMED_OUT, out or ""
    except BaseException:
        # the child has its own session, out of reach of the worker's kill of the run's group: a run stopped
        # at its timeout must not leave a coding agent calling the model
        _kill(p)
        raise
    out = out or ""
    if check and p.returncode != 0:
        raise ProcessFailed(f"{cmd if isinstance(cmd, str) else ' '.join(cmd)}: exit {p.returncode}: {_tail(out, 300)}")
    return p.returncode, out


def _sh_capped(p, timeout, max_turns, turn_ended, tee_path=None, stdin=None):
    """run_process's streaming path: a reader thread keeps the lines, tees them and counts the turns; the caller waits
    for the exit or the timeout. Either limit kills the whole group, as the plain path does."""
    lines, capped = [], threading.Event()
    tee = None
    if tee_path:
        try:
            os.makedirs(os.path.dirname(tee_path), exist_ok=True)
            tee = open(tee_path, "w")
        except OSError:
            tee = None

    def feed():
        try:
            p.stdin.write(stdin)
        except (OSError, ValueError):
            pass
        finally:
            try:
                p.stdin.close()
            except (OSError, ValueError):
                pass

    def ends_turn(line):
        try:
            return turn_ended(line)
        except Exception:   # noqa: BLE001 -- a reader that died would leave the pipe undrained and the child blocked
            return False

    def read():
        nonlocal tee
        turns = 0
        for line in p.stdout:
            lines.append(line)
            if tee:
                try:
                    tee.write(line)
                    tee.flush()
                except (OSError, ValueError):
                    tee = None
            if max_turns and turns < max_turns and ends_turn(line):
                turns += 1
                if turns >= max_turns:
                    capped.set()
                    _kill(p)

    def close_tee():
        if not reader.is_alive():
            p.stdout.close()
        if tee is not None:
            try:
                tee.close()
            except OSError:
                pass

    reader = threading.Thread(target=read, daemon=True)
    reader.start()
    if stdin is not None:
        threading.Thread(target=feed, daemon=True).start()
    try:
        p.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        _kill(p)
        p.wait()
        reader.join(10)
        close_tee()
        return TIMED_OUT, "".join(lines)
    except BaseException:
        _kill(p)
        raise
    reader.join(10)
    close_tee()
    return (TURN_CAPPED if capped.is_set() else p.returncode), "".join(lines)


def _inside(run_dir, rel):
    if not isinstance(rel, str) or not rel or os.path.isabs(rel) or ".." in rel.split("/"):
        raise HarnessError(f"path {rel!r} is not inside the run directory")
    return os.path.join(run_dir, rel)


def execute(h, req, base_env, run=run_process):
    """Run one station through harness `h`: build the command, write its files (mode 600), run it in its own process
    group with the turn cap and a tee of the native stream, read the files parse() asked for, and return
    (Episode, exit code, the native stream). `run` is the process runner (a caller's own wrapper of run_process, so a
    test's patch of it holds). Not caught: a build's HarnessError, and the BaseException of a run the worker ended (the
    group is killed first)."""
    if req.tools not in h.tools_served:
        raise HarnessError(f"harness {h.name} does not serve tools={req.tools}")
    if req.resume and not h.can_resume:
        raise HarnessError(f"harness {h.name} cannot resume a session")
    inv = h.build(req)
    for rel, text in (inv.files or {}).items():
        path = _inside(req.run_dir, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write(text)
    env = dict(base_env)
    env.update(h.phone_home_env())
    env.update(inv.env or {})
    tee = None
    if h.tees:
        if inv.stream_path:
            tee = inv.stream_path
        elif req.episode:
            tee = os.path.join(req.run_dir, EPISODES, req.episode + ".stream")
    code, stream = run(inv.argv, inv.cwd or req.workdir, env, req.timeout, max_turns=req.max_turns or None,
                       turn_ended=h.turn_ended if req.max_turns else None, tee_path=tee, stdin=inv.stdin)
    files = {}
    for rel in inv.outputs or []:
        try:
            with open(_inside(req.run_dir, rel), errors="replace") as f:
                files[rel] = f.read()
        except (OSError, HarnessError):
            files[rel] = ""
    ep = h.parse(stream, files)
    ep.harness, ep.station = h.name, req.station
    if tee:
        ep.raw_path = os.path.relpath(tee, req.run_dir)
    ep.notes = list(inv.notes or []) + [n for n in ep.notes if n not in (inv.notes or [])]
    if code == TIMED_OUT:
        ep.exit_reason = "timeout"
    elif code == TURN_CAPPED:
        ep.exit_reason = "turn_cap"
    elif code != 0 and ep.exit_reason == "completed":
        ep.exit_reason, ep.error = "error", ep.error or f"exit {code}"
    return ep, code, stream


# the episode files

def episode_doc(ep, alias, seq, code, resume_ref=""):
    """What a caller keeps of an episode: the normalized Episode (schema EPISODE_VERSION) with the alias it ran under,
    its order in the run, the exit code the runner saw and the reference a later call resumes it by."""
    return dict(ep.to_dict(), version=EPISODE_VERSION, alias=alias, seq=seq, code=code, resume_ref=resume_ref)


def write_episode(run_dir, stem, doc):
    """Atomically: written beside, renamed over. Returns the path relative to run_dir."""
    d = os.path.join(run_dir, EPISODES)
    os.makedirs(d, exist_ok=True)
    path = os.path.join(d, stem + ".json")
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(doc, f)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)
    return os.path.join(EPISODES, stem + ".json")


def last_episode(run_dir):
    """The highest order number among the run's episode files (0 when there are none): a resumed run goes on from it."""
    n = 0
    for f in os.listdir(os.path.join(run_dir, EPISODES)) if os.path.isdir(os.path.join(run_dir, EPISODES)) else []:
        m = re.match(r"(\d+)-.*\.json$", f)
        if m:
            n = max(n, int(m.group(1)))
    return n


def read_episode(path):
    with open(path) as f:
        doc = json.load(f)
    if not isinstance(doc, dict) or doc.get("version") not in READABLE_EPISODE_VERSIONS:
        raise ValueError("not an episode file of version %s" % " or ".join(map(str, READABLE_EPISODE_VERSIONS)))
    if doc["version"] == 1 and "station" not in doc:
        doc = dict(doc, station=doc.get("role", ""))
    return doc


# the capability table as data: fleets/harnesses.json

HARNESSES_FILE = os.path.join(HERE, "..", "fleets", "harnesses.json")
HARNESSES_VERSION = 1

# `flotilla` is the platform's own in-process loop (runtime/harness.py): no process, so no adapter and no file to find by
# name; it declares what a station on it can do all the same. It serves `all` (every tool the station names) and `none`;
# `read` has no meaning for tools the runtime serves, so it is refused. No sessions to resume.
IN_PROCESS = {
    "flotilla": {"tools_served": ["all", "none"], "can_resume": False, "resume_after_kill": False, "enforces_readonly": False,
                 "supports_hooks": True, "reports_cost": True, "reports_usage": True, "supports_turn_cap": True,
                 "native_system_prompt": True, "prompt_via_stdin": False},
}


def capabilities_doc():
    """The document flotillad checks a version's stations against (fleets/harnesses.json): per harness, its `tools_served`
    and every capability in CAPABILITIES, from the adapters' own declarations and IN_PROCESS."""
    out = {}
    for name in names():
        h = get(name)
        out[name] = dict(capabilities(h), tools_served=[t for t in TOOLS if t in h.tools_served])
    for name, row in IN_PROCESS.items():
        if name in out:
            raise HarnessError(f"{name} is an adapter and an in-process harness")
        out[name] = {k: row[k] for k in sorted(row)}
    return {"version": HARNESSES_VERSION, "harnesses": out}


def capabilities_json():
    return json.dumps(capabilities_doc(), indent=2, sort_keys=True) + "\n"


def _cli(argv):
    if argv[:1] == ["capabilities"] and set(argv[1:]) <= {"--write"}:
        text = capabilities_json()
        if "--write" in argv:
            with open(HARNESSES_FILE, "w") as f:
                f.write(text)
        else:
            print(text, end="")
        return 0
    print("usage: python3 runtime/adapter.py capabilities [--write]   (--write: fleets/harnesses.json)")
    return 2


if __name__ == "__main__":
    import adapter as _adapter   # not __main__: harness_<name>.py imports `adapter` and registers into that module's registry
    sys.exit(_adapter._cli(sys.argv[1:]))
