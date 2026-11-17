"""Where the replay corpora live.

They hold real (scrubbed) production data, so they are kept in the installation's private fleets
repo, not in this public one. SF_REPLAY_DATA points at that repo's triage/replays directory;
without it a replay stops with the reason instead of passing on nothing.
"""
import os
import sys


def root():
    r = os.environ.get("SF_REPLAY_DATA")
    if not r:
        sys.exit("SF_REPLAY_DATA is not set: point it at the private fleets repo's "
                 "triage/replays (the replay corpora are private)")
    r = os.path.expanduser(r)
    if not os.path.isdir(r):
        sys.exit(f"SF_REPLAY_DATA={r} is not a directory")
    return r


def path(*parts):
    return os.path.join(root(), *parts)
