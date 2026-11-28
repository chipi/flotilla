"""ops/sop-act: the SOP-aware acting routine (operator step 1 build plan C6: match, follow or investigate). Started by flotillad with
the case (FLOTILLA_CASE), it loads the fleet's SOPs (runtime/sops.py) and decides:

  an SOP matches    -> {"outcome": "plan", "action": <fix[0]>, ...}: the registry's preconditions for the run's stage are
                       re-checked as rerun_backup.py checks them (fresh numbers), and the plan's detail cites the SOP by name and
                       sha256. `rung` is a request: policy still takes the lowest of the routine's, the action's, the host's and the
                       installation's caps.
  no SOP matches    -> {"outcome": "hand_over", "stage": "investigate", ...}: the investigate routine takes the case. Two SOPs equally
                       specific are no match either: the reason says so, nothing is guessed.
  an SOP the registry does not allow, or the SOP folder broken -> {"outcome": "escalate", ...}

It plans only; policy decides. The plan's params come from the registry's declaration and the case (the target's host), never from the
SOP's text: an SOP names an action, never a value (rule 4).

Where the SOPs and the registry live is the installation's, set by the spec on the run's command:
  --fleet NAME           the fleet whose sops/ folder is read (<code root>/fleets/NAME/sops, or --sops DIR)
  FLOTILLA_REGISTRY      the registry files in effect, separated like PATH (default: the image's)
"""
import argparse
import json
import math
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "runtime"))
import common  # noqa: E402
import dgx_holder  # noqa: E402
import free_ratio  # noqa: E402
import health  # noqa: E402
import rerun_backup  # noqa: E402
import service_up  # noqa: E402
import sops  # noqa: E402

INVESTIGATE = "investigate"
INVESTIGATED = "investigated: "   # how an investigated plan's detail begins; the SOP draft reads it
# The facts a registry condition needs, fetched only when the action carries that condition: name -> (fact, how to read it).
FACTS = {"backup_age_above": ("age", common.backup_age_hours)}


def disk_free_ratio(case, query=None):
    """The free ratio of the case's own mountpoint (its target is host:<instance><mountpoint>), read now; None when the target is not
    one or the disk reports nothing. The metric names are the node exporter's, or DISK_AVAIL_METRIC and DISK_SIZE_METRIC."""
    where = free_ratio.decode_target((case or {}).get("target"))
    if where is None:
        return None
    avail = os.environ.get("DISK_AVAIL_METRIC") or free_ratio.AVAIL_METRIC
    size = os.environ.get("DISK_SIZE_METRIC") or free_ratio.SIZE_METRIC
    return free_ratio.read_ratio(where[0], where[1], avail, size, query)


disk_free_ratio.needs_case = True   # a fact read from the case, not from the environment alone
FACTS["free_ratio_below"] = ("free_ratio", disk_free_ratio)


def service_up_reading(case, service, query=None):
    """The newest reading of one service (the precondition's argument) on the case's own instance, read now: 1 up, 0 down, None when the
    case is not on a host or the service reports nothing in the window (unknown, never up and never down). The metric and the label are
    SERVICE_METRIC and SERVICE_LABEL, which the flow sets. A 1 is only as good as the gauge: a TCP-open check says the port is open, not
    that the model serves."""
    service_up.plain_service(service)
    metric, label = common.env("SERVICE_METRIC"), common.env("SERVICE_LABEL")
    inst = service_up.host_of((case or {}).get("target"))
    return None if inst is None else service_up.read_up(inst, service, metric, label, query)


service_up_reading.needs_case = True   # a fact read from the case, and from the condition's argument (the service)
service_up_reading.needs_arg = True
FACTS[service_up.CONDITION] = ("service_up", service_up_reading)


def holder_reading(case, query=None, query_vector=None):
    """Whether a job holds the case's host, read now: "held (why)", "free (why)" or None (unknown). See ops/dgx_holder.py: the robotics
    mode's trainer that cannot be confirmed down is held, and an unknown reading never permits a mode switch."""
    state, why = dgx_holder.read(case, os.environ, query, query_vector)
    return None if state is None else f"{state} ({why})"


holder_reading.needs_case = True
FACTS["holder_active"] = FACTS["no_holder_active"] = ("holder", holder_reading)


def health_fact(pick, fetch=health.fetch):
    """A fact read from the health route named by HEALTH_SOURCE, one fresh read per fact: pick(body, name) is True, False or None. A route
    that cannot be read, or whose body is refused, reads None: unknown, never up. A source that is not configured is a ConfigError."""
    def read(case, arg):
        body = health.fact_body(common.env("HEALTH_SOURCE"), os.environ, fetch)
        return None if body is None else pick(body, arg)
    read.needs_case = read.needs_arg = True
    return read


def _component_up(body, name):
    s = health.component_status(body, name)
    return None if s == "unknown" else s == "up"


capability_reading = health_fact(health.capability_ready)
component_reading = health_fact(_component_up)
FACTS["capability_ready"] = ("capability", capability_reading)
FACTS["component_up"] = ("component", component_reading)


def _free_ratio_below(arg, facts):
    return facts["free_ratio"] < float(arg)


def _service_down(arg, facts):
    return facts[f"service_up:{arg}"] < 1


# What this routine evaluates: the backup's, the disk's, a service's, whether a job holds the host, and the health route's. A fact that is
# None (the disk, the service or the route reports nothing) is unknown, never "holds" and never resolved.
CONDITIONS = dict(rerun_backup.CONDITIONS, free_ratio_below=_free_ratio_below, **{service_up.CONDITION: _service_down},
                  holder_active=lambda arg, facts: str(facts["holder"]).startswith("held"),
                  no_holder_active=lambda arg, facts: str(facts["holder"]).startswith("free"),
                  capability_ready=lambda arg, facts: facts[f"capability:{arg}"] is True,
                  component_up=lambda arg, facts: facts[f"component:{arg}"] is True)
HEALTH_CONDITIONS = {"capability_ready", "component_up"}
UNKNOWN_WITHOUT_READING = {"free_ratio_below", service_up.CONDITION, "holder_active", "no_holder_active"} | HEALTH_CONDITIONS


def fills(name, p):
    """Whether code can fill this registry param from the case alone: a host action's target_host, or (a routine action has no
    target_host) a text param named `host` (the target's host) or `target` (the whole target)."""
    return p.get("kind") == "target_host" or (p.get("kind") == "text" and name in ("host", "target"))


MODEL_SAYS = " | model says: "
HEAD = re.compile(r"(?:run \S+: )?investigated: (no fix|\S+) \| reads: ")
URL = re.compile(r"(?i)\b(?:https?|ftp)://\S+|\bwww\.\S+")
MENTION = re.compile(r"<[!@#][^>]*>|(?i:@(?:here|channel|everyone))\b")
MAX_MODEL_TEXT = 500


def clean_model_text(text):
    """What the model said, as a case may carry it: one line, no link, no Slack mention or crowd call, no angle brackets, cut."""
    t = " ".join(str(text or "").split())
    t = MENTION.sub("[mention removed]", URL.sub("[link removed]", t)).replace("<", "‹").replace(">", "›")
    return t if len(t) <= MAX_MODEL_TEXT else t[:MAX_MODEL_TEXT - 1] + "…"


def investigated_detail(action, reads, reason, refused=0, failed=0, checks=""):
    """An investigation's detail on the case: the code's fields first (the action, the reads it accepted as JSON, how many it refused
    or that failed, the preconditions), then the model's own words last and labelled. parse_investigated reads only the code's part."""
    field = json.dumps({"reads": reads, "refused": refused, "failed": failed}, separators=(",", ":"))
    return f"{INVESTIGATED}{action or 'no fix'} | reads: {field}" + (f" | checks: {checks}" if checks else "") + \
        MODEL_SAYS + clean_model_text(reason)


def parse_investigated(detail):
    """(action or None for no fix, the reads) of an investigated detail, or None when it does not begin as one. Only the code's JSON
    field, which comes before any model text, is read: a model that writes a reads line of its own is read as the text it is."""
    m = HEAD.match(detail or "")
    if not m:
        return None
    try:
        obj, _ = json.JSONDecoder().raw_decode(detail, m.end())
    except ValueError:
        return None
    reads = obj.get("reads") if isinstance(obj, dict) else None
    if not isinstance(reads, list):
        return None
    for r in reads:
        if (not isinstance(r, dict) or not isinstance(r.get("name"), str) or not sops.METRIC.fullmatch(r["name"])
                or not isinstance(r.get("instance", ""), str) or not isinstance(r.get("mountpoint", ""), str) or isinstance(r.get("value"), bool)
                or not (r.get("value") is None or isinstance(r["value"], (int, float)))):
            return None
    return (None if m.group(1) == "no fix" else m.group(1)), reads


def params_for(action, case):
    """The plan's params, filled by code from the registry's declaration and the case's target. Any other param is a value no SOP and no
    model may supply: SopError."""
    out = {}
    for k, p in (action.get("params") or {}).items():
        if not fills(k, p):
            raise sops.SopError(f"{action['name']} needs {k} ({p.get('kind')}), a value only a person gives")
        value = case.get("target") or "" if k == "target" else sops.case_host(case)
        if not value or (p.get("kind") == "text" and len(value) > int(p.get("max") or 0)):
            raise sops.SopError(f"{action['name']} needs {k} from the case, and the target is {case.get('target')!r}")
        out[k] = value
    return out


def check(action, stage, fetch=None, case=None, cache=None):
    """Re-check the action's preconditions for the stage, as rerun_backup does: ("holds", [lines]) or ("resolved", detail) when one no
    longer holds, or ("unknown", detail) for a condition this routine does not know, or one that has no reading. A condition the host
    checks is recorded as its. A fact whose reader sets needs_case is read from the case (the disk's mountpoint). A `cache` shared by
    several checks (one decision's alternatives) reads each fact once."""
    facts, lines = ({} if cache is None else cache), []
    fetch = FACTS if fetch is None else fetch
    for c in common.preconditions(action, stage):
        name, arg = common.parse(c)
        if name not in CONDITIONS and name not in fetch:
            return "unknown", f"precondition {name} is not one this routine knows"
        if name in CONDITIONS and CONDITIONS[name] is None:
            lines.append(f"{c}: the host's to check")
            continue
        key, read = fetch[name]
        if getattr(read, "needs_arg", False):   # a fact per argument: service_down(a) and service_down(b) read two services
            key = f"{key}:{arg}"
        if key not in facts:
            facts[key] = (read(case, arg) if getattr(read, "needs_arg", False)
                          else read(case) if getattr(read, "needs_case", False) else read())
        if isinstance(facts[key], float) and not math.isfinite(facts[key]):
            return "unknown", f"{c}: the reading is {facts[key]}, not a number"
        if facts[key] is None and name in UNKNOWN_WITHOUT_READING:
            return "unknown", f"{c}: no reading" + (" (the health route could not be read, was refused, or does not name it)"
                                                    if name in HEALTH_CONDITIONS else " for the case's target")
        if not CONDITIONS[name](arg, facts):
            return "resolved", f"{c} no longer holds"
        lines.append(f"{c}: holds" + (f" ({facts[key]:.1%} free)" if name == "free_ratio_below" else
                                       f" (reads {facts[key]:g})" if name == service_up.CONDITION else
                                       f" ({facts[key]})" if name in ("holder_active", "no_holder_active") else ""))
    return "holds", lines


def fix_alternatives():
    """Whether an SOP's several fixes are alternatives (the first whose preconditions hold): SOP_FIXES=first_holding, set by sop-act@4's
    param. Without it an SOP with several fixes is escalated, as step 1 always did."""
    return os.environ.get("SOP_FIXES", "").strip() == "first_holding"


def decide(case, sop_list, registry, stage, fetch=None, alternatives=None, health_fetch=None):
    sop, why = sops.match(sop_list, case)
    if sop is None:
        return {"outcome": "hand_over", "stage": INVESTIGATE, "detail": f"{why} among {len(sop_list)} SOP(s): investigate",
                "data": {"why": why, "sops": [s.name for s in sop_list]}}
    alternatives = fix_alternatives() if alternatives is None else alternatives
    if len(sop.fix) > 1 and not alternatives:
        return {"outcome": "escalate", "detail": f"{sop.cite()}: {len(sop.fix)} fix steps; step 1 follows single-step SOPs only"}
    try:
        actions = sops.fix_actions(sop, registry, kind=case.get("kind"))
        if alternatives:
            odd = [a["name"] for a in actions if (a.get("check") or {}).get("verify") != sop.verify["check"]]
            if odd:
                raise sops.SopError(f"{', '.join(odd)}: the registry's check is not {sop.verify['check']!r}, the one the SOP verifies "
                                    f"(alternatives share one check)")
        plans = [(a, params_for(a, case)) for a in actions]
    except sops.SopError as e:
        return {"outcome": "escalate", "detail": f"{sop.cite()}: refused: {e}"}
    if health_fetch is not None:
        fetch = dict(FACTS if fetch is None else fetch, capability_ready=("capability", health_fact(health.capability_ready, health_fetch)),
                     component_up=("component", health_fact(_component_up, health_fetch)))
    cache, skipped = {}, []
    for i, (action, params) in enumerate(plans):
        state, lines = check(action, stage, fetch, case, cache)
        if state == "unknown":   # never step past an unknown to a later alternative: it may be the one that holds
            return {"outcome": "escalate", "detail": f"{sop.cite()}: " + "; ".join(skipped + [lines])}
        if state == "resolved":
            skipped.append(f"fix[{i}] {action['name']} skipped: {lines}")
            continue
        detail = f"{sop.cite()}: fix[{i}] {action['name']}; rung {sop.rung} requested (policy decides); " + "; ".join(lines)
        return {"outcome": "plan", "action": action["name"], "params": params, "detail": detail + "".join("; " + k for k in skipped)}
    if len(plans) == 1:
        return {"outcome": "resolved_itself", "detail": f"{sop.cite()}: {skipped[0].split(' skipped: ', 1)[1]}"}
    return {"outcome": "resolved_itself", "detail": f"{sop.cite()}: " + "; ".join(skipped)}


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--fleet", required=True)
    ap.add_argument("--sops", help="the sops folder (default: <code root>/fleets/<fleet>/sops)")
    args = ap.parse_args(argv)
    case = json.loads(common.env("FLOTILLA_CASE"))
    stage = os.environ.get("FLEETD_STAGE", "shadow")
    try:
        sop_list = sops.load_all(args.sops or sops.sops_dir(args.fleet), record=False)
        out = decide(case, sop_list, sops.load_registry(), stage)
        used = sops.match(sop_list, case)[0]
        if used is not None:   # what entered the run: the SOP it followed, not every SOP the fleet has
            sops.record(used)
    except sops.SopError as e:
        out = {"outcome": "escalate", "detail": f"the SOPs cannot be used: {e}"}
    common.report(out)
    print(f"SOP ACT case={case.get('id')} stage={stage} outcome={out['outcome']} {out.get('detail', '')}")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (common.ConfigError, OSError) as e:
        print(f"SOP ACT config: {e}", file=sys.stderr)
        sys.exit(2)
