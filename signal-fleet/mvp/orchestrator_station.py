"""The triage cycle of a version that declares the `decide` station (docs/wip/v2-design.md sections 2 and 8).

orchestrator.py and triage.py are the code the flat routine runs, and they stay as they are. This is the entry point beside them for
a version whose spec declares a station named decide on harness flotilla (fleets/triage/cycle/v1). flotillad hands such a run
FLOTILLA_STATIONS and FLOTILLA_MODELS, and what the model step needs from the spec comes from there and nowhere else:

  the system text   the station's `system` text, hash-checked (runtime/stations.py), its two slots filled with the probe count and the menu
  the model         the station's alias, resolved through FLOTILLA_MODELS (SF_TRIAGE_MODEL and SF_OPENROUTER_URL are not read)
  the probes        the station's tool_names; the gates on its decision and the hooks around each probe: its gates and hooks
  the turns         max_turns (a probe a turn, the last turn decides) and the reply cap max_output_tokens; the version's flow limits
                    (call_timeout_s, reply_echo_chars, decision_echo_chars, result_chars) are the first fleet's loop kept like for like
  the skills        only the ones the station lists may be loaded

install() puts them into the seams triage.py already has (its _harness_cfg, _client, REPLY_MAX_TOKENS) and replaces investigate() by a copy
that records the station's text where the original records the playbook's. The cycle, the probes, the gates, the ledger and the way a model
call fails are the code orchestrator.py and triage.py have; signal-fleet/mvp/test_orchestrator_station.py sends both paths the same signals
against the same scripted endpoint and compares the requests byte for byte and the decisions.

  python3 orchestrator_station.py --cycle --limit 10 --no-dry-run
"""
import argparse
import copy
import hashlib
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import config  # noqa: E402
import observ  # noqa: E402
import orchestrator  # noqa: E402
import probes  # noqa: E402
import triage  # noqa: E402  (puts flotilla's runtime/ on the path when it is not already)

try:
    import stations
except ImportError:
    sys.path.insert(0, os.path.join(HERE, "..", "..", "runtime"))
    import stations  # noqa: E402
import harness  # noqa: E402
import hooks  # noqa: E402
import loaded  # noqa: E402
import model_client  # noqa: E402
import prompts  # noqa: E402
import skills  # noqa: E402

STATION = "decide"
TEXT = "system"
FLEET = "triage"
FLOW_LIMITS = ("call_timeout_s", "reply_echo_chars", "decision_echo_chars", "result_chars")
# harness.run_json_step implements only these three quirks as true until RFC-0002 §9 step 6 removes them one by one
QUIRKS_ON = ("bad_json_uses_turn", "empty_retry", "final_decision_call")


class StationConfigError(RuntimeError):
    """The version's station is not one this entry point can run: a message that names what is wrong, before any model call."""


def _positive(name, v):
    if not isinstance(v, int) or isinstance(v, bool) or v <= 0:
        raise StationConfigError(f"{name} must be a positive whole number, got {v!r}")
    return v


def settings(environ=None):
    """The station's settings as the cycle uses them: (harness cfg, system text, reply cap, call timeout, station dict, Rendered).
    Raises StationConfigError, or stations.StationsError for a text or slot the version does not declare."""
    environ = os.environ if environ is None else environ
    st = stations.load(environ)
    if st is None:
        raise StationConfigError("FLOTILLA_STATIONS is not set: this entry point runs a version that declares the decide station "
                                 "(the flat routine runs orchestrator.py)")
    s = st.station(STATION)
    if s.get("harness") != "flotilla":
        raise StationConfigError(f"station {STATION}: harness {s.get('harness')!r}; triage's loop is harness flotilla")
    lim, flow = s.get("limits") or {}, st.limits()
    max_turns = _positive(f"station {STATION} limits.max_turns", lim.get("max_turns"))
    reply_cap = _positive(f"station {STATION} limits.max_output_tokens", lim.get("max_output_tokens"))
    values = {k: _positive(f"version limits.{k}", flow.get(k)) for k in FLOW_LIMITS}
    if max_turns < 2:
        raise StationConfigError(f"station {STATION} limits.max_turns {max_turns}: at least a probe turn and the deciding turn")
    cfg = {"protocol": "json", "tools": list(s.get("tool_names") or []), "gates": list(s.get("gates") or []),
           "limits": {"max_tool_calls": max_turns - 1, "max_steps": max_turns, "tool_calls_per_turn": 1},
           "quirks": dict({k: True for k in QUIRKS_ON}, reply_echo_chars=values["reply_echo_chars"],
                          decision_echo_chars=values["decision_echo_chars"], result_chars=values["result_chars"]),
           "hooks": s.get("hooks") or None}
    # the checks triage._harness_cfg makes of a harness block, made here of the station
    if set(cfg["tools"]) != set(probes.PROBES):
        raise StationConfigError("triage harness: tool_names must be the probe menu's (the playbook lists them all)")
    unknown = [g for g in cfg["gates"] if g not in triage._GATES]
    if unknown or not cfg["gates"]:
        raise StationConfigError(f"triage harness: gates {unknown or 'none'} not registered")
    if cfg["limits"]["max_tool_calls"] != triage.MAX_PROBES:
        raise StationConfigError(f"triage harness: max_turns {max_turns} makes {cfg['limits']['max_tool_calls']} probes, not MAX_PROBES "
                                 f"{triage.MAX_PROBES}, the count the playbook names")
    try:
        hooks.build(cfg["hooks"], triage._HOOKS)
    except hooks.HookError as e:
        raise StationConfigError(f"triage harness: {e}") from e
    allowed = set(s.get("skills") or [])
    for r in skills.rules_from_env(triage._DEFAULT_RULES, environ):
        for name in r["load"]:
            if name not in allowed:
                raise StationConfigError(f"the skills rule loads {name}, which station {STATION} does not list in skills")
    r = st.render(STATION, TEXT, {"max_probes": str(triage.MAX_PROBES), "probe_menu": probes.menu()}, fleet=FLEET)
    text = r.text[:-1] if r.text.endswith("\n") else r.text   # the file ends in one newline; the prompt does not
    rendered = stations.Rendered(r.station, r.name, text, r.template_sha256)
    return cfg, text, reply_cap, values["call_timeout_s"], s, rendered


class StationClient:
    """The model client with the station's name and text on every call (the calls line says which station and which text), and the
    call timeout of the version's limits where triage._call says 90."""

    def __init__(self, client, rendered, timeout):
        self.client, self.rendered, self.timeout = client, rendered, timeout

    def chat(self, alias, messages, **kw):
        kw["timeout"] = self.timeout
        return self.client.chat(alias, messages, station=STATION, texts=[self.rendered.record()], **kw)


def install(environ=None):
    """Make triage.py run the decide station of this run's version. Returns the system text."""
    environ = os.environ if environ is None else environ
    cfg, text, reply_cap, call_timeout, station, rendered = settings(environ)
    client = model_client.from_env(environ, transport=triage._post_json_transport)
    alias = station.get("model")
    a = client._alias(alias)   # ConfigError for an alias FLOTILLA_MODELS does not have
    # triage._call and its failure messages read these; the key is the alias's own, never a second setting
    config.OPENROUTER_KEY = client._key(a) or "none"
    config.OPENROUTER_URL = a.base_url.rstrip("/") + triage._ENDPOINT_PATH
    config.TRIAGE_MODEL = a.model
    triage._harness_cfg = lambda: copy.deepcopy(cfg)
    triage._client = lambda: (StationClient(client, rendered, call_timeout), alias)
    triage.REPLY_MAX_TOKENS = reply_cap
    triage.PROMPT_SHA = hashlib.sha1(text.encode()).hexdigest()[:8]
    triage.investigate = lambda signal, max_probes=None, probe_table=None: investigate(signal, text, max_probes, probe_table, station)
    return text


def investigate(signal, system, max_probes=None, probe_table=None, station=None):
    """triage.investigate with the station's system text: the same turns, probes, gates and record. A station that declares `reads:`
    (RFC-0014 section 4.8) also offers the model its read tools; one that does not is called exactly as before."""
    n = triage.MAX_PROBES if max_probes is None else max_probes
    triage._exact_start()
    triage._TURNS["key"] = signal.get("fingerprint") or None
    triage._EPISODES["n"] += 1
    loaded.record("playbook", "source", "triage system text", f"station {STATION} {TEXT} @ {triage.PROMPT_SHA}",
                  hashlib.sha256(system.encode()).hexdigest())
    loaded.record("input", "untrusted", f"{signal.get('source') or 'signal'}: {signal.get('alertname') or '-'}",
                  triage._trim(str(signal.get("summary") or ""), 200))
    messages = [{"role": "system", "content": system},
                {"role": "user", "content": triage._signal_intro(signal)}]
    t0 = time.time()
    trace_id = observ.new_trace_id()
    cfg = triage._harness_cfg()
    if n != cfg["limits"]["max_tool_calls"]:
        cfg = dict(cfg, limits=dict(cfg["limits"], max_tool_calls=n, max_steps=n + 1))
    # the station's name is passed only with its reads: without them the recorded-tools key stays what it was
    live_reads = {"station": STATION, "reads": station} if station and station.get("reads") else {}
    res = harness.run_json_step(
        messages,
        call=lambda m, step: triage._call(m, trace_id=trace_id, step=step),
        tool=lambda name, args: triage._probe(signal, name, args, probe_table),
        cfg=cfg,
        errors={"fatal": (SystemExit, triage.TriagerDown), "defer": triage.TriageDeferred, "defer_prefix": "triager call failed: "},
        on_quirk=triage._mark_quirk, on_event=triage._on_event, fleet_hooks=triage._HOOKS, **live_reads)
    if not res.in_loop:
        d, gates = triage._gate(res.decision, signal, res.trace)
        return triage._finish(signal, d, res.trace, gates, res.usage, t0, trace_id=trace_id)
    pre_gate = copy.deepcopy(res.decision)
    d, gates = triage._gate(res.decision, signal, res.trace)
    out = triage._finish(signal, d, res.trace, gates, res.usage, t0, trace_id=trace_id)
    out["_pre_gate"] = pre_gate
    return out


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--poll", "--cycle", dest="poll", action="store_true", help="one daemon cycle: grafana + glitchtip (fleetd contract)")
    ap.add_argument("--synthetic", action="store_true", help="synthetic staleness signal")
    ap.add_argument("--glitchtip", action="store_true", help="process unresolved GlitchTip errors")
    ap.add_argument("--limit", type=int, default=5, help="max GlitchTip triages (model calls) per run; up to POLL_WIDTH issues are read")
    ap.add_argument("--no-dry-run", action="store_true", help="really create issues (gated)")
    args = ap.parse_args(argv)
    try:
        install()
    except (StationConfigError, stations.StationsError, prompts.PromptError, model_client.ConfigError) as e:
        print(f"[triage] REFUSED to start: {e}", file=sys.stderr)
        return 2
    import telemetry
    telemetry.setup()
    if args.poll:
        return 1 if orchestrator.run_poll(limit=args.limit, dry_run=not args.no_dry_run) >= 2 else 0
    if args.glitchtip:
        orchestrator.run_glitchtip(limit=args.limit, dry_run=not args.no_dry_run)
    else:
        orchestrator.run_once(use_synthetic=args.synthetic, dry_run=not args.no_dry_run)
    return 0


if __name__ == "__main__":
    sys.exit(main())
