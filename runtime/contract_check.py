#!/usr/bin/env python3
"""Check what a real endpoint does against what an alias's capabilities claim (RFC-0002 §11: verified
before an alias is marked native or object). Four small calls; prints what each did; never the key.

  python3 runtime/contract_check.py --base-url http://127.0.0.1:4001/v1 --model fleet-triage-flash \\
      --kind litellm --key-env OPENROUTER_API_KEY --cost reported
  python3 runtime/contract_check.py --base-url http://<x3>:11435/v1 --model qwen3:30b-a3b-instruct-2507-q4_K_M --cost zero

Checks: json_object gives parseable JSON, with usage, and cost as the alias's cost says (reported: a
number from the endpoint; zero: 0); native tools give a tool call (skipped with --tools none); max_tokens
is honoured; an unknown model is a typed error, not a transient one. Ends CONTRACT PASS / FAIL.
Verified 2026-11-09: the mini's LiteLLM (fleet-triage-flash) and the X3's Ollama (qwen3 30b) both PASS.
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import model_client as mc  # noqa: E402


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--model", required=True)
    ap.add_argument("--kind", default="openai", choices=mc.KINDS)
    ap.add_argument("--key-env", help="the variable holding the key (never printed)")
    ap.add_argument("--cost", default="unknown", choices=("reported", "zero", "unknown"))
    ap.add_argument("--tools", default="native", choices=("native", "none"))
    ap.add_argument("--timeout", type=int, default=120)
    a = ap.parse_args()
    ep = {"base_url": a.base_url, "kind": a.kind, "defaults": {"tools": a.tools, "json": "object", "cost": a.cost}}
    if a.key_env:
        ep["key_env"] = a.key_env
    cfg = {"endpoints": {"e": ep}, "aliases": {"m": {"endpoint": "e", "model": a.model},
                                               "m-plain": {"endpoint": "e", "model": a.model, "json": "none"},
                                               "bogus": {"endpoint": "e", "model": "no-such-model-contract-check"}}}
    c = mc.Client(mc.load(cfg), tracer=None)
    fails = []

    def show(name, r):
        u = r.get("usage") or {}
        print(f"{name}: model={r.get('model')} deployment={'set' if r.get('model_id') else None} "
              f"usage_reported={r.get('usage_reported')} tokens={u.get('prompt_tokens')}/{u.get('completion_tokens')} "
              f"cost={r.get('cost_usd')} ({r.get('cost_source')}) finish={r.get('finish_reason')} latency={r.get('latency_s')}s")

    r = c.chat("m", [{"role": "user", "content": 'Answer with JSON only: {"ok": true}'}], max_tokens=20, timeout=a.timeout)
    show("json_object", r)
    try:
        json.loads(r["content"])
    except ValueError:
        fails.append(f"json_object: content is not JSON: {r['content'][:80]!r}")
    if not r.get("usage_reported"):
        fails.append("json_object: no usage reported")
    if a.cost == "reported" and not r["cost_source"].startswith("reported"):
        fails.append(f"cost: reported, but the endpoint reported none ({r['cost_source']})")
    if a.cost == "zero" and r["cost_usd"] != 0.0:
        fails.append("cost: zero, but the cost is not 0")

    if a.tools == "native":
        tools = [{"type": "function", "function": {"name": "get_metric", "description": "Read one metric by name.",
                  "parameters": {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}}}]
        r = c.chat("m-plain", [{"role": "user", "content": "Use the get_metric tool to read the metric named pg_up."}],
                   tools=tools, max_tokens=80, timeout=a.timeout)
        show("tools", r)
        calls = r.get("tool_calls") or []
        if not calls or (calls[0].get("function") or {}).get("name") != "get_metric":
            fails.append(f"tools: native, but no get_metric call (content {str(r.get('content'))[:60]!r})")

    r = c.chat("m-plain", [{"role": "user", "content": "Count from 1 to 200, comma separated."}], max_tokens=5, timeout=a.timeout)
    show("max_tokens=5", r)
    if (r.get("usage") or {}).get("completion_tokens", 99) > 8:
        fails.append("max_tokens not honoured")

    try:
        c.chat("bogus", [{"role": "user", "content": "hi"}], max_tokens=1, timeout=a.timeout)
        fails.append("an unknown model was answered")
    except mc.Transient as e:
        fails.append(f"an unknown model is Transient (would be retried forever): {e}")
    except mc.ModelError as e:
        print(f"unknown model: {type(e).__name__} (HTTP {e.record.get('status')})")

    for f in fails:
        print("FAIL", f)
    print("CONTRACT PASS" if not fails else f"CONTRACT FAIL ({len(fails)})")
    return 0 if not fails else 1


if __name__ == "__main__":
    sys.exit(main())
