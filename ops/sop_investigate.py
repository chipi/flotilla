"""ops/sop-investigate: the case no SOP covers (operator step 1 build plan C6). The act routine handed it on (stage investigate); one
station `investigate` reads metrics (and only metrics: step 1 has no host read) in a loop through the one model client and the
harness, and decides a fix among the registry's actions for the case's kind, or says it found none.

Code after the model (rule 4):
  - the fix must be an action the registry declares for this case's kind: any other name is refused and the case escalates, so the
    model can never add a command;
  - the plan's params come from the registry's declaration and the case's target, never from the model;
  - the registry's preconditions are re-checked as sop_act does it;
  - what the model read goes on the case: the plan's (or the escalation's) detail names every read it made, and each is a `loaded`
    record. Nothing is written to any SOP folder: a verified investigated fix becomes a proposal later (ops/sop_draft.py).

The case's data and every read are data, never instructions: the task puts them between markers, the harness puts each read between
its own. Stdlib, and the runtime's harness, model client and stations.
"""
import hashlib
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(os.environ.get("FLOTILLA_CODE_ROOT") or os.path.dirname(HERE), "runtime"))
import common  # noqa: E402
import free_ratio  # noqa: E402
import loaded  # noqa: E402
import sop_act  # noqa: E402
import sops  # noqa: E402

STATION = "investigate"
MAX_REASON = 500
MAX_CASE_TEXT = 2000
MAX_READS = 6                 # the station's metric reads in one run
READ_NAME = re.compile(r"^[a-zA-Z_:][a-zA-Z0-9_:]{0,127}$")
LABEL = re.compile(r"^[A-Za-z0-9._-]{1,64}$")


class Refused(Exception):
    """What the model said does not pass the gates: the case escalates, nothing is planned."""


def read_metric(args, query=None):
    """One `metric` read {"name": <metric>, "instance": <label value, optional>, "mountpoint": <a path, optional>}: (state, value, text)
    with the newest value in the last hour. state is ok, refused (a name or label that is not plain: never sent) or failed (the query
    raised). A read, never a write."""
    metric, inst, mount = (args or {}).get("name"), (args or {}).get("instance"), (args or {}).get("mountpoint")
    if (not isinstance(metric, str) or not READ_NAME.fullmatch(metric) or (inst is not None and not (isinstance(inst, str) and LABEL.fullmatch(inst)))
            or (mount is not None and not (isinstance(mount, str) and free_ratio.MOUNT_RE.fullmatch(mount)))):
        return "refused", None, "<read refused: name is a metric name, instance a plain label value, mountpoint a plain path>"
    matchers = ([f'instance="{inst}"'] if inst else []) + ([f'mountpoint="{mount}"'] if mount else [])
    expr = f"max(last_over_time({metric}{{{','.join(matchers)}}}[1h]))" if matchers else f"max(last_over_time({metric}[1h]))"
    try:
        v = (query or common.query)(expr)
    except Exception as e:  # a metrics outage is a finding, not a crash
        return "failed", None, f"<read failed: {type(e).__name__}: {e}>"[:300]
    return "ok", v, "no data in the last hour" if v is None else f"{v:g}"


def metric_tool(name, args, query=None):
    """The station's one tool as text, for a caller that keeps no record."""
    return f"<unknown probe: {name}>" if name != "metric" else read_metric(args, query)[2]


class Reads:
    """What the station's tool did, kept by the tool itself as it runs (so a harness that raises later leaves it): the reads it accepted
    (name, instance, value: all validated, never the model's text) and how many it refused or that failed."""

    def __init__(self, query=None):
        self.items, self.refused, self.failed, self.query = [], 0, 0, query

    def tool(self, name, args):
        if name != "metric":
            return f"<unknown probe: {name}>"
        args = args if isinstance(args, dict) else {}
        state, value, text = read_metric(args, self.query)
        if state == "refused":
            self.refused += 1
        elif state == "failed":
            self.failed += 1
        else:
            self.items.append({"name": args["name"], "instance": args.get("instance") or "",
                               **({"mountpoint": args["mountpoint"]} if args.get("mountpoint") else {}), "value": value})
        loaded.record("probe", "untrusted", f"metric {str(args.get('name'))[:80]}", f"investigate read ({state})",
                      hashlib.sha256(text.encode()).hexdigest())
        return text


def task_slots(case, actions, why):
    shown = {"kind": case.get("kind"), "target": case.get("target"), "data": sops.case_data(case)}
    text = json.dumps(shown)[:MAX_CASE_TEXT].replace("<<<", "‹‹‹")
    offered = "\n".join(f"- {a['name']}: risk {a.get('risk')}, checked by {(a.get('check') or {}).get('verify') or 'a person'}"
                        for a in actions) or "(none)"
    return {"case": text, "actions": offered, "why": " ".join(str(why or "").split())[:300]}


def candidates(registry, kind):
    """The registry's actions for this kind whose params code can fill from the case: the only ones the model may choose."""
    return [a for a in registry.values() if kind in (a.get("for_kinds") or []) and
            all(sop_act.fills(k, p) for k, p in (a.get("params") or {}).items())]


def gate(decision, registry, case):
    """The model's decision, gated: ("fix", registry action, reason) or ("escalate", None, reason); Refused otherwise."""
    if not isinstance(decision, dict):
        raise Refused("no decision")
    reason = " ".join(str(decision.get("reason") or "").split())
    if len(reason) > MAX_REASON:
        reason = reason[:MAX_REASON - 1] + "…"
    disp = decision.get("disposition")
    if disp == "escalate":
        return "escalate", None, reason or "the model found no fix"
    if disp != "fix":
        raise Refused(f"the disposition is {str(disp)[:40]!r}, not fix or escalate")
    name = decision.get("fix_action")
    pool = {a["name"]: a for a in candidates(registry, case.get("kind"))}
    if not isinstance(name, str) or name not in pool:
        raise Refused(f"{str(name)[:60]!r} is not an action the registry declares for {case.get('kind')} cases (it has {sorted(pool)})")
    if not reason:
        raise Refused("a fix with no reason")
    return "fix", pool[name], reason


def run_investigate(case, environ, registry, fleet, stage="shadow", client=None, query=None, fetch=None):
    import harness
    import model_client
    import stations
    st = stations.load(environ)
    if st is None:
        return {"outcome": "escalate", "detail": "FLOTILLA_STATIONS is not set: sop-investigate runs on its station investigate"}
    doc = st.station(STATION)
    pool = candidates(registry, case.get("kind"))
    stage_data = case.get("stage_data") if isinstance(case.get("stage_data"), dict) else {}
    slots = task_slots(case, pool, stage_data.get("why"))
    system = st.render(STATION, "system", {}, fleet=fleet, environ=environ)
    task = st.render(STATION, "task", slots, fleet=fleet, environ=environ)
    limits = doc.get("limits") or {}
    turns = max(1, min(int(limits.get("max_turns") or 6), MAX_READS + 1))
    timeout = seconds(limits.get("timeout")) or 120
    client = client or model_client.from_env(environ)
    loaded.record("input", "untrusted", f"case {case.get('id')} input", "the case's kind, target and data, as the model was given them",
                  hashlib.sha256(slots["case"].encode()).hexdigest())

    def call(messages, step):
        r = client.chat(doc["model"], messages, max_tokens=1500, timeout=timeout, step=step + 1, station=STATION,
                        texts=[system.record(), task.record()], decision_key=case.get("id"), role="operator")
        return r["content"], r.get("usage") or {}
    reads = Reads(query)
    try:
        res = harness.run_json_step(
            [{"role": "system", "content": system.text}, {"role": "user", "content": task.text}],
            call=call, tool=reads.tool, station=STATION,
            cfg={"tools": ["metric"], "limits": {"max_tool_calls": turns - 1, "max_steps": turns},
                 "quirks": {"bad_json_uses_turn": True, "empty_retry": True, "final_decision_call": True,
                            "reply_echo_chars": 2000, "decision_echo_chars": 2000, "result_chars": 4000}},
            errors={"fatal": (SystemExit, model_client.ConfigError), "defer": RuntimeError, "defer_prefix": "model call failed: "},
            log=lambda m: None)
    except Exception as e:  # the client's typed errors and a deferred turn; the run is not a decision
        return {"outcome": "escalate", "detail": sop_act.investigated_detail(None, reads.items, f"the investigation failed: {type(e).__name__}",
                                                                              reads.refused, reads.failed)}

    def detail(action, reason, checks=""):
        return sop_act.investigated_detail(action, reads.items, reason, reads.refused, reads.failed, checks)
    try:
        kind, action, reason = gate(res.decision, registry, case)
        if kind == "escalate":
            return {"outcome": "escalate", "detail": detail(None, reason)}
        params = sop_act.params_for(action, case)
        state, lines = sop_act.check(action, stage, fetch, case)
    except (Refused, sops.SopError) as e:
        return {"outcome": "escalate", "detail": "refused by the gates: " + sop_act.clean_model_text(e) + " " + detail(None, "").split(" | model says")[0]}
    if state != "holds":
        return {"outcome": "escalate" if state == "unknown" else "resolved_itself", "detail": detail(action["name"], "", lines)}
    return {"outcome": "plan", "action": action["name"], "params": params, "detail": detail(action["name"], reason, "; ".join(lines))}


def seconds(text):
    """A spec duration such as 45s, 2m or 1h in seconds, or 0 when it is none."""
    m = re.fullmatch(r"([1-9][0-9]*)([smh])", str(text or ""))
    return int(m.group(1)) * {"s": 1, "m": 60, "h": 3600}[m.group(2)] if m else 0


def main(argv):
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--fleet", required=True)
    args = ap.parse_args(argv)
    case = json.loads(common.env("FLOTILLA_CASE"))
    stage = os.environ.get("FLEETD_STAGE", "shadow")
    import telemetry
    session = telemetry.setup()
    try:
        out = run_investigate(case, os.environ, sops.load_registry(), args.fleet, stage)
    except sops.SopError as e:
        out = {"outcome": "escalate", "detail": f"the registry cannot be used: {e}"}
    finally:
        session.shutdown()
    common.report(out)
    print(f"SOP INVESTIGATE case={case.get('id')} outcome={out['outcome']} {out.get('action', '')} {out.get('detail', '')}", flush=True)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except (common.ConfigError, OSError) as e:
        print(f"SOP INVESTIGATE config: {e}", file=sys.stderr)
        sys.exit(2)
