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
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "runtime"))
import common  # noqa: E402
import rerun_backup  # noqa: E402
import sops  # noqa: E402

INVESTIGATE = "investigate"
INVESTIGATED = "investigated: "   # how an investigated plan's detail begins; the SOP draft reads it
# The facts a registry condition needs, fetched only when the action carries that condition: name -> (fact, how to read it).
FACTS = {"backup_age_above": ("age", common.backup_age_hours)}


def fills(name, p):
    """Whether code can fill this registry param from the case alone: a host action's target_host, or (a routine action has no
    target_host) a text param named `host` (the target's host) or `target` (the whole target)."""
    return p.get("kind") == "target_host" or (p.get("kind") == "text" and name in ("host", "target"))


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


def check(action, stage, fetch=None):
    """Re-check the action's preconditions for the stage, as rerun_backup does: ("holds", [lines]) or ("resolved", detail) when one no
    longer holds, or ("unknown", detail) for a condition this routine does not know. A condition the host checks is recorded as its."""
    facts, lines = {}, []
    fetch = FACTS if fetch is None else fetch
    for c in common.preconditions(action, stage):
        name, arg = common.parse(c)
        if name not in rerun_backup.CONDITIONS and name not in fetch:
            return "unknown", f"precondition {name} is not one this routine knows"
        if name in rerun_backup.CONDITIONS and rerun_backup.CONDITIONS[name] is None:
            lines.append(f"{c}: the host's to check")
            continue
        key, read = fetch[name]
        if key not in facts:
            facts[key] = read()
        if not rerun_backup.CONDITIONS[name](arg, facts):
            return "resolved", f"{c} no longer holds"
        lines.append(f"{c}: holds")
    return "holds", lines


def decide(case, sop_list, registry, stage, fetch=None):
    sop, why = sops.match(sop_list, case)
    if sop is None:
        return {"outcome": "hand_over", "stage": INVESTIGATE, "detail": f"{why} among {len(sop_list)} SOP(s): investigate",
                "data": {"why": why, "sops": [s.name for s in sop_list]}}
    try:
        action = sops.fix_actions(sop, registry, kind=case.get("kind"))[0]
        params = params_for(action, case)
    except sops.SopError as e:
        return {"outcome": "escalate", "detail": f"{sop.cite()}: refused: {e}"}
    state, lines = check(action, stage, fetch)
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
        sop_list = sops.load_all(args.sops or sops.sops_dir(args.fleet))
        out = decide(case, sop_list, sops.load_registry(), stage)
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
