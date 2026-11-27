"""ops/sop-draft: the SOP draft (operator step 1 build plan C6, decisions 4 and 5). Reads the cases of the kinds it is given that closed
`verified` in the last days, through flotillad's worker listener (as ops/case_review.py does), and for each one an investigation
solved and a person approved, drafts the SOP it teaches: a new `<fleet>/sops/<id>/SKILL.md`, or an update when the fleet already has
that id. A case an SOP solved teaches nothing new: it is not drafted.

It writes nothing to any SOP folder, ever (rule 3: nothing rewrites its own playbooks or specs at runtime). It reports a detection:
a case of kind `sop_proposal`, target `sop:<fleet>/<id>`, whose data is
  {sop_id, mode: add|update, fleet, path, body: the whole SKILL.md, evidence: [case ids], diff: the ready unified diff}
A person reads the diff in the case and commits it by hand (decision 5: until the X3 workers are fixed, no github.open_pr for it).
The draft is code, not a model: the SOP's applies_to, fix and verify come from the case and the registry, its prose from the case's
own record, so a proposal can be checked against the case it names.

It reports every proposal still pending on each run (a case no routine acts on is closed by flotillad once its monitor stops
reporting it), and stops when a person closed the proposal's case after the evidence, or the SOP already holds the evidence.

  --fleet NAME     the fleet whose SOPs are drafted (<code root>/fleets/NAME/sops, or --sops DIR)
  --kinds a,b      the case kinds to learn from
  --days N         how far back a verified case counts (default 30)
  SOP_REPO_PREFIX  the fleets repo's path prefix of a fleet's folder (default none: `<fleet>/sops/<id>/SKILL.md`)
Run with FLOTILLA_API_URL / FLOTILLA_API_TOKEN_FILE (the worker sets them) and FLOTILLA_PLAN_FILE.
"""
import argparse
import json
import os
import re
import sys
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "runtime"))
import common  # noqa: E402
import flotilla_api  # noqa: E402
import process_change  # noqa: E402
import sop_act  # noqa: E402
import sops  # noqa: E402

KIND = "sop_proposal"
MAX_DATA = 8000     # flotillad drops a detection's data over 8192 bytes
MAX_FINDING = 300
FINDING = re.compile(r"investigated: ([a-z][a-z0-9_.]*): (.*?) \[reads: ")
READ = re.compile(r"metric:([a-zA-Z_:][a-zA-Z0-9_:]*)=")
SLUG = re.compile(r"[^a-z0-9]+")


def ts(s):
    if not s:
        return None
    return datetime.fromisoformat(s.rstrip("Z").split(".")[0]).replace(tzinfo=timezone.utc)


def slug(text):
    s = SLUG.sub("-", str(text).lower()).strip("-")[:60].strip("-")
    return s if re.match(r"^[a-z]", s) else ("sop-" + s).strip("-")


def event_data(events, kind):
    for e in events:
        if e["kind"] == kind and e.get("data"):
            return sops.case_data({"data": e["data"]})
    return {}


def investigated_plan(full):
    """(the plan record, its detail) of a verified case an investigation solved and an operator approved, else None: an SOP-followed
    case is not one (it teaches nothing the SOP lacks), and nor is an investigated plan nobody approved."""
    events, plans = full.get("events") or [], full.get("plans") or []
    planned = [e for e in events if e["kind"] == "case.planned"]
    if not planned or not plans or sop_act.INVESTIGATED not in (planned[-1].get("detail") or ""):
        return None
    if not any(e["kind"] == "case.executing" and e.get("by") == "operator" for e in events):
        return None
    return plans[-1], planned[-1]["detail"]


def build(fleet, case, full, registry, existing):
    """(the proposed Sop, the old Sop or None) for a verified, investigated case, or (None, why not)."""
    got = investigated_plan(full)
    if got is None:
        return None, "not an approved investigated fix"
    plan, detail = got
    action = registry.get(plan["action"])
    if action is None:
        return None, f"{plan['action']} is not in the registry"
    check = (action.get("check") or {})
    if not check.get("verify") or not sops.WITHIN.match(str(check.get("within"))):
        return None, f"{action['name']} has no check a SOP can name"
    data = event_data(full.get("events") or [], "case.detected")
    alert = str(data.get("alert") or "")
    sop_id = slug(alert or case["kind"])
    found = FINDING.search(detail)
    reads = list(dict.fromkeys(READ.findall(detail)))
    fix = [{"action": action["name"]}]
    old = existing.get(sop_id)
    if old is not None:
        if case["id"] in old.evidence:
            return None, "the SOP already cites this case"
        note = f"Also solved by {action['name']} ({case['id']}, verified)."
        new = sops.Sop(name=old.name, description=old.description, applies_to=old.applies_to, fix=old.fix, verify=old.verify,
                       rung=old.rung, body=old.body.rstrip() + "\n\n" + note, diagnosis=old.diagnosis, rollback=old.rollback,
                       evidence=old.evidence + [case["id"]])
        return new, old
    what = f"{case['kind']} on {case['target']}" + (f", alert {alert}" if alert else "")
    body = [f"Drafted from {case['id']}: an investigation found this fix and a person approved it. Review it before accepting.", "",
            f"What was seen: {what}.",
            "Diagnosis: " + (", ".join(f"read the metric {r}" for r in reads) if reads else "none was recorded") + ".",
            f"Fix: {action['name']} (risk {action.get('risk')}), checked by {check['verify']} within {check['within']}."]
    if found:
        body.append("The investigation's finding: " + found.group(2)[:MAX_FINDING])
    new = sops.Sop(
        name=sop_id, description=f"Use when a {case['kind']} case" + (f" reports the alert {alert}" if alert else "") + " and no earlier SOP covers it.",
        applies_to={"kinds": [case["kind"]], "hosts": ["*"], "alerts": [alert] if alert else []},
        fix=fix, verify={"check": check["verify"], "within": check["within"]}, rung=1, body="\n".join(body),
        diagnosis=[{"read": "metric", "name": r} for r in reads], evidence=[case["id"]])
    return new, None


def proposal(fleet, sop, old, prefix=""):
    """The detection for one drafted SOP, or (None, why): the diff is built here, and the data must fit what flotillad keeps."""
    path = process_change.sop_path(fleet, sop.name, prefix)
    text = sops.render(sop)
    try:
        sops.parse(text, sop.name)
    except sops.SopError as e:
        return None, f"the draft does not parse: {e}"
    diff = process_change.file_diff(path, None if old is None else old_text(old), text)
    if not diff:
        return None, "the draft changes nothing"
    mode = "add" if old is None else "update"
    data = {"sop_id": sop.name, "mode": mode, "fleet": fleet, "path": path, "body": text, "evidence": sop.evidence, "diff": diff}
    if len(json.dumps(data)) > MAX_DATA:
        return None, "the draft is over what a case keeps"
    return {"kind": KIND, "target": f"sop:{fleet}/{sop.name}", "title": f"SOP {mode}: {fleet}/{sop.name}",
            "detail": f"{mode} {path} from {', '.join(sop.evidence)}", "data": data}, ""


def old_text(old):
    with open(old.path) as f:
        return f.read()


def settled(target, closed, since):
    """Whether a person already answered this proposal: a sop_proposal case on the target closed after the evidence closed."""
    return any(c["target"] == target and ts(c.get("closed_at")) and since and ts(c["closed_at"]) >= since for c in closed)


def draft(api, fleet, kinds, existing, registry, prefix="", days=30, now=None, log=print):
    now = now or datetime.now(timezone.utc)
    all_cases = api.cases(all=1, limit=1000)
    closed = [c for c in all_cases if c["kind"] == KIND and c["status"] == "closed"]
    out, seen = [], set()
    for c in all_cases:
        if c["kind"] not in kinds or c["status"] != "closed" or c.get("resolution") != "verified":
            continue
        when = ts(c.get("closed_at"))
        if not when or now - when > timedelta(days=days):
            continue
        sop, old = build(fleet, c, api.case(c["id"]), registry, existing)
        if sop is None:
            continue
        target = f"sop:{fleet}/{sop.name}"
        if target in seen or settled(target, closed, when):
            continue
        det, why = proposal(fleet, sop, old, prefix)
        if det is None:
            log(f"SOP DRAFT {c['id']}: {why}")
            continue
        seen.add(target)
        out.append(det)
    return out


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--fleet", required=True)
    ap.add_argument("--kinds", required=True)
    ap.add_argument("--sops")
    ap.add_argument("--days", type=int, default=30)
    args = ap.parse_args(argv)
    existing = {s.name: s for s in sops.load_all(args.sops or sops.sops_dir(args.fleet))}
    dets = draft(flotilla_api.from_env(), args.fleet, [k for k in args.kinds.split(",") if k], existing, sops.load_registry(),
                 os.environ.get("SOP_REPO_PREFIX", ""), args.days)
    common.report({"outcome": "detections", "detections": dets})
    print(f"SOP DRAFT proposals={len(dets)}")
    for d in dets:
        print(f"  {d['target']}: {d['detail']}")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (common.ConfigError, flotilla_api.APIError, sops.SopError, OSError) as e:
        print(f"SOP DRAFT: {e}", file=sys.stderr)
        sys.exit(2)
