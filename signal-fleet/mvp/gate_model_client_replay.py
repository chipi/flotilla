"""Replay for RFC-0002 §9 step 2: triage._call on the model client, against _call as it was before.

  python3 gate_model_client_replay.py CAPTURE_DIR [CAPTURE_DIR ...] [--baseline FILE]

Two parts, each the old code (triage.py at BASE_REV, or --baseline FILE where there is no git, as in a
worker container) against the working tree, with post_json faked (nothing is sent anywhere):

1. History: every model turn recorded in the captures' `exact` (results/probes/*.json, since 2026-11-09).
   The recorded request gives the messages, trace id and step; the recorded reply is answered back. Both
   codes must send the recorded request byte for byte, return the same value, and record the same turn
   (latency aside).
2. Failures: 401, 402, 403, 404, 429, 500 and 503 answers, a timeout, a dropped connection, an empty
   reply, a reply without `choices`, a reply that is not an object. Through investigate(): the same
   outcome (TriagerDown, TriageDeferred, or the exception raised), TriageDeferred's text keeps the
   "triager call failed" prefix, and two identical failures give the same text (the fleet session's
   eval adapter retries once and compares them).

Exit 0 when everything matches, 1 on any difference, 2 when no recorded turn was found (an empty corpus
proves nothing). Captures hold production data: run it on the host that has them; it prints counts,
never content.
"""
import argparse
import copy
import glob
import http.client
import importlib.util
import json
import os
import socket
import subprocess
import sys
import tempfile
import urllib.error

os.environ.setdefault("OPENROUTER_API_KEY", "replay-dummy")
os.environ.setdefault("SF_OBSERV_DISABLED", "1")
# every run on flotilla gets the installation's models section; the mini's names its aliases triage-pro
# and triage-flash (models fleet-triage-pro, -flash). It must change nothing about triage's call.
os.environ["FLOTILLA_MODELS"] = json.dumps({
    "endpoints": {"gateway": {"base_url": "http://litellm:4000/v1", "kind": "litellm", "key_env": "OPENROUTER_API_KEY",
                              "defaults": {"tools": "native", "json": "object", "cost": "reported"}}},
    "aliases": {"triage-flash": {"endpoint": "gateway", "model": "fleet-triage-flash"},
                "triage-pro": {"endpoint": "gateway", "model": "fleet-triage-pro"}}})

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
BASE_REV = "092a73f"   # flotilla main before step 2
GATEWAY = "http://litellm:4000/v1/chat/completions"

import config  # noqa: E402


def _load(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def baseline(path=None):
    if path:
        return _load(path, "triage_baseline")
    src = subprocess.run(["git", "-C", HERE, "show", f"{BASE_REV}:signal-fleet/mvp/triage.py"],
                         capture_output=True, text=True, check=True).stdout
    tmp = tempfile.NamedTemporaryFile("w", suffix=".py", delete=False, dir=HERE, prefix="_triage_base_")
    tmp.write(src)
    tmp.close()
    try:
        return _load(tmp.name, "triage_baseline")
    finally:
        os.unlink(tmp.name)


def run_call(mod, messages, trace_id, step, answer):
    """mod._call with post_json faked: answer is a dict (the parsed reply) or an exception to raise."""
    sent = []

    def post(url, body, headers=None, timeout=60):
        sent.append({"url": url, "data": json.dumps(body), "headers": headers, "timeout": timeout})
        if isinstance(answer, BaseException):
            raise answer
        return copy.deepcopy(answer)
    mod.post_json = post
    mod.config.CAPTURE_PROBES = True
    mod._exact_start()
    try:
        out = ("return", mod._call(messages, trace_id=trace_id, step=step))
    except Exception as e:  # noqa: BLE001
        out = ("raise", type(e).__name__)
    turns = [{k: v for k, v in t.items() if k != "latency_s"} for t in (mod._EXACT or {}).get("turns", [])]
    mod.config.CAPTURE_PROBES = False
    return out, sent, turns


def history(old, new, dirs):
    n, bad = 0, []
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
                meta = body.get("metadata") or {}
                trace_id = meta.get("existing_trace_id")
                config.TRIAGE_MODEL = body["model"]
                config.OPENROUTER_URL = GATEWAY if meta else "https://openrouter.ai/api/v1/chat/completions"
                answer = {"choices": [{"message": {"content": t["reply"]}}], "usage": t.get("usage"),
                          "model": t.get("model")}
                n += 1
                o_out, o_sent, o_turns = run_call(old, body["messages"], trace_id, t.get("step"), answer)
                n_out, n_sent, n_turns = run_call(new, body["messages"], trace_id, t.get("step"), answer)
                name = f"turn #{n} (step {t.get('step')})"   # an index, never the file name: it names a held-out item
                if not n_sent:
                    bad.append(f"{name}: the new code sent nothing ({n_out[1]})")
                elif n_sent[0]["data"] != t["request"]:
                    bad.append(f"{name}: request differs from the recorded one")
                if (o_sent, o_out) != (n_sent, n_out):
                    bad.append(f"{name}: wire or return differs from the old code")
                if o_turns != n_turns:
                    bad.append(f"{name}: exact turn record differs from the old code")
    return n, bad


def wire(old, new):
    """Every endpoint kind with and without a trace id: OpenRouter direct takes no metadata even with one,
    a case the recorded history (all through the gateway) does not have."""
    msgs = [{"role": "system", "content": "playbook ü"}, {"role": "user", "content": "SIGNAL:\n{}"}]
    answer = {"choices": [{"message": {"content": "{}"}}], "usage": {"prompt_tokens": 1}, "model": "m"}
    bad = []
    for url in (GATEWAY, "https://openrouter.ai/api/v1/chat/completions"):
        for trace_id in (None, "t1"):
            for step in (None, 0, 3):
                config.TRIAGE_MODEL, config.OPENROUTER_URL = "fleet-triage-pro", url
                if run_call(old, msgs, trace_id, step, answer) != run_call(new, msgs, trace_id, step, answer):
                    bad.append(f"wire {url} trace {trace_id} step {step}: differs from the old code")
    return bad


def _http(code, body=b'{"error": "replayed"}'):
    import io
    return urllib.error.HTTPError(GATEWAY, code, "replayed", {}, io.BytesIO(body))


FAILURES = {
    "HTTP 401": lambda: _http(401), "HTTP 402": lambda: _http(402), "HTTP 403": lambda: _http(403),
    "HTTP 404": lambda: _http(404), "HTTP 429": lambda: _http(429), "HTTP 500": lambda: _http(500),
    "HTTP 503 no body": lambda: urllib.error.HTTPError(GATEWAY, 503, "replayed", {}, None),
    "timeout": lambda: socket.timeout("timed out"),
    "conn-drop": lambda: http.client.RemoteDisconnected("Remote end closed connection without response"),
    "empty reply": lambda: {"choices": [{"message": {"content": ""}}], "usage": {}},
    "no choices": lambda: {"usage": {}},
    "not an object": lambda: ["x"],
}
SIGNAL = {"fingerprint": "grafana:replay", "occurrence_id": "replay", "source": "grafana",
          "alertname": "replayed signal", "labels": {}, "annotations": {}}


def outcome(mod, make):
    def post(*a, **k):
        v = make()
        if isinstance(v, BaseException):
            raise v
        return v
    mod.post_json = post
    try:
        d = mod.triage(dict(SIGNAL))
    except mod.TriagerDown:
        return "TriagerDown", ""
    except Exception as e:  # noqa: BLE001
        return type(e).__name__, str(e)
    return "decided:" + str(d.get("disposition")), ""


def failures(old, new):
    config.TRIAGE_MODEL, config.OPENROUTER_URL = "fleet-triage-pro", GATEWAY
    rows, bad = [], []
    for name, make in FAILURES.items():
        o, n, n2 = outcome(old, make), outcome(new, make), outcome(new, make)
        rows.append((name, o[0], n[0]))
        if o[0] != n[0]:
            bad.append(f"{name}: old {o[0]}, new {n[0]}")
        pre = "triager call failed"
        if o[1].startswith(pre) and not n[1].startswith(pre):
            bad.append(f"{name}: the text lost its \"{pre}\" prefix")
        if not o[1].startswith(pre) and n[1] != o[1]:
            bad.append(f"{name}: the text changed (only the part after \"{pre}\" may)")
        if n[1] != n2[1]:
            bad.append(f"{name}: two identical failures gave different texts")
    return rows, bad


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--baseline")
    a = ap.parse_args()
    old = baseline(a.baseline)
    new = _load(os.path.join(HERE, "triage.py"), "triage_new")
    n, bad = history(old, new, a.dirs)
    print(f"history: {n} recorded model turns replayed, {len(bad)} differences")
    wbad = wire(old, new)
    print(f"wire: 12 endpoint, trace and step combinations, {len(wbad)} differences")
    bad += wbad
    rows, fbad = failures(old, new)
    print("failures:  " + "  ".join(f"{name}: {o}/{nw}" for name, o, nw in rows))
    for b in (bad + fbad)[:20]:
        print("  DIFF", b)
    if n == 0:
        print("MODEL CLIENT REPLAY: NO DATA (no recorded turn found)")
        return 2
    ok = not bad and not fbad
    print(f"MODEL CLIENT REPLAY: {'PASS' if ok else 'FAIL'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
