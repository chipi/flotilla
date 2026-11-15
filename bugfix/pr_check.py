"""bugfix/pr-check: the check routine for github.open_pr (registry check pr_resolved, RFC-0010 §3).

Run by flotillad while the case is in checking, after bugfix/open-pr opened the draft PR from
flotilla/<case id>. It reads that PR, read only (the repos are public; GITHUB_TOKEN_FILE optional):
  merged                     -> {"outcome": "verified", "detail": "merged as <sha> at <time>"}
  closed without merging     -> {"outcome": "failed", "detail": "closed without merging at <time>"}
  open (draft or ready)      -> {"outcome": "not_yet", "detail": "open"}
The check's window (14 days, the registry's) escalates a PR nobody decided on.

Environment: FLOTILLA_CASE, FLOTILLA_CHECK, FLOTILLA_PLAN_FILE; GITHUB_API (default https://api.github.com).
"""
import json
import os
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import open_pr  # noqa: E402


def get(url, token=None):
    req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json", "User-Agent": "flotilla-bugfix"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)


def check(case, chk, env):
    repo = (chk.get("params") or {}).get("repo", "")
    if not open_pr.REPO_RE.match(repo):
        return {"outcome": "failed", "detail": f"the plan names no repo ({repo!r}): nothing to check"}
    branch = "flotilla/" + case["id"]
    token = None
    if env.get("GITHUB_TOKEN_FILE"):
        with open(env["GITHUB_TOKEN_FILE"]) as f:
            token = f.read().strip()
    api = env.get("GITHUB_API", "https://api.github.com").rstrip("/")
    q = urllib.parse.urlencode({"head": f"{repo.split('/')[0]}:{branch}", "state": "all"})
    prs = get(f"{api}/repos/{repo}/pulls?{q}", token)
    if not prs:
        return {"outcome": "not_yet", "detail": f"no PR from {branch} yet"}
    pr = prs[0]
    if pr.get("merged_at"):
        return {"outcome": "verified", "detail": f"{repo}#{pr['number']} merged as {(pr.get('merge_commit_sha') or '')[:12]} at {pr['merged_at']}"}
    if pr.get("state") == "closed":
        return {"outcome": "failed", "detail": f"{repo}#{pr['number']} closed without merging at {pr.get('closed_at')}"}
    return {"outcome": "not_yet", "detail": f"{repo}#{pr['number']} is open{' (draft)' if pr.get('draft') else ''}"}


def main(environ=None):
    env = dict(os.environ if environ is None else environ)
    case = json.loads(env["FLOTILLA_CASE"])
    out = check(case, json.loads(env["FLOTILLA_CHECK"]), env)
    with open(env["FLOTILLA_PLAN_FILE"], "w") as f:
        json.dump(out, f)
    print(f"BUGFIX PR-CHECK case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}")
    return out


if __name__ == "__main__":
    main()
