"""ops/metric-monitor (block metric-monitor@2): the code-only monitor of a filesystem's free ratio (RFC-0016 §8, disk step 1). It reads
the free ratio (avail / size, both metrics) per group of labels, and reports one detection for each group below the threshold. flotillad
opens the case (keyed by kind and target) and clears it when a run no longer reports the group. It plans nothing and reaches nothing.

  threshold   the registry action's free_ratio_below precondition for the run's stage: its one home, the same number the acting step
              re-checks and case-review's proposals change
  group_by    the labels one detection is made per: instance first (the case's host), then mountpoint. The case target is
              host:<instance><mountpoint>, one case per group
  exclude     series to leave out, {label: regex}; a series matching any pair is dropped
  clear_after hysteresis: a group stays reported until this many readings in a row (one per step) are clear, so a disk that hovers at the
              threshold is one case and not many. The readings are VictoriaMetrics's own history, so the monitor keeps no state

Environment (the block's params, set on the run's command by the generator):
  MONITOR_ACTION, MONITOR_METRIC, MONITOR_SIZE_METRIC, MONITOR_GROUP_BY (a, b), MONITOR_EXCLUDE (a JSON object, validated here),
  MONITOR_CLEAR_AFTER (default 1), MONITOR_STEP (default 10m: the step's `every`), VM_URL, FLOTILLA_PLAN_FILE, FLEETD_STAGE.
  --kind is the flow's case kind.
"""
import json
import os
import re
import sys
import time
from dataclasses import dataclass, field

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
import free_ratio  # noqa: E402

MAX_DETECTIONS = 50          # flotillad refuses a report with more
MAX_CLEAR_AFTER = 100
KIND_RE = re.compile(r"^[a-z][a-z0-9_]{0,39}$")
STEP_RE = re.compile(r"^([1-9][0-9]{0,3})([sm])$")


@dataclass
class Config:
    kind: str
    action: str
    metric: str
    size_metric: str
    group_by: list
    exclude: dict = field(default_factory=dict)
    clear_after: int = 1
    step: int = 600


def parse_config(environ, kind):
    """The run's configuration from its environment, or ConfigError: nothing here ever reaches a query unchecked."""
    def need(name):
        v = (environ.get(name) or "").strip()
        if not v:
            raise common.ConfigError(f"{name} is not set")
        return v
    if not KIND_RE.match(kind or ""):
        raise common.ConfigError(f"--kind {str(kind)[:40]!r} is not a case kind")
    metrics = []
    for n in ("MONITOR_METRIC", "MONITOR_SIZE_METRIC"):
        v = need(n)
        if not free_ratio.METRIC_RE.match(v):
            raise common.ConfigError(f"{n} {v[:40]!r} is not a metric name")
        metrics.append(v)
    group_by = [g.strip() for g in need("MONITOR_GROUP_BY").split(",")]
    if not all(free_ratio.LABEL_RE.match(g) for g in group_by) or group_by[0] != "instance" or len(set(group_by)) != len(group_by):
        raise common.ConfigError("MONITOR_GROUP_BY is distinct label names, instance first (the case's host)")
    exclude = {}
    if (environ.get("MONITOR_EXCLUDE") or "").strip():
        try:
            exclude = json.loads(environ["MONITOR_EXCLUDE"])
        except ValueError as e:
            raise common.ConfigError(f"MONITOR_EXCLUDE is not JSON: {e}")
        free_ratio.check_exclude(exclude)
    clear_after = (environ.get("MONITOR_CLEAR_AFTER") or "1").strip()
    if not clear_after.isdigit() or not 1 <= int(clear_after) <= MAX_CLEAR_AFTER:
        raise common.ConfigError(f"MONITOR_CLEAR_AFTER is a count from 1 to {MAX_CLEAR_AFTER}, not {clear_after[:20]!r}")
    m = STEP_RE.match((environ.get("MONITOR_STEP") or "10m").strip())
    if not m:
        raise common.ConfigError("MONITOR_STEP is seconds or minutes such as 10m")
    step = int(m.group(1)) * {"s": 1, "m": 60}[m.group(2)]
    return Config(kind=kind, action=need("MONITOR_ACTION"), metric=metrics[0], size_metric=metrics[1], group_by=group_by,
                  exclude=exclude, clear_after=int(clear_after), step=step)


def group_key(cfg, labels):
    return tuple(labels.get(g, "") for g in cfg.group_by)


def title_of(labels, value, limit, held):
    where = labels.get("mountpoint") or ""
    where = f"{where} on {labels['instance']}" if where else labels["instance"]
    if held:
        return f"{where} was below {limit:.0%} free and is {value:.0%} now, not yet clear"
    return f"{where} has {value:.0%} free, below {limit:.0%}"


def run(cfg, limit, now_rows, history_rows):
    """(detections, summary lines) from the readings: now_rows [(labels, ratio)] is the newest reading of each group, history_rows
    [(labels, [(time, ratio)])] the last clear_after readings (empty when clear_after is 1). A group is reported while it is below the
    limit now, and while it was below it in any of the last clear_after readings: it clears after clear_after clear readings in a row."""
    now = {group_key(cfg, lab): (lab, v) for lab, v in now_rows}
    hist = {group_key(cfg, lab): [v for _, v in pts[-cfg.clear_after:]] for lab, pts in history_rows}
    found, skipped = [], []
    for key in sorted(set(now) | set(hist)):
        cur = now.get(key)
        below = [v for v in hist.get(key, []) if v < limit]
        if cur is not None and cur[1] < limit:
            value, held, lab = cur[1], False, cur[0]
        elif below:
            # clear now (or gone), but not yet clear_after clear readings in a row: held; its value is the newest reading, else the last low one
            held = True
            lab, value = cur if cur is not None else (dict(zip(cfg.group_by, key)), below[-1])
        else:
            continue
        lab = {g: lab.get(g, "") for g in cfg.group_by}
        target = free_ratio.encode_target(lab)
        if target is None:
            skipped.append(f"{', '.join(f'{k}={v}' for k, v in lab.items())}: not reported, its labels cannot make a case target")
            continue
        measure = {"name": "free_ratio", "value": round(value, 4), "threshold": limit,
                   "source": {"file": "host/registry.json", "action": cfg.action, "key": "pre", "condition": free_ratio.CONDITION}}
        detail = (f"free ratio {value:.1%}, below {limit:.0%}" if not held else
                  f"free ratio {value:.1%} now; below {limit:.0%} within the last {cfg.clear_after} readings, held until "
                  f"{cfg.clear_after} are clear")
        found.append({"kind": cfg.kind, "target": target, "title": title_of(lab, value, limit, held), "detail": detail,
                      "data": {"measure": measure, "labels": lab, "held": held}})
    found.sort(key=lambda d: (d["data"]["measure"]["value"], d["target"]))
    summary = skipped
    if len(found) > MAX_DETECTIONS:
        summary = [f"{len(found)} groups are below {limit:.0%}: the {MAX_DETECTIONS} fullest are reported"] + summary
        found = found[:MAX_DETECTIONS]
    return found, summary


def main(argv):
    kind = argv[argv.index("--kind") + 1] if "--kind" in argv and argv.index("--kind") + 1 < len(argv) else ""
    cfg = parse_config(os.environ, kind)
    stage = os.environ.get("FLEETD_STAGE", "shadow")
    limit = free_ratio.limit_of(common.action(cfg.action), stage)
    expr = f"min by ({', '.join(cfg.group_by)}) ({free_ratio.ratio_expr(cfg.metric, cfg.size_metric, cfg.exclude)})"
    now = time.time()
    rows = common.query_vector(expr)
    history = common.query_matrix(expr, now - (cfg.clear_after - 1) * cfg.step, now, cfg.step) if cfg.clear_after > 1 else []
    found, summary = run(cfg, limit, rows, history)
    report = {"outcome": "detections", "detections": found}
    if summary:
        report["summary"] = summary
    common.report(report)
    print(f"METRIC MONITOR groups={len(rows)} threshold={limit:g} clear_after={cfg.clear_after} stage={stage} detections={len(found)}"
          + "".join(f"\nMETRIC MONITOR {s}" for s in summary))


def entry(argv):
    try:
        main(argv)
    except common.ConfigError as e:
        print(f"METRIC MONITOR config: {e}", file=sys.stderr)
        sys.exit(2)


if __name__ == "__main__":
    entry(sys.argv[1:])
