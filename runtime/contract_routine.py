#!/usr/bin/env python3
"""platform/model-contract (G15): run contract_check.py's checks on every alias in FLOTILLA_MODELS (the
aliases its spec names, security T4), once a day and on demand. Each check is one line in the run's calls file (FLOTILLA_CALLS_FILE) with its
alias, check, ok and detail plus the call's tokens and cost, so flotillad keeps them and the Models page
shows the last checks. The checks' calls are not also logged as ordinary calls. Never prints a key.

  python3 runtime/contract_routine.py [--spend-file PATH] [--timeout 60]

Exit 0 when every alias passes, 1 when any check failed (the run is red, the page names the check).
"""
import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import contract_check as cc  # noqa: E402
import model_client as mc  # noqa: E402


def alias_config(cfg, name):
    """A models config holding one real alias three ways: as it is, without JSON mode, and a model that
    does not exist, all on its own endpoint."""
    a = dict(cfg["aliases"][name])
    ep_name = a["endpoint"]
    out = {"endpoints": {ep_name: cfg["endpoints"][ep_name]},
           "aliases": {"main": a, "plain": dict(a, json="none"),
                       "bogus": {"endpoint": ep_name, "model": "no-such-model-contract-check"}}}
    if name in (cfg.get("prices") or {}):
        out["prices"] = {"main": cfg["prices"][name], "plain": cfg["prices"][name]}
    return out


def line(name, r):
    rec = r["rec"] or {}
    u = rec.get("usage") or {}
    return {"at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "alias": name, "endpoint": "",
            "model": rec.get("model") or "", "deployment": rec.get("model_id"),
            "tokens_in": u.get("prompt_tokens"), "tokens_out": u.get("completion_tokens"),
            "cost_usd": rec.get("cost_usd"), "cost_source": rec.get("cost_source") or "",
            "latency_s": rec.get("latency_s"), "status": rec.get("status"), "error": None,
            "check": r["check"], "check_ok": r["ok"], "detail": r["detail"][:300]}


def main(argv=None, environ=None, transport=None):
    environ = dict(os.environ if environ is None else environ)
    ap = argparse.ArgumentParser()
    ap.add_argument("--spend-file", default="")
    ap.add_argument("--timeout", type=int, default=60)
    a = ap.parse_args(argv)
    raw = environ.get("FLOTILLA_MODELS")
    if not raw:
        print("FLOTILLA_MODELS is not set: this installation has no models to check")
        return 1
    cfg = json.loads(raw)
    calls_file = environ.pop("FLOTILLA_CALLS_FILE", "")
    spend, failed = 0.0, []
    for name in sorted(cfg.get("aliases") or {}):
        sub = alias_config(cfg, name)
        aliases = mc.load(sub)
        m = aliases["main"]
        c = mc.Client(aliases, environ=environ, transport=transport, tracer=None)
        results = cc.run_checks(c, tools=m.tools, cost=m.cost, json_mode=m.json_mode, timeout=a.timeout,
                                main="main", plain="plain", bogus="bogus")
        for r in results:
            spend += (r["rec"] or {}).get("cost_usd") or 0.0
            print(f"{name} {r['check']}: {'pass' if r['ok'] else 'FAIL'}{' (' + r['detail'] + ')' if r['detail'] else ''}")
            if not r["ok"]:
                failed.append(f"{name} {r['check']}")
            if calls_file:
                try:
                    with open(calls_file, "a") as f:
                        f.write(json.dumps(line(name, r)) + "\n")
                except OSError as e:
                    print(f"the calls file: {e}")
    if a.spend_file:
        with open(a.spend_file, "w") as f:
            f.write(f"{spend:.6f}\n")
    print("CONTRACT PASS" if not failed else f"CONTRACT FAIL: {', '.join(failed)}")
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())
