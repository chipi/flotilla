"""Compare the triage fleet's decisions in shadow (flotilla) with the live fleet's (fleetd), per signal.

  shadow_compare.py LIVE_LEDGER SHADOW_LEDGER --since 2026-11-08T18:45:00Z [--json] [--names]

By default it prints counts only: how many were decided by both, how many alike, the pairs of
dispositions that differ and how often, and the one-sided counts by source. No occurrence id, title
or reason. Every live decision since 2026-11-07 is on the held-out test side of the fleet's
evaluation (fleet-eval-data projects/triage-heldout/SPLIT.md), and reading one is recorded in its
exposed.tsv (rule 7). `--names` lists the occurrences and both reasons: for debugging a difference
only, and then recorded.

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
    ap.add_argument("--names", action="store_true",
                    help="list occurrences and reasons (held-out test side: record what you read)")
    a = ap.parse_args()
    since = a.since.replace("Z", "+00:00")
    live, shadow = rows(a.live, since), rows(a.shadow, since)
    both = sorted(set(live) & set(shadow))
    same = [k for k in both if live[k]["disposition"] == shadow[k]["disposition"]]
    differ = [k for k in both if live[k]["disposition"] != shadow[k]["disposition"]]
    live_only, shadow_only = sorted(set(live) - set(shadow)), sorted(set(shadow) - set(live))

    def by_source(keys, side):
        out = {}
        for k in keys:
            s = side[k].get("source") or "?"
            out[s] = out.get(s, 0) + 1
        return dict(sorted(out.items()))
    pairs = {}
    for k in differ:
        p = f"{live[k]['disposition']}/{shadow[k]['disposition']}"
        pairs[p] = pairs.get(p, 0) + 1
    res = {
        "since": a.since,
        "live_rows": len(live), "shadow_rows": len(shadow),
        "both": len(both), "same": len(same), "differ": len(differ),
        "differ_pairs": dict(sorted(pairs.items())),
        "live_only": len(live_only), "shadow_only": len(shadow_only),
        "live_only_by_source": by_source(live_only, live), "shadow_only_by_source": by_source(shadow_only, shadow),
    }
    if a.names:
        res["live_only_ids"], res["shadow_only_ids"] = live_only, shadow_only
        res["differences"] = [{"occurrence": k, "live": live[k]["disposition"], "shadow": shadow[k]["disposition"],
                               "live_reason": (live[k].get("reason") or "")[:200],
                               "shadow_reason": (shadow[k].get("reason") or "")[:200]} for k in differ]
    if a.json:
        json.dump(res, sys.stdout, indent=1)
        print()
        return
    print(f"since {a.since}: live {res['live_rows']} decisions, shadow {res['shadow_rows']}")
    print(f"both decided {res['both']}: same {res['same']}, differ {res['differ']}"
          + (f" (live/shadow: {', '.join(f'{p} {n}' for p, n in res['differ_pairs'].items())})" if pairs else ""))
    print(f"live only {res['live_only']} {res['live_only_by_source']}, "
          f"shadow only {res['shadow_only']} {res['shadow_only_by_source']}")
    if a.names:
        for d in res["differences"]:
            print(f"  DIFFER {d['occurrence']}: live {d['live']} / shadow {d['shadow']}")
            print(f"     live:   {d['live_reason']}")
            print(f"     shadow: {d['shadow_reason']}")
        print(f"live only: {', '.join(live_only)}")
        print(f"shadow only: {', '.join(shadow_only)}")


if __name__ == "__main__":
    main()
