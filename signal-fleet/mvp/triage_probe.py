"""End-to-end probe of signal -> correlate -> triage (no action).

Uses the live shop-staleness signal if firing, else a synthetic one, pulls the
real correlated evidence, and runs the triager. Prints the structured disposition.
"""
import json

import correlate
import sources
import triage

SYNTHETIC = {
    "fingerprint": "grafana:synthetic-shop-stale",
    "source": "grafana",
    "alertname": "Shop launch data stale (no refresh in 7h)",
    "labels": {"alertname": "Shop launch data stale"},
    "summary": "synthetic — shop data refresh alarm; verify against evidence",
    "startsAt": None, "raw": {},
}


def main():
    sig = sources.staleness_alert()
    print("LIVE signal" if sig else "no live signal -> synthetic")
    sig = sig or SYNTHETIC
    ev = correlate.evidence_for_staleness(sig)
    print("evidence queries:", {k: (len(v) if isinstance(v, str) else 1)
                                 for k, v in ev["queries"].items()})
    disp = triage.triage(sig, ev)
    print("=== DISPOSITION ===")
    print(json.dumps(disp, indent=2))


if __name__ == "__main__":
    main()
