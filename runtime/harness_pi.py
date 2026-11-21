"""runtime/harness_pi: pi (pi -p --mode json) as a harness (ADR-0020, v2-design.md section 3).

pi is the reference adapter. build() serves any station and any `tools` value: the prompt on stdin, the system text
with --append-system-prompt, the model route as a provider of its own models.json under <run_dir>/pi (the key is named
there as `!cat <file>` or `$VAR`, never written), the session store under <run_dir>/pi, and a resume by the session
reference. The pi stream's reading is here too: assistant_text, stream_error and usage (bugfix/fix.py keeps the names as
wrappers) and parse(), which turns a stream into the normalized Episode.

The bugfix chain's v1 calls (bugfix/fix.py: its fix and reproducer through harnesses/pi.sh and repro.sh, its review through
bugfix/pi_review.py) keep their command paths: bugfix/test_pi_golden.py pins them byte for byte. bugfix/fix2.py, the chain of a
version that declares stations, runs its fix, reproducer and review through this adapter; bugfix/differential.py lists what differs
from v1.
"""
import glob
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adapter  # noqa: E402

# SR-0001 H8: the steps that only read (verdict, advisor, review) get pi's read tools, no shell and no
# writes; "read only" in a prompt did not stop the advisor starting a dev server. The fix step keeps pi's own.
READ_ONLY_TOOLS = "read,grep,find,ls"

# pi's stop reasons as the normalized ones (OpenAI's finish reasons)
STOP = {"toolUse": "tool_calls", "stop": "stop", "length": "length", "error": "error"}


def turn_ended(line):
    """Whether this stdout line is the end of an assistant message: one model call of pi's JSON stream."""
    if '"message_end"' not in line:
        return False
    try:
        e = json.loads(line)
    except ValueError:
        return False
    return isinstance(e, dict) and (e.get("message") or {}).get("role") == "assistant"


def assistant_text(stream):
    """The assistant's final text from a pi --mode json stream: the last message_end of the assistant."""
    text = ""
    for line in (stream or "").splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            e = json.loads(line)
        except ValueError:
            continue
        m = e.get("message") or {}
        if e.get("type") == "message_end" and m.get("role") == "assistant":
            t = "".join(p.get("text") or "" for p in m.get("content") or [] if isinstance(p, dict) and p.get("type") == "text")
            if t.strip():
                text = t
    return text.strip()


def stream_error(stream):
    """The error of a pi stream's last assistant message, "" when it did not end in one."""
    err = ""
    for line in (stream or "").splitlines():
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if not isinstance(e, dict):
            continue
        m = e.get("message") or {}
        if e.get("type") == "message_end" and m.get("role") == "assistant":
            err = (m.get("errorMessage") or "error") if m.get("stopReason") == "error" else ""
    return err


def usage(stream):
    """turns and output tokens of a pi stream (chain.go's parseUsageFull), for the record."""
    turns = out = 0
    for line in (stream or "").splitlines():
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if not isinstance(e, dict):
            continue
        m = e.get("message") or {}
        if e.get("type") == "message_end" and m.get("role") == "assistant":
            turns += 1
            out += (m.get("usage") or {}).get("output") or 0
    return turns, out


def _events(stream):
    for line in (stream or "").splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if isinstance(e, dict):
            yield e


def _result_text(m):
    """The text of a toolResult message: its text parts, joined."""
    c = m.get("content")
    if isinstance(c, str):
        return c
    return "".join(p.get("text") or "" for p in c or [] if isinstance(p, dict) and p.get("type") == "text")


def _turn(n, m, results):
    u = m.get("usage") if isinstance(m.get("usage"), dict) else None
    cost = (u.get("cost") or {}).get("total") if u and isinstance(u.get("cost"), dict) else None
    tools = []
    for p in m.get("content") or []:
        if isinstance(p, dict) and p.get("type") == "toolCall" and isinstance(p.get("name"), str):
            got = results.get(p.get("id"))
            # pi's stream has no duration for a tool call: none is recorded
            tools.append(adapter.action(p["name"], p.get("arguments") if isinstance(p.get("arguments"), dict) else None,
                                        got["result"] if got else None, ok=got["ok"] if got else None))
    stop = m.get("stopReason")
    return adapter.Turn(
        n=n, model=m.get("model"), time=adapter.iso_utc(m.get("timestamp")),
        text="".join(p.get("text") or "" for p in m.get("content") or [] if isinstance(p, dict) and p.get("type") == "text"),
        tools=tools,
        usage=adapter.usage_dict(u.get("input"), u.get("output"), u.get("cacheRead"), u.get("cacheWrite")) if u else None,
        cost_est=float(cost) if isinstance(cost, (int, float)) and not isinstance(cost, bool) else None,
        stop=STOP.get(stop, stop) if isinstance(stop, str) else "",
        error=(str(m.get("errorMessage") or "error")[:200]) if stop == "error" else None)


def models_json(route, provider):
    """pi's models.json for one route: a provider speaking OpenAI completions at the route's base URL. The key is named,
    never written: `$VAR` for the endpoint's key_env, `!cat <file>` for its key_file (pi reads it at call time), "none"
    for an endpoint that takes no Authorization (pi wants a value). `route.meta["pi_model"]`, when the caller has it, is
    what pi's catalog knows about the model (context window, output limit, prices)."""
    if route.key_env:
        key = "$" + route.key_env
    elif route.key_file:
        key = "!cat " + route.key_file
    else:
        key = "none"
    entry = dict({"id": route.model}, **(route.meta.get("pi_model") or {}))
    return {"providers": {provider: {"baseUrl": route.base_url, "api": "openai-completions", "apiKey": key, "models": [entry]}}}


def trim_session(path):
    """Drop a last line a kill cut short, so pi finds a session of whole JSON lines (ADR-0019)."""
    try:
        with open(path, "rb") as f:
            data = f.read()
        if not data or data.endswith(b"\n"):
            return
        with open(path, "wb") as f:
            f.write(data[:data.rfind(b"\n") + 1])
    except OSError:
        pass


def session_file(run_dir, ref):
    """The saved session file of `ref` (a session id, or a path inside the run directory) under <run_dir>/pi/sessions,
    "" when there is none or the path leaves the run directory."""
    if os.path.isabs(ref) or os.sep in ref:
        real, root = os.path.realpath(ref), os.path.realpath(run_dir) + os.sep
        return real if real.startswith(root) and os.path.isfile(real) else ""
    found = sorted(glob.glob(os.path.join(run_dir, "pi", "sessions", "*", "*" + glob.escape(ref) + "*.jsonl")))
    return found[-1] if found else ""


class Pi(adapter.Harness):
    name = "pi"
    tools_served = frozenset(adapter.TOOLS)
    can_resume = True
    resume_after_kill = True      # trim_session drops the line a kill cut short (ADR-0019)
    enforces_readonly = True      # --tools read,grep,find,ls and --no-tools: the harness does not offer the rest
    supports_hooks = False        # pi has extensions, but the platform defines no before/after-tool hook delivery for it
    reports_cost = True           # usage.cost.total of each message: its catalog's list price, an estimate
    reports_usage = True
    supports_turn_cap = True      # a message_end line per assistant message
    native_system_prompt = True   # --append-system-prompt <file>
    prompt_via_stdin = True       # `pi -p` with no message argument reads its prompt from stdin
    tees = False   # pi keeps its own session files; its stream is not written beside them

    def build(self, req):
        agent = os.path.join(req.run_dir, "pi")
        provider = "flotilla-" + (req.model.endpoint or "gateway")
        tools = {"none": ["--no-tools"], "read": ["--tools", READ_ONLY_TOOLS]}.get(req.tools, [])
        # --no-context-files by default: the workspace's own AGENTS.md or CLAUDE.md is never read (S2)
        context = ["--no-context-files"] if req.no_context_files else []
        system = ["--append-system-prompt", req.system_path] if req.system_path else []
        resume = []
        if req.resume:
            f = session_file(req.run_dir, req.resume)
            if not f:
                raise adapter.HarnessError(f"pi has no saved session {req.resume!r} under the run directory to resume")
            trim_session(f)
            resume = ["--session", f]
        return adapter.Invocation(
            argv=["pi", "-p", "--mode", "json", "--provider", provider, "--model", req.model.model, *tools, *context,
                  *system, *resume],
            env={"PI_CODING_AGENT_DIR": agent}, stdin=req.prompt, cwd=req.workdir,
            files={"pi/models.json": json.dumps(models_json(req.model, provider), indent=2)})

    def phone_home_env(self):
        return {"PI_OFFLINE": "1", "PI_SKIP_VERSION_CHECK": "1"}

    def turn_ended(self, line):
        return turn_ended(line)

    def parse(self, stream_text, files):
        turns, results, sid, n = [], {}, None, 0
        for e in _events(stream_text):
            m = e.get("message") if isinstance(e.get("message"), dict) else {}
            if e.get("type") == "session" and sid is None and isinstance(e.get("id"), str):
                sid = e["id"]
            elif e.get("type") == "message_end" and m.get("role") == "toolResult" and isinstance(m.get("toolCallId"), str):
                results[m["toolCallId"]] = {"ok": m.get("isError") is not True, "result": _result_text(m)}
            elif e.get("type") == "message_end" and m.get("role") == "assistant":
                n += 1
                turns.append((n, m))
        built = [_turn(i, m, results) for i, m in turns]
        err = stream_error(stream_text)
        return adapter.Episode(harness="pi", session_id=sid, turns=built, answer=assistant_text(stream_text),
                               error=err, exit_reason="error" if err else "completed",
                               usage_total=adapter.sum_usage(built), cost_est=adapter.sum_cost(built))


PI = adapter.register(Pi())
