"""The business examples' executor and checks, against the stand-in systems (store.py).

  apply.py execute   the executing routine of an approved plan (FLOTILLA_PLAN, FLOTILLA_CASE): checks the
                     plan's hash is the one the person or policy approved, re-checks what the plan assumed
                     (the field still has its old value, the campaign is still ready, the prospect is not
                     suppressed), then carries it out once.
                     -> {"outcome": "executed", "detail", "ref"} or {"outcome": "escalate", "detail"}
  apply.py check     the check of an executed action (FLOTILLA_CHECK): reads the system and reports
                     {"outcome": "verified"} when the change is there, {"outcome": "not_yet"} otherwise.

An installation replaces store.py with an adapter to its CRM, campaign tool and mail service; the checks
here stay what decides that an action happened.
"""
import datetime
import hashlib
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import store  # noqa: E402


def go_json(v):
    """json.dumps as Go's encoding/json writes it (as bugfix/open_pr.py), so the hash is policy's."""
    s = json.dumps(v, separators=(",", ":"), ensure_ascii=False)
    for a, b in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), (" ", "\\u2028"), (" ", "\\u2029")):
        s = s.replace(a, b)
    return s


def plan_hash(case_id, action, params):
    """policy.Plan.Hash: sha256 of the JSON of [case id, action, [[key, value] sorted by key]]."""
    return hashlib.sha256(go_json([case_id, action, [[k, params[k]] for k in sorted(params)]]).encode()).hexdigest()


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def update_field(p):
    contacts = store.load("contacts")
    c = store.by_id(contacts).get(p["contact"])
    if c is None:
        return "escalate", f"contact {p['contact']} is gone", ""
    if p["field"] in c.get("locked", []):
        return "escalate", f"a person locked {c['id']}'s {p['field']} since the plan", ""
    if str(c.get(p["field"])) != p["from"]:
        return "escalate", f"{c['id']}'s {p['field']} is {c.get(p['field'])!r} now, not {p['from']!r}: changed since the plan", ""
    c[p["field"]] = p["to"]
    c.setdefault("history", []).append({"at": now(), "by": "flotilla", "field": p["field"], "from": p["from"], "to": p["to"]})
    store.save("contacts", contacts)
    return "executed", f"{c['id']} {p['field']} -> {p['to']!r}", f"contact {c['id']}"


def merge(p):
    contacts = store.load("contacts")
    by = store.by_id(contacts)
    keep, drop = by.get(p["keep"]), by.get(p["drop"])
    if keep is None or drop is None:
        return "escalate", "one of the two contacts is gone", ""
    for k, v in drop.items():
        if k not in ("id", "created", "history", "locked") and not keep.get(k):
            keep[k] = v
    keep.setdefault("history", []).append({"at": now(), "by": "flotilla", "merged": drop["id"]})
    contacts = [c for c in contacts if c["id"] != drop["id"]]
    store.save("contacts", contacts)
    store.append("merged", dict(drop, merged_into=keep["id"], merged_at=now()))
    return "executed", f"{drop['id']} merged into {keep['id']}", f"contact {keep['id']}"


def campaign(p, status):
    campaigns = store.load("campaigns")
    c = store.by_id(campaigns).get(p["campaign"])
    if c is None or c.get("status") != "ready":
        return "escalate", f"campaign {p['campaign']} is not ready now", ""
    c["status"] = status
    c[status + "_at"] = now()
    if p.get("reason"):
        c["held_because"] = p["reason"]
    store.save("campaigns", campaigns)
    return "executed", f"{c['id']} {status}", f"campaign {c['id']}"


def send(p):
    prospects = store.by_id(store.load("prospects"))
    pr = prospects.get(p["prospect"])
    suppressed = {s.lower() for s in store.load("suppression")}
    if pr is None or pr.get("do_not_contact") or p["to"].lower() in suppressed:
        return "escalate", f"{p['prospect']} may not be contacted now", ""
    if any(m["prospect"] == p["prospect"] for m in store.load("outbox", [])):
        return "escalate", f"{p['prospect']} was already contacted", ""
    store.append("outbox", {"prospect": p["prospect"], "to": p["to"], "subject": p["subject"], "body": p["body"],
                            "facts": p["facts"], "sent": now()})
    return "executed", f"first touch to {p['prospect']} sent", f"outbox {p['prospect']}"


def flag(p):
    accounts = store.load("accounts")
    a = store.by_id(accounts).get(p["account"])
    if a is None:
        return "escalate", f"account {p['account']} is gone", ""
    a["risk"], a["risk_note"], a["risk_flagged_at"] = p["risk"], p["note"], now()
    store.save("accounts", accounts)
    return "executed", f"{a['id']} flagged {p['risk']} for its owner", f"account {a['id']}"


ACTIONS = {
    "crm.update_field": update_field,
    "crm.merge": merge,
    "campaign.launch": lambda p: campaign(p, "launched"),
    "campaign.hold": lambda p: campaign(p, "held"),
    "email.send": send,
    "crm.flag_account": flag,
}


def execute(case, plan):
    fn = ACTIONS.get(plan.get("action"))
    if fn is None:
        return {"outcome": "escalate", "detail": f"{plan.get('action')!r} is not an action this executor carries out"}
    prm = plan.get("params") or {}
    if plan_hash(case["id"], plan["action"], prm) != plan.get("plan_hash"):
        return {"outcome": "escalate", "detail": "the plan's hash is not the approved one: nothing done"}
    outcome, detail, ref = fn(prm)
    out = {"outcome": outcome, "detail": detail}
    if ref:
        out["ref"] = ref
    return out


def check(chk):
    """verified or not_yet, read from the system after the action."""
    p, v = chk.get("params") or {}, chk.get("verify")
    ok = False
    if v == "crm_field_set":
        c = store.by_id(store.load("contacts")).get(p.get("contact"))
        ok = c is not None and str(c.get(p.get("field"))) == p.get("to")
    elif v == "crm_merged":
        by = store.by_id(store.load("contacts"))
        ok = p.get("keep") in by and p.get("drop") not in by
    elif v == "campaign_status":
        c = store.by_id(store.load("campaigns")).get(p.get("campaign"))
        want = {"campaign.launch": "launched", "campaign.hold": "held"}.get(chk.get("action"))
        ok = c is not None and want is not None and c.get("status") == want
    elif v == "email_in_outbox":
        ok = any(m["prospect"] == p.get("prospect") and m["subject"] == p.get("subject") for m in store.load("outbox", []))
    elif v == "account_flagged":
        a = store.by_id(store.load("accounts")).get(p.get("account"))
        ok = a is not None and a.get("risk") == p.get("risk")
    else:
        return {"outcome": "failed", "detail": f"no check named {v!r} here"}
    return {"outcome": "verified" if ok else "not_yet", "detail": f"{v}: {'there' if ok else 'not there yet'}"}


def main(argv):
    if argv[:1] == ["execute"]:
        case, plan = json.loads(os.environ["FLOTILLA_CASE"]), json.loads(os.environ["FLOTILLA_PLAN"])
        out = execute(case, plan)
    elif argv[:1] == ["check"]:
        out = check(json.loads(os.environ["FLOTILLA_CHECK"]))
    else:
        raise store.Error("usage: apply.py execute | check")
    store.report(out)
    print(f"BUSINESS {argv[0].upper()} outcome={out['outcome']} {out.get('detail', '')}", flush=True)
    return out


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (store.Error, OSError, KeyError, ValueError) as e:
        print(f"BUSINESS failed: {e}", file=sys.stderr)
        sys.exit(2)
