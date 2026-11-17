"""renewals: customer success, accounts at risk before they renew.

  renewals.py scan    the monitor: a renewal_risk detection for each account renewing within WINDOW_DAYS
                      whose risk score reaches THRESHOLD. The score is code, from the account's own
                      numbers; no model.
  renewals.py flag    the acting routine for renewal_risk cases: re-scores the account now and plans
                      crm.flag_account with the score's reasons as the note for its owner (reversible: it
                      may earn auto), or closes the case when the risk is gone. It never writes to the
                      customer: the owner decides what to say.
  renewals.py replay  the score over the past renewals (history.json): which of them it would have flagged,
                      and how many of the accounts that churned. A change to the score is replayed so
                      before it ships.

The score (0 to 100): active seats down 30 % or more against 90 days before, 40; an escalated support
ticket open, 30; an invoice overdue, 20; no login by an admin in 30 days, 10.
"""
import datetime
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import store  # noqa: E402

KIND = "renewal_risk"
WINDOW_DAYS = 60
THRESHOLD = 50


def score(a):
    """(score, [reasons])"""
    s, why = 0, []
    before, now = a.get("seats_90d_ago") or 0, a.get("seats_active") or 0
    if before and (before - now) / before >= 0.30:
        s += 40
        why.append(f"active seats {before} -> {now} in 90 days")
    if a.get("escalated_tickets", 0) > 0:
        s += 30
        why.append(f"{a['escalated_tickets']} escalated support ticket(s) open")
    if a.get("invoice_overdue"):
        s += 20
        why.append("an invoice is overdue")
    if a.get("days_since_admin_login", 0) > 30:
        s += 10
        why.append(f"no admin login in {a['days_since_admin_login']} days")
    return s, why


def renewal(a, today):
    """The renewal date: `renewal` (a date), or `renewal_in_days` from today (the example's seed data)."""
    if a.get("renewal"):
        return datetime.date.fromisoformat(a["renewal"])
    return today + datetime.timedelta(days=int(a["renewal_in_days"]))


def due(a, today):
    d = (renewal(a, today) - today).days
    return 0 <= d <= WINDOW_DAYS


def scan(accounts, today):
    out = []
    for a in accounts:
        if not due(a, today):
            continue
        s, why = score(a)
        if s >= THRESHOLD:
            out.append({"kind": KIND, "target": f"account:{a['id']}", "title": f"{a['name']} renews {renewal(a, today)}: risk {s}",
                        "detail": "; ".join(why),
                        "data": {"account": a["id"], "score": s, "reasons": why,
                                 "measure": {"name": "renewal_risk_score", "value": s, "threshold": THRESHOLD}}})
    return out


def decide(case, accounts, today):
    if case.get("kind") != KIND:
        return {"outcome": "escalate", "detail": "not a renewal_risk case"}
    a = store.by_id(accounts).get((case.get("data") or {}).get("account"))
    if a is None:
        return {"outcome": "resolved_itself", "detail": "the account is gone"}
    if not due(a, today):
        return {"outcome": "resolved_itself", "detail": f"{a['id']} renews {renewal(a, today)}, outside the window"}
    s, why = score(a)
    if s < THRESHOLD:
        return {"outcome": "resolved_itself", "detail": f"{a['id']}'s risk is {s} now, below {THRESHOLD}"}
    level = "high" if s >= 70 else "medium"
    note = f"Renewal {renewal(a, today)}, risk {s}: " + "; ".join(why) + ". Scored by code; decide the outreach yourself."
    return {"outcome": "plan", "action": "crm.flag_account", "detail": f"{a['id']}: risk {s} ({level})",
            "params": {"account": a["id"], "risk": level, "note": note[:1000]}}


def replay(history):
    """What the score would have said on each past renewal, 60 days before it, against what happened."""
    flagged = [h for h in history if score(h)[0] >= THRESHOLD]
    churned = [h for h in history if h["churned"]]
    caught = [h for h in flagged if h["churned"]]
    return {"renewals": len(history), "churned": len(churned), "flagged": len(flagged), "caught": len(caught),
            "false_alarms": len(flagged) - len(caught), "missed": [h["id"] for h in churned if h not in flagged]}


def main(argv, today=None):
    today = today or datetime.date.today()
    if argv[:1] == ["replay"]:
        r = replay(store.load("history"))
        print("RENEWALS REPLAY " + " ".join(f"{k}={v}" for k, v in r.items()), flush=True)
        return r
    accounts = store.load("accounts")
    if argv[:1] == ["scan"]:
        found = scan(accounts, today)
        store.report({"outcome": "detections", "detections": found})
        print(f"RENEWALS SCAN accounts={len(accounts)} at_risk={len(found)}", flush=True)
        return found
    if argv[:1] == ["flag"]:
        case = json.loads(os.environ["FLOTILLA_CASE"])
        out = decide(case, accounts, today)
        store.report(out)
        print(f"RENEWALS FLAG case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}", flush=True)
        return out
    raise store.Error("usage: renewals.py scan | flag | replay")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (store.Error, OSError, KeyError, ValueError) as e:
        print(f"RENEWALS failed: {e}", file=sys.stderr)
        sys.exit(2)
