"""The replay entry point (ADR-0013; docs/wip/replay-entry-point.md): run one of the fleet's decisions over a
recorded world, with nothing sent, so an evaluation project never patches this code's internals.

    import replay_api as ra
    with ra.World(now="2026-10-02T08:33:25Z", ledger=rows, github=gh, keys=None) as world:
        out = ra.file(world, signal, issue, "bug")        # file_or_update
        out = ra.escalate(world, signal, decision)        # file_escalation
        out = ra.sweep(world, occurrences, firing)        # one stale sweep
        out = ra.gates(world, signal, decision, trace=trace, gates={"lookups": [...]})  # the gates alone
        out = ra.triage(world, signal, model={"replies": turns}, tools={"results": table}, gates={"lookups": [...]})

`gh(method, path, payload)` is the caller's: every GitHub call goes to it, writes included, and its answer is what
the code reads back; the caller decides what time its reads stand at. Each call returns
{"api", "result", "writes", "ledger", "record"}. API_VERSION rises only for a breaking change.

Inside a call: the ledger is the world's temporary filed.tsv (the tsv backend, never flotillad's store), the clock
is the world's `now`, GlitchTip notes and Grafana annotations do nothing, observability is off, and the decision's
record is kept in memory, never written, and nothing the code prints is shown (a record may be held-out data).
One call at a time in a process: the code's module attributes are swapped for the call and restored after it, also
on an exception.
"""
import contextlib
import datetime
import io
import os
import tempfile
import threading

import config
import filing

try:
    import callrec
except ImportError:  # fleet code from before RFC-0002 §9 step 1 records nothing: no record
    callrec = None

API_VERSION = 1
_KEY_FNS = ("normalized_key", "group_key_for", "alert_key", "repo_for")
_BUSY = threading.Lock()


def _when(v):
    if isinstance(v, datetime.datetime):
        d = v
    else:
        d = datetime.datetime.fromisoformat(str(v).replace("Z", "+00:00"))
    return d if d.tzinfo else d.replace(tzinfo=datetime.timezone.utc)


def _is_write(method, path):
    # GitHub's GraphQL reads (duplicate_of) are POSTs; every other POST, PATCH, PUT or DELETE writes
    return method != "GET" and not path.startswith("/graphql")


class World:
    """The ledger, GitHub and the clock one decision sees. The ledger carries forward across calls on the same world."""

    def __init__(self, now, ledger=(), github=None, keys=None):
        if github is None:
            raise ValueError("a world needs github: gh(method, path, payload) -> the JSON GitHub answered")
        self.now = _when(now)
        self.github = github
        self.keys = dict(keys) if keys else None
        fd, self.path = tempfile.mkstemp(prefix="replay-ledger-", suffix=".tsv")
        os.close(fd)
        cols = list(filing.FILED_COLS)
        with open(self.path, "w") as f:   # filed.tsv's format: the code's header, one row per line, tab-separated
            f.write("\t".join(cols) + "\n")
            for r in ledger:
                f.write("\t".join(str(r.get(c, "")) for c in cols) + "\n")

    def ledger(self):
        """The ledger now, as rows keyed by the file's own header (padded: a short row reads its missing columns empty)."""
        with open(self.path) as f:
            lines = f.read().splitlines()
        if not lines:
            return []
        head = lines[0].split("\t")
        return [dict(zip(head, ln.split("\t") + [""] * len(head))) for ln in lines[1:] if ln]

    def close(self):
        if os.path.exists(self.path):
            os.unlink(self.path)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()


@contextlib.contextmanager
def _swapped(target, values, env=False):
    """Set attributes of a module (or variables of os.environ with env=True) for the block, then restore them."""
    missing = object()
    if env:
        saved = {k: os.environ.get(k, missing) for k in values}
        os.environ.update(values)
    else:
        saved = {k: getattr(target, k, missing) for k in values}
        for k, v in values.items():
            setattr(target, k, v)
    try:
        yield
    finally:
        for k, v in saved.items():
            if env:
                if v is missing:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v
            elif v is missing:
                delattr(target, k)
            else:
                setattr(target, k, v)


@contextlib.contextmanager
def _quiet():
    """Discard what the fleet's code prints while it runs over a record: the probe loop logs each probe's result and
    the filing and sweep log their decisions, and a record may be the held-out side, which a replay never shows."""
    with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
        yield


@contextlib.contextmanager
def _call(world, use_keys):
    """The decision's world, for the length of one call."""
    if not _BUSY.acquire(blocking=False):
        raise RuntimeError("replay_api runs one call at a time in a process: use processes for parallel replays")
    writes = []

    def gh(method, path, payload=None):
        if _is_write(method, path):
            writes.append({"method": method, "path": path, "payload": payload})
        return world.github(method, path, payload)

    attrs = {"FILED": world.path, "_gh": gh, "_now": lambda: world.now,
             "_glitchtip_note": lambda *a, **k: None, "_grafana_annotation": lambda *a, **k: None,
             "_LEDGER_CLIENT": None}
    if use_keys and world.keys is not None:
        k, real_repo_for = world.keys, filing.repo_for
        attrs.update({"normalized_key": lambda signal, event_summary=None: k.get("norm_key", ""),
                      "group_key_for": lambda alertname, title="", project="": k.get("group_key", ""),
                      "alert_key": lambda repo, source, alertname: k.get("alert_key", ""),
                      "repo_for": lambda signal: k.get("repo") or real_repo_for(signal)})
    try:
        with contextlib.ExitStack() as stack:
            stack.enter_context(_quiet())
            stack.enter_context(_swapped(os.environ, {"SF_LEDGER_BACKEND": "tsv", "SF_OBSERV_DISABLED": "1"},
                                         env=True))
            stack.enter_context(_swapped(config, {"CAPTURE_PROBES": True}))
            stack.enter_context(_swapped(filing, attrs))
            if callrec is not None:
                stack.enter_context(_swapped(callrec, {"LAST_FILING": None}))
            yield writes
    finally:
        _BUSY.release()


def _out(world, result, writes, record):
    return {"api": API_VERSION, "result": str(result), "writes": writes, "ledger": world.ledger(),
            "record": record}


def file(world, signal, issue, kind):
    """One file decision through filing.file_or_update."""
    with _call(world, use_keys=True) as writes:
        result = filing.file_or_update(signal, issue, kind)
        record = callrec.LAST_FILING if callrec is not None else None
    return _out(world, result, writes, record)


def escalate(world, signal, decision):
    """One escalate decision through filing.file_escalation; `decision` carries `question` and `reason`."""
    with _call(world, use_keys=True) as writes:
        result = filing.file_escalation(signal, decision)
        record = callrec.LAST_FILING if callrec is not None else None
    return _out(world, result, writes, record)


def sweep(world, occurrences, firing, quiet_days=None):
    """One stale sweep at the world's `now`. `occurrences`: (ts, fingerprint, source, alertname) tuples or dicts with
    those keys; `firing`: the alert names firing then. The sweep derives every issue's keys with the code's own key
    functions, so the world's recorded keys are not used."""
    import sources
    import stale
    occ = [(o["ts"], o["fingerprint"], o["source"], o["alertname"]) if isinstance(o, dict) else tuple(o)
           for o in occurrences]
    names = sorted(set(firing))
    kept = []
    stale_attrs = {"_occurrences": lambda: list(occ)}
    if quiet_days is not None:
        real_is_stale = stale.is_stale
        stale_attrs["is_stale"] = lambda seen, human, now, quiet_days_=None: real_is_stale(
            seen, human, now, quiet_days=float(quiet_days))

    with _call(world, use_keys=False) as writes, \
            _swapped(os.environ, {"SF_STALE_NUDGE": "1"}, env=True), \
            _swapped(stale, stale_attrs), \
            _swapped(sources, {"firing_alerts": lambda: [{"labels": {"alertname": n}} for n in names]}), \
            contextlib.ExitStack() as stack:
        if callrec is not None:
            stack.enter_context(_swapped(callrec, {"write_sweep": lambda rec, now, occurrences: kept.append(rec.rec)}))
        result = stale.run_pass(dry_run=False, now=world.now, force=True)
    return _out(world, result, writes, kept[-1] if kept else None)


class LookupMissing(LookupError):
    """A gate asked for a lookup the record does not hold: refused rather than answered live."""


def gates(world, signal, decision, trace=(), gates="live"):
    """The registered gates alone (triage._gate in the routine's configured order) over the model's own decision, no
    model call. `trace`: the probe trace the decision was made on ({"probe", "args", "result"} each; the dismiss gate
    reads it). `gates`: "live" (the installation's lookups, read-only) or {"lookups": [{"kind", "args", "result" |
    "error"}, ...]} as recorded; a lookup the record lacks raises LookupMissing. Returns the usual shape with
    "decision" (after the gates), "model_decision" (as given) and the gates string as "result"; "record" holds the
    lookups the gates made."""
    import copy
    import guards
    import triage
    model_decision = copy.deepcopy(decision)
    attrs, missing = {}, []
    if gates != "live":
        recorded = list((gates or {}).get("lookups") or [])

        def restarts_near(instance, at, before_s=600, after_s=300, data=None):
            for look in recorded:
                if look.get("kind") == "restarts_near" and look.get("args") == {"instance": instance, "at": at}:
                    if "error" in look:
                        raise RuntimeError(look["error"])
                    return list(look.get("result") or [])
            # the edge gate turns any lookup error into an escalation, so the miss is noted and raised after it
            missing.append(f"restarts_near for {instance} at {at!r}")
            raise RuntimeError("no recorded lookup")
        attrs["restarts_near"] = restarts_near
    if not _BUSY.acquire(blocking=False):
        raise RuntimeError("replay_api runs one call at a time in a process: use processes for parallel replays")
    try:
        with _quiet(), _swapped(os.environ, {"SF_OBSERV_DISABLED": "1"}, env=True), \
                _swapped(config, {"CAPTURE_PROBES": True}), _swapped(guards, attrs), \
                _swapped(triage, {"_EXACT": None}):
            if hasattr(triage, "_exact_start"):   # fleet code from before the exact records has none: no lookups
                triage._exact_start()
            d, result = triage._gate(copy.deepcopy(decision), signal, list(trace))
            lookups = list((triage._EXACT or {}).get("lookups") or [])
    finally:
        _BUSY.release()
    if missing:
        raise LookupMissing("the record holds no " + "; ".join(missing))
    return {"api": API_VERSION, "result": result, "writes": [], "ledger": world.ledger(),
            "record": {"lookups": lookups}, "decision": d, "model_decision": model_decision}


def _reply(turn):
    """One recorded turn ({"reply", "usage"} | {"status"} | neither: a timeout, as triage recorded it) or a plain
    reply string, as the answer post_json gives."""
    import socket
    import urllib.error
    if isinstance(turn, str):
        return {"choices": [{"message": {"content": turn}}], "usage": {}, "model": "replay"}
    if "reply" in turn:
        if not turn["reply"]:
            return {"choices": [{"message": {"content": ""}}], "usage": {}}
        return {"choices": [{"message": {"content": turn["reply"]}}], "usage": turn.get("usage") or {},
                "model": turn.get("model") or "replay"}
    if turn.get("status"):
        import io
        return urllib.error.HTTPError("http://replay.invalid", int(turn["status"]), "replayed status", {},
                                      io.BytesIO(b"{}"))
    return socket.timeout("timed out")


def triage(world, signal, model, tools, gates="live", max_probes=None):
    """One triage step (triage.investigate: the probe loop, the decision, the gates) over a world, each seam
    recorded or live.

    model:  recorded: {"replies": [turn, ...]} the recorded replies in order (a capture's exact.turns, or reply
            strings), with optional "model" (the alias the requests named), "url" (the endpoint) and "trace_id",
            so the requests come out as recorded; or {"stub": decision}: a fixed decision, no model call at all.
            live (model replay): {"alias": name, "k": runs, "body": extra request fields (reasoning_effort,
            max_tokens), "timeout": seconds, "reply_in_tool_call": a tool name whose call carries the reply,
            "tags": [...], "url": endpoint}. Through the gateway, live requests are tagged "flotilla-replay" (and
            the caller's tags) in their metadata, so its traces tell them from production. The key is the
            installation's (OPENROUTER_API_KEY); without it the call is refused.
    tools:  {"results": probe table} (a capture's `probes`), or "live": the installation's probes, read-only
            (freezing: the run's probe trace is what was frozen).
    gates:  {"lookups": [...]} as for ra.gates, or "live".
    Returns the usual shape with "decision" (after the gates), "model_decision" (before them), and the record:
    the turns, lookups and requests sent ("exact"), the probe trace and meta. With k > 1, "runs" holds every
    run's result, decision, model_decision and record, and the top level is the first run's."""
    import copy
    import json
    import guards
    import observ
    import triage as t
    live_tools = tools == "live"
    if not live_tools and not (isinstance(tools, dict) and "results" in tools):
        raise ValueError("tools: {'results': probe table} or 'live'")
    live_model = isinstance(model, dict) and "alias" in model
    if not isinstance(model, dict) or not (live_model or {"replies", "stub"} & set(model)):
        raise ValueError("model: {'replies': [...]}, {'stub': decision} or {'alias': name, ...}")
    if live_model and not config.OPENROUTER_KEY:
        raise RuntimeError("a live model needs the installation's key (OPENROUTER_API_KEY); none is set")
    missing = []
    real_post = t.post_json

    def recorded_post(sent, queue):
        def post(url, body, headers=None, timeout=60):
            sent.append(json.dumps(body))
            a = queue.pop(0) if queue else {"choices": [{"message": {"content": ""}}], "usage": {}}
            if isinstance(a, BaseException):
                raise a
            return copy.deepcopy(a)
        return post

    def live_post(sent):
        tags = ["flotilla-replay"] + list(model.get("tags") or [])
        tool_name = model.get("reply_in_tool_call")

        def post(url, body, headers=None, timeout=60):
            if config.via_gateway():   # the gateway's Langfuse reads the tags; OpenRouter direct takes no metadata
                body = dict(body, metadata=dict(body.get("metadata") or {}, tags=tags, trace_name="flotilla-replay"))
            body = dict(body, **(model.get("body") or {}))
            if "reasoning_effort" in body and config.via_gateway():
                # a LiteLLM gateway refuses reasoning_effort for most hosted models unless the request names it
                body["allowed_openai_params"] = sorted(set(body.get("allowed_openai_params") or []) |
                                                       {"reasoning_effort"})
            sent.append(json.dumps(body))
            resp = real_post(url, body, headers=headers, timeout=model.get("timeout") or timeout)
            if tool_name:
                msg = ((resp.get("choices") or [{}])[0]).get("message") or {}
                calls = msg.get("tool_calls") or []
                if not (msg.get("content") or "").strip() and len(calls) == 1 \
                        and (calls[0].get("function") or {}).get("name") == tool_name:
                    args = calls[0]["function"].get("arguments")
                    msg["content"] = args if isinstance(args, str) else json.dumps(args)
            return resp
        return post

    cfg = {"CAPTURE_PROBES": True}
    if model.get("model") or model.get("alias"):
        cfg["TRIAGE_MODEL"] = model.get("alias") or model["model"]
    if model.get("url"):
        cfg["OPENROUTER_URL"] = model["url"]
    if not live_model and not config.OPENROUTER_KEY:
        cfg["OPENROUTER_KEY"] = "replay"   # never sent anywhere: post_json is the record's
    g_attrs = {}
    if gates != "live":
        recorded = list((gates or {}).get("lookups") or [])

        def restarts_near(instance, at, before_s=600, after_s=300, data=None):
            for look in recorded:
                if look.get("kind") == "restarts_near" and look.get("args") == {"instance": instance, "at": at}:
                    if "error" in look:
                        raise RuntimeError(look["error"])
                    return list(look.get("result") or [])
            missing.append(f"restarts_near for {instance} at {at!r}")
            raise RuntimeError("no recorded lookup")
        g_attrs["restarts_near"] = restarts_near
    tid = model.get("trace_id")
    k = int(model.get("k") or 1) if live_model else 1
    runs = []
    if not _BUSY.acquire(blocking=False):
        raise RuntimeError("replay_api runs one call at a time in a process: use processes for parallel replays")
    try:
        for _ in range(k):
            sent = []
            t_attrs = {"_EXACT": None, "post_json": live_post(sent) if live_model else
                       recorded_post(sent, [_reply(x) for x in model.get("replies") or []])}
            if "stub" in model:
                stub = json.dumps(dict(model["stub"], action="decide"))
                t_attrs["_call"] = lambda messages, trace_id=None, step=None: (stub, {})
            with _quiet(), _swapped(os.environ, {"SF_OBSERV_DISABLED": "1"}, env=True), _swapped(config, cfg), \
                    _swapped(t, t_attrs), _swapped(guards, g_attrs), \
                    _swapped(observ, {"new_trace_id": (lambda: tid)} if tid else {}):
                d = t.investigate(copy.deepcopy(signal), max_probes=max_probes,
                                  probe_table=None if live_tools else tools["results"])
            if missing:
                raise LookupMissing("the record holds no " + "; ".join(missing))
            meta = d.get("_meta") or {}
            decision = {key: v for key, v in d.items() if not key.startswith("_")}
            runs.append({"result": f"{decision.get('disposition')} ({meta.get('gates')})", "decision": decision,
                         "model_decision": d.get("_pre_gate"),
                         "record": {"exact": dict(d.get("_exact") or {}, requests=sent),
                                    "probe_trace": d.get("_probe_trace"), "meta": meta}})
    finally:
        _BUSY.release()
    out = dict(runs[0], api=API_VERSION, writes=[], ledger=world.ledger())
    if k > 1:
        out["runs"] = runs
    return out
