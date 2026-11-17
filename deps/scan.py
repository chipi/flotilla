"""deps/scan: the dependency monitor. For each repo in the fleet's repos file, reads the pinned Python
requirements on the default branch, asks OSV.dev which pins have known vulnerabilities and PyPI which
are behind the latest release, and reports one dependency_update detection per pin to move: every
vulnerable pin, and the outdated ones when the repo asks (`outdated: true`). flotillad opens the case;
this run only reads and reports. Code only, no model.

  python3 deps/scan.py --repos fleets/deps/repos.json
"""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402


def scan(repo, gh, http=common.http_json):
    """The detections for one repo, and its summary line."""
    name, manifest = repo["repo"], repo["manifest"]
    sha = gh.head(name)
    found = common.pins(gh.file(name, manifest, sha))
    vul = common.vulns([(n, v) for _, n, v in found], http)
    out = []
    for _, n, v in found:
        ids = vul.get((n, v), [])
        new, released = common.latest(n, http)
        behind = new != v and new in released
        if not ids and not (repo.get("outdated") and behind):
            continue
        why = f"{len(ids)} known vulnerabilit{'y' if len(ids) == 1 else 'ies'} ({', '.join(ids[:5])})" if ids else "behind the latest release"
        out.append({"kind": common.KIND, "target": f"repo:{name}/deps/{common.norm(n)}",
                    "title": f"{name}: {n} {v} -> {new}: {why}",
                    "detail": f"{manifest} pins {n}=={v} at {sha[:7]}; PyPI's latest is {new}; OSV: {', '.join(ids) or 'none'}",
                    "data": {"repo": name, "manifest": manifest, "name": n, "current": v, "latest": new, "vulns": ids,
                             "measure": {"name": "known_vulnerabilities", "value": len(ids), "threshold": 0}}})
    return out, f"DEPS SCAN {name} pins={len(found)} findings={len(out)} at {sha[:7]}"


def main(argv=None, gh=None, http=common.http_json):
    gh = gh or common.GitHub()
    found, lines = [], []
    for r in common.repos(argv):
        t = time.time()
        d, line = scan(r, gh, http)
        found += d
        lines.append(f"{line} {time.time() - t:.0f}s")
    common.report({"outcome": "detections", "detections": found})
    for ln in lines:
        print(ln, flush=True)
    for d in found:
        print(f"DEPS SCAN finding {d['target']}: {d['title']}", flush=True)
    return found


if __name__ == "__main__":
    try:
        main()
    except (common.Error, OSError, KeyError, ValueError) as e:
        print(f"DEPS SCAN failed: {e}", file=sys.stderr)
        sys.exit(2)
