"""runtime/steprec.py: a step of a run's harness in the run's calls file (FLOTILLA_CALLS_FILE).

A step is a line of kind "step" in the model client's line format: no alias, model, tokens or cost, its text in
`detail`, `role` the part of the run it concerns ("hook" for a hook's effect). The worker returns the file's lines
with the run and flotillad keeps them in run_calls (internal/store KindStep). bugfix/callrec_pi.py writes the same
line for the bugfix chain (its worker image has no runtime/; bugfix/test_callrec_pi.py checks the two agree).
A failure to write never fails the run. Stdlib only.
"""
import json
import os
import sys
import time

STEP_MAX = 500


def step_line(role, detail, decision_key=None, episode="", episode_seq=None, now=None):
    return {"at": now or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "alias": "", "endpoint": None, "model": "",
            "deployment": None, "tokens_in": None, "tokens_out": None, "cost_usd": None, "cost_source": "",
            "latency_s": None, "status": None, "error": None, "detail": " ".join(str(detail).split())[:STEP_MAX],
            "finish_reason": "", "step": None, "decision_key": decision_key, "role": role, "episode": episode or "",
            "episode_seq": episode_seq, "tools": [], "kind": "step"}


def write(role, detail, environ=None, **kw):
    path = (os.environ if environ is None else environ).get("FLOTILLA_CALLS_FILE", "")
    if not path:
        return
    try:
        with open(path, "a") as f:
            f.write(json.dumps(step_line(role, detail, **kw)) + "\n")
    except OSError as e:
        print(f"steprec: the calls file {path}: {e}", file=sys.stderr)
