"""What the ops routines share (pipeline v1, RFC-0004 §10): a VictoriaMetrics query, the registry's
declaration of an action, its preconditions, and writing the run's report. Python stdlib only.

Environment (from the worker: ROUTINE_<KEY> reaches a run as <KEY>):
  VM_URL               VictoriaMetrics, through the egress proxy (HTTP_PROXY)
  BACKUP_INSTANCE      the `instance` label of the backup's metric (an installation fact)
  FLOTILLA_REGISTRY    the registry declaration (default: the copy the image carries)
  FLOTILLA_PLAN_FILE   where the run's report goes (set by the worker)
  FLEETD_STAGE         the run's autonomy: shadow uses an action's pre_shadow
"""
import json
import os
import re
import urllib.parse
import urllib.request

REGISTRY = os.environ.get("FLOTILLA_REGISTRY", "/opt/flotilla/host/registry.json")
RERUN = "backup.rerun_observability"


class ConfigError(Exception):
    pass


def env(name):
    v = os.environ.get(name, "").strip()
    if not v:
        raise ConfigError(f"{name} is not set")
    return v


def query(expr, timeout=20):
    """One instant query: the value of a single-series result, or None for an empty one."""
    url = env("VM_URL").rstrip("/") + "/api/v1/query"
    body = urllib.parse.urlencode({"query": expr}).encode()
    with urllib.request.urlopen(urllib.request.Request(url, data=body, method="POST"), timeout=timeout) as r:
        out = json.load(r)
    if out.get("status") != "success":
        raise RuntimeError(f"VictoriaMetrics: {out}")
    result = out["data"]["result"]
    if not result:
        return None
    if len(result) != 1:
        raise RuntimeError(f"expected one series, got {len(result)}")
    return float(result[0]["value"][1])


def backup_age_hours():
    """Hours since the mini's off-box backup last succeeded, or None when it has not in 30 days. The
    same expression as the homelab's homelab-backup-offsite-stale alert."""
    inst = env("BACKUP_INSTANCE")
    if not re.fullmatch(r"[A-Za-z0-9._-]+", inst):
        raise ConfigError("BACKUP_INSTANCE: a label value such as homelab")
    v = query(f'time() - max(last_over_time(homelab_backup_offsite_last_success_timestamp{{instance="{inst}"}}[30d]))')
    return None if v is None else v / 3600


def action(name, path=REGISTRY):
    with open(path) as f:
        for a in json.load(f)["actions"]:
            if a["name"] == name:
                return a
    raise ConfigError(f"{name} is not declared in {path}")


def preconditions(a, stage):
    """The action's preconditions for the run's stage: shadow's lower threshold in shadow (RFC-0004 §10)."""
    if stage == "shadow" and a.get("pre_shadow"):
        return a["pre_shadow"]
    return a["pre"]


COND = re.compile(r"^([a-z][a-z0-9_]*)(?:\(([^()]*)\))?$")
DURATION = re.compile(r"^(\d+)h$")


def parse(cond):
    m = COND.match(cond)
    if not m:
        raise ConfigError(f"precondition {cond!r}")
    return m.group(1), m.group(2)


def hours(arg):
    m = DURATION.match(arg or "")
    if not m:
        raise ConfigError(f"a duration in whole hours such as 20h, not {arg!r}")
    return int(m.group(1))


def report(obj):
    """Write the run's report to its plan file (the worker reads and removes it)."""
    path = env("FLOTILLA_PLAN_FILE")
    with open(path, "w") as f:
        json.dump(obj, f)
