"""bugfix/callrec_pi.py: the model calls of a bugfix run's pi episodes, for the run's calls file.

The bugfix chain makes its model calls inside pi (verdict, fix, review, advisor), not through
runtime/model_client.py, so nothing wrote the run's calls file (FLOTILLA_CALLS_FILE) for it. pi saves each
episode as a session file under <PI_CODING_AGENT_DIR>/sessions/*/*.jsonl, one JSON line per message
({"type": "message", "message": {"role", "model", "usage", "content", "stopReason"}}, the shape
bugfix/spend.py's pi_cost reads). After an episode ends, Recorder writes one calls-file line per assistant
message, in the model client's line format plus role, episode, episode_seq and tools, so the worker returns
them with the run and flotillad keeps them in run_calls.

Cost is the gateway's bill, never pi's catalog figure (5 to 7 times the bill on some days, bugfix/spend.py).
pi makes its calls through the run's LiteLLM key, so the key's spend over one episode (spend.Meter.episode) is
that episode's bill; each turn gets the share of it that its input + output tokens are of the episode's
(cost_source "gateway-share"; cache reads are not weighed, so a share is an apportioning, the episode's total
is the measurement). An episode whose bill could not be measured, or that has no tokens to share it by, writes
cost null and cost_source "".

A turn's tools are [{"name", "arg", "ok"}]: the call's main argument, redacted and cut at 160 characters, and
`ok` from the matching toolResult message's isError (left out when the session has no result for the call).

The chain's own steps (a verdict, a gate's outcome, an advisor's pin) are lines too, kind "step", with no
alias, model, tokens or cost and their text in `detail`: Recorder.step. A failure to write never fails the run.
"""
import glob
import json
import os
import re
import sys
import time

# pi's stop reasons as the model client's finish reasons (OpenAI's); one pi does not have a mapping for stays as it is
FINISH = {"toolUse": "tool_calls", "stop": "stop", "length": "length"}

ARG_MAX = 160
MASK = "<redacted>"
STEP_MAX = 500

# What a tool argument can carry that must not be recorded. The same rules as runtime/toolrec.py: the bugfix worker
# image holds bugfix/ and not runtime/, so it cannot import them; test_callrec_pi.py checks the two agree.
_RULES = [
    (re.compile(r"sk-or-v1-[A-Za-z0-9]{16,}|sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}"
                r"|glsa_[A-Za-z0-9_]{16,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}"
                r"|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+"), MASK),
    (re.compile(r"(?i)(authorization\s*[:=]\s*)(?:(?:bearer|basic|token)\s+)?[^\s'\"]+"), r"\1" + MASK),
    (re.compile(r"(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}"), r"\1 " + MASK),
    (re.compile(r"(://)[^/\s:@'\"]+:[^/\s@'\"]+@"), r"\1" + MASK + "@"),
    (re.compile(r"(?i)(--?(?:token|password|passwd|secret|api-?key)(?:=|\s+))(?:\"[^\"]*\"|'[^']*'|[^\s'\"]+)"), r"\1" + MASK),
    (re.compile(r"(?i)(\"[^\"]*(?:key|token|secret|passw(?:or)?d|credential)[^\"]*\"\s*:\s*)\"[^\"]*\""), r"\1\"" + MASK + "\""),
    (re.compile(r"(?i)((?:key|token|secret|passw(?:or)?d|credential)[A-Za-z0-9_.-]{0,64}=)(?:\"[^\"]*\"|'[^']*'|[^\s'\"&;]+)"),
     r"\1" + MASK),
    (re.compile(r"\b[A-Za-z0-9._%+-]{1,64}@[A-Za-z][A-Za-z0-9.-]*\.[A-Za-z]{2,}\b"), "<email>"),
    (re.compile(r"(?<![A-Za-z]/)\b\d{1,3}(?:\.\d{1,3}){3}\b"), "<ip>"),
]
_PREFERRED = ("command", "pattern", "path", "file_path", "filePath", "file", "url", "query")


def redact(text):
    for rx, repl in _RULES:
        text = rx.sub(repl, text)
    return text


def clean_arg(text, n=ARG_MAX):
    """text redacted (before it is cut: a cut through a key would leave half of it), on one line, at most n characters."""
    text = " ".join(redact(str(text)).split())
    return text if len(text) <= n else text[:n - 1].rstrip() + "…"


def primary_arg(args):
    """The main argument of a toolCall's `arguments`: a preferred key's string (bash's command, grep's pattern, a
    path), else the first string or number in it; "" when there is none."""
    if not isinstance(args, dict):
        return ""
    for k in _PREFERRED:
        if isinstance(args.get(k), str) and args[k]:
            return args[k]
    for v in args.values():
        if isinstance(v, str) and v:
            return v
        if isinstance(v, (int, float)) and not isinstance(v, bool):
            return str(v)
    return ""


def _now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _messages(path):
    """(session id, [assistant message lines], {toolCallId: its result was an error}) of one session file. Lines
    that are not JSON objects, and lines of other kinds, are skipped; the id is the header's, else the file's name."""
    sid, out, failed = "", [], {}
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
            elif isinstance(m, dict) and m.get("role") == "toolResult" and isinstance(m.get("toolCallId"), str):
                failed[m["toolCallId"]] = m.get("isError") is True
    return sid or os.path.splitext(os.path.basename(path))[0], out, failed


def _tools(m, failed):
    out = []
    for p in m.get("content") or []:
        if not (isinstance(p, dict) and p.get("type") == "toolCall" and isinstance(p.get("name"), str)):
            continue
        t = {"name": p["name"], "arg": clean_arg(primary_arg(p.get("arguments")))}
        if p.get("id") in failed:
            t["ok"] = not failed[p["id"]]
        out.append(t)
    return out


def _call(line, step, role, alias, episode, episode_seq, decision_key, now, failed):
    m = line["message"]
    usage = m.get("usage") if isinstance(m.get("usage"), dict) else {}
    stop = m.get("stopReason")
    ts = line.get("timestamp")
    return {"at": ts if isinstance(ts, str) and ts else now, "alias": alias, "endpoint": None,
            "model": m.get("model"), "deployment": None,
            "tokens_in": usage.get("input"), "tokens_out": usage.get("output"),
            "cost_usd": None, "cost_source": "",   # the episode's bill, apportioned (apportion), never pi's figure
            "latency_s": None, "status": None,
            "error": (str(m.get("errorMessage") or "error")[:200]) if stop == "error" else None,
            "finish_reason": FINISH.get(stop, stop) if isinstance(stop, str) else "", "step": step,
            "decision_key": decision_key, "role": role, "episode": episode, "episode_seq": episode_seq,
            "tools": _tools(m, failed)}


def session_lines(path, role, alias, episode_seq=None, decision_key=None, start=0, now=None):
    """The calls-file lines of one pi session file: one per assistant message after the first `start` of them
    (a resumed session goes on in its file; `start` is what was recorded before), in order. `step` is the
    message's 1-based position in the session, the episode is the session's id. Cost is not set here."""
    try:
        sid, msgs, failed = _messages(path)
    except OSError:
        return []
    now = now or _now()
    return [_call(line, i, role, alias, sid, episode_seq, decision_key, now, failed)
            for i, line in enumerate(msgs, 1) if i > start]


def apportion(lines, cost):
    """Put `cost` (an episode's gateway bill in USD, None when it could not be measured) on the episode's lines by
    each one's share of the episode's tokens (input + output), so the shares sum to it: the last line with tokens
    takes the remainder. A line with no token count at all gets none (null). An episode with no tokens, or no
    measured bill, gets null and cost_source "" on every line: never a guess, never pi's figure."""
    weights = [None if c.get("tokens_in") is None and c.get("tokens_out") is None
               else (c.get("tokens_in") or 0) + (c.get("tokens_out") or 0) for c in lines]
    total = sum(w for w in weights if w)
    if cost is None or total <= 0:
        for c in lines:
            c["cost_usd"], c["cost_source"] = None, ""
        return
    last = max(i for i, w in enumerate(weights) if w)
    given = 0.0
    for i, (c, w) in enumerate(zip(lines, weights)):
        if w is None:
            c["cost_usd"], c["cost_source"] = None, ""
            continue
        c["cost_usd"] = (cost - given) if i == last else cost * w / total
        c["cost_source"] = "gateway-share"
        given += c["cost_usd"]


def step_line(role, detail, position, decision_key=None, now=None):
    """The calls-file line of one step of the chain: kind "step", no alias, model, tokens or cost, its text in
    `detail` (cut at STEP_MAX), `role` the role it concerns, `episode_seq` its position among the run's steps."""
    return {"at": now or _now(), "alias": "", "endpoint": None, "model": "", "deployment": None,
            "tokens_in": None, "tokens_out": None, "cost_usd": None, "cost_source": "", "latency_s": None,
            "status": None, "error": None, "detail": " ".join(str(detail).split())[:STEP_MAX], "finish_reason": "",
            "step": None, "decision_key": decision_key, "role": role, "episode": "", "episode_seq": position,
            "tools": [], "kind": "step"}


class Recorder:
    """Appends the calls of pi's sessions to the run's calls file. record() after each episode writes what
    the sessions gained since the last one, so every episode's calls carry that episode's role and alias; a
    session's episode_seq is its order of first appearance in the run. With a `meter` (spend.Meter) each
    record() also measures the episode's gateway bill and puts its share on every line; `measures` says that
    every episode is measured, so the meter's mark is the run's settled spend."""

    def __init__(self, agent_dir, environ, decision_key=None, meter=None):
        self.dir, self.path, self.key = agent_dir, (environ or {}).get("FLOTILLA_CALLS_FILE", ""), decision_key
        self.meter = meter
        self.done, self.seq, self.steps = {}, {}, 0

    @property
    def measures(self):
        return bool(self.meter and self.path)

    def record(self, role, alias):
        if not self.path:
            return
        lines = []
        try:
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
        except Exception as e:  # noqa: BLE001 — the calls are a record, never a reason to fail the run
            print(f"bugfix calls: the sessions {self.dir}: {e}", file=sys.stderr)
        cost = None
        try:
            # settles the key's spend: seconds of waiting. The lines are written even when the wait is cut short by
            # the worker's SIGTERM (a BaseException), without their cost.
            cost = self.meter.episode() if self.meter else None
        except Exception as e:  # noqa: BLE001
            print(f"bugfix calls: the episode's gateway bill: {e}", file=sys.stderr)
        except BaseException:
            apportion(lines, None)
            self._write(lines)
            raise
        apportion(lines, cost)
        self._write(lines)

    def step(self, role, detail):
        """One line for a step of the chain (kind "step"), written now."""
        if not self.path:
            return
        self.steps += 1
        self._write([step_line(role, detail, self.steps, self.key)])

    def _write(self, lines):
        if not lines:
            return
        try:
            with open(self.path, "a") as out:
                out.write("".join(json.dumps(c) + "\n" for c in lines))
        except Exception as e:  # noqa: BLE001
            print(f"bugfix calls: the calls file {self.path}: {e}", file=sys.stderr)


def _mtime(f):
    try:
        return os.path.getmtime(f)
    except OSError:
        return 0.0
