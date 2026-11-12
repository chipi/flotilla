"""Replay for RFC-0002 §9 step 4: triage's probe loop on the shared harness (runtime/harness.py).

  python3 gate_harness_replay.py CAPTURE_DIR [CAPTURE_DIR ...] [--baseline FILE]

The old code is triage.py at BASE_REV (or --baseline FILE, a copy beside triage.py, where there is no
git); the new is the working tree. post_json, the probes and the edge gate's lookup are served from the
records; nothing is sent anywhere.

1. Exact replay: for every capture with `exact` turns, the recorded replies are fed back turn by turn,
   the probe results come from the capture's probe table and the edge lookups from `exact.lookups`.
   Every request the new loop sends equals the recorded one byte for byte, and the old and new runs
   end alike: decision, pre-gate decision, gates string, probe trace, turn records (latency and the new
   `quirk` field aside). The new run's disposition and gates also equal the capture's.
2. Quirks, old against new on scripted replies: empty replies, bad JSON, a decision that is not flat
   (before and after the turns run out, the final call answering, failing or empty), probes past the
   budget, an unknown probe, transport failures, 401/402/403, and a TriagerDown inside the final call.
   Same outcome, the same requests, the same deferral text twice (the fleet session's adapter compares
   two deferrals).
3. Seams: `_gate` wrapped the way the fleet session's adapter wraps it sees the model's own decision
   before a gate changes it, and a turn that hit a quirk carries its name.

Exit 0 when everything matches, 1 on a difference, 2 when no capture had recorded turns. Captures hold
production data: run it on the host that has them; it prints counts, never content.
"""
import argparse
import contextlib
import copy
import glob
import http.client
import importlib.util
import io
import json
import os
import socket
import subprocess
import sys
import tempfile
import urllib.error

os.environ.setdefault("OPENROUTER_API_KEY", "replay-dummy")
os.environ.setdefault("SF_OBSERV_DISABLED", "1")
for k in ("SF_NO_CLASS_HINT", "FLOTILLA_SKILLS", "FLOTILLA_HARNESS", "FLOTILLA_MODELS"):
    os.environ.pop(k, None)

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
BASE_REV = "ed7c0a0"   # flotilla main before step 4
GATEWAY = "http://litellm:4000/v1/chat/completions"

import config  # noqa: E402
import guards  # noqa: E402
import observ  # noqa: E402

# a run's Langfuse trace id goes into each request's metadata: fixed here, or no two runs could match
observ.new_trace_id = lambda: "0" * 32


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


def _http(code):
    return urllib.error.HTTPError(GATEWAY, code, "replayed", {}, io.BytesIO(b'{"error": "replayed"}'))


def drive(mod, signal, answers, table=None, lookups=None, max_probes=None):
    """Run mod.investigate with post_json answering from `answers` in order (a dict reply, an exception,
    or None for an empty reply). Returns (outcome, sent requests, comparable result)."""
    queue, sent = list(answers), []

    def post(url, body, headers=None, timeout=60):
        sent.append(json.dumps(body))
        a = queue.pop(0) if queue else None
        if callable(a):
            a = a()   # a fresh exception each run: an HTTPError's body can be read only once
        if isinstance(a, BaseException):
            raise a
        if a is None:
            return {"choices": [{"message": {"content": ""}}], "usage": {}}
        return copy.deepcopy(a)
    mod.post_json = post
    looks = list(lookups or [])

    def restarts_near(instance, at, data=None):
        lk = looks.pop(0) if looks else {"result": []}
        if "error" in lk:
            raise RuntimeError(lk["error"])
        return lk.get("result") or []
    real_rn, guards.restarts_near = guards.restarts_near, restarts_near
    config.CAPTURE_PROBES = True
    try:
        # discarded: the probe loop prints each probe's result, and captures may be the held-out side
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            d = mod.investigate(dict(signal), max_probes=max_probes, probe_table=table)
        res = {k: d.get(k) for k in ("disposition", "reason", "question", "file", "dismissal_evidence",
                                     "marker", "certainty", "_pre_gate", "_probe_trace")}
        res["gates"] = (d.get("_meta") or {}).get("gates")
        res["usage"] = (d.get("_meta") or {}).get("usage")
        res["turns"] = [{k: v for k, v in t.items() if k not in ("latency_s", "quirk")}
                        for t in (d.get("_exact") or {}).get("turns", [])]
        res["lookups"] = (d.get("_exact") or {}).get("lookups")
        out = ("decided", res)
    except Exception as e:  # noqa: BLE001
        out = (type(e).__name__, str(e))
    finally:
        guards.restarts_near = real_rn
        config.CAPTURE_PROBES = False
    return out, sent


def reply(content, usage=None):
    return {"choices": [{"message": {"content": content}}], "usage": usage or {"prompt_tokens": 1, "completion_tokens": 1},
            "model": "m"}


def exact(old, new, dirs):
    n, bad = 0, []
    for d in dirs:
        for path in sorted(glob.glob(os.path.join(d, "*.json"))):
            try:
                cap = json.load(open(path))
            except (OSError, ValueError):
                continue
            ex = cap.get("exact") or {}
            turns = ex.get("turns") or []
            if not turns or not isinstance(cap.get("signal"), dict):
                continue
            answers = []
            for t in turns:
                if "reply" in t:
                    answers.append(reply(t.get("reply"), t.get("usage")) if t.get("reply") else None)
                elif t.get("status"):
                    answers.append(lambda s=int(t["status"]): _http(s))
                else:
                    answers.append(lambda: socket.timeout("timed out"))
            first = json.loads(turns[0]["request"])
            config.TRIAGE_MODEL = first.get("model") or config.TRIAGE_MODEL
            config.OPENROUTER_URL = GATEWAY if first.get("metadata") else "https://openrouter.ai/api/v1/chat/completions"
            tid = (first.get("metadata") or {}).get("existing_trace_id") or "0" * 32
            observ.new_trace_id = lambda tid=tid: tid   # the run's own trace id, as recorded in its requests
            n += 1
            name = f"capture #{n}"   # an index, never the file name: it names a held-out item
            o, o_sent = drive(old, cap["signal"], answers, cap.get("probes") or {}, ex.get("lookups"))
            w, w_sent = drive(new, cap["signal"], answers, cap.get("probes") or {}, ex.get("lookups"))
            if w_sent != [t["request"] for t in turns]:
                bad.append(f"{name}: the new loop's requests differ from the recorded ones")
            if (o, o_sent) != (w, w_sent):
                bad.append(f"{name}: the new run ends differently from the old one")
            fd = cap.get("fleet_decision") or {}
            if w[0] == "decided" and (w[1]["disposition"], w[1]["gates"]) != (fd.get("disposition"), fd.get("gates")):
                bad.append(f"{name}: disposition or gates differ from the capture's")
    return n, bad


SIG = {"source": "grafana", "alertname": "Something odd", "summary": "plain", "labels": {"a": "b"}}
TABLE = {"service_logs:{}": "logs: all quiet", "occurrence_history:{}": {"count": 1, "level": "warning"}}
P = '{"action":"probe","probe":"service_logs","probe_args":{}}'
DISMISS = ('{"action":"decide","disposition":"dismiss","reason":"benign","certainty":"high",'
           '"dismissal_evidence":"service_logs: all quiet","immediate_recommendation":null}')
NOFLAT = '{"action":"decide","decision":{"disposition":"dismiss"}}'
SCRIPTS = {
    "decide at once": [reply(DISMISS)],
    "probe then decide": [reply(P), reply(DISMISS)],
    "empty then decide": [None, reply(DISMISS)],
    "bad json then decide": [reply("not json at all"), reply(DISMISS)],
    "three probes then decide": [reply(P), reply(P), reply(P), reply(DISMISS)],
    "probe past the budget": [reply(P), reply(P), reply(P), reply(P)],
    "not flat early, then flat": [reply(NOFLAT), reply(DISMISS)],
    "not flat last, final call answers": [reply(P), reply(P), reply(P), reply(NOFLAT), reply(DISMISS)],
    "not flat last, final call fails": [reply(P), reply(P), reply(P), reply(NOFLAT), lambda: socket.timeout("timed out")],
    "not flat last, final 401 swallowed": [reply(P), reply(P), reply(P), reply(NOFLAT), lambda: _http(401)],
    "unknown probe": [reply('{"action":"probe","probe":"nope","probe_args":{}}'), reply(DISMISS)],
    "all bad json": [reply("x")] * 4,
    "empty on the last turn": [reply(P), reply(P), reply(P), None],
    "all empty": [None] * 4,
    "timeout": [lambda: socket.timeout("timed out")],
    "dropped connection": [lambda: http.client.RemoteDisconnected("Remote end closed connection without response")],
    "HTTP 500": [lambda: _http(500)], "HTTP 429": [lambda: _http(429)],
    "HTTP 401": [lambda: _http(401)], "HTTP 402": [lambda: _http(402)], "HTTP 403": [lambda: _http(403)],
    "dismiss without evidence (a gate downgrades)": [reply('{"action":"decide","disposition":"dismiss","reason":"r"}')],
}


def quirks(old, new):
    config.TRIAGE_MODEL, config.OPENROUTER_URL = "fleet-triage-pro", GATEWAY
    bad = []
    with open(os.path.join(HERE, "..", "..", "fleets", "triage", "harness-default.json")) as f:
        spec_json = json.dumps(json.load(f))   # what flotillad hands the run in FLOTILLA_HARNESS
    for env in ({}, {"FLOTILLA_HARNESS": spec_json}):
        os.environ.update(env)
        try:
            for name, answers in SCRIPTS.items():
                for mp in (None, 1):
                    o, o_sent = drive(old, SIG, answers, TABLE, max_probes=mp)
                    w, w_sent = drive(new, SIG, answers, TABLE, max_probes=mp)
                    w2, _ = drive(new, SIG, answers, TABLE, max_probes=mp)
                    tag = f"{name} (max_probes {mp}{', FLOTILLA_HARNESS' if env else ''})"
                    if (o, o_sent) != (w, w_sent):
                        bad.append(f"{tag}: old {o[0]}, new {w[0]}; requests {'same' if o_sent == w_sent else 'differ'}")
                    if w != w2:
                        bad.append(f"{tag}: two identical runs differ")
        finally:
            os.environ.pop("FLOTILLA_HARNESS", None)
    return len(SCRIPTS) * 4, bad


def seams(new):
    bad = []
    config.TRIAGE_MODEL, config.OPENROUTER_URL = "fleet-triage-pro", GATEWAY
    seen, real = [], new._gate

    def wrapped(d, signal, trace, edge_data=None):   # as the fleet session's adapter wraps it
        seen.append(d.get("disposition"))
        return real(d, signal, trace, edge_data)
    new._gate = wrapped
    try:
        out, _ = drive(new, SIG, SCRIPTS["dismiss without evidence (a gate downgrades)"], TABLE)
    finally:
        new._gate = real
    if seen != ["dismiss"] or out[1]["disposition"] != "escalate":
        bad.append(f"a wrapped _gate saw {seen}, the run ended {out[1].get('disposition')}")
    config.CAPTURE_PROBES = True
    new.post_json = lambda *a, **k: {"choices": [{"message": {"content": ""}}], "usage": {}}
    try:
        try:
            new.investigate(dict(SIG), probe_table=TABLE)
        except Exception:  # noqa: BLE001 — all empty: deferred, the marks are what is checked
            pass
        marks = [t.get("quirk") for t in new._EXACT["turns"]]
    finally:
        config.CAPTURE_PROBES = False
    if marks != ["empty_retry"] * 4:
        bad.append(f"quirk marks on four empty turns: {marks}")
    return bad


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--baseline")
    a = ap.parse_args()
    old = baseline(a.baseline)
    new = _load(os.path.join(HERE, "triage.py"), "triage_new")
    n, bad = exact(old, new, a.dirs)
    print(f"exact replay: {n} captures, {len(bad)} differences")
    cases, qbad = quirks(old, new)
    print(f"quirks: {cases} scripted runs, old against new, {len(qbad)} differences")
    sbad = seams(new)
    print(f"seams: {'ok' if not sbad else 'FAIL'}")
    for b in (bad + qbad + sbad)[:25]:
        print("  DIFF", b)
    if n == 0:
        print("HARNESS REPLAY: NO DATA (no capture with recorded turns)")
        return 2
    ok = not (bad or qbad or sbad)
    print(f"HARNESS REPLAY: {'PASS' if ok else 'FAIL'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
