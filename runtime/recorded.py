"""runtime/recorded.py: recorded tool results, so a station answers its tool calls from a captured run or a scenario's fixtures instead of the
live tools (v2-design.md section 6; the input envelope's `recorded_tool_results`, runtime/input.schema.json).

  FLOTILLA_RECORDED_TOOLS=<file>   the file holds {"<station>": [{"tool", "args", "result", "ok"}]} (the envelope's field, as a file)

A call is matched by (station, tool name, normalised arguments), never by position. Arguments are normalised to one canonical JSON text: keys
sorted, strings NFC, stripped and with runs of whitespace collapsed, integral floats written as integers. A key recorded once answers every
repeat of it; a key recorded several times answers in order, and the last answer repeats. A call with no recorded result raises RecordedMiss:
the run fails, and the live tool is never called. When the variable is not set nothing here is on the path (run_json_step does not wrap).
Stdlib only.
"""
import json
import os
import re
import unicodedata


class RecordedMiss(Exception):
    """The run asked for a tool result that was not recorded. It fails the run; it is never answered by the live tool."""


def _norm(v):
    if isinstance(v, str):
        return re.sub(r"\s+", " ", unicodedata.normalize("NFC", v)).strip()
    if isinstance(v, float) and v.is_integer():
        return int(v)
    if isinstance(v, dict):
        return {_norm(k): _norm(x) for k, x in v.items()}
    if isinstance(v, (list, tuple)):
        return [_norm(x) for x in v]
    return v


def key(station, tool, args):
    """The match key of a call: station, tool and the canonical text of its normalised arguments."""
    return (station or "", _norm(tool), json.dumps(_norm(args if args is not None else {}), sort_keys=True, separators=(",", ":"), ensure_ascii=False))


class Replayer:
    def __init__(self, recorded):
        if not isinstance(recorded, dict):
            raise ValueError("recorded tool results are an object: station -> list of {tool, args, result, ok}")
        self.answers, self.used = {}, {}
        for station, calls in recorded.items():
            if not isinstance(calls, list):
                raise ValueError(f"recorded tool results of {station!r} are not a list")
            for c in calls:
                if not isinstance(c, dict) or not isinstance(c.get("tool"), str) or "result" not in c or not isinstance(c.get("args", {}), dict):
                    raise ValueError(f"a recorded call of {station!r} needs tool, args and result")
                self.answers.setdefault(key(station, c["tool"], c.get("args") or {}), []).append(c["result"])

    def answer(self, station, tool, args):
        k = key(station, tool, args)
        got = self.answers.get(k)
        if not got:
            raise RecordedMiss(f"no recorded result for station {station!r} tool {tool!r} with arguments {k[2]}")
        i = self.used.get(k, 0)
        self.used[k] = i + 1
        return got[min(i, len(got) - 1)]

    def wrap(self, station, live):
        """A tool function that answers from the recordings and never calls `live`."""
        return lambda name, args: self.answer(station, name, args)


def from_env(environ=None):
    """The Replayer named by FLOTILLA_RECORDED_TOOLS, or None when it is not set."""
    environ = os.environ if environ is None else environ
    path = environ.get("FLOTILLA_RECORDED_TOOLS", "")
    if not path:
        return None
    with open(path) as f:
        return Replayer(json.load(f))
