"""ops/verify-run-processed (block health-verify@1): the check routine for pipeline.rerun_scheduled's registry check
(window_failed_episodes_processed). Started by flotillad while a case is in checking, with the case (FLOTILLA_CASE) and what to verify
(FLOTILLA_CHECK). It reads the re-run's outcome from the route named by PROCESS_SOURCE (host config: SOURCE_<NAME>) and reports

  the re-run finished, processed episodes, and none failed  -> {"outcome": "verified", ...}
  it failed as a whole, or episodes failed in it            -> {"outcome": "failed", ...}   (the case escalates: it did not do what was wanted)
  not finished, not the re-run, unreadable, or any doubt    -> {"outcome": "not_yet", ...}  (flotillad looks again after its backoff)

The shape it reads is the health contract's T5 (docs/wip/health-contract.md), as flotilla understands it today:

  {"run_id": ..., "trigger": "on_demand", "status": "running" | "finished" | "failed", "started_at": ISO, "finished_at": ISO or null,
   "episodes": {"processed": n, "failed": [{"episode": ..., "stage": ..., "error_type": ...}], "skipped": n}}

NOT EXERCISED: no producer serves T5 yet, so this has been tested on fixtures of that shape only. The contract reads an outcome by id, and
FLOTILLA_CHECK does not carry the run id the re-run's executor would record, so the route the installation names is "the newest on-demand
run": a run counts only if its trigger is on_demand and it started after the case went to checking. A re-run that processed no episode
is not verified by that alone. Nothing here writes. Environment: PROCESS_SOURCE, SOURCE_<NAME>, HEALTH_ALLOW_HTTP.
"""
import datetime
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import common  # noqa: E402
import health  # noqa: E402
import service_up  # noqa: E402

VERIFY = "window_failed_episodes_processed"
MAX_LISTED = 5


def when(s):
    if not isinstance(s, str):
        return None
    try:
        return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None


def shaped(doc):
    """The outcome, when it is T5-shaped; None otherwise (anything else is doubt)."""
    if not isinstance(doc, dict):
        return None
    ep = doc.get("episodes")
    if (not isinstance(ep, dict) or isinstance(ep.get("processed"), bool) or not isinstance(ep.get("processed"), int)
            or not isinstance(ep.get("failed"), list) or isinstance(ep.get("skipped"), bool) or not isinstance(ep.get("skipped"), int)):
        return None
    if doc.get("status") not in ("running", "finished", "failed") or not isinstance(doc.get("trigger"), str):
        return None
    if not isinstance(doc.get("run_id"), (str, int)) or when(doc.get("started_at")) is None:
        return None
    if any(not isinstance(f, dict) for f in ep["failed"]):
        return None
    return doc


def decide(check, case, doc):
    if check.get("verify") != VERIFY:
        raise common.ConfigError(f"this routine checks {VERIFY}, not {check.get('verify')!r}")
    if service_up.host_of(case.get("target")) is None:
        raise common.ConfigError(f"the case's target {str(case.get('target'))[:60]!r} is not on a host")
    run = shaped(doc)
    if run is None:
        return {"outcome": "not_yet", "detail": "the run outcome is missing or not in the shape the health contract's T5 asks for"}
    since = when(check.get("since"))
    if run["trigger"] != "on_demand" or (since is not None and when(run["started_at"]) < since):
        return {"outcome": "not_yet", "detail": f"the newest run ({str(run['run_id'])[:40]}) is not the re-run (trigger {str(run['trigger'])[:20]}, "
                                                f"started {run['started_at']})"}
    ep = run["episodes"]
    if run["status"] == "running":
        return {"outcome": "not_yet", "detail": f"the re-run {str(run['run_id'])[:40]} is still running ({ep['processed']} processed so far)"}
    if run["status"] == "failed" or ep["failed"]:
        shown = "; ".join(f"{str(f.get('episode'))[:40]} at {str(f.get('stage'))[:30]}: {str(f.get('error_type'))[:30]}"
                          for f in ep["failed"][:MAX_LISTED])
        return {"outcome": "failed", "detail": f"the re-run {str(run['run_id'])[:40]} {run['status']}: {len(ep['failed'])} episode(s) failed"
                                               + (f" ({shown})" if shown else "")}
    if ep["processed"] < 1:
        return {"outcome": "not_yet", "detail": f"the re-run {str(run['run_id'])[:40]} finished and processed no episode ({ep['skipped']} skipped)"}
    return {"outcome": "verified", "detail": f"the re-run {str(run['run_id'])[:40]} finished: {ep['processed']} processed, none failed, "
                                             f"{ep['skipped']} skipped"}


def main(fetch=None):
    case = json.loads(common.env("FLOTILLA_CASE"))
    check = json.loads(common.env("FLOTILLA_CHECK"))
    fetch = fetch or (lambda url: json.loads(health.fetch_raw(url)))
    url = health.source_url(common.env("PROCESS_SOURCE"))
    try:
        doc = fetch(url)
    except (common.ConfigError, ValueError, OSError) as e:   # the message is never the URL: it is host config
        doc = None
        reason = type(e).__name__
    out = decide(check, case, doc)
    if doc is None:
        out["detail"] = f"the run outcome could not be read ({reason}): " + out["detail"]
    common.report(out)
    print(f"VERIFY RUN PROCESSED case={case.get('id')} outcome={out['outcome']} {out['detail']}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"VERIFY RUN PROCESSED config: {e}", file=sys.stderr)
        sys.exit(2)
