"""ops/verify-free-ratio (block metric-verify@2): the check routine for disk.cleanup_known (its registry check disk_free_above_threshold,
RFC-0016 §8). Started by flotillad while a case is in checking, with the case (FLOTILLA_CASE) and what to verify (FLOTILLA_CHECK):

  the case's mountpoint has at least the threshold free  -> {"outcome": "verified", ...}
  otherwise, or no reading                                -> {"outcome": "not_yet", ...}

The threshold is the registry action's free_ratio_below precondition: the number the monitor opened the case at, one home. flotillad
looks again after its backoff and escalates the case as not verified when the check's window (the action's `within`) ends first. A check
that is not this routine's, or a target that is not a mountpoint, is a configuration error (exit 2: no result, the case is not moved).
Read only. Environment: VM_URL, MONITOR_METRIC and MONITOR_SIZE_METRIC (the node exporter's names by default), MONITOR_ACTION.
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
import free_ratio  # noqa: E402

VERIFY = "disk_free_above_threshold"


def decide(check, case, limit, ratio):
    if check.get("verify") != VERIFY:
        raise common.ConfigError(f"this routine checks {VERIFY}, not {check.get('verify')!r}")
    where = free_ratio.decode_target(case.get("target"))
    if where is None:
        raise common.ConfigError(f"the case's target {str(case.get('target'))[:60]!r} is not an instance and a mountpoint")
    inst, mount = where
    if ratio is None:
        return {"outcome": "not_yet", "detail": f"no reading for {mount} on {inst}; the threshold is {limit:.0%} free"}
    if ratio >= limit:
        return {"outcome": "verified", "detail": f"{mount} on {inst} has {ratio:.1%} free, back at or above {limit:.0%}"}
    return {"outcome": "not_yet", "detail": f"{mount} on {inst} has {ratio:.1%} free, below {limit:.0%}"}


def main():
    case = json.loads(common.env("FLOTILLA_CASE"))
    check = json.loads(common.env("FLOTILLA_CHECK"))
    action = common.action(common.env("MONITOR_ACTION"))
    limit = free_ratio.limit_of(action, os.environ.get("FLEETD_STAGE", "shadow"))
    where = free_ratio.decode_target(case.get("target"))
    ratio = None
    if where is not None:
        ratio = free_ratio.read_ratio(where[0], where[1], os.environ.get("MONITOR_METRIC") or free_ratio.AVAIL_METRIC,
                                      os.environ.get("MONITOR_SIZE_METRIC") or free_ratio.SIZE_METRIC)
    out = decide(check, case, limit, ratio)
    common.report(out)
    print(f"VERIFY FREE RATIO case={case.get('id')} outcome={out['outcome']} {out['detail']}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"VERIFY FREE RATIO config: {e}", file=sys.stderr)
        sys.exit(2)
