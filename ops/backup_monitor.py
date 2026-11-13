"""ops/backup-monitor: the code-only Ops monitor of pipeline v1 (RFC-0004 §10). Reads the age of the
mini's off-box backup and reports a failed_backup detection on BACKUP_TARGET when it is above the
threshold of backup.rerun_observability's backup_age_above precondition for the run's stage (20h in
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


def detect(age, limit, target):
    if age is None:
        return [{"kind": "failed_backup", "target": target, "detail": "no successful off-box backup in 30 days"}]
    if age > limit:
        return [{"kind": "failed_backup", "target": target, "detail": f"backup {age:.1f}h old, above {limit}h"}]
    return []


def main():
    target = common.env("BACKUP_TARGET")
    stage = os.environ.get("FLEETD_STAGE", "shadow")
    limit = threshold_hours(common.action(common.RERUN), stage)
    age = common.backup_age_hours()
    found = detect(age, limit, target)
    common.report({"outcome": "detections", "detections": found})
    shown = "none" if age is None else f"{age:.1f}h"
    print(f"BACKUP MONITOR age={shown} threshold={limit}h stage={stage} detections={len(found)}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"BACKUP MONITOR config: {e}", file=sys.stderr)
        sys.exit(2)
