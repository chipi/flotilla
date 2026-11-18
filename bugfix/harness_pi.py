"""bugfix/harness_pi: pi (pi -p --mode json) as a harness (ADR-0020).

pi is the chain's own harness and keeps its command paths: the fix and the reproducer run through harnesses/pi.sh and
repro.sh, the verdict and the advisor through their leaves, and the review is built here, to the same argv Chain.review
always sent (bugfix/test_pi_golden.py pins it). What this module adds is the pi stream's reading in one place:
assistant_text, stream_error and usage (fix.py keeps the names as wrappers) and parse(), which turns a stream into the
normalized Episode.
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import callrec_pi  # noqa: E402
import harness  # noqa: E402

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


def _turn(n, m, failed):
    u = m.get("usage") if isinstance(m.get("usage"), dict) else None
    cost = (u.get("cost") or {}).get("total") if u and isinstance(u.get("cost"), dict) else None
    tools = []
    for p in m.get("content") or []:
        if isinstance(p, dict) and p.get("type") == "toolCall" and isinstance(p.get("name"), str):
            t = {"name": p["name"], "arg": callrec_pi.clean_arg(callrec_pi.primary_arg(p.get("arguments")))}
            if p.get("id") in failed:
                t["ok"] = not failed[p["id"]]
            tools.append(t)
    stop = m.get("stopReason")
    return harness.Turn(
        n=n, model=m.get("model"),
        text="".join(p.get("text") or "" for p in m.get("content") or [] if isinstance(p, dict) and p.get("type") == "text"),
        tools=tools,
        usage=harness.usage_dict(u.get("input"), u.get("output"), u.get("cacheRead"), u.get("cacheWrite")) if u else None,
        cost_est=float(cost) if isinstance(cost, (int, float)) and not isinstance(cost, bool) else None,
        stop=STOP.get(stop, stop) if isinstance(stop, str) else "",
        error=(str(m.get("errorMessage") or "error")[:200]) if stop == "error" else None)


class Pi(harness.Harness):
    name = "pi"
    roles = frozenset(harness.ROLES)
    can_resume = True
    enforces_readonly = True
    tees = False   # pi keeps its own session files; its stream is not written beside them

    def build(self, req):
        """The review's command, as Chain.review always built it. The other roles run through pi's leaf scripts, which
        read their settings from the environment (PI_*, BUGFIX_PI_RULES), not from a request."""
        if req.role != "review":
            raise harness.HarnessError(f"pi serves the {req.role} role through its leaf script, not through build()")
        provider = req.model.meta.get("pi_provider") or "flotilla-" + req.model.endpoint
        rules = ["--no-context-files", "--append-system-prompt", req.rules_path] if req.rules_path else []
        if req.tools == "none" and rules:   # the rules are about using tools; a review without them has none
            rules = ["--no-context-files"]
        tools = {"none": ["--no-tools"], "read": ["--tools", READ_ONLY_TOOLS]}.get(req.tools, [])
        return harness.Invocation(argv=["pi", "-p", "--mode", "json", "--provider", provider, "--model", req.model.model,
                                        *tools, *rules, req.prompt], cwd=req.workdir)

    def turn_ended(self, line):
        return turn_ended(line)

    def parse(self, stream_text, files):
        turns, failed, sid, n = [], {}, None, 0
        for e in _events(stream_text):
            m = e.get("message") if isinstance(e.get("message"), dict) else {}
            if e.get("type") == "session" and sid is None and isinstance(e.get("id"), str):
                sid = e["id"]
            elif e.get("type") == "message_end" and m.get("role") == "toolResult" and isinstance(m.get("toolCallId"), str):
                failed[m["toolCallId"]] = m.get("isError") is True
            elif e.get("type") == "message_end" and m.get("role") == "assistant":
                n += 1
                turns.append((n, m))
        built = [_turn(i, m, failed) for i, m in turns]
        err = stream_error(stream_text)
        return harness.Episode(harness="pi", role="", session_id=sid, turns=built, answer=assistant_text(stream_text),
                               error=err, exit_reason="error" if err else "completed",
                               usage_total=harness.sum_usage(built), cost_est=harness.sum_cost(built))


PI = harness.register(Pi())
