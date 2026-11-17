"""outbound: a first touch to each new prospect, drafted only from what is known about them.

  outbound.py scan   the monitor: a prospect_new detection for each prospect with no first touch yet.
  outbound.py draft  the acting routine for prospect_new cases: drafts the email and plans email.send
                     (an email cannot be unsent: a person approves every send), or escalates with the
                     gate that refused it, or closes the case when the prospect was contacted meanwhile.

The draft is the template's sentences plus personal sentences, and every personal sentence is built
from one fact in the enrichment data and cites its id; a sentence with no fact behind it is never
written. Gates in code, before a plan: not on the suppression list or marked do-not-contact; an EU
contact needs a recorded lawful basis; at most DAILY_CAP sends a day; the footer has the
unsubscribe link and the sender's address.
"""
import datetime
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import store  # noqa: E402

KIND = "prospect_new"
DAILY_CAP = 20
EU = {"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR", "DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU",
      "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE"}
# the personal sentence each kind of fact may become; a fact of another kind is not used
SENTENCES = {
    "hiring": "I saw {company} is hiring for {value}.",
    "launch": "Congratulations on launching {value}.",
    "stack": "I noticed {company} runs {value}.",
}


def draft(p, facts, sender):
    """(subject, body, cited fact ids). Only the prospect's own facts, each sentence citing one."""
    mine = [f for f in facts if f.get("prospect") == p["id"] and f.get("kind") in SENTENCES and f.get("value")]
    lines = [f"Hi {p['first_name']},", ""]
    cited = []
    for f in mine[:2]:
        lines.append(SENTENCES[f["kind"]].format(company=p["company"], value=f["value"]) + f"  [fact {f['id']}]")
        cited.append(f["id"])
    lines += ["", sender["pitch"], "", sender["signature"], "", f"Unsubscribe: {sender['unsubscribe']}", sender["address"]]
    return f"{p['company']} and {sender['company']}", "\n".join(lines), cited


def gates(p, suppression, outbox, sender, body, today):
    if p.get("do_not_contact") or p["email"].lower() in {s.lower() for s in suppression}:
        return "do-not-contact: on the suppression list or marked"
    if p.get("country") in EU and not p.get("lawful_basis"):
        return f"an EU contact ({p.get('country')}) with no recorded lawful basis"
    if sum(1 for m in outbox if m.get("sent", "")[:10] == today) >= DAILY_CAP:
        return f"the day's cap of {DAILY_CAP} sends is reached"
    if sender["unsubscribe"] not in body or sender["address"] not in body:
        return "the footer lacks the unsubscribe link or the address"
    return ""


def scan(prospects, outbox):
    touched = {m["prospect"] for m in outbox}
    return [{"kind": KIND, "target": f"prospect:{p['id']}", "title": f"New prospect {p['first_name']} at {p['company']}",
             "detail": f"{p['id']}: no first touch yet", "data": {"prospect": p["id"]}}
            for p in prospects if p["id"] not in touched]


def decide(case, prospects, facts, suppression, outbox, sender, today=None):
    if case.get("kind") != KIND:
        return {"outcome": "escalate", "detail": "not a prospect_new case"}
    pid = (case.get("data") or {}).get("prospect")
    p = store.by_id(prospects).get(pid)
    if p is None:
        return {"outcome": "resolved_itself", "detail": f"prospect {pid} is gone"}
    if any(m["prospect"] == pid for m in outbox):
        return {"outcome": "resolved_itself", "detail": f"{pid} was contacted meanwhile"}
    today = today or datetime.date.today().isoformat()
    subject, body, cited = draft(p, facts, sender)
    why = gates(p, suppression, outbox, sender, body, today)
    if why:
        return {"outcome": "escalate", "detail": f"{pid}: not sent: {why}"}
    return {"outcome": "plan", "action": "email.send",
            "detail": f"{pid}: first touch, {len(cited)} personal sentence(s), each citing its fact",
            "params": {"prospect": pid, "to": p["email"], "subject": subject, "body": body, "facts": ",".join(cited) or "none"}}


def main(argv):
    prospects, outbox = store.load("prospects"), store.load("outbox", [])
    if argv[:1] == ["scan"]:
        found = scan(prospects, outbox)
        store.report({"outcome": "detections", "detections": found})
        print(f"OUTBOUND SCAN prospects={len(prospects)} new={len(found)}", flush=True)
        return found
    if argv[:1] == ["draft"]:
        case = json.loads(os.environ["FLOTILLA_CASE"])
        out = decide(case, prospects, store.load("enrichment"), store.load("suppression"), outbox, store.load("sender"))
        store.report(out)
        print(f"OUTBOUND DRAFT case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}", flush=True)
        return out
    raise store.Error("usage: outbound.py scan | draft")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (store.Error, OSError, KeyError, ValueError) as e:
        print(f"OUTBOUND failed: {e}", file=sys.stderr)
        sys.exit(2)
