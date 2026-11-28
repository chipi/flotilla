"""ops/verify-service-up (block service-verify@1): the check routine for a service action's registry check (service_up_again). Started by
flotillad while a case is in checking, with the case (FLOTILLA_CASE) and what to verify (FLOTILLA_CHECK):

  the case's service read up for the whole of the last WINDOW  -> {"outcome": "verified", ...}
  otherwise, or no reading                                     -> {"outcome": "not_yet", ...}

The service is the one the registry action's service_down precondition names (one home, as the monitor and the acting step read it). A
service that came up a moment ago and went down again is not verified: the read is the minimum over the window. flotillad looks again
after its backoff and escalates the case as not verified when the check's window (the action's `within`) ends first. A check that is not
this routine's, or a target that is not on a host, is a configuration error (exit 2: no result, the case is not moved).
Read only. Environment: VM_URL, MONITOR_ACTION, MONITOR_METRIC, MONITOR_SERVICE_LABEL.
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
import service_up  # noqa: E402

VERIFY = "service_up_again"
WINDOW = "5m"


def read(instance, service, metric, label, query=None):
    """The lowest reading of the service in the last WINDOW (1 when it was up throughout), or None when it reports nothing."""
    return (query or common.query)(f"min(min_over_time({service_up.selector(metric, label, service, instance)}[{WINDOW}]))")


def decide(check, case, service, value):
    if check.get("verify") != VERIFY:
        raise common.ConfigError(f"this routine checks {VERIFY}, not {check.get('verify')!r}")
    inst = service_up.host_of(case.get("target"))
    if inst is None:
        raise common.ConfigError(f"the case's target {str(case.get('target'))[:60]!r} is not on a host")
    if value is None:
        return {"outcome": "not_yet", "detail": f"no reading for {service} on {inst} in the last {WINDOW}"}
    if value >= 1:
        return {"outcome": "verified", "detail": f"{service} on {inst} read up throughout the last {WINDOW}"}
    return {"outcome": "not_yet", "detail": f"{service} on {inst} read down within the last {WINDOW}"}


def main():
    case = json.loads(common.env("FLOTILLA_CASE"))
    check = json.loads(common.env("FLOTILLA_CHECK"))
    service = service_up.service_of(common.action(common.env("MONITOR_ACTION")), os.environ.get("FLEETD_STAGE", "shadow"))
    inst = service_up.host_of(case.get("target"))
    value = None
    if inst is not None and check.get("verify") == VERIFY:
        value = read(inst, service, common.env("MONITOR_METRIC"), common.env("MONITOR_SERVICE_LABEL"))
    out = decide(check, case, service, value)
    common.report(out)
    print(f"VERIFY SERVICE UP case={case.get('id')} outcome={out['outcome']} {out['detail']}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"VERIFY SERVICE UP config: {e}", file=sys.stderr)
        sys.exit(2)
