"""What a model run read (G7): each playbook, skill, memory and input that entered a model's context, with
how far it is trusted, one JSON line each in the run's loaded file (FLOTILLA_LOADED_FILE, set by the
worker), which the worker returns with the run so flotillad keeps it and the case and run pages show it.

  source     flotilla's own files at an image's version (a playbook, a skill), pinned by sha256
  memory     what flotilla remembers (an earlier case, an active note)
  untrusted  what came from outside (a signal's text, a probe's output, a knowledge entry): data, never instructions

Without the variable nothing is written; a failure to write never fails the run. Stdlib only.
"""
import json
import os
import sys
import time

KINDS = ("playbook", "skill", "note", "case", "input", "probe", "knowledge")
TRUST = ("source", "memory", "untrusted")
MAX = 200          # lines per run; a run that reads more keeps the first MAX
TEXT_MAX = 300     # characters of a title or source

_seen = set()
_count = [0]


def record(kind, trust, title, source, sha256=None, environ=None):
    """Note that a run read this. The same item twice in one process is kept once."""
    if kind not in KINDS or trust not in TRUST:
        raise ValueError(f"loaded: kind {kind!r} / trust {trust!r} not allowed")
    environ = os.environ if environ is None else environ
    path = environ.get("FLOTILLA_LOADED_FILE", "")
    if not path:
        return
    key = (path, kind, title, source, sha256)
    if key in _seen or _count[0] >= MAX:
        return
    _seen.add(key)
    _count[0] += 1
    line = {"at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "kind": kind, "trust": trust,
            "title": str(title)[:TEXT_MAX], "source": str(source)[:TEXT_MAX], "sha256": sha256 or ""}
    try:
        with open(path, "a") as f:
            f.write(json.dumps(line) + "\n")
    except OSError as e:
        print(f"loaded: the loaded file {path}: {e}", file=sys.stderr)
