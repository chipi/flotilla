#!/usr/bin/env python3
"""Check what a real endpoint does against what an alias's capabilities claim (RFC-0002 §11: verified
before an alias is marked native or object). Four small calls; prints what each did; never the key.

  python3 runtime/contract_check.py --base-url http://127.0.0.1:4001/v1 --model fleet-triage-flash \\
      --kind litellm --key-env OPENROUTER_API_KEY --cost reported
  python3 runtime/contract_check.py --base-url http://<worker-host>:11435/v1 --model qwen3:30b-a3b-instruct-2507-q4_K_M --cost zero

Checks: json_object gives parseable JSON, with usage, and cost as the alias's cost says (reported: a
number from the endpoint; zero: 0); native tools give a tool call (skipped with --tools none); max_tokens
is honoured; an unknown model is a typed error, not a transient one. Ends CONTRACT PASS / FAIL.
Verified 2026-11-09: the host's LiteLLM (fleet-triage-flash) and the second machine's Ollama (qwen3 30b) both PASS.
The same checks run daily on every configured alias as platform/model-contract (contract_routine.py).
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import model_client as mc  # noqa: E402

TOOL = [{"type": "function", "function": {"name": "get_metric", "description": "Read one metric by name.",
         "parameters": {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}}}]


def run_checks(c, tools="native", cost="unknown", timeout=120, main="m", plain="m-plain", bogus="bogus", json_mode="object"):
    """The four checks on a client whose aliases main (as configured), plain (json none) and bogus (a model
    that does not exist) share one endpoint. An alias without JSON mode is not held to JSON output.
    Returns [{check, ok, detail, rec}], rec being the call's record (None when the call raised)."""
    out = []

    def add(check, fails, rec, note=""):
        out.append({"check": check, "ok": not fails, "detail": "; ".join(fails) or note, "rec": rec})

    try:
        # room for a reasoning model to think first (the host's flash and pro: 20 tokens were all reasoning)
        r = c.chat(main, [{"role": "user", "content": 'Answer with JSON only: {"ok": true}'}], max_tokens=256, timeout=timeout)
        fails = []
        if json_mode == "object":
            try:
                json.loads(r["content"])
            except (ValueError, TypeError):
                fails.append(f"content is not JSON: {str(r.get('content'))[:80]!r}")
        if not r.get("usage_reported"):
            fails.append("no usage reported")
        if cost == "reported" and not str(r.get("cost_source", "")).startswith("reported"):
            fails.append(f"cost: reported, but the endpoint reported none ({r.get('cost_source')})")
        if cost == "zero" and r.get("cost_usd") != 0.0:
            fails.append("cost: zero, but the cost is not 0")
        add("json_object", fails, r)
    except mc.ModelError as e:
        add("json_object", [f"{type(e).__name__}: {e}"], e.record)

    if tools == "native":
        try:
            r = c.chat(plain, [{"role": "user", "content": "Use the get_metric tool to read the metric named pg_up."}],
                       tools=TOOL, max_tokens=80, timeout=timeout)
            calls = r.get("tool_calls") or []
            ok = calls and (calls[0].get("function") or {}).get("name") == "get_metric"
            add("tools", [] if ok else [f"native, but no get_metric call (content {str(r.get('content'))[:60]!r})"], r)
        except mc.ModelError as e:
            add("tools", [f"{type(e).__name__}: {e}"], e.record)

    try:
        r = c.chat(plain, [{"role": "user", "content": "Count from 1 to 200, comma separated."}], max_tokens=5, timeout=timeout)
        add("max_tokens", [] if (r.get("usage") or {}).get("completion_tokens", 99) <= 8 else ["max_tokens not honoured"], r)
    except mc.EmptyCompletion as e:
        # a reasoning model spends 5 tokens thinking and returns no content: the limit held all the same
        used = ((e.record or {}).get("usage") or {}).get("completion_tokens")
        if used is not None and used <= 8:
            add("max_tokens", [], e.record, f"honoured: {used} tokens, all reasoning, no content")
        else:
            add("max_tokens", [f"EmptyCompletion with {used} completion tokens: {e}"], e.record)
    except mc.ModelError as e:
        add("max_tokens", [f"{type(e).__name__}: {e}"], e.record)

    try:
        r = c.chat(bogus, [{"role": "user", "content": "hi"}], max_tokens=1, timeout=timeout)
        add("unknown_model", ["an unknown model was answered"], r)
    except mc.Transient as e:
        add("unknown_model", [f"an unknown model is Transient (would be retried forever): {e}"], e.record)
    except mc.ModelError as e:
        add("unknown_model", [], None, f"{type(e).__name__} (HTTP {e.record.get('status')})")
    return out


def show(name, r):
    if r is None:
        return
    u = r.get("usage") or {}
    print(f"{name}: model={r.get('model')} deployment={'set' if r.get('model_id') else None} "
          f"usage_reported={r.get('usage_reported')} tokens={u.get('prompt_tokens')}/{u.get('completion_tokens')} "
          f"cost={r.get('cost_usd')} ({r.get('cost_source')}) finish={r.get('finish_reason')} latency={r.get('latency_s')}s")


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
    results = run_checks(c, tools=a.tools, cost=a.cost, timeout=a.timeout)
    for r in results:
        show(r["check"], r["rec"])
        if r["ok"] and r["detail"]:
            print(f"{r['check']}: {r['detail']}")
    fails = [r for r in results if not r["ok"]]
    for r in fails:
        print("FAIL", f"{r['check']}: {r['detail']}")
    print("CONTRACT PASS" if not fails else f"CONTRACT FAIL ({len(fails)})")
    return 0 if not fails else 1


if __name__ == "__main__":
    sys.exit(main())
