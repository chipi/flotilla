"""Replay for v2 step 5: triage on the decide station (orchestrator_station.py), against triage.py as it was before.

  flotillad -stations fleets/triage/cycle/v1/ -models MODELS > contract.json
  python3 gate_stations_replay.py CAPTURE_DIR [CAPTURE_DIR ...] --contract contract.json [--baseline FILE]

The installation runs this on its private captures before it moves its own triage to stations (step 6). Two parts, each the old code
(triage.py at BASE_REV, or --baseline FILE where there is no git, as in a worker container) against the same code with the contract's
stations installed, with post_json faked (nothing is sent anywhere):

1. History: every model turn recorded in the captures' `exact` (results/probes/*.json) whose model is the one the station's alias resolves
   to. The recorded request gives the messages, trace id and step; the recorded reply is answered back. Both codes must send the recorded
   request byte for byte, return the same value and record the same turn (latency aside). Turns of any other model are counted, not
   compared: the station names one model.
2. The same wire and failure checks as gate_model_client_replay.py (401, 402, 403, 404, 429, 500 and 503 answers, a timeout, a dropped
   connection, an empty reply, a reply without `choices`, a reply that is not an object): the same outcome, TriagerDown, TriageDeferred or
   the exception raised, and the "triager call failed" prefix kept.

Exit 0 when everything matches, 1 on any difference, 2 when no recorded turn was compared (an empty corpus proves nothing). Captures hold
production data: run it on the host that has them; it prints counts, never content. What it proves is the request bytes and the failure
handling turn by turn; the model's decisions on a real corpus are the installation's own replay of the decisions (gate_harness_replay.py).
"""
import argparse
import glob
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import gate_model_client_replay as gmc  # noqa: E402  (its environment defaults, its history, wire and failure runners)

config = gmc.config
gmc.BASE_REV = "d931379"   # flotilla main before the stations of step 5: triage.py and orchestrator.py as the flat routine runs them


def load_contract(path):
    with open(path) as f:
        env = json.load(f)["env"]
    for k in ("FLOTILLA_STATIONS", "FLOTILLA_MODELS"):
        if not env.get(k):
            raise SystemExit(f"gate_stations_replay: {path} has no {k}: it is the output of flotillad -stations for a version that declares stations, with -models")
    return env


def history(old, new, dirs, model):
    n, skipped, bad = 0, 0, []
    for d in dirs:
        for path in sorted(glob.glob(os.path.join(d, "*.json"))):
            try:
                cap = json.load(open(path))
            except (OSError, ValueError):
                continue
            for t in ((cap.get("exact") or {}).get("turns") or []):
                if not t.get("request") or "reply" not in t:
                    continue
                body = json.loads(t["request"])
                if body.get("model") != model:
                    skipped += 1
                    continue
                meta = body.get("metadata") or {}
                trace_id = meta.get("existing_trace_id")
                config.TRIAGE_MODEL = model
                answer = {"choices": [{"message": {"content": t["reply"]}}], "usage": t.get("usage"), "model": t.get("model")}
                n += 1
                o_out, o_sent, o_turns = gmc.run_call(old, body["messages"], trace_id, t.get("step"), answer)
                n_out, n_sent, n_turns = gmc.run_call(new, body["messages"], trace_id, t.get("step"), answer)
                name = f"turn #{n} (step {t.get('step')})"   # an index, never the file name: it names a held-out item
                if not n_sent:
                    bad.append(f"{name}: the station path sent nothing ({n_out[1]})")
                elif n_sent[0]["data"] != t["request"]:
                    bad.append(f"{name}: request differs from the recorded one")
                if [(s["url"], s["data"], s["timeout"]) for s in o_sent] != [(s["url"], s["data"], s["timeout"]) for s in n_sent] or o_out != n_out:
                    bad.append(f"{name}: wire or return differs from the old code")
                if o_turns != n_turns:
                    bad.append(f"{name}: exact turn record differs from the old code")
    return n, skipped, bad


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--contract", required=True)
    ap.add_argument("--baseline")
    a = ap.parse_args()
    env = load_contract(a.contract)
    os.environ.update({k: v for k, v in env.items() if k.startswith("FLOTILLA_") and k not in ("FLOTILLA_SHADOW",)})
    for ep in (json.loads(env["FLOTILLA_MODELS"]).get("endpoints") or {}).values():
        if ep.get("key_env"):
            os.environ.setdefault(ep["key_env"], "replay-dummy")
    old = gmc.baseline(a.baseline)
    import orchestrator_station
    import triage
    orchestrator_station.install()
    new = triage
    model = config.TRIAGE_MODEL   # what install() set: the model the station's alias resolves to
    n, skipped, bad = history(old, new, a.dirs, model)
    print(f"history: {n} recorded model turns on {model} replayed, {skipped} on another model not compared, {len(bad)} differences")
    rows, fbad = gmc.failures(old, new)
    print("failures:  " + "  ".join(f"{name}: {o}/{nw}" for name, o, nw in rows))
    for b in (bad + fbad)[:20]:
        print("  DIFF", b)
    if n == 0:
        print("STATIONS REPLAY: NO DATA (no recorded turn of the station's model found)")
        return 2
    ok = not bad and not fbad
    print(f"STATIONS REPLAY: {'PASS' if ok else 'FAIL'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
