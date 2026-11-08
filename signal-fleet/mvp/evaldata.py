"""Where the replay corpora live.

They hold real (scrubbed) production data, so they are kept in the private fleet-eval-data repo
(projects/signal-fleet-replays), not in this public one. SF_EVAL_DATA points at that directory;
without it a replay stops with the reason instead of passing on nothing.
"""
import os
import sys


def root():
    r = os.environ.get("SF_EVAL_DATA")
    if not r:
        sys.exit("SF_EVAL_DATA is not set: point it at a fleet-eval-data checkout's "
                 "projects/signal-fleet-replays (the replay corpora are private)")
    r = os.path.expanduser(r)
    if not os.path.isdir(r):
        sys.exit(f"SF_EVAL_DATA={r} is not a directory")
    return r


def path(*parts):
    return os.path.join(root(), *parts)
