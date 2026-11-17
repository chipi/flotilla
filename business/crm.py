"""crm: sales-ops data hygiene on the CRM's contacts.

  crm.py scan   the monitor: a crm_fix detection for each contact with a field to normalize (email case
                and spaces, a country name instead of its ISO code) and each pair of contacts with the
                same email (a duplicate). Code only, read only.
  crm.py fix    the acting routine for crm_fix cases: re-checks the contact now and plans
                crm.update_field (reversible: it may earn auto) or crm.merge (cannot be undone: a person
                approves each), or closes the case when the contact changed meanwhile.

Gates in code, before anything is planned: only the fields on FIELDS; never a field a person edited
(a contact's `locked` list); never a merge of contacts whose emails differ once normalized, and the
older record is the one kept.
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import store  # noqa: E402

KIND = "crm_fix"
FIELDS = ("email", "country")
COUNTRIES = {"germany": "DE", "deutschland": "DE", "france": "FR", "united kingdom": "GB", "uk": "GB",
             "united states": "US", "usa": "US", "spain": "ES", "italy": "IT", "netherlands": "NL"}


def norm_email(e):
    return (e or "").strip().lower()


def wanted(c):
    """The normalized value of each field that differs, as {field: value}."""
    out = {}
    if c.get("email") and norm_email(c["email"]) != c["email"]:
        out["email"] = norm_email(c["email"])
    country = (c.get("country") or "").strip()
    if country and country.lower() in COUNTRIES:
        out["country"] = COUNTRIES[country.lower()]
    return {f: v for f, v in out.items() if f in FIELDS and f not in c.get("locked", [])}


def duplicates(contacts):
    """(kept, dropped) pairs: the same normalized email; the older record is kept."""
    seen, pairs = {}, []
    for c in sorted(contacts, key=lambda c: (c.get("created", ""), c["id"])):
        e = norm_email(c.get("email"))
        if not e:
            continue
        if e in seen:
            pairs.append((seen[e], c))
        else:
            seen[e] = c
    return pairs


def scan(contacts):
    out = []
    for c in contacts:
        for field, value in sorted(wanted(c).items()):
            out.append({"kind": KIND, "target": f"crm:contact/{c['id']}/{field}",
                        "title": f"Contact {c['id']}: {field} {c.get(field)!r} -> {value!r}",
                        "detail": f"{c['id']} ({c.get('name', '')}): {field} is {c.get(field)!r}, normalized {value!r}",
                        "data": {"op": "update", "contact": c["id"], "field": field, "from": c.get(field), "to": value}})
    for keep, drop in duplicates(contacts):
        out.append({"kind": KIND, "target": f"crm:contact/{drop['id']}/merge",
                    "title": f"Contacts {keep['id']} and {drop['id']} share {norm_email(keep['email'])}",
                    "detail": f"{drop['id']} ({drop.get('name', '')}) duplicates {keep['id']} ({keep.get('name', '')}), the older",
                    "data": {"op": "merge", "keep": keep["id"], "drop": drop["id"], "email": norm_email(keep["email"])}})
    return out


def decide(case, contacts):
    d = (case.get("data") or {})
    if case.get("kind") != KIND:
        return {"outcome": "escalate", "detail": "not a crm_fix case"}
    by = store.by_id(contacts)
    if d.get("op") == "update":
        c = by.get(d.get("contact"))
        if c is None:
            return {"outcome": "resolved_itself", "detail": f"contact {d.get('contact')} is gone"}
        if d.get("field") not in FIELDS:
            return {"outcome": "escalate", "detail": f"{d.get('field')!r} is not a field it may change"}
        if d["field"] in c.get("locked", []):
            return {"outcome": "escalate", "detail": f"a person edited {c['id']}'s {d['field']}: it is not changed by an agent"}
        now = wanted(c).get(d["field"])
        if now is None:
            return {"outcome": "resolved_itself", "detail": f"{c['id']}'s {d['field']} is already {c.get(d['field'])!r}"}
        return {"outcome": "plan", "action": "crm.update_field", "detail": f"{c['id']} {d['field']}: {c.get(d['field'])!r} -> {now!r}",
                "params": {"contact": c["id"], "field": d["field"], "from": str(c.get(d["field"])), "to": now}}
    if d.get("op") == "merge":
        keep, drop = by.get(d.get("keep")), by.get(d.get("drop"))
        if keep is None or drop is None:
            return {"outcome": "resolved_itself", "detail": "one of the two contacts is gone"}
        if norm_email(keep.get("email")) != norm_email(drop.get("email")) or not norm_email(keep.get("email")):
            return {"outcome": "escalate", "detail": f"{keep['id']} and {drop['id']} no longer share an email: not merged"}
        if (keep.get("created", ""), keep["id"]) > (drop.get("created", ""), drop["id"]):
            keep, drop = drop, keep
        return {"outcome": "plan", "action": "crm.merge", "detail": f"merge {drop['id']} into {keep['id']} (same email, {keep['id']} is older)",
                "params": {"keep": keep["id"], "drop": drop["id"]}}
    return {"outcome": "escalate", "detail": "a crm_fix case without an op it knows"}


def main(argv):
    contacts = store.load("contacts")
    if argv[:1] == ["scan"]:
        found = scan(contacts)
        store.report({"outcome": "detections", "detections": found})
        print(f"CRM SCAN contacts={len(contacts)} findings={len(found)}", flush=True)
        return found
    if argv[:1] == ["fix"]:
        case = json.loads(os.environ["FLOTILLA_CASE"])
        out = decide(case, contacts)
        store.report(out)
        print(f"CRM FIX case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}", flush=True)
        return out
    raise store.Error("usage: crm.py scan | fix")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (store.Error, OSError, KeyError, ValueError) as e:
        print(f"CRM failed: {e}", file=sys.stderr)
        sys.exit(2)
