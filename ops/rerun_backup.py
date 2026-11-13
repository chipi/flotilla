"""ops/rerun-backup: the acting routine for failed_backup cases (pipeline v1 step 7, RFC-0004 §7). Started
by flotillad with the case (FLOTILLA_CASE), it re-checks with fresh numbers and decides:

  the backup is fine again        -> {"outcome": "resolved_itself", ...}
  the case is not one it can act on -> {"outcome": "escalate", ...}
  otherwise                       -> {"outcome": "plan", "action": "backup.rerun_observability", ...}

It plans only; policy decides and the host agent runs (ADR-0005 decision 4). The preconditions are the
registry's for the run's stage: backup_age_above is evaluated here; not_running is the host's to check
(launchctl print, before it runs anything: ADR-0006 §7), recorded as such.
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402


def backup_age_above(arg, facts):
    return facts["age"] is None or facts["age"] > common.hours(arg)


# Each precondition name the routine's action uses: evaluated here, or None when the host checks it.
CONDITIONS = {"backup_age_above": backup_age_above, "not_running": None}


def decide(case, a, stage, age):
    if case.get("kind") != "failed_backup" or not case.get("target", "").startswith("host:"):
        return {"outcome": "escalate", "detail": f"not a failed_backup case on a host: {case.get('kind')} {case.get('target')}"}
    host = case["target"].split(":", 1)[1]
    facts = {"age": age}
    shown = "none in 30 days" if age is None else f"{age:.1f}h"
    checks = []
    for c in common.preconditions(a, stage):
        name, arg = common.parse(c)
        if name not in CONDITIONS:
            return {"outcome": "escalate", "detail": f"precondition {name} is not one this routine knows"}
        fn = CONDITIONS[name]
        if fn is None:
            checks.append(f"{c}: the host's to check")
            continue
        if not fn(arg, facts):
            return {"outcome": "resolved_itself", "detail": f"backup {shown} old: {c} no longer holds"}
        checks.append(f"{c}: holds (age {shown})")
    return {"outcome": "plan", "action": a["name"], "params": {"host": host}, "detail": "; ".join(checks)}


def main():
    case = json.loads(common.env("FLOTILLA_CASE"))
    stage = os.environ.get("FLEETD_STAGE", "shadow")
    a = common.action(common.RERUN)
    out = decide(case, a, stage, common.backup_age_hours())
    common.report(out)
    print(f"RERUN BACKUP case={case.get('id')} stage={stage} outcome={out['outcome']} {out.get('detail', '')}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"RERUN BACKUP config: {e}", file=sys.stderr)
        sys.exit(2)
