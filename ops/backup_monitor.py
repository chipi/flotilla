"""ops/backup-monitor: the code-only Ops monitor of pipeline v1 (RFC-0004 §10). Reads the age of the
mini's off-box backup and reports a failed_backup detection on BACKUP_TARGET when it is above the
threshold of backup.rerun_observability's backup_age_above precondition for the run's stage (25h in
shadow, 26h otherwise): the case is opened at the same threshold its re-check uses, so a shadow case is
planned rather than closed as resolved (RFC-0004 §10).

flotillad opens the case (keys by code, the kind checked against the spec's `detects`); this run only
reports. It prints one line with its numbers.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402


def threshold_hours(a, stage):
    for c in common.preconditions(a, stage):
        name, arg = common.parse(c)
        if name == "backup_age_above":
            return common.hours(arg)
    raise common.ConfigError(f"{a['name']} has no backup_age_above precondition")


def host_of(target):
    """host:mini -> mini, for the case's title."""
    return target.split(":", 1)[-1].split("/", 1)[0]


def measure(age, limit, cycle, stage, a):
    """What the detection measured (ADR-0016 §3), for the post-incident review: the value, the threshold,
    the source's normal cycle, and which declaration the threshold came from."""
    key = "pre_shadow" if stage == "shadow" and a.get("pre_shadow") else "pre"
    return {"measure": {"name": "backup_age_hours", "value": None if age is None else round(age, 2), "threshold": limit,
                        "cycle_hours": None if cycle is None else round(cycle, 2),
                        "source": {"file": "host/registry.json", "action": a["name"], "key": key,
                                   "condition": "backup_age_above"}}}


def detect(age, limit, target, data=None):
    found = []
    if age is None:
        found = [{"kind": "failed_backup", "target": target, "detail": "no successful off-box backup in 30 days",
                  "title": f"No successful off-box backup on {host_of(target)} in 30 days"}]
    elif age > limit:
        found = [{"kind": "failed_backup", "target": target, "detail": f"backup {age:.1f}h old, above {limit}h",
                  "title": f"The newest offsite backup on {host_of(target)} is {age:.0f} hours old"}]
    for d in found:
        if data:
            d["data"] = data
    return found


def main():
    target = common.env("BACKUP_TARGET")
    stage = os.environ.get("FLEETD_STAGE", "shadow")
    a = common.action(common.RERUN)
    limit = threshold_hours(a, stage)
    age = common.backup_age_hours()
    cycle = None
    if age is None or age > limit:
        try:
            cycle = common.backup_cycle_hours()
        except Exception as e:  # the cycle is evidence for the review, never a reason to miss a detection
            print(f"BACKUP MONITOR cycle unknown: {e}")
    found = detect(age, limit, target, measure(age, limit, cycle, stage, a))
    common.report({"outcome": "detections", "detections": found})
    shown = "none" if age is None else f"{age:.1f}h"
    cyc = "-" if cycle is None else f"{cycle:.2f}h"
    print(f"BACKUP MONITOR age={shown} threshold={limit}h cycle={cyc} stage={stage} detections={len(found)}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"BACKUP MONITOR config: {e}", file=sys.stderr)
        sys.exit(2)
