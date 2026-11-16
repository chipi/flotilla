"""bugfix/open-pr: the executing routine for github.open_pr (RFC-0010 §3), on worker-bugfix-act.

flotillad starts it once, after a person approved the plan, with FLOTILLA_CASE and FLOTILLA_PLAN (the
approved plan and its hash). It runs no repo code and holds no model key; it holds the GitHub App's key.

  1. the plan is whole: its hash, computed here as flotillad computes it, must be the plan_hash flotillad
     sent (a plan changed on the way is refused). That a person approved this hash is flotillad's check,
     at the approval (ApprovePlan binds the approval to it); this one only sees that it arrived intact
  2. a token for this execution only: a JWT from the App's key (github_app.js), exchanged for an
     installation token, which expires within the hour; it is never in argv or a URL, only in a mode-600
     file in the run's own temporary directory, read by git's credential helper and deleted with it
  3. the base commit is fetched (no blobs, no checkout) and the diff applies to it exactly (git apply
     --cached --check), else it escalates
  4. one commit with the diff, pushed to flotilla/<case id> (new: a branch that exists escalates)
  5. a draft PR from that branch to the repo's default branch, with the plan's title and body
  {"outcome": "executed", "detail": "draft PR <url>", "ref": "<repo>#<n>"} or {"outcome": "escalate", ...}

The operator merges, always: nothing here merges, approves or marks a PR ready.

Environment: FLOTILLA_CASE, FLOTILLA_PLAN, FLOTILLA_PLAN_FILE; GITHUB_APP_ID, GITHUB_APP_INSTALLATION_ID,
GITHUB_APP_KEY_FILE (default /run/flotilla/github-app.pem); GITHUB_API (default https://api.github.com),
GIT_REMOTE_BASE (default https://github.com); BUGFIX_SCRATCH (its own scratch).
"""
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ACTION = "github.open_pr"
SHA_RE = re.compile(r"^[0-9a-f]{7,40}$")
REPO_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")


class Escalate(Exception):
    pass


def go_json(v):
    """json.dumps as Go's encoding/json writes it: no spaces, UTF-8 kept, and <, >, &, U+2028, U+2029
    escaped (Go's HTML-safe default), so the hash below is the one policy.Plan.Hash computes."""
    s = json.dumps(v, separators=(",", ":"), ensure_ascii=False)
    for a, b in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), (" ", "\\u2028"), (" ", "\\u2029")):
        s = s.replace(a, b)
    return s


def plan_hash(case_id, action, params):
    """policy.Plan.Hash: sha256 of the JSON of [case id, action, [[key, value] sorted by key]]."""
    pairs = [[k, params[k]] for k in sorted(params)]
    return hashlib.sha256(go_json([case_id, action, pairs]).encode("utf-8")).hexdigest()


def api(method, url, token, body=None, auth="Bearer"):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method, headers={
        "Accept": "application/vnd.github+json", "User-Agent": "flotilla-bugfix", "X-GitHub-Api-Version": "2022-11-28"})
    req.add_header("Authorization", f"{auth} {token}")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        with e:
            try:
                return e.code, json.loads(e.read() or b"null")
            except ValueError:
                return e.code, None


# What a pusher's token may do (SR-0001): write the branch and the draft PR, nothing else. The App itself may
# hold more (the operator's App writes issues too and is installed on all 18 repos, 2026-11-16), so every
# token is narrowed to the plan's one repository and these permissions when it is made.
TOKEN_PERMISSIONS = {"contents": "write", "pull_requests": "write", "metadata": "read"}


def installation_token(env, repo):
    app, inst = env.get("GITHUB_APP_ID", ""), env.get("GITHUB_APP_INSTALLATION_ID", "")
    key = env.get("GITHUB_APP_KEY_FILE", "/run/flotilla/github-app.pem")
    if not app.isdigit() or not inst.isdigit() or not os.path.exists(key):
        raise Escalate("no GitHub App configured on this worker (GITHUB_APP_ID, GITHUB_APP_INSTALLATION_ID, its key file)")
    p = subprocess.run(["node", os.path.join(HERE, "github_app.js"), key, app], capture_output=True, text=True, timeout=30)
    if p.returncode != 0 or not p.stdout:
        raise Escalate(f"the App's JWT could not be made: {p.stderr.strip()[:200]}")
    base = env.get("GITHUB_API", "https://api.github.com").rstrip("/")
    st, out = api("POST", f"{base}/app/installations/{inst}/access_tokens", p.stdout.strip(),
                  body={"repositories": [repo.split("/", 1)[1]], "permissions": TOKEN_PERMISSIONS})
    if st != 201 or not isinstance(out, dict) or not out.get("token"):
        raise Escalate(f"GitHub refused an installation token ({st})")
    return out["token"]


def git(args, cwd, env, check=True):
    p = subprocess.run(["git", *args], cwd=cwd, env=env, capture_output=True, text=True, timeout=600)
    if check and p.returncode != 0:
        raise Escalate(f"git {args[0]}: {(p.stderr or p.stdout).strip()[:300]}")
    return p


def execute(case, plan, env):
    if plan.get("action") != ACTION:
        raise Escalate(f"{plan.get('action')} is not {ACTION}")
    prm = plan.get("params") or {}
    if plan_hash(case["id"], plan["action"], prm) != plan.get("plan_hash"):
        raise Escalate("the plan is not the approved one: its hash does not match")
    repo, base, diff = prm.get("repo", ""), prm.get("base", ""), prm.get("diff", "")
    if not REPO_RE.match(repo) or not SHA_RE.match(base) or not diff:
        raise Escalate("the plan's repo, base or diff is malformed")
    branch = "flotilla/" + case["id"]
    token = installation_token(env, repo)
    gapi = env.get("GITHUB_API", "https://api.github.com").rstrip("/")
    if len(base) < 40:
        # git fetches a commit by its full id only: a process change's base is the image's sha-<7>
        st, c = api("GET", f"{gapi}/repos/{repo}/commits/{base}", token)
        if st != 200 or not isinstance(c, dict) or not SHA_RE.match(c.get("sha") or "") or not c["sha"].startswith(base):
            raise Escalate(f"the base commit {base} is gone from {repo} ({st})")
        base = c["sha"]
    work = tempfile.mkdtemp(dir=env.get("BUGFIX_SCRATCH") or None)
    try:
        helper = os.path.join(work, "cred.sh")
        tokfile = os.path.join(work, "tok")
        with open(tokfile, "w") as f:
            f.write(token)
        os.chmod(tokfile, 0o600)
        with open(helper, "w") as f:  # git asks this for the password: the token stays out of argv and the URL
            f.write(f"#!/bin/sh\necho username=x-access-token\necho password=$(cat {tokfile})\n")
        os.chmod(helper, 0o700)
        genv = dict(env, GIT_TERMINAL_PROMPT="0", GIT_AUTHOR_NAME="flotilla-bugfix", GIT_AUTHOR_EMAIL="flotilla-bugfix@users.noreply.github.com",
                    GIT_COMMITTER_NAME="flotilla-bugfix", GIT_COMMITTER_EMAIL="flotilla-bugfix@users.noreply.github.com")
        remote = env.get("GIT_REMOTE_BASE", "https://github.com").rstrip("/") + "/" + repo
        wt = os.path.join(work, "wt")
        cfg = ["-c", f"credential.helper=!{helper}", "-c", "core.hooksPath=/dev/null"]
        git(["init", "-q", wt], work, genv)
        git(cfg + ["remote", "add", "origin", remote], wt, genv)
        # the base's commit and trees only: blobs come on demand for the files the diff touches (orrery's
        # tree is 6.8 GB); nothing is checked out, the diff is applied to the index
        if git(cfg + ["fetch", "-q", "--depth", "1", "--filter=blob:none", "origin", base], wt, genv, check=False).returncode != 0:
            raise Escalate(f"the base commit {base[:12]} is gone from {repo}")
        git(["read-tree", "FETCH_HEAD"], wt, genv)
        git(["update-ref", "HEAD", "FETCH_HEAD"], wt, genv)
        patch = os.path.join(work, "plan.diff")
        with open(patch, "w") as f:
            f.write(diff if diff.endswith("\n") else diff + "\n")
        if git(cfg + ["apply", "--check", "--cached", patch], wt, genv, check=False).returncode != 0:
            raise Escalate(f"the approved diff does not apply to {base[:12]}")
        git(cfg + ["apply", "--cached", patch], wt, genv)
        git(cfg + ["commit", "-q", "-m", prm.get("title") or "flotilla change", "-m", f"flotilla case {case['id']}, plan {plan.get('plan_hash', '')[:12]}"], wt, genv)
        if git(cfg + ["ls-remote", "--exit-code", "--heads", "origin", branch], wt, genv, check=False).returncode == 0:
            raise Escalate(f"the branch {branch} already exists on {repo}: not overwritten")
        git(cfg + ["push", "-q", "origin", f"HEAD:refs/heads/{branch}"], wt, genv)
        st, info = api("GET", f"{gapi}/repos/{repo}", token)
        default = (info or {}).get("default_branch") or "main"
        st, pr = api("POST", f"{gapi}/repos/{repo}/pulls", token, {"title": prm.get("title", ""), "body": prm.get("body", ""),
                                                                   "head": branch, "base": default, "draft": True})
        if st != 201 or not isinstance(pr, dict) or not pr.get("number"):
            raise Escalate(f"the branch {branch} is pushed, but GitHub refused the PR ({st}): open it by hand or delete the branch")
        return {"outcome": "executed", "detail": f"draft PR {pr.get('html_url', '')} from {branch}", "ref": f"{repo}#{pr['number']}"}
    finally:
        shutil.rmtree(work, ignore_errors=True)


def main(environ=None):
    env = dict(os.environ if environ is None else environ)
    case = json.loads(env["FLOTILLA_CASE"])
    plan = json.loads(env["FLOTILLA_PLAN"])
    try:
        out = execute(case, plan, env)
    except Escalate as e:
        out = {"outcome": "escalate", "detail": str(e)}
    with open(env["FLOTILLA_PLAN_FILE"], "w") as f:
        json.dump(out, f)
    print(f"BUGFIX OPEN-PR case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}")
    return out


if __name__ == "__main__":
    main()
