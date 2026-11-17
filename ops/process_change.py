"""review/process-change: the acting routine for process_change cases (ADR-0016). Started by flotillad
with the case (FLOTILLA_CASE, its proposal in `data`), it re-checks the proposal against the action
registry in effect (FLOTILLA_REGISTRY: the image's, or the installation's own in its fleets folder) and
decides:
  the change is already made, or the file moved on -> {"outcome": "resolved_itself", ...}
  a proposal it cannot read                        -> {"outcome": "escalate", ...}
  a change it can write as a diff -> {"outcome": "plan", "action": "github.open_pr", "params": {repo,
      base, title, body, diff}}: approved, the bugfix fleet's executing routine opens it as a draft PR
      on the repo that holds the registry (RFC-0010 §3)
  otherwise (no value proposed, no repo named, or no commit to base it on) -> {"outcome": "plan", "action":
      "repo.process_change", "params": {file, rule, change}}: approved, a person applies it and records
      the commit
The diff is what a person reads in Approvals; the plan's hash binds it. Policy caps a repo change at
propose.

Where the registry lives is the installation's, set by the spec on the run's command:
  PROCESS_CHANGE_REPO   the repo that holds the registry (owner/name); unset, every change is by hand
  PROCESS_CHANGE_FILE   the registry's path in that repo
The PR's base is the commit the registry was deployed from: the `.commit` file beside it (a fleets
folder deploy_fleets.sh exported), or else the image's own commit (FLOTILLA_VERSION).
"""
import difflib
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402

ACTION = "repo.process_change"
PR_ACTION = "github.open_pr"
# The proposal's `file` names the action registry (the measure's source, and the `file` enum of
# repo.process_change); the registry in effect is FLOTILLA_REGISTRY, wherever it lives.
FILES = ("host/registry.json",)
CHANGE_MAX = 4000


def where():
    """The registry on disk, its repo and its path there (the last two "" when the spec names none)."""
    return (common.REGISTRY, os.environ.get("PROCESS_CHANGE_REPO", "").strip(),
            os.environ.get("PROCESS_CHANGE_FILE", "").strip())


def current(action, key, registry):
    """The list under `key` of `action` in the registry in effect."""
    with open(registry) as f:
        for a in json.load(f)["actions"]:
            if a["name"] == action:
                return list(a.get(key) or [])
    raise common.ConfigError(f"{action} is not declared in {registry}")


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


def deployed_commit(registry, version=None):
    """The commit the registry came from: `.commit` beside it, else the image's."""
    try:
        with open(os.path.join(os.path.dirname(registry), ".commit")) as f:
            c = f.read().strip()
        if re.fullmatch(r"[0-9a-f]{7,40}", c):
            return c
    except OSError:
        pass
    return image_commit(version)


def image_commit(version=None):
    """The commit this image was built from: FLOTILLA_VERSION is sha-<7> (images.yml); "" for a dev
    build, which has no commit to base a PR on."""
    v = os.environ.get("FLOTILLA_VERSION", "") if version is None else version
    m = re.fullmatch(r"sha-([0-9a-f]{7,40})", v)
    return m.group(1) if m else ""


def registry_diff(registry, file, action, key, frm, to):
    """The unified diff, against `file` in the repo, that changes `frm` to `to` in `key` of `action`, on
    that one line of the registry in effect, or "" when the line cannot be found exactly once. The result
    is checked by parsing it: the list must change in that one place and nowhere else."""
    with open(registry) as f:
        old = f.read()
    lines = old.splitlines(keepends=True)
    start = [i for i, ln in enumerate(lines) if re.search(r'"name":\s*' + re.escape(json.dumps(action)), ln)]
    if len(start) != 1:
        return ""
    hits = []
    for i in range(start[0] + 1, len(lines)):
        if re.search(r'"name":\s*"', lines[i]):
            break
        if re.match(r'\s*' + re.escape(json.dumps(key)) + r'\s*:', lines[i]) and lines[i].count(json.dumps(frm)) == 1:
            hits.append(i)
    if len(hits) != 1:
        return ""
    new_lines = list(lines)
    new_lines[hits[0]] = lines[hits[0]].replace(json.dumps(frm), json.dumps(to))
    new = "".join(new_lines)
    want = json.loads(old)
    for a in want["actions"]:
        if a["name"] == action:
            a[key] = [to if x == frm else x for x in a[key]]
    if json.loads(new) != want:
        return ""
    return "".join(difflib.unified_diff(lines, new_lines, "a/" + file, "b/" + file))


def decide(case, place=None, version=None):
    registry, repo, file = place or where()
    p = (case.get("data") or {}).get("proposal")
    if case.get("kind") != "process_change" or not isinstance(p, dict) or not p.get("rule"):
        return {"outcome": "escalate", "detail": "not a process_change case with a proposal"}
    if p.get("from") and p.get("to"):
        if p.get("file") not in FILES:
            return {"outcome": "escalate", "detail": f"{p.get('file')} is not a file a process change may edit"}
        have = current(p["action"], p["key"], registry)
        if p["to"] in have and p["from"] not in have:
            return {"outcome": "resolved_itself", "detail": f"{p['key']} of {p['action']} already has {p['to']}"}
        if p["from"] not in have:
            return {"outcome": "resolved_itself", "detail": f"{p['key']} of {p['action']} no longer has {p['from']} (it has {have})"}
        base = deployed_commit(registry, version)
        diff = registry_diff(registry, file, p["action"], p["key"], p["from"], p["to"]) if repo and file else ""
        if base and diff:
            title = f"{p['action']} {p['key']}: {p['from']} -> {p['to']} (process change, {p['rule']})"
            body = change_text(p, f"Proposed by flotilla's post-incident review on {case.get('id')} (ADR-0016).")
            return {"outcome": "plan", "action": PR_ACTION,
                    "detail": f"{p['from']} -> {p['to']}: re-checked against {file} at {base}, as a PR",
                    "params": {"repo": repo, "base": base, "title": title[:200], "body": body, "diff": diff}}
        why = ("no repo named for process changes (PROCESS_CHANGE_REPO, PROCESS_CHANGE_FILE)" if not (repo and file)
               else "no commit to base a PR on (a dev build)" if not base
               else f"{p['key']} of {p['action']} is not one line it can change")
        return {"outcome": "plan", "action": ACTION, "detail": f"{p['from']} -> {p['to']}: re-checked against {p['file']}; by hand: {why}",
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
