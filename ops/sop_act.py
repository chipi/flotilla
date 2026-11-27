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
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "runtime"))
import common  # noqa: E402
import free_ratio  # noqa: E402
import rerun_backup  # noqa: E402
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


def _free_ratio_below(arg, facts):
    return facts["free_ratio"] < float(arg)


# What this routine evaluates: the backup's, and the disk's. A fact that is None (the disk reports nothing) is unknown, never "holds".
CONDITIONS = dict(rerun_backup.CONDITIONS, free_ratio_below=_free_ratio_below)
UNKNOWN_WITHOUT_READING = {"free_ratio_below"}


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


def check(action, stage, fetch=None, case=None):
    """Re-check the action's preconditions for the stage, as rerun_backup does: ("holds", [lines]) or ("resolved", detail) when one no
    longer holds, or ("unknown", detail) for a condition this routine does not know, or one that has no reading. A condition the host
    checks is recorded as its. A fact whose reader sets needs_case is read from the case (the disk's mountpoint)."""
    facts, lines = {}, []
    fetch = FACTS if fetch is None else fetch
    for c in common.preconditions(action, stage):
        name, arg = common.parse(c)
        if name not in CONDITIONS and name not in fetch:
            return "unknown", f"precondition {name} is not one this routine knows"
        if name in CONDITIONS and CONDITIONS[name] is None:
            lines.append(f"{c}: the host's to check")
            continue
        key, read = fetch[name]
        if key not in facts:
            facts[key] = read(case) if getattr(read, "needs_case", False) else read()
        if facts[key] is None and name in UNKNOWN_WITHOUT_READING:
            return "unknown", f"{c}: no reading for the case's target"
        if not CONDITIONS[name](arg, facts):
            return "resolved", f"{c} no longer holds"
        lines.append(f"{c}: holds" + (f" ({facts[key]:.1%} free)" if name == "free_ratio_below" else ""))
    return "holds", lines


def decide(case, sop_list, registry, stage, fetch=None):
    sop, why = sops.match(sop_list, case)
    if sop is None:
        return {"outcome": "hand_over", "stage": INVESTIGATE, "detail": f"{why} among {len(sop_list)} SOP(s): investigate",
                "data": {"why": why, "sops": [s.name for s in sop_list]}}
    if len(sop.fix) > 1:
        return {"outcome": "escalate", "detail": f"{sop.cite()}: {len(sop.fix)} fix steps; step 1 follows single-step SOPs only"}
    try:
        action = sops.fix_actions(sop, registry, kind=case.get("kind"))[0]
        params = params_for(action, case)
    except sops.SopError as e:
        return {"outcome": "escalate", "detail": f"{sop.cite()}: refused: {e}"}
    state, lines = check(action, stage, fetch, case)
    if state == "unknown":
        return {"outcome": "escalate", "detail": f"{sop.cite()}: {lines}"}
    if state == "resolved":
        return {"outcome": "resolved_itself", "detail": f"{sop.cite()}: {lines}"}
    detail = f"{sop.cite()}: fix[0] {action['name']}; rung {sop.rung} requested (policy decides); " + "; ".join(lines)
    return {"outcome": "plan", "action": action["name"], "params": params, "detail": detail}


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
