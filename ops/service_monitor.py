"""ops/service-monitor (block service-monitor@1): the code-only monitor of a service gauge (the DGX case, step 1, on RFC-0016 §8's model). It
reads a gauge (1 up, 0 down, a series per service) per group of labels and reports one detection for each group whose watched service has
read down for `readings` readings in a row. flotillad opens the case (keyed by kind and target) and clears it when a run no longer reports
the group. It plans nothing and reaches nothing.

  action      the registry action whose service_down(<service>) precondition names the service: its one home, the same condition the
              acting step re-checks with a fresh read and the verify reads. Every service it names must be in `services`
  services    the values of the service label to watch
  group_by    the labels one detection is made per: instance first (the case's host), then at most one more. The case target is
              host:<instance>, and /<value> for the second label
  context     optional: a metric whose `context_label` value (where it reads 1, such as a mode gauge) is reported with the detection
  readings    this many readings in a row must read down before a detection (default 2: 15 to 20 minutes at a 10m step)
  clear_after hysteresis: a held group is reported until this many readings in a row read up (default 2)

An absent series is unknown, never up: a group that was down is held, and reported, until `clear_after` readings in a row that actually read
1. The monitor keeps no state: it reads the last day of the gauge's own history (a reading per step) and replays it, so a host or exporter
that stops reporting while its service is down keeps its case open (after a day with no reading at all the history holds nothing and the
case clears). A query that fails fails the run: nothing is reported, so no case is cleared on the strength of a broken read.

Environment (the block's params, set on the run's command by the generator):
  MONITOR_ACTION, MONITOR_METRIC, MONITOR_SERVICE_LABEL, MONITOR_SERVICES (a, b), MONITOR_GROUP_BY (a, b), MONITOR_CONTEXT and
  MONITOR_CONTEXT_LABEL (both or neither), MONITOR_READINGS, MONITOR_CLEAR_AFTER, MONITOR_STEP (minutes, default 10m: the step's `every`),
  VM_URL, FLOTILLA_PLAN_FILE. --kind is the flow's case kind.
"""
import os
import re
import sys
import time
from dataclasses import dataclass
from typing import Optional

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
import free_ratio  # noqa: E402
import service_up  # noqa: E402

MAX_DETECTIONS = 50          # flotillad refuses a report with more
MAX_COUNT = 100
HOLD_SECONDS = 86400         # how far back the gauge's history is read: how long a group with no reading at all stays held
MAX_READINGS = 1441          # a day at a 1m step
MIN_WINDOW = 600             # the newest sample within it is the reading, whatever the step
KIND_RE = re.compile(r"^[a-z][a-z0-9_]{0,39}$")
STEP_RE = re.compile(r"^([1-9][0-9]{0,3})m$")
UP, DOWN = 1.0, 0.0


@dataclass
class Config:
    kind: str
    action: str
    metric: str
    service_label: str
    services: list
    group_by: list
    context: Optional[str] = None
    context_label: Optional[str] = None
    readings: int = 2
    clear_after: int = 2
    step: int = 600


def parse_config(environ, kind):
    """The run's configuration from its environment, or ConfigError: nothing here ever reaches a query unchecked."""
    def val(name):
        return (environ.get(name) or "").strip()

    def need(name):
        if not val(name):
            raise common.ConfigError(f"{name} is not set")
        return val(name)

    def count(name, default):
        v = val(name) or default
        if not v.isdigit() or not 1 <= int(v) <= MAX_COUNT:
            raise common.ConfigError(f"{name} is a count from 1 to {MAX_COUNT}, not {v[:20]!r}")
        return int(v)
    if not KIND_RE.match(kind or ""):
        raise common.ConfigError(f"--kind {str(kind)[:40]!r} is not a case kind")
    group_by = [g.strip() for g in need("MONITOR_GROUP_BY").split(",")]
    if (not all(free_ratio.LABEL_RE.match(g) for g in group_by) or group_by[0] != "instance"
            or len(set(group_by)) != len(group_by) or len(group_by) > service_up.MAX_GROUP_LABELS):
        raise common.ConfigError(f"MONITOR_GROUP_BY is distinct label names, instance first and at most {service_up.MAX_GROUP_LABELS}")
    services = [service_up.plain_service(s.strip()) for s in need("MONITOR_SERVICES").split(",")]
    if len(set(services)) != len(services):
        raise common.ConfigError("MONITOR_SERVICES names a service twice")
    context, context_label = val("MONITOR_CONTEXT") or None, val("MONITOR_CONTEXT_LABEL") or None
    if (context is None) != (context_label is None):
        raise common.ConfigError("MONITOR_CONTEXT and MONITOR_CONTEXT_LABEL are set together or not at all")
    if context:
        service_up.plain_metric(context)
        service_up.plain_label(context_label)
    m = STEP_RE.match(val("MONITOR_STEP") or "10m")
    if not m:
        raise common.ConfigError("MONITOR_STEP is whole minutes such as 10m")
    return Config(kind=kind, action=need("MONITOR_ACTION"), metric=service_up.plain_metric(need("MONITOR_METRIC")),
                  service_label=service_up.plain_label(need("MONITOR_SERVICE_LABEL")), services=services, group_by=group_by,
                  context=context, context_label=context_label, readings=count("MONITOR_READINGS", "2"),
                  clear_after=count("MONITOR_CLEAR_AFTER", "2"), step=int(m.group(1)) * 60)


def check_services(cfg, action):
    """Every service the action's precondition names is watched: a flow that watches another service would open cases the acting step
    can only call resolved."""
    named = service_up.services_of(action)
    missing = [s for s in named if s not in cfg.services]
    if missing:
        raise common.ConfigError(f"{cfg.action} names {', '.join(missing)} in {service_up.CONDITION}, which services does not watch")


def lookback(cfg):
    """How many readings (one per step, the newest now) are read: the last day, and never fewer than the two streaks need."""
    return max(HOLD_SECONDS // cfg.step + 1, cfg.readings, cfg.clear_after)


def expr(cfg):
    window = max(cfg.step, MIN_WINDOW)
    return (f"min by ({', '.join(cfg.group_by)}) "
            f"(last_over_time({service_up.selector(cfg.metric, cfg.service_label, cfg.services)}[{window}s]))")


def scan(readings, need_down, need_up):
    """(open, the down streak at the end) of the readings, oldest first, each 1 (up), 0 (down) or None (no reading). A group opens at
    need_down down readings in a row and closes at need_up up readings in a row; a reading that is absent is unknown: it counts for
    neither, breaks both streaks, and so never opens one and never closes one."""
    opened, down, up = False, 0, 0
    for r in readings:
        if r is None:
            down = up = 0
        elif r < 1:
            down, up = down + 1, 0
        else:
            down, up = 0, up + 1
        if down >= need_down:
            opened = True
        elif opened and up >= need_up:
            opened = False
    return opened, down


def title_of(what, inst, held, ctx):
    return f"{what} on {inst} is down" + (", not yet clear" if held else "") + (f", {ctx[0]} {ctx[1] if ctx[1] is not None else 'unknown'}" if ctx else "")


def run(cfg, groups, context):
    """(detections, summary lines) from the readings: groups [(labels, [reading])] the last lookback readings of each group, oldest first
    (a missing one is None), context {instance: value of the context label} or empty. A group is reported while it is open: down for
    `readings` readings in a row, and not since up for `clear_after` readings in a row."""
    found, skipped = [], []
    for labels, readings in groups:
        opened, down = scan(readings, cfg.readings, cfg.clear_after)
        if not opened:
            continue
        lab = {g: labels.get(g, "") for g in cfg.group_by}
        target = service_up.encode_target(cfg.group_by, lab)
        if target is None:
            skipped.append(f"{', '.join(f'{k}={v}' for k, v in lab.items())}: not reported, its labels cannot make a case target")
            continue
        held = down < cfg.readings
        last = readings[-1]
        what = lab[cfg.service_label] if cfg.service_label in lab else ", ".join(cfg.services)
        ctx = (cfg.context_label, context.get(lab["instance"])) if cfg.context else None
        if not held:
            detail = f"{what} read down for the last {down} readings (a detection needs {cfg.readings})"
        elif last is None:
            detail = (f"{what} has no reading now (an absent series is unknown, not up): held until {cfg.clear_after} readings in a "
                      f"row read up")
        elif last < 1:
            detail = f"{what} read down again, {down} of {cfg.readings} readings: held until {cfg.clear_after} readings in a row read up"
        else:
            detail = f"{what} reads up again: held until {cfg.clear_after} readings in a row read up"
        if ctx:
            detail += f"; {ctx[0]} {ctx[1] if ctx[1] is not None else 'unknown'}"
        known = [r for r in readings if r is not None]
        measure = {"name": "service_up", "value": int(known[-1] >= 1) if known else 0, "threshold": 1,
                   "source": {"file": "host/registry.json", "action": cfg.action, "key": "pre", "condition": service_up.CONDITION}}
        data = {"measure": measure, "labels": lab, "held": held, "readings": {"down_in_a_row": down, "needed": cfg.readings}}
        if ctx:
            data["context"] = {"metric": cfg.context, "label": ctx[0], "value": ctx[1]}
        found.append({"kind": cfg.kind, "target": target, "title": title_of(what, lab["instance"], held, ctx), "detail": detail,
                      "data": data})
    found.sort(key=lambda d: d["target"])
    summary = skipped
    if len(found) > MAX_DETECTIONS:
        summary = [f"{len(found)} groups are down: the first {MAX_DETECTIONS} are reported"] + summary
        found = found[:MAX_DETECTIONS]
    return found, summary


def readings_of(cfg, matrix, start):
    """The matrix as run() takes it: [(labels, [reading per step from start])]. A point lands on the step nearest its time; a value that
    is not 0 or a 1 or more is no reading."""
    n = lookback(cfg)
    groups = {}
    for lab, pts in matrix:
        key = tuple(lab.get(g, "") for g in cfg.group_by)
        row = groups.setdefault(key, ({g: lab.get(g, "") for g in cfg.group_by}, [None] * n))[1]
        for t, v in pts:
            i = round((t - start) / cfg.step)
            r = UP if v >= 1 else DOWN if v == 0 else None
            if 0 <= i < n and r is not None:
                row[i] = r if row[i] is None else min(row[i], r)
    return [groups[k] for k in sorted(groups)]


def context_of(cfg, rows):
    """{instance: the context label's value}: where it reads 1 (two at once join with +)."""
    out = {}
    for lab, _ in rows:
        if lab.get("instance") and lab.get(cfg.context_label):
            out.setdefault(lab["instance"], []).append(lab[cfg.context_label])
    return {i: "+".join(sorted(set(v))) for i, v in out.items()}


def main(argv):
    kind = argv[argv.index("--kind") + 1] if "--kind" in argv and argv.index("--kind") + 1 < len(argv) else ""
    cfg = parse_config(os.environ, kind)
    check_services(cfg, common.action(cfg.action))
    now = int(time.time())
    n = lookback(cfg)
    start = now - (n - 1) * cfg.step
    matrix = common.query_matrix(expr(cfg), start, now, cfg.step)
    groups = readings_of(cfg, matrix, start)
    context = {}
    if cfg.context and any(scan(r, cfg.readings, cfg.clear_after)[0] for _, r in groups):
        context = context_of(cfg, common.query_vector(
            f"max by (instance, {cfg.context_label}) (last_over_time({cfg.context}[{service_up.WINDOW}]) == 1)"))
    found, summary = run(cfg, groups, context)
    report = {"outcome": "detections", "detections": found}
    if summary:
        report["summary"] = summary
    common.report(report)
    print(f"SERVICE MONITOR groups={len(groups)} readings={cfg.readings} clear_after={cfg.clear_after} detections={len(found)}"
          + "".join(f"\nSERVICE MONITOR {s}" for s in summary))


def entry(argv):
    try:
        main(argv)
    except common.ConfigError as e:
        print(f"SERVICE MONITOR config: {e}", file=sys.stderr)
        sys.exit(2)


if __name__ == "__main__":
    entry(sys.argv[1:])
