"""campaigns: a marketing campaign checked before it goes out.

  campaigns.py scan    the monitor: a campaign_ready detection for each campaign marked ready.
  campaigns.py decide  the acting routine for campaign_ready cases: runs the launch checks and plans
                       campaign.launch when every one passes (sending cannot be undone: a person
                       approves each launch), or campaign.hold with what failed (reversible: it may
                       earn auto), or closes the case when the campaign is no longer ready.

The checks are code and they decide; nothing here asks a model whether a campaign is fine:
  links      every link is on the approved landing pages list (landing_pages.json)
  utm        every link carries utm_source, utm_medium and utm_campaign, lowercase words, and
             utm_campaign is the campaign's own slug
  merge      no unfilled merge field ({{...}}) in the subject or the body
  copy       the subject and the body are the approved copy (copy.json) for the campaign, unchanged
  footer     the body has the unsubscribe link and the sender's postal address
"""
import json
import os
import re
import sys
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import store  # noqa: E402

KIND = "campaign_ready"
UTM = re.compile(r"^[a-z0-9]+(?:[-_][a-z0-9]+)*$")
LINK = re.compile(r"https?://[^\s\"'<>)]+")
MERGE = re.compile(r"\{\{[^}]*\}\}")


def checks(c, landing, copy, footer):
    """The failed checks, as [(check, what)]; empty when the campaign may launch."""
    bad = []
    text = c.get("subject", "") + "\n" + c.get("body", "")
    links = LINK.findall(c.get("body", ""))
    for u in links:
        p = urllib.parse.urlsplit(u)
        page = urllib.parse.urlunsplit((p.scheme, p.netloc, p.path, "", ""))
        if p.path.rstrip("/").endswith("unsubscribe"):
            continue
        if page not in landing:
            bad.append(("links", f"{page} is not an approved landing page"))
        q = dict(urllib.parse.parse_qsl(p.query))
        for k in ("utm_source", "utm_medium", "utm_campaign"):
            if not UTM.match(q.get(k, "")):
                bad.append(("utm", f"{page}: {k} is {q.get(k)!r}"))
        if q.get("utm_campaign") and q["utm_campaign"] != c.get("slug"):
            bad.append(("utm", f"{page}: utm_campaign {q['utm_campaign']!r} is not the campaign's {c.get('slug')!r}"))
    for m in MERGE.findall(text):
        bad.append(("merge", f"unfilled merge field {m}"))
    want = copy.get(c["id"])
    if not want:
        bad.append(("copy", "no approved copy for this campaign"))
    else:
        for part in ("subject", "body"):
            if c.get(part, "") != want.get(part, ""):
                bad.append(("copy", f"the {part} is not the approved copy"))
    body = c.get("body", "")
    if footer["unsubscribe"] not in body:
        bad.append(("footer", "no unsubscribe link"))
    if footer["address"] not in body:
        bad.append(("footer", "no postal address"))
    return bad


def scan(campaigns):
    return [{"kind": KIND, "target": f"campaign:{c['id']}", "title": f"Campaign {c['id']} ({c.get('name', '')}) is ready",
             "detail": f"{c['id']} marked ready for {c.get('audience', '?')}", "data": {"campaign": c["id"]}}
            for c in campaigns if c.get("status") == "ready"]


def decide(case, campaigns, landing, copy, footer):
    if case.get("kind") != KIND:
        return {"outcome": "escalate", "detail": "not a campaign_ready case"}
    c = store.by_id(campaigns).get((case.get("data") or {}).get("campaign"))
    if c is None or c.get("status") != "ready":
        return {"outcome": "resolved_itself", "detail": "the campaign is no longer ready (launched, held or gone)"}
    bad = checks(c, set(landing), copy, footer)
    if not bad:
        return {"outcome": "plan", "action": "campaign.launch", "detail": f"{c['id']}: every launch check passes",
                "params": {"campaign": c["id"]}}
    why = "; ".join(f"{k}: {w}" for k, w in bad)
    return {"outcome": "plan", "action": "campaign.hold", "detail": f"{c['id']}: {len(bad)} check(s) failed",
            "params": {"campaign": c["id"], "reason": why[:2000]}}


def main(argv):
    campaigns = store.load("campaigns")
    if argv[:1] == ["scan"]:
        found = scan(campaigns)
        store.report({"outcome": "detections", "detections": found})
        print(f"CAMPAIGNS SCAN campaigns={len(campaigns)} ready={len(found)}", flush=True)
        return found
    if argv[:1] == ["decide"]:
        case = json.loads(os.environ["FLOTILLA_CASE"])
        out = decide(case, campaigns, store.load("landing_pages"), store.load("copy"), store.load("sender"))
        store.report(out)
        print(f"CAMPAIGNS DECIDE case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}", flush=True)
        return out
    raise store.Error("usage: campaigns.py scan | decide")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (store.Error, OSError, KeyError, ValueError) as e:
        print(f"CAMPAIGNS failed: {e}", file=sys.stderr)
        sys.exit(2)
