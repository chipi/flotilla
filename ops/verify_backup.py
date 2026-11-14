"""ops/verify-backup: the check routine for backup.rerun_observability (RFC-0004 §7, its registry check
backup_newer_than_execution). Started by flotillad while a case is in checking, with the case
(FLOTILLA_CASE) and what to verify (FLOTILLA_CHECK: since = when the host reported the rerun done):

  the backup last succeeded at or after `since`  -> {"outcome": "verified", ...}
  otherwise                                      -> {"outcome": "not_yet", ...}

flotillad looks again after its backoff, and escalates the case as not verified when the check's
window ends first. A check that is not this routine's, or a bad `since`, is a configuration error
(exit 2: no result, so the case is not moved).
"""
import json
import os
import sys
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402

VERIFY = "backup_newer_than_execution"


def parse_time(s):
    """An RFC 3339 time as flotillad writes it (nanoseconds, Z), to Unix seconds."""
    if not isinstance(s, str) or not s.endswith("Z"):
        raise common.ConfigError(f"since: an RFC 3339 UTC time, not {s!r}")
    head, _, frac = s[:-1].partition(".")
    try:
        t = datetime.strptime(head, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
    except ValueError:
        raise common.ConfigError(f"since: an RFC 3339 UTC time, not {s!r}")
    if frac and not frac.isdigit():
        raise common.ConfigError(f"since: an RFC 3339 UTC time, not {s!r}")
    return t + (float("0." + frac) if frac else 0.0)


def show(t):
    return datetime.fromtimestamp(t, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def decide(check, last_success):
    if check.get("verify") != VERIFY:
        raise common.ConfigError(f"this routine checks {VERIFY}, not {check.get('verify')!r}")
    since = parse_time(check.get("since"))
    if last_success is None:
        return {"outcome": "not_yet", "detail": f"no successful backup in 30 days; the rerun was done at {show(since)}"}
    if last_success >= since:
        return {"outcome": "verified", "detail": f"backup succeeded at {show(last_success)}, after the rerun at {show(since)}"}
    return {"outcome": "not_yet", "detail": f"the newest backup is {show(last_success)}, before the rerun at {show(since)}"}


def main():
    case = json.loads(common.env("FLOTILLA_CASE"))
    check = json.loads(common.env("FLOTILLA_CHECK"))
    out = decide(check, common.backup_last_success())
    common.report(out)
    print(f"VERIFY BACKUP case={case.get('id')} outcome={out['outcome']} {out['detail']}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"VERIFY BACKUP config: {e}", file=sys.stderr)
        sys.exit(2)
