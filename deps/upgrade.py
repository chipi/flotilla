"""deps/upgrade: the acting routine for dependency_update cases. Started by flotillad with the case
(FLOTILLA_CASE), it re-checks the pin against the repo's default branch now and decides, with no model:
  the pin moved since the scan (upgraded, or removed) -> {"outcome": "resolved_itself", ...}
  a case it cannot read, or a version PyPI does not release -> {"outcome": "escalate", ...}
  otherwise -> {"outcome": "plan", "action": "github.open_pr", "params": {repo, base, title, body, diff}}:
      a one-line change of the pin to PyPI's latest release, based on the head commit it read
The gates are code: only that one line changes, the new version is a released, unyanked one, and the
diff is checked by applying it. The repo's own CI on the PR is the test that decides; bugfix/pr-check
watches it. A person approves the plan at propose, and merges.
"""
import difflib
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402

ACTION = "github.open_pr"


def bump(text, line, name, frm, to):
    """The manifest with that one pin moved, or None when the line is not that pin any more."""
    lines = text.splitlines(keepends=True)
    if line >= len(lines):
        return None
    m = common.PIN.match(lines[line].strip())
    if not m or common.norm(m["name"]) != common.norm(name) or m["version"] != frm:
        return None
    lines[line] = lines[line].replace(f"=={frm}", f"=={to}", 1)
    return "".join(lines)


def decide(case, gh, http=common.http_json):
    d = case.get("data") or {}
    if case.get("kind") != common.KIND or not all(d.get(k) for k in ("repo", "manifest", "name", "current")):
        return {"outcome": "escalate", "detail": "not a dependency_update case with its repo, manifest and pin"}
    repo, manifest, name, cur = d["repo"], d["manifest"], d["name"], d["current"]
    if not common.REPO.match(repo):
        return {"outcome": "escalate", "detail": f"{repo!r} is not a repo"}
    base = gh.head(repo)
    text = gh.file(repo, manifest, base)
    now = {common.norm(n): (i, v) for i, n, v in common.pins(text)}
    if common.norm(name) not in now:
        return {"outcome": "resolved_itself", "detail": f"{manifest} at {base[:7]} no longer pins {name}"}
    line, have = now[common.norm(name)]
    new, released = common.latest(name, http)
    if have != cur:
        return {"outcome": "resolved_itself", "detail": f"{manifest} at {base[:7]} pins {name}=={have}, not {cur}"}
    if new == cur:
        return {"outcome": "resolved_itself", "detail": f"{name} {cur} is PyPI's latest"}
    if new not in released:
        return {"outcome": "escalate", "detail": f"PyPI's latest {name} {new} is not a released, unyanked version"}
    after = bump(text, line, name, cur, new)
    if after is None or [ln for ln in difflib.ndiff(text.splitlines(), after.splitlines()) if ln[:1] in "+-"] != \
            [f"- {text.splitlines()[line]}", f"+ {after.splitlines()[line]}"]:
        return {"outcome": "escalate", "detail": f"cannot change {name}'s pin on one line of {manifest}"}
    diff = "".join(difflib.unified_diff(text.splitlines(keepends=True), after.splitlines(keepends=True),
                                        "a/" + manifest, "b/" + manifest))
    ids = common.vulns([(name, cur)], http).get((name, cur), [])
    why = f"fixes {', '.join(ids)}" if ids else "behind the latest release"
    body = "\n".join([f"Moves {name} from {cur} to {new} in {manifest} ({why}).", "",
                      f"Proposed by flotilla's dependency fleet on {case.get('id')}; re-checked against {base[:7]}.",
                      "The repo's CI on this PR is the test that decides; nothing else changes."])
    return {"outcome": "plan", "action": ACTION, "detail": f"{name} {cur} -> {new} ({why}), on {base[:7]}",
            "params": {"repo": repo, "base": base, "title": f"{name}: {cur} -> {new} ({why})"[:200],
                       "body": body[:8000], "diff": diff}}


def main(gh=None, http=common.http_json):
    case = json.loads(os.environ["FLOTILLA_CASE"])
    out = decide(case, gh or common.GitHub(), http)
    common.report(out)
    print(f"DEPS UPGRADE case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}", flush=True)
    return out


if __name__ == "__main__":
    try:
        main()
    except (common.Error, OSError, KeyError, ValueError) as e:
        print(f"DEPS UPGRADE failed: {e}", file=sys.stderr)
        sys.exit(2)
