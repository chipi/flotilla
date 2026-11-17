"""The installation's facts for the triage fleet (vision plan step 7): which repo a signal files into,
which repos the bugfix chain builds, where the assistant's backend is, which logs and metrics a
project's probes read, and what the scrubber replaces. flotilla's code holds none of them.

They are read from fleets/triage/installation.json in the fleet folder: on a host, the
installation's own fleets (mounted over the image's); in a checkout or the image, the example
fleet's placeholders. SF_INSTALLATION names another file (the tests' fixture, a replay's). A
missing or unreadable file stops the fleet: routing on empty maps would file nothing, quietly.
"""
import json
import os

_CODE_ROOT = os.environ.get("FLOTILLA_CODE_ROOT") or os.path.normpath(
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
PATH = os.environ.get("SF_INSTALLATION") or os.path.join(_CODE_ROOT, "fleets", "triage", "installation.json")

with open(PATH) as _f:
    DATA = json.load(_f)


def get(key, default):
    return DATA.get(key, default)
