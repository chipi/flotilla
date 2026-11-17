"""The business examples' stand-in systems: a CRM, a campaign tool, a mail outbox, usage and tickets, each
a JSON file in one folder. An installation replaces this module with an adapter to its own systems; the
routines only call load, save and append.

  BIZ_DATA   the folder the stand-in systems live in (writable; created on first use)
  BIZ_SEED   the fleet folder's seed data, copied into BIZ_DATA for each file not there yet
"""
import json
import os
import shutil
import tempfile


class Error(Exception):
    pass


def folder():
    d = os.environ.get("BIZ_DATA", "").strip()
    if not d:
        raise Error("BIZ_DATA is not set: the folder of the stand-in systems")
    os.makedirs(d, exist_ok=True)
    seed = os.environ.get("BIZ_SEED", "").strip()
    if seed:
        for name in os.listdir(seed):
            if name.endswith(".json") and not os.path.exists(os.path.join(d, name)):
                shutil.copyfile(os.path.join(seed, name), os.path.join(d, name))
    return d


def load(name, default=None):
    p = os.path.join(folder(), name + ".json")
    if not os.path.exists(p):
        if default is not None:
            return default
        raise Error(f"no {name}.json in {folder()}")
    with open(p) as f:
        return json.load(f)


def save(name, data):
    """Written whole, through a temporary file, so a reader never sees half a file."""
    d = folder()
    fd, tmp = tempfile.mkstemp(dir=d, prefix="." + name)
    with os.fdopen(fd, "w") as f:
        json.dump(data, f, indent=1)
    os.replace(tmp, os.path.join(d, name + ".json"))


def append(name, item):
    items = load(name, [])
    items.append(item)
    save(name, items)


def by_id(items):
    return {x["id"]: x for x in items}


def report(obj):
    plan = os.environ.get("FLOTILLA_PLAN_FILE")
    if plan:
        with open(plan, "w") as f:
            json.dump(obj, f)
