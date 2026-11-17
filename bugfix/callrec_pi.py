"""bugfix/callrec_pi.py: the model calls of a bugfix run's pi episodes, for the run's calls file.

The bugfix chain makes its model calls inside pi (verdict, fix, review, advisor), not through
runtime/model_client.py, so nothing wrote the run's calls file (FLOTILLA_CALLS_FILE) for it. pi saves each
episode as a session file under <PI_CODING_AGENT_DIR>/sessions/*/*.jsonl, one JSON line per message
({"type": "message", "message": {"role", "model", "usage", "content", "stopReason"}}, the shape
bugfix/spend.py's pi_cost reads). After an episode ends, Recorder writes one calls-file line per assistant
message, in the model client's line format plus role, episode, episode_seq and tools, so the worker returns
them with the run and flotillad keeps them in run_calls.

Cost is pi's catalog figure (cost_source "pi"), not the gateway's bill (spend.py): the run's spend is still
the bill. A failure to write never fails the run.
"""
import glob
import json
import os
import sys
import time

# pi's stop reasons as the model client's finish reasons (OpenAI's); one pi does not have a mapping for stays as it is
FINISH = {"toolUse": "tool_calls", "stop": "stop", "length": "length"}


def _now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _messages(path):
    """(session id, [assistant message lines]) of one session file. Lines that are not JSON objects, and lines
    of other kinds, are skipped; the id is the header's, else the file's name."""
    sid, out = "", []
    with open(path) as f:
        for raw in f:
            try:
                line = json.loads(raw)
            except ValueError:
                continue
            if not isinstance(line, dict):
                continue
            if line.get("type") == "session" and not sid and isinstance(line.get("id"), str):
                sid = line["id"]
            m = line.get("message")
            if isinstance(m, dict) and m.get("role") == "assistant":
                out.append(line)
    return sid or os.path.splitext(os.path.basename(path))[0], out


def _call(line, step, role, alias, episode, episode_seq, decision_key, now):
    m = line["message"]
    usage = m.get("usage") if isinstance(m.get("usage"), dict) else {}
    cost = usage.get("cost") if isinstance(usage.get("cost"), dict) else {}
    stop = m.get("stopReason")
    tools = [p["name"] for p in m.get("content") or [] if isinstance(p, dict) and p.get("type") == "toolCall"
             and isinstance(p.get("name"), str)]
    ts = line.get("timestamp")
    return {"at": ts if isinstance(ts, str) and ts else now, "alias": alias, "endpoint": None,
            "model": m.get("model"), "deployment": None,
            "tokens_in": usage.get("input"), "tokens_out": usage.get("output"),
            "cost_usd": cost.get("total"), "cost_source": "pi" if cost.get("total") is not None else None,
            "latency_s": None, "status": None,
            "error": (str(m.get("errorMessage") or "error")[:200]) if stop == "error" else None,
            "finish_reason": FINISH.get(stop, stop) if isinstance(stop, str) else "", "step": step,
            "decision_key": decision_key, "role": role, "episode": episode, "episode_seq": episode_seq,
            "tools": tools}


def session_lines(path, role, alias, episode_seq=None, decision_key=None, start=0, now=None):
    """The calls-file lines of one pi session file: one per assistant message after the first `start` of them
    (a resumed session goes on in its file; `start` is what was recorded before), in order. `step` is the
    message's 1-based position in the session, the episode is the session's id."""
    try:
        sid, msgs = _messages(path)
    except OSError:
        return []
    now = now or _now()
    return [_call(line, i, role, alias, sid, episode_seq, decision_key, now)
            for i, line in enumerate(msgs, 1) if i > start]


class Recorder:
    """Appends the calls of pi's sessions to the run's calls file. record() after each episode writes what
    the sessions gained since the last one, so every episode's calls carry that episode's role and alias; a
    session's episode_seq is its order of first appearance in the run."""

    def __init__(self, agent_dir, environ, decision_key=None):
        self.dir, self.path, self.key = agent_dir, (environ or {}).get("FLOTILLA_CALLS_FILE", ""), decision_key
        self.done, self.seq = {}, {}

    def record(self, role, alias):
        if not self.path:
            return
        try:
            lines = []
            for f in sorted(glob.glob(os.path.join(self.dir, "sessions", "*", "*.jsonl")), key=_mtime):
                new = session_lines(f, role, alias, self.seq.get(f), self.key, self.done.get(f, 0))
                if not new:
                    continue
                if f not in self.seq:
                    self.seq[f] = len(self.seq) + 1
                    for c in new:
                        c["episode_seq"] = self.seq[f]
                self.done[f] = self.done.get(f, 0) + len(new)
                lines += new
            if lines:
                with open(self.path, "a") as out:
                    out.write("".join(json.dumps(c) + "\n" for c in lines))
        except Exception as e:  # noqa: BLE001 — the calls are a record, never a reason to fail the run
            print(f"bugfix calls: the calls file {self.path}: {e}", file=sys.stderr)


def _mtime(f):
    try:
        return os.path.getmtime(f)
    except OSError:
        return 0.0
