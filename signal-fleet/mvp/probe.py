"""Live probe for the trigger + correlation halves of the first slice.

Proves: (1) the fleet can poll Grafana for firing alerts, (2) the correlation
queries return real shop evidence. If the shop-staleness alert isn't firing,
a synthetic signal drives the correlation test so the query surface is still
exercised.
"""
import json

import correlate
import sources


def _preview(val, n=220):
    s = val if isinstance(val, str) else json.dumps(val)
    return f"{len(s)} chars :: " + s[:n].replace("\n", " ")


def main():
    print("== firing alerts (Grafana poll) ==")
    try:
        fa = sources.firing_alerts()
        print("active_count:", len(fa))
        for a in fa[:10]:
            print("  -", a.get("labels", {}).get("alertname"))
    except Exception as e:
        print("  grafana poll ERROR:", e)

    sig = sources.staleness_alert()
    if sig is None:
        print("== no live shop-staleness signal -> synthetic (correlation test) ==")
        sig = {
            "fingerprint": "grafana:synthetic-shop-stale",
            "source": "grafana",
            "alertname": "Shop launch data stale (no refresh in 7h)",
            "labels": {"alertname": "Shop launch data stale"},
            "summary": "synthetic — shop data refresh has not run in 7h",
            "startsAt": None, "raw": {},
        }
    else:
        print("== LIVE shop-staleness signal ==")
    print("signal:", sig["alertname"], "| fp:", sig["fingerprint"])

    print("== correlated evidence bundle ==")
    ev = correlate.evidence_for_staleness(sig)
    for name, val in ev["queries"].items():
        print(f"  [{name}] {_preview(val)}")


if __name__ == "__main__":
    main()
