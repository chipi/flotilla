"""ops/case-review: the post-incident review (ADR-0016). Reads the cases closed in the last 7 days (and
the ones they recur on) through flotillad's worker listener, applies code rules, and proposes:
  - a process_change detection: flotillad opens the case, ops/process-change plans it, a person approves;
  - a note on a case, which a person activates in Knowledge before any agent reads it.
It changes nothing itself. Each proposal names its evidence. Rules (RFC-0009 §2):

  below_cycle      a case cleared on its own while its measure stayed within the source's normal cycle:
                   the threshold sits below the cycle; propose it just above what was seen
  self_clearing    the same kind and target cleared on its own 3 times in 7 days (no cycle known)
  fix_not_holding  a case recurring after a verified fix, twice in 30 days
  wrong_diagnosis  a person's Wrong verdict: a note drafted from the case
  refused_plan     a plan a person rejected, with their reason: a note

Run with FLOTILLA_API_URL / FLOTILLA_API_TOKEN_FILE (the worker sets them) and FLOTILLA_PLAN_FILE.
"""
import json
import math
import os
import sys
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
import flotilla_api  # noqa: E402

WINDOW = timedelta(days=7)
RECUR_WINDOW = timedelta(days=30)
SELF_CLEARS = 3
QUIET = timedelta(days=30)  # after a rejected process change, the same proposal waits this long
TAG = "case-review"


def ts(s):
    if not s:
        return None
    s = s.rstrip("Z").split(".")[0]
    return datetime.fromisoformat(s).replace(tzinfo=timezone.utc)


def measures(events):
    """Every measure the case's detections reported, in order."""
    out = []
    for e in events:
        if e.get("data") and e["kind"] in ("case.detected", "signal.absorbed", "signal.attached"):
            try:
                m = json.loads(e["data"]).get("measure")
            except ValueError:
                continue
            if isinstance(m, dict) and isinstance(m.get("value"), (int, float)):
                out.append(m)
    return out


def executed(events):
    return any(e["kind"] == "case.executing" for e in events)


def cleared_itself(case, events):
    return case["status"] == "closed" and not executed(events) and \
        case.get("resolution") in ("plan_recorded", "resolved_itself") and \
        any(e["kind"] in ("incident.cleared", "case.closed") for e in events)


def proposed_threshold(peak, cycle):
    """The smallest whole hour above both what was seen and the cycle."""
    top = max(peak, cycle)
    return math.floor(top) + 1


def below_cycle(case, events):
    if not cleared_itself(case, events):
        return None
    ms = measures(events)
    if not ms:
        return None
    last = ms[-1]
    cycle, threshold, src = last.get("cycle_hours"), last.get("threshold"), last.get("source") or {}
    if not isinstance(cycle, (int, float)) or not isinstance(threshold, (int, float)) or not src.get("file"):
        return None
    peak = max(m["value"] for m in ms)
    if threshold >= cycle or peak > cycle * 1.1:
        return None  # the threshold is above the cycle, or the source really ran late: not this rule
    new = proposed_threshold(peak, cycle)
    subject = f"{src['file']}/{src['action']}/{src['key']}"
    cond_from, cond_to = f"{src['condition']}({threshold}h)", f"{src['condition']}({new}h)"
    return {"rule": "below_cycle", "subject": subject, "file": src["file"], "action": src["action"], "key": src["key"],
            "from": cond_from, "to": cond_to,
            "evidence": [f"{case['id']}: opened at {ms[0]['value']}h, the highest seen {peak}h, cleared on its own with no fix",
                         f"the source's normal cycle: {cycle}h (median between successes over 14 days)",
                         f"the threshold {threshold}h is below the cycle, so a case opens on every healthy cycle"],
            "title": f"The {src['key']} threshold of {src['action']} ({threshold}h) is below the source's {cycle:.0f}h cycle"}


def detection(p):
    target = "repo:" + p["subject"]
    return {"kind": "process_change", "target": target, "title": p["title"],
            "detail": f"{p['rule']}: {p['from']} -> {p['to']} in {p['file']}" if p.get("to") else f"{p['rule']}: {p['title']}",
            "data": {"proposal": p}}


def settled(target, closed_pcs, now, since=None):
    """Whether a closed process change already answers this proposal: one a person dismissed inside QUIET,
    or any closed after the evidence it would rest on (applied, or found no longer needed)."""
    for c in closed_pcs:
        if c["target"] != target:
            continue
        t = ts(c.get("closed_at"))
        if not t:
            continue
        if c.get("resolution") == "operator_resolved" and now - t < QUIET:
            return True
        if since and t >= since:
            return True
    return False


def notes_for(case, events, already):
    """Notes to propose from one case: (rule, text), skipping what this review already proposed."""
    out = []
    for e in events:
        if e["kind"] == "feedback.wrong" and "wrong_diagnosis" not in already:
            out.append(("wrong_diagnosis", f"({TAG}: wrong_diagnosis) {case['kind']} on {case['target']}: "
                        f"{e['actor']} judged the diagnosis wrong on {case['id']}. What was it instead? Edit this note "
                        f"to say, or drop it."))
        if e["kind"] == "case.escalated" and e.get("from") == "awaiting_approval" and \
                (e.get("detail") or "").startswith("rejected:") and "refused_plan" not in already:
            out.append(("refused_plan", f"({TAG}: refused_plan) {case['kind']} on {case['target']}: "
                        f"{e['actor']} rejected the plan on {case['id']}: {e['detail'][len('rejected:'):].strip()}"))
    return out


def review(api, now=None):
    now = now or datetime.now(timezone.utc)
    all_cases = api.cases(all=1, limit=1000)
    recent = [c for c in all_cases if c["status"] == "closed" and ts(c.get("closed_at")) and now - ts(c["closed_at"]) < WINDOW]
    closed_pcs = [c for c in all_cases if c["kind"] == "process_change" and c["status"] == "closed"]
    detections, notes, seen = [], [], set()
    self_clears, recurs = {}, {}
    for c in recent:
        if c["kind"] == "process_change":
            continue
        full = api.case(c["id"])
        events = full.get("events", [])
        p = below_cycle(c, events)
        if p:
            d = detection(p)
            if d["target"] not in seen and not settled(d["target"], closed_pcs, now, ts(c["closed_at"])):
                seen.add(d["target"])
                detections.append(d)
        elif cleared_itself(c, events):
            self_clears.setdefault((c["kind"], c["target"]), []).append(c["id"])
        if c.get("recurrence_of"):
            prev = api.case(c["recurrence_of"])["case"]
            if prev.get("resolution") == "verified" and now - ts(c["opened_at"]) < RECUR_WINDOW:
                recurs.setdefault((c["kind"], c["target"]), []).append(c["id"])
        already = {r for e in events if e["kind"] == "note.proposed" for r in ("wrong_diagnosis", "refused_plan")
                   if f"{TAG}: {r}" in (e.get("detail") or "")}
        for rule, text in notes_for(c, events, already):
            notes.append((c["id"], rule, text))
    for (kind, target), ids in sorted(self_clears.items()):
        if len(ids) >= SELF_CLEARS:
            subject = f"checks/{kind}/{target.replace(':', '_')}"
            p = {"rule": "self_clearing", "subject": subject, "kind": kind, "case_target": target,
                 "evidence": [f"{len(ids)} {kind} cases on {target} cleared on their own in 7 days: {', '.join(ids)}"],
                 "title": f"{kind} on {target} cleared on its own {len(ids)} times in 7 days: look at its threshold"}
            d = detection(p)
            if not settled(d["target"], closed_pcs, now, now - WINDOW):
                detections.append(d)
    for (kind, target), ids in sorted(recurs.items()):
        if len(ids) >= 2:
            subject = f"fixes/{kind}/{target.replace(':', '_')}"
            p = {"rule": "fix_not_holding", "subject": subject, "kind": kind, "case_target": target,
                 "evidence": [f"{len(ids)} {kind} cases on {target} came back after a verified fix in 30 days: {', '.join(ids)}"],
                 "title": f"The fix for {kind} on {target} does not hold: escalate this kind instead of fixing it again"}
            d = detection(p)
            if not settled(d["target"], closed_pcs, now, now - RECUR_WINDOW):
                detections.append(d)
    return detections, notes


def main():
    api = flotilla_api.from_env()
    detections, notes = review(api)
    proposed = 0
    for case_id, rule, text in notes:
        try:
            api.propose_note(case_id, text)
            proposed += 1
        except flotilla_api.APIError as e:
            print(f"CASE REVIEW note on {case_id} ({rule}): {e}")
    common.report({"outcome": "detections", "detections": detections})
    print(f"CASE REVIEW process_changes={len(detections)} notes={proposed}")
    for d in detections:
        print(f"  {d['target']}: {d['detail']}")


if __name__ == "__main__":
    try:
        main()
    except (common.ConfigError, flotilla_api.APIError) as e:
        print(f"CASE REVIEW: {e}", file=sys.stderr)
        sys.exit(2)
