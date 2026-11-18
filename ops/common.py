"""What the ops routines share (pipeline v1, RFC-0004 §10): a VictoriaMetrics query, the registry's
declaration of an action, its preconditions, and writing the run's report. Python stdlib only.

Environment (from the worker: ROUTINE_<KEY> reaches a run as <KEY>):
  VM_URL               VictoriaMetrics, through the egress proxy (HTTP_PROXY)
  BACKUP_METRIC        the metric that holds the backup's last success time, in epoch seconds
  BACKUP_INSTANCE      the `instance` label of the backup's metric
  BACKUP_ACTION        the registry action that reruns the backup
  (all three are the installation's own: its fleet specs set them on the run's command)
  FLOTILLA_REGISTRY    the registry declaration (default: the copy the image carries)
  FLOTILLA_PLAN_FILE   where the run's report goes (set by the worker)
  FLEETD_STAGE         the run's autonomy: shadow uses an action's pre_shadow
"""
import json
import os
import re
import statistics
import time
import urllib.parse
import urllib.request

REGISTRY = os.environ.get("FLOTILLA_REGISTRY", "/opt/flotilla/host/registry.json")


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


def rerun_action():
    """The registry action that reruns the backup (BACKUP_ACTION)."""
    return env("BACKUP_ACTION")


def _metric():
    m = env("BACKUP_METRIC")
    if not re.fullmatch(r"[a-zA-Z_:][a-zA-Z0-9_:]*", m):
        raise ConfigError(f"BACKUP_METRIC {m!r} is not a metric name")
    return m


def _instance():
    inst = env("BACKUP_INSTANCE")
    if not re.fullmatch(r"[A-Za-z0-9._-]+", inst):
        raise ConfigError("BACKUP_INSTANCE: a label value such as backup-01")
    return inst


def backup_age_hours():
    """Hours since the host's off-box backup last succeeded, or None when it has not in 30 days. The
    same expression as the installation's backup-offsite-stale alert."""
    v = query(f'time() - max(last_over_time({_metric()}{{instance="{_instance()}"}}[30d]))')
    return None if v is None else v / 3600


def backup_last_success():
    """When the host's off-box backup last succeeded (Unix seconds), or None when it has not in 30 days."""
    return query(f'max(last_over_time({_metric()}{{instance="{_instance()}"}}[30d]))')


def query_range(expr, start, end, step, timeout=30):
    """A range query: every (time, value) of every series, flattened."""
    url = env("VM_URL").rstrip("/") + "/api/v1/query_range"
    body = urllib.parse.urlencode({"query": expr, "start": int(start), "end": int(end), "step": step}).encode()
    with urllib.request.urlopen(urllib.request.Request(url, data=body, method="POST"), timeout=timeout) as r:
        out = json.load(r)
    if out.get("status") != "success":
        raise RuntimeError(f"VictoriaMetrics: {out}")
    return [(float(t), float(v)) for s in out["data"]["result"] for t, v in s["values"]]


def cycle_hours(successes):
    """The source's normal cycle (ADR-0016 Q4): the median gap between distinct success times, in hours;
    None with fewer than three successes."""
    times = sorted(set(successes))
    if len(times) < 3:
        return None
    return statistics.median(b - a for a, b in zip(times, times[1:])) / 3600


def backup_cycle_hours(days=14, now=None):
    """How often the off-box backup succeeds: the median interval between its successes over `days`."""
    now = time.time() if now is None else now
    pts = query_range(f'max({_metric()}{{instance="{_instance()}"}})',
                      now - days * 86400, now, "30m")
    return cycle_hours([v for _, v in pts])


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
