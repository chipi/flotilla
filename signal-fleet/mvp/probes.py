"""Typed probe menu for the investigation loop (SIGNALS §7.3).

Each probe is a deterministic, bounded query the triager requests BY NAME — the
model never writes raw queries (safety + replayability). `run_probe` dispatches
either live or against a frozen probe->response table (the eval replay, EVAL.md /
§7.3 "freeze the probe->response table").
"""
import json
import re

import correlate
import installation


def _event_detail(signal, args):
    iid = signal.get("issue_id")
    if not iid:
        return "<not applicable: not a GlitchTip issue>"
    return correlate._event_summary(iid)


def _occurrence_history(signal, args):
    r = signal.get("raw") or {}
    hist = {k: r.get(k) for k in
            ("count", "userCount", "firstSeen", "lastSeen", "level", "status", "culprit")}
    return hist if any(v is not None for v in hist.values()) else "<no occurrence history>"


# A GlitchTip project slug is not the label its logs and metrics carry: the logs are labelled by app and by
# `service` per surface, the metrics by job (VictoriaLogs `stats by (app, service)`, VictoriaMetrics
# `count by (job) (up)`). Without this map app:<slug> matched no logs for most projects, so their dismissals
# could not be corroborated and escalated.
# A slug not here keeps the old query; the model's own `service` argument still wins.
# Both maps are the installation's (installation.py): probe_log_selector and probe_metric_job.
LOG_SELECTOR = dict(installation.get("probe_log_selector", {}))
METRIC_JOB = dict(installation.get("probe_metric_job", {}))


# The name and the window go into LogsQL and PromQL strings: a plain name and a plain duration only.
NAME_OK = re.compile(r"^[A-Za-z0-9_.-]{1,64}$")
WINDOW_OK = re.compile(r"^[0-9]{1,4}[smhd]$")


def _name(signal, args):
    """The service to query: the model's argument, else the signal's project; None unless a plain name. The model
    knows a project by its GlitchTip slug, so the maps apply to its argument too."""
    svc = str(args.get("service") or signal.get("labels", {}).get("project", ""))
    return svc if NAME_OK.match(svc) else None


def _service_logs(signal, args):
    svc = _name(signal, args)
    window = str(args.get("window", "6h"))
    if not svc:
        return "<no service to query>"
    if not WINDOW_OK.match(window):
        window = "6h"
    selector = LOG_SELECTOR.get(svc, f"app:{svc}")
    return correlate.vl_query(f"{selector} AND _time:{window} | sort by (_time) desc | limit 15")


def _metric(signal, args):
    svc = _name(signal, args)
    if not svc:
        return "<no service to query>"
    kind = args.get("kind", "5xx")
    job = METRIC_JOB.get(svc)
    sel = f'job="{job}"' if job else f'job=~".*{svc}.*"'
    q = {
        "5xx": f'sum(rate(http_requests_total{{{sel},status=~"5.."}}[5m]))',
        "up": f'up{{{sel}}}',
    }.get(kind, f'up{{{sel}}}')
    return correlate.vm_query(q)


def _trace(signal, args):
    tid = args.get("trace_id")
    if not tid:
        return "<no trace_id provided>"
    r = correlate.vt_trace(tid)
    return r if (isinstance(r, dict) and r.get("data")) else f"<trace {tid[:8]} not found>"


_SOURCE_STATE = installation.get("source_state", {})


def _source_state(signal, args):
    """The data-refresh state (last successful refresh) — the staleness case; the query is the installation's."""
    if not _SOURCE_STATE.get("query"):
        return "<not configured: no source_state query>"
    return correlate.vl_query(_SOURCE_STATE["query"])


# name -> (one-line description for the menu, fn)
PROBES = {
    "event_detail": ("the error's latest event: culprit, stack frames, tags, trace_id, platform", _event_detail),
    "occurrence_history": ("count, first/last seen, level, status of the issue", _occurrence_history),
    "service_logs": ("recent logs for a service — args {service?, window?}", _service_logs),
    "metric": ("a metric for a service — args {service?, kind: 5xx|up}", _metric),
    "trace": ("a distributed trace by id — args {trace_id}", _trace),
    "source_state": (_SOURCE_STATE.get("description", "data-refresh state (last successful refresh)"),
                     _source_state),
}


def menu():
    return "\n".join(f"- {name}: {desc}" for name, (desc, _) in PROBES.items())


def probe_key(name, args):
    return name + ":" + json.dumps(args or {}, sort_keys=True)


def run_probe(signal, name, args, table=None):
    """Dispatch a probe. If `table` (a frozen probe->response map) is given, replay
    from it (eval); else run live. Never raises — returns a '<…>' marker on failure.

    Replay coverage on the ARGS axis (review R7): the eager-menu freeze covers every
    probe NAME, but the model can request non-default ARGS. On an exact-key miss, fall
    back to (a) the default-args key for this probe, then (b) ANY frozen key for this
    probe name — so arg drift resolves to the same probe's response. A genuine miss
    returns a <TABLE-MISS …> sentinel, which the scorer counts as EVAL NOISE, never
    as model behavior."""
    if table is not None:
        key = probe_key(name, args)
        if key in table:
            return table[key]
        alt = probe_key(name, {})
        if alt in table:
            return table[alt]
        for k in table:
            if k.split(":", 1)[0] == name:
                return table[k]
        return f"<TABLE-MISS: {key}>"
    entry = PROBES.get(name)
    if not entry:
        return f"<unknown probe: {name}>"
    try:
        return entry[1](signal, args or {})
    except Exception as e:  # noqa: BLE001
        return f"<error: {e}>"
