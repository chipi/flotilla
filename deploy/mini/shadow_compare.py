"""Compare the triage fleet's decisions in shadow (flotilla) with the live fleet's (fleetd), per signal.

  shadow_compare.py LIVE_LEDGER SHADOW_LEDGER --since 2026-11-08T18:45:00Z [--json]

Both ledgers are the fleet's dispositions.tsv. Only rows written since --since count (the shadow was
seeded from a copy of the live ledger, so older rows are the same rows). A signal is its occurrence
id (fingerprint@first-seen): the same occurrence, decided by both, is the comparison.

Reports:
  both       occurrences decided by both, and how many got the same disposition
  differ     the occurrences whose disposition differs (each listed with both reasons)
  live only  decided by fleetd but not the shadow (the shadow missed it, or was paused)
  shadow only

The model is not deterministic, so a difference is not a failure by itself: each one is read and
explained (phase-2-plan 2.7). Exit 0 always; the numbers are the result.
"""
import argparse
import csv
import json
import sys


def rows(path, since):
    out = {}
    with open(path, newline="") as f:
        for r in csv.DictReader(f, delimiter="\t"):
            if (r.get("ts") or "") < since:
                continue
            if r.get("disposition") == "recurrence":
                continue          # a count change on an occurrence already decided, not a decision
            key = r.get("occurrence_id") or r.get("fingerprint")
            out[key] = r          # the newest row per occurrence wins (file order)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("live")
    ap.add_argument("shadow")
    ap.add_argument("--since", required=True, help="ISO time; rows before it are the shared seed")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()
    since = a.since.replace("Z", "+00:00")
    live, shadow = rows(a.live, since), rows(a.shadow, since)
    both = sorted(set(live) & set(shadow))
    same = [k for k in both if live[k]["disposition"] == shadow[k]["disposition"]]
    differ = [k for k in both if live[k]["disposition"] != shadow[k]["disposition"]]
    res = {
        "since": a.since,
        "live_rows": len(live), "shadow_rows": len(shadow),
        "both": len(both), "same": len(same), "differ": len(differ),
        "live_only": sorted(set(live) - set(shadow)), "shadow_only": sorted(set(shadow) - set(live)),
        "differences": [{"occurrence": k, "live": live[k]["disposition"], "shadow": shadow[k]["disposition"],
                         "live_reason": (live[k].get("reason") or "")[:200],
                         "shadow_reason": (shadow[k].get("reason") or "")[:200]} for k in differ],
    }
    if a.json:
        json.dump(res, sys.stdout, indent=1)
        print()
        return
    print(f"since {a.since}: live {res['live_rows']} decisions, shadow {res['shadow_rows']}")
    print(f"both decided {res['both']}: same {res['same']}, differ {res['differ']}")
    for d in res["differences"]:
        print(f"  DIFFER {d['occurrence']}: live {d['live']} / shadow {d['shadow']}")
        print(f"     live:   {d['live_reason']}")
        print(f"     shadow: {d['shadow_reason']}")
    print(f"live only {len(res['live_only'])}: {', '.join(res['live_only'][:10])}")
    print(f"shadow only {len(res['shadow_only'])}: {', '.join(res['shadow_only'][:10])}")


if __name__ == "__main__":
    main()
