"""ops/process-change: the acting routine for process_change cases (ADR-0016). Started by flotillad with
the case (FLOTILLA_CASE, its proposal in `data`), it re-checks the proposal against the repo files in its
own image (the deployed commit) and decides:
  the change is already made, or the file moved on -> {"outcome": "resolved_itself", ...}
  a proposal it cannot read                        -> {"outcome": "escalate", ...}
  otherwise -> {"outcome": "plan", "action": "repo.process_change", "params": {file, rule, change}}
The change is the diff a person reads in Approvals; the plan's hash binds it. Policy caps a repo change
at propose: a person approves, then applies it and records the commit (until step 4 opens a PR).
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402

ACTION = "repo.process_change"
CODE_ROOT = os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla")
FILES = ("host/registry.json",)  # what a process change may edit (the registry's enum for `file`)
CHANGE_MAX = 4000


def current(file, action, key, root=CODE_ROOT):
    """The list under `key` of `action` in the repo file, as the image has it."""
    with open(os.path.join(root, file)) as f:
        for a in json.load(f)["actions"]:
            if a["name"] == action:
                return list(a.get(key) or [])
    raise common.ConfigError(f"{action} is not declared in {file}")


def change_text(p, extra=""):
    lines = []
    if p.get("from") and p.get("to"):
        lines += [f"{p['file']}, action {p['action']}, {p['key']}:", f"- \"{p['from']}\"", f"+ \"{p['to']}\""]
    else:
        lines += [p.get("title", ""), "No value proposed: decide it, then change the check."]
    lines += ["", "Why (the evidence):"] + [f"- {e}" for e in p.get("evidence", [])]
    if extra:
        lines += ["", extra]
    text = "\n".join(lines)
    return text if len(text) <= CHANGE_MAX else text[:CHANGE_MAX - 1] + "…"


def decide(case, root=CODE_ROOT):
    p = (case.get("data") or {}).get("proposal")
    if case.get("kind") != "process_change" or not isinstance(p, dict) or not p.get("rule"):
        return {"outcome": "escalate", "detail": "not a process_change case with a proposal"}
    if p.get("from") and p.get("to"):
        if p.get("file") not in FILES:
            return {"outcome": "escalate", "detail": f"{p.get('file')} is not a file a process change may edit"}
        have = current(p["file"], p["action"], p["key"], root)
        if p["to"] in have and p["from"] not in have:
            return {"outcome": "resolved_itself", "detail": f"{p['key']} of {p['action']} already has {p['to']}"}
        if p["from"] not in have:
            return {"outcome": "resolved_itself", "detail": f"{p['key']} of {p['action']} no longer has {p['from']} (it has {have})"}
        return {"outcome": "plan", "action": ACTION, "detail": f"{p['from']} -> {p['to']}: re-checked against {p['file']}",
                "params": {"file": p["file"], "rule": p["rule"], "change": change_text(p)}}
    return {"outcome": "plan", "action": ACTION, "detail": f"{p['rule']}: no value proposed",
            "params": {"file": "none", "rule": p["rule"], "change": change_text(p)}}


def main():
    case = json.loads(common.env("FLOTILLA_CASE"))
    out = decide(case)
    common.report(out)
    print(f"PROCESS CHANGE case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"PROCESS CHANGE config: {e}", file=sys.stderr)
        sys.exit(2)
