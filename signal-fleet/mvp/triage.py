"""Investigation-driven triager (SIGNALS §7.3, minimal round-6 version).

A bounded menu-driven probe loop (hard cap N): each turn the model either requests
a typed probe (probes.py) OR decides ONE of four terminal dispositions —
dismiss / cleanup / file / escalate(with a question). Deterministic + replayable.

Deterministic gates (no LLM):
- intent gate on File (§4.1, UNCHANGED — investigation earns what/where/how-often,
  never intent; certainty is not a source),
- cleanup gate (a machine-checkable test/noise marker must be present),
- dismiss gate (must have run a corroborating probe).
Certainty is metadata only, never a gate.
"""
import copy
import datetime
import hashlib
import json
import os
import re
import sys
import time
import urllib.error

import config
import guards
import http_util
import observ
import probes
from http_util import post_json

# The model client (RFC-0002 §9 step 2) lives at flotilla's runtime/model_client.py: ../../runtime from
# here, /opt/flotilla/runtime in the worker image. An extract of signal-fleet/ must carry it beside.
try:
    import harness
    import model_client
    import skills
except ImportError:
    sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "runtime"))
    try:
        import harness
        import model_client
        import skills
    except ImportError as e:
        raise ImportError("triage needs flotilla's runtime/ (harness.py, model_client.py, skills.py) beside "
                          "signal-fleet/ (../../runtime from signal-fleet/mvp)") from e

# The playbook and the class hints (RFC-0002 §9 step 3) live in fleets/triage/, beside signal-fleet/:
# FLOTILLA_CODE_ROOT in the worker image, ../../ from here in a checkout or an extract.
_CODE_ROOT = os.environ.get("FLOTILLA_CODE_ROOT") or os.path.normpath(
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
_FLEET = "triage"
_FLEET_DIR = os.path.join(_CODE_ROOT, "fleets", _FLEET)

# With capture on (SF_CAPTURE_PROBES=1), what an exact replay needs is recorded per decision
# (ADR-0004 / RFC-0002 §9 step 1): every model call's request body as sent, its raw reply, usage
# and the gateway's deployment id; every gate lookup with its inputs and result; the run's time.
# investigate() starts a record, _call and _edge_gate add to it, _finish puts it on the decision
# as `_exact`, and capture.record writes it as the capture's `exact` field. Off: None, nothing kept.
_EXACT = None


def _exact_start():
    global _EXACT
    _EXACT = ({"now": datetime.datetime.now(datetime.timezone.utc).isoformat(),
               "turns": [], "lookups": [], "skills": []} if config.CAPTURE_PROBES else None)


def _exact_add(kind, item):
    if _EXACT is not None:
        try:
            _EXACT[kind].append(json.loads(json.dumps(item, default=str)))
        except Exception:  # noqa: BLE001 — recording must never break triage
            pass


class TriagerDown(Exception):
    """The triager LLM gateway is PERSISTENTLY unavailable (auth 401/403) — not a
    one-off transport flake. Propagated out of triage() so the orchestrator fails
    CLOSED (one aggregate issue), instead of escalating every signal individually.
    This is the 2026-08-13→19 flood's core fix (a wiped litellm key 401'd every
    call and the fleet fail-open escalated ~90 signals)."""

class TriageDeferred(Exception):
    """A one-off transport failure (timeout, dropped connection, 429, 5xx) calling the
    triager. Not a disposition: the orchestrator records nothing, so the same signal is
    simply triaged again next cycle (#11, 2026-10-01). Before, each such failure became
    a per-signal "[escalation] … triager call failed — retry?" issue for a REAL signal
    that never got triaged (#73 on 09-29, #80 on 09-30)."""


ALLOWED_INTENT = {"reporter", "spec", "repo-data", "code-invariant", "baseline",
                  "operator-rule", "slo"}
MAX_PROBES = int(config.env("SF_MAX_PROBES", "3"))
# occurrence_history stays a corroborator but is content-checked, not just usability-
# checked (review R7): it is self-derived from the same issue, so it is ALWAYS usable
# — only a BENIGN-looking history (low count / low level / resolved) may corroborate.
CORROB_PROBES = {"service_logs", "metric", "trace", "source_state", "occurrence_history"}
INDEP_PROBES = {"service_logs", "metric", "trace", "source_state"}
# \btest\b misses camelCase hooks (crashOrreryTest) — the known crash-test hook
# is listed explicitly; do NOT loosen to bare `test` (matches "latest").
# staging/\bdev\b/\bverify\b: non-prod environments and verification runs are
# noise-at-source in the prod tracker (PODCAST-7: env=dev from a laptop run).
CLEANUP_MARKERS = re.compile(
    r"delete me|safe to delete|\btest\b|validation|probe|smoke|e2e|ladder-verify|"
    r"wiring|placeholder|dashless|crashorrerytest|staging|\bdev\b|\bverify\b", re.I)

# Operational states that are NOT code bugs: a guardrail firing (its own incident,
# working as designed), a billing state, or a DOWNSTREAM effect of one. These are
# classified deterministically and acknowledged-not-ticketed — the guardrail/alert
# already fired; the fleet must not turn each occurrence into a bug ticket. 72 of
# the 2026-08-13→19 flood were "cost soft cap". Downstream (provider-not-initialized
# / fallback-failed) is folded to the same class here (the correlation half of #6:
# they share the budget/cap root). See docs/signal-fleet-guide.md (fail closed; operational markers).
OPERATIONAL_MARKERS = [
    (re.compile(r"cost soft cap|cost cap exceeded|costcapexceeded|soft[- ]?cap exceeded", re.I),
     "cost-cap"),
    (re.compile(r"no budget|no credit|insufficient (credit|budget|fund)|\b402\b|"
                r"payment required|out of credit|quota exceeded", re.I),
     "provider-budget"),
    (re.compile(r"openaiprovider not initialized|provider not initialized|"
                r"fallback tier failed|all fallbacks failed|fallback failed", re.I),
     "provider-fallback"),
]

# An upstream provider's SHARED pool refused (OpenRouter 429, limit_source=
# upstream_provider_shared_pool): not our key, not our quota, not our code, and LiteLLM
# already retried (num_retries). Operator call 2026-10-01 (homelab #17): not actionable,
# never ticketed. A 429 against OUR key/quota carries a different limit_source and still
# reaches the triager. GlitchTip cuts the title before limit_source, so this marker also
# reads the full exception value (raw.metadata.value) — only this marker does, so the
# markers above keep their exact pre-existing reach.
UPSTREAM_LIMIT_MARKER = re.compile(
    r"limit_source\W+upstream_provider_shared_pool|temporarily rate-limited upstream", re.I)

_OPERATIONAL_REASON = {
    "upstream-rate-limit": ("operational state (upstream-rate-limit) — the provider's shared "
                            "pool refused upstream of us; nothing in our code or config to fix "
                            "(homelab #17). Acknowledged, not ticketed."),
    "assistant-failover": ("operational state (assistant-failover) — LiteLLM's call to the X3 "
                           "assistant failed (stopped, hung or unreachable) and the `assistant` "
                           "alias failed over to OpenRouter, so the caller got its answer "
                           "(RFC-0008). An outage of the X3 is the x3-assistant-down alert's job. "
                           "Acknowledged, not ticketed."),
}


def operational_class(signal):
    """Return the operational class (cost-cap/provider-budget/provider-fallback/
    upstream-rate-limit) if the signal is an operational state, else None.
    Deterministic, no LLM."""
    hay = " ".join(str(signal.get(k, "")) for k in ("alertname", "summary")) + \
        " " + json.dumps(signal.get("labels", {}))
    for rx, cls in OPERATIONAL_MARKERS:
        if rx.search(hay):
            return cls
    meta = ((signal.get("raw") or {}).get("metadata") or {}).get("value", "")
    if UPSTREAM_LIMIT_MARKER.search(hay + " " + str(meta)):
        return "upstream-rate-limit"
    # The latest event names the X3 assistant and nothing else (sources.attach_backend): a call
    # the `assistant` alias failed over. Any other backend, or none found, reaches the triager.
    if (signal.get("labels") or {}).get("project") == "litellm" and \
            (signal.get("raw") or {}).get("_backends") == [config.ASSISTANT_BACKEND]:
        return "assistant-failover"
    return None


# #3/#2 — secondary classes that BIAS routing (work_type) but NEVER gate the
# disposition. The LLM still owns file-vs-escalate; the class is a per-signal HINT
# that steers `bug` → `config-enhancement` for provider/environment/recoverable
# conditions and templates the acceptance criteria. PRECEDENCE (review R5):
# operational_class runs FIRST (dismiss, no LLM); signal_class only classifies what
# reaches investigation, so a "fallback failed" (operational → dismiss) never reaches
# the recoverable hint. Markers are SPECIFIC/server-side so they can't fire on the
# existing client-side fixtures (a bare "timeout" is NOT enough — a named upstream
# is required), which keeps the eval's existing behavior unchanged.
SIGNAL_CLASS_MARKERS = [
    (re.compile(r"(deepgram|openai|anthropic|gemini|whisper|elevenlabs|assemblyai|"
                r"\bs3\b|\bgcs\b)\b.{0,40}?(timed out|timeout|temporarily unavailable|"
                r"connection reset|\b50[234]\b)", re.I), "external-transient"),
    (re.compile(r"\b(write|read) operation timed out\b", re.I), "external-transient"),
    (re.compile(r"data_inspection_failed|content[_ ]?policy|moderation_blocked|"
                r"invalid_request_error.{0,40}(safety|policy)", re.I), "external"),
    (re.compile(r"permission denied|read-only file system|no space left|cargo_home|"
                r"/root/\.cargo|lanceerror\(io\)|errno 1[38]|errno 28|disk quota exceeded",
                re.I), "environment"),
    (re.compile(r"recoverablesummarizationerror|recoverable\w*error|repaired \d+/\d+|"
                r"\bdegraded\b|recovered gracefully|extractive fallback|reroll succeeded",
                re.I), "recoverable"),
]

# The class hints are skills (fleets/triage/skills/hint-<class>/SKILL.md, ADR-0012), loaded by the
# routine's rules (spec skills.rule, FLOTILLA_SKILLS; default-rules.json without flotillad). Each is
# defeasible: it ends by handing the decision back, so the class stays a BIAS, not a gate (the
# anchor-override fixture verifies the LLM can overrule it).
_DEFAULT_RULES = os.path.join(_FLEET_DIR, "skills", "default-rules.json")


def signal_class(signal):
    """Deterministic secondary class or None. A HINT to the triager (a skill, see _signal_intro),
    NEVER a disposition gate. Runs conceptually AFTER operational_class."""
    hay = " ".join(str(signal.get(k, "")) for k in ("alertname", "summary")) + \
        " " + json.dumps(signal.get("labels", {}))
    for rx, cls in SIGNAL_CLASS_MARKERS:
        if rx.search(hay):
            return cls
    return None

def _read_playbook():
    path = os.path.join(_FLEET_DIR, "playbooks", "triage.md")
    try:
        with open(path, encoding="utf-8") as f:
            text = f.read()
    except OSError as e:
        raise RuntimeError(f"triage's playbook is missing: {path} ({e.strerror})") from e
    return text[:-1] if text.endswith("\n") else text   # the file ends in one newline; the prompt does not


# The playbook (fleets/triage/playbooks/triage.md): the routine's own instructions, with two
# placeholders, the probe count and the probe menu.
_SYSTEM = _read_playbook()


def _sys():
    return _SYSTEM % (MAX_PROBES, probes.menu())


PROMPT_SHA = hashlib.sha1(_sys().encode()).hexdigest()[:8]


def _signal_intro(signal):
    intro = "SIGNAL:\n" + json.dumps(
        {k: signal.get(k) for k in ("source", "alertname", "labels", "summary")}, indent=2)
    cls = signal_class(signal)   # #3/#2 — per-signal HINT, keeps PROMPT_SHA stable
    # SF_NO_CLASS_HINT suppresses the hint — used ONLY for the eval A/B to reproduce
    # pre-#3/#2 behavior on the identical fixtures. Never set in production.
    if cls and not os.environ.get("SF_NO_CLASS_HINT"):
        for name in skills.match(skills.rules_from_env(_DEFAULT_RULES), {"class": cls}):
            s = skills.load(name, _FLEET, _CODE_ROOT)
            _exact_add("skills", {k: s[k] for k in ("name", "layer", "version", "sha256")})
            intro += "\n\n" + s["text"]
    return intro


def _trim(v, n=1800):
    s = v if isinstance(v, str) else json.dumps(v)
    return s if len(s) <= n else s[:n] + f"\n…[truncated {len(s)-n} of {len(s)} chars]"


_ENDPOINT_PATH = "/chat/completions"


def _client():
    """The model client and the alias for this call, always built from triage's own settings
    (SF_OPENROUTER_URL, OPENROUTER_API_KEY, SF_TRIAGE_MODEL), so the request is the same bytes as before.
    FLOTILLA_MODELS (flotillad's models section) is not read here: its aliases are named differently
    (the mini's triage-pro is model fleet-triage-pro), and choosing triage's model from it is a later
    behaviour change, not this move."""
    url = config.OPENROUTER_URL
    if not url.endswith(_ENDPOINT_PATH):
        raise model_client.ConfigError(f"SF_OPENROUTER_URL {url} does not end in {_ENDPOINT_PATH}")
    cfg = {"endpoints": {"triager": {"base_url": url[:-len(_ENDPOINT_PATH)], "key_env": "OPENROUTER_API_KEY",
                                     "kind": "litellm" if config.via_gateway() else "openai",
                                     "defaults": {"json": "object", "tools": "json", "cost": "reported"}}},
           "aliases": {config.TRIAGE_MODEL: {"endpoint": "triager", "model": config.TRIAGE_MODEL}}}
    return (model_client.Client(model_client.load(cfg), transport=_post_json_transport,
                                environ={"OPENROUTER_API_KEY": config.OPENROUTER_KEY}),
            config.TRIAGE_MODEL)


_LAST_RESPONSE = None   # the dict post_json returned for the call in progress: _call's return comes from it


def _post_json_transport(url, data, headers, timeout):
    """The client's transport over this module's post_json, looked up at call time, so a patched
    triage.post_json (the fleet session's eval adapter, gate_triager_failure_replay) still sees the request
    as a dict, the timeout as a keyword, and has its returned dict used as the response."""
    global _LAST_RESPONSE
    _LAST_RESPONSE = None
    http_util.LAST_HEADERS = {}
    try:
        auth = {"Authorization": headers["Authorization"]} if "Authorization" in headers else {}
        resp = post_json(url, json.loads(data), headers=auth, timeout=timeout)
    except urllib.error.HTTPError as e:
        try:
            text = e.read().decode("utf-8", "replace")
        except Exception:  # noqa: BLE001 — a replayed HTTPError has no body
            text = str(e.reason or "")
        return e.code, {k.lower(): v for k, v in (e.headers or {}).items()}, text
    _LAST_RESPONSE = resp
    return 200, dict(http_util.LAST_HEADERS), json.dumps(resp)


def _call(messages, trace_id=None, step=None):
    if not config.OPENROUTER_KEY:
        raise SystemExit("[triage] OPENROUTER_API_KEY not set")
    try:
        client, alias = _client()
    except model_client.ConfigError as e:
        raise TriagerDown(f"triager model config: {e}") from e   # persistent: fail closed
    # The gateway's Langfuse logger adds its generation (model, usage, cost) to this trace;
    # `existing_trace_id` leaves the trace's name/input/output to the fleet.
    kw = {"trace_id": trace_id, "generation_name": f"triage-turn-{(step or 0) + 1}"} if trace_id else {}
    try:
        rec = client.chat(alias, messages, timeout=90, **kw)
    except model_client.EmptyCompletion as e:
        rec = e.record   # a 200 without content: triage retries the turn, as before
    except model_client.ModelError as e:
        r = e.record
        _exact_add("turns", {"step": step, "request": r.get("request"), "status": r.get("status"),
                             "error": r.get("error"), "latency_s": r.get("latency_s")})
        if isinstance(e, model_client.AuthOrBilling):
            # 401/403 = the gateway rejected our key (wiped litellm virtual key is the
            # known cause) — PERSISTENT, every call will fail. Fail closed, don't escalate.
            if r.get("status") in (401, 403):
                raise TriagerDown(f"HTTP {r['status']} auth failure to the triager gateway "
                                  f"({config.OPENROUTER_URL}) — litellm virtual key rejected "
                                  f"(recreate: infra/litellm/README.md)") from e
            # 402 = out of credit — as persistent as a rejected key, and the same per-signal
            # flood without this (11 escalation issues on 2026-08-13). Fail closed too.
            raise TriagerDown(f"HTTP 402 Payment Required from the triager gateway "
                              f"({config.OPENROUTER_URL}) — provider credit exhausted") from e
        raise   # investigate() turns it into TriageDeferred("triager call failed: …")
    resp = _LAST_RESPONSE
    _exact_add("turns", {"step": step, "request": rec.get("request"),
                         "reply": ((resp.get("choices") or [{}])[0].get("message") or {}).get("content"),
                         "usage": resp.get("usage"), "model": resp.get("model"),
                         "model_id": rec.get("model_id"), "latency_s": rec.get("latency_s")})
    return resp["choices"][0]["message"]["content"], resp.get("usage", {})


def _parse(raw):
    m = re.search(r"\{.*\}", raw, re.S)
    if not m:
        raise ValueError("no JSON object")
    return json.loads(m.group(0))


def _usable(v):
    if isinstance(v, str):
        v = v.strip()
        return bool(v) and not v.startswith("<")
    if isinstance(v, dict):
        data = v.get("data")
        if isinstance(data, dict):
            return bool(data.get("result"))
        return bool(v)
    return bool(v)


# ---- deterministic gates ----
def _intent_gate(d):
    if d.get("disposition") != "file":
        return d, "n/a"
    f = d.get("file") or {}
    acc = f.get("acceptance") or []
    uncited = sum(1 for c in acc if not (isinstance(c, dict) and c.get("intent_source") in ALLOWED_INTENT))
    if uncited or not acc:
        d["disposition"] = "escalate"
        d["reason"] = f"intent gate: {uncited or 'no'} uncited acceptance — " + d.get("reason", "")
        d["question"] = "What is the intended (citable) behavior that 'fixed' means?"
        d["file"] = None
        return d, "downgraded"
    return d, "passed"


def _cleanup_gate(d, signal):
    if d.get("disposition") != "cleanup":
        return d, "n/a"
    text = " ".join(str(signal.get(k, "")) for k in ("alertname", "summary")) + " " + json.dumps(signal.get("labels", {}))
    if CLEANUP_MARKERS.search(text) or signal.get("labels", {}).get("environment") in ("test", "dev"):
        return d, "passed"
    d["disposition"] = "escalate"
    d["reason"] = "cleanup gate: no machine-checkable test/noise marker — " + d.get("reason", "")
    d["question"] = "Is this genuinely test/noise, safe to clean up at the source?"
    return d, "downgraded (no marker)"


def _benign_history(v):
    """Content check for occurrence_history (review R7): it is self-derived and thus
    always usable, so only a BENIGN-looking history may corroborate a dismiss."""
    if not isinstance(v, dict):
        return False
    lvl = str(v.get("level", "")).lower()
    status = str(v.get("status", "")).lower()
    try:
        cnt = int(v.get("count") or 0)
    except (TypeError, ValueError):
        cnt = 0
    return lvl in ("debug", "info") or status in ("resolved", "ignored") or cnt <= 1


def _corroborates(t):
    """An independent probe with usable content corroborates benign. occurrence_history
    may corroborate ONLY when its content is benign — not merely because it returned."""
    p, r = t["probe"], t["result"]
    if p not in CORROB_PROBES or not _usable(r):
        return False
    if p == "occurrence_history":
        return _benign_history(r)
    return p in INDEP_PROBES


def _to_escalate(d, reason, question):
    d["disposition"] = "escalate"
    d["reason"] = reason + " — " + d.get("reason", "")
    d["question"] = question
    return d


def _dismiss_gate(d, trace):
    if d.get("disposition") != "dismiss":
        return d, "n/a"
    if not str(d.get("dismissal_evidence") or "").strip():
        _to_escalate(d, "dismiss gate: no dismissal_evidence cited",
                     "Dismissed without citing corroborating evidence — confirm benign?")
        return d, "downgraded (no evidence cited)"
    if any(_corroborates(t) for t in trace):
        return d, "passed"
    _to_escalate(d, "dismiss gate: no independent corroborating probe",
                 "Dismissed without independent/benign corroboration — confirm benign?")
    return d, "downgraded (no corroboration)"


EDGE_INSTANCE = "prod-podcast"   # the prod Caddy edge; the edge alert carries only `host`


def _edge_gate(d, signal, data=None):
    """An edge-5xx burst that coincided with a container start or OOM kill on the edge's
    host is not dismissable: probes run after the restart look healthy because the
    container is back. 2026-10-26 13:46:30 burst (closelistening.app, 11 5xx) was
    dismissed on exactly that while player-api was being OOM-killed."""
    if d.get("disposition") != "dismiss" or not signal.get("alertname", "").startswith(guards.EDGE_5XX_PREFIX):
        return d, "n/a"
    look = {"kind": "restarts_near", "args": {"instance": EDGE_INSTANCE, "at": signal.get("startsAt") or ""}}
    try:
        hits = guards.restarts_near(EDGE_INSTANCE, signal.get("startsAt") or "", data=data)
        _exact_add("lookups", dict(look, result=hits))
    except Exception as e:  # noqa: BLE001 — can't check, so can't clear the burst
        _exact_add("lookups", dict(look, error=f"{type(e).__name__}: {e}"[:300]))
        _to_escalate(d, f"edge gate: restart/OOM lookup failed ({e})",
                     "Could not check for a container restart behind this 5xx burst — confirm benign?")
        return d, "downgraded (lookup failed)"
    if not hits:
        return d, "passed"
    _to_escalate(d, f"edge gate: container(s) {', '.join(hits)} restarted or were OOM-killed on "
                    f"{EDGE_INSTANCE} around the burst",
                 f"The 5xx burst coincides with {', '.join(hits)} restarting — was that the cause?")
    return d, "downgraded (restart nearby)"


# The registered gates (RFC-0002 §4), each with its short name in the gates string. They run here, in
# the routine's configured order, and nowhere else: the fleet session's evaluation wraps _gate to see the
# model's own decision before any gate changes it, and its gate replay calls _gate directly.
_GATES = {
    "intent": ("i", lambda d, signal, trace, edge_data: _intent_gate(d)),
    "cleanup": ("c", lambda d, signal, trace, edge_data: _cleanup_gate(d, signal)),
    "dismiss_corroborated": ("d", lambda d, signal, trace, edge_data: _dismiss_gate(d, trace)),
    "edge": ("e", lambda d, signal, trace, edge_data: _edge_gate(d, signal, edge_data)),
}


def _gate(d, signal, trace, edge_data=None):
    parts = []
    for name in _harness_cfg()["gates"]:
        short, fn = _GATES[name]
        d, res = fn(d, signal, trace, edge_data)
        parts.append(f"{short}:{res}")
    return d, "/".join(parts)


_HARNESS_DEFAULT = os.path.join(_FLEET_DIR, "harness-default.json")
_HARNESS_DEFAULT_CFG = None


def _harness_cfg():
    """The routine's harness configuration: FLOTILLA_HARNESS (flotillad, from the spec's `harness:`) or,
    without it (eval runs), fleets/triage/harness-default.json, which a Go test keeps equal to the spec."""
    global _HARNESS_DEFAULT_CFG
    raw = os.environ.get("FLOTILLA_HARNESS")
    if raw:
        cfg = json.loads(raw)
    else:
        if _HARNESS_DEFAULT_CFG is None:
            with open(_HARNESS_DEFAULT, encoding="utf-8") as f:
                _HARNESS_DEFAULT_CFG = json.load(f)
        cfg = _HARNESS_DEFAULT_CFG
    if cfg.get("protocol") != "json":
        raise RuntimeError(f"triage harness: protocol {cfg.get('protocol')!r}; triage speaks json")
    if set(cfg.get("tools") or []) != set(probes.PROBES):
        raise RuntimeError("triage harness: tools must be the probe menu's (the playbook lists them all)")
    unknown = [g for g in cfg.get("gates") or [] if g not in _GATES]
    if unknown or not cfg.get("gates"):
        raise RuntimeError(f"triage harness: gates {unknown or 'none'} not registered")
    if cfg["limits"]["max_tool_calls"] != MAX_PROBES:
        raise RuntimeError(f"triage harness: max_tool_calls {cfg['limits']['max_tool_calls']} is not "
                           f"MAX_PROBES {MAX_PROBES}, the count the playbook names")
    return cfg


def _mark_quirk(name):
    """The quirk a turn hit, on that turn's record (step 6 counts the turns each quirk touches)."""
    if _EXACT is not None and _EXACT["turns"]:
        _EXACT["turns"][-1]["quirk"] = name


def _finish(signal, disp, trace, gates, usage, t0, trace_id=None):
    table_miss = sum(1 for t in trace
                     if isinstance(t["result"], str) and t["result"].startswith("<TABLE-MISS"))
    r = config.RATES.get(config.TRIAGE_MODEL)
    cost = 0.0
    if r and isinstance(usage, dict):
        cost = (usage.get("prompt_tokens") or 0) * r[0] + (usage.get("completion_tokens") or 0) * r[1]
    disp["_meta"] = {"model": config.TRIAGE_MODEL, "prompt_ver": config.TRIAGE_PROMPT_VER,
                     "prompt_sha": PROMPT_SHA, "gates": gates,
                     "probes": [t["probe"] for t in trace], "n_probes": len(trace),
                     "table_miss": table_miss, "cost_usd": round(cost, 6),
                     "certainty": disp.get("certainty"), "usage": usage}
    # what the model asked and got, for capture.py; "shown" is the exact trimmed text it read
    disp["_probe_trace"] = [dict(t, shown=_trim(t["result"])) for t in trace]
    if _EXACT is not None:
        disp["_exact"] = _EXACT
    observ.finalize(signal, disp, usage=usage, latency_s=time.time() - t0, trace_id=trace_id)
    return disp


def investigate(signal, max_probes=None, probe_table=None):
    n = MAX_PROBES if max_probes is None else max_probes
    _exact_start()
    messages = [{"role": "system", "content": _sys()},
                {"role": "user", "content": _signal_intro(signal)}]
    # usage accumulates across ALL turns — reconciliation vs LiteLLM metering
    # (2026-07-30) showed last-turn-only recording undercounted tokens ~4x
    t0 = time.time()
    trace_id = observ.new_trace_id()   # one Langfuse trace: the fleet's + every gateway call
    cfg = _harness_cfg()
    if n != cfg["limits"]["max_tool_calls"]:
        cfg = dict(cfg, limits=dict(cfg["limits"], max_tool_calls=n, max_steps=n + 1))
    # _call and the probes are looked up here at call time: the fleet session's evaluation patches them
    res = harness.run_json_step(
        messages,
        call=lambda m, step: _call(m, trace_id=trace_id, step=step),
        tool=lambda name, args: probes.run_probe(signal, name, args, table=probe_table),
        cfg=cfg,
        errors={"fatal": (SystemExit, TriagerDown), "defer": TriageDeferred, "defer_prefix": "triager call failed: "},
        on_quirk=_mark_quirk)
    if not res.in_loop:
        # Out of turns without a decision (F7): the same escalation as an unusable reply. (An empty
        # last reply deferred inside the harness, so the orchestrator escalates after DEFER_LIMIT cycles.)
        d, gates = _gate(res.decision, signal, res.trace)
        return _finish(signal, d, res.trace, gates, res.usage, t0, trace_id=trace_id)
    # The gates rewrite the decision in place (the intent gate drops the acceptance criteria): keep
    # what the model decided, so a gate change can be measured on captured decisions without re-running
    # the model. Kept for capture only; nothing that decides reads it.
    pre_gate = copy.deepcopy(res.decision)
    d, gates = _gate(res.decision, signal, res.trace)
    out = _finish(signal, d, res.trace, gates, res.usage, t0, trace_id=trace_id)
    out["_pre_gate"] = pre_gate
    return out


# back-compat: orchestrator calls triage.triage(signal, evidence); investigation
# does its own lazy probing, so `evidence` is ignored.
def triage(signal, evidence=None, attempts=3):
    # deterministic operational-state gate (no LLM): guardrail/billing/downstream
    # states are acknowledged-not-ticketed, and this holds even when the triager is
    # down (so a 401 outage can't turn a cost-cap storm into bug tickets).
    cls = operational_class(signal)
    if cls:
        _exact_start()
        disp = {
            "disposition": "dismiss",
            "reason": _OPERATIONAL_REASON.get(cls, (
                f"operational state ({cls}) — a guardrail/billing state or its "
                f"downstream, not a code bug; the alert already fired. "
                f"Acknowledged, not ticketed.")),
            "dismissal_evidence": f"deterministic operational classifier matched '{cls}'",
            "certainty": "high",
            "_meta": {"model": "none(operational-gate)",
                      "prompt_ver": config.TRIAGE_PROMPT_VER, "prompt_sha": PROMPT_SHA,
                      "gates": f"operational:{cls}", "probes": [], "n_probes": 0,
                      "table_miss": 0, "cost_usd": 0.0, "certainty": "high",
                      "operational_class": cls, "usage": {}},
        }
        if _EXACT is not None:
            disp["_exact"] = _EXACT
        observ.finalize(signal, disp, usage={}, latency_s=0.0)
        print(f"    [operational:{cls}] {signal.get('alertname','')[:70]} -> dismiss (no ticket)")
        return disp
    return investigate(signal)
