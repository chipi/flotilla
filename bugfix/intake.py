"""bugfix/intake: the bugfix fleet's monitor (RFC-0010 §1). Code only, no model, nothing outward.

Every run reads the open issues of each repo that carry one of the intake labels and reports one
code_bug detection per issue, target repo:<owner>/<repo>/issues/<n>; flotillad opens a case for a new
one and absorbs a known one into its case. An issue no longer reported (closed, or its label removed)
has cleared. The issue's number, title, body (cut to fit) and labels go in the detection's data for the
acting routine; the body is the reporter's text, untrusted evidence.

A failed read fails the run: a run that could not see the issues must not report none (that would
clear every case).

  python3 bugfix/intake.py --repo chipi/orrery --label bug --label triage-fleet/routed:bugfix
Environment: FLOTILLA_PLAN_FILE (the worker's), GITHUB_API (default https://api.github.com),
GITHUB_TOKEN_FILE (optional: a read token, for the rate limit; public repos need none).
"""
import argparse
import json
import os
import re
import sys
import urllib.parse
import urllib.request

MAX_DETECTIONS = 50  # flotillad refuses a report with more
MAX_DATA = 7500      # flotillad keeps detection data up to 8192 bytes; room for the JSON around it
MAX_REPORT = 64 * 1024  # flotillad refuses a whole report over this (internal/envelope MaxBytes)
REPORT_ROOM = 2048      # the envelope and each detection's kind, target, detail and title, with margin per detection
MIN_DATA = 600          # below this a detection's data is only its metadata: the body is gone entirely
# An issue triage filed from a dev environment's error is about a branch's unmerged code, not main's: there is
# nothing on main to fix (bugfix phase A's #651, #659). Triage no longer files them; the ones it filed before
# carry `bug`, and their body names the signal as fp: `glitchtip:<SHORT-ID>` (signal-fleet/mvp/actions.py), a
# dev project's short id being <PROJECT>-DEV-<n>. Such an issue is never a case, whatever labels it carries.
DEV_FP = re.compile(r"fp: `glitchtip:[A-Za-z0-9-]*-DEV-[A-Za-z0-9]+`", re.IGNORECASE)


def is_dev(it):
    return bool(DEV_FP.search(it.get("body") or ""))


class IntakeError(Exception):
    pass


def get(url, token=None, timeout=20):
    req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json", "User-Agent": "flotilla-bugfix-intake"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r)
    except Exception as e:  # any failure fails the run (see the docstring)
        raise IntakeError(f"GET {url}: {e}") from e


def open_issues(api, repo, labels, token=None):
    """The open issues (not pull requests) of repo carrying any of labels and not from a dev environment, by number."""
    found = {}
    for label in labels:
        q = urllib.parse.urlencode({"state": "open", "labels": label, "per_page": 100})
        items = get(f"{api}/repos/{repo}/issues?{q}", token)
        if not isinstance(items, list):
            raise IntakeError(f"{repo} label {label}: not a list of issues")
        for it in items:
            if "pull_request" not in it and not is_dev(it):
                found[it["number"]] = it
    return found


def data_for(repo, it, limit=MAX_DATA):
    """The detection's data: the issue, with the body cut so the whole stays under `limit` bytes."""
    d = {"issue": {"repo": repo, "number": it["number"], "title": it.get("title") or "", "url": it.get("html_url") or "",
                   "labels": sorted(lb.get("name", "") for lb in it.get("labels") or []), "updated_at": it.get("updated_at") or "",
                   "body": it.get("body") or "", "body_cut": False}}
    body = d["issue"]["body"]
    while len(json.dumps(d).encode()) > limit and body:
        body = body[: max(0, len(body) - max(200, len(body) // 10))]
        d["issue"]["body"], d["issue"]["body_cut"] = body, True
    return d


def detections(repo, issues, kind="code_bug", limit=MAX_DATA):
    out = []
    for n in sorted(issues, reverse=True):
        it = issues[n]
        name = repo.split("/", 1)[1]
        out.append({"kind": kind, "target": f"repo:{repo}/issues/{n}",
                    "detail": f"{repo}#{n} is open with an intake label",
                    "title": f"{name} #{n}: {it.get('title') or ''}", "data": data_for(repo, it, limit)})
    return out


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", action="append", required=True)
    ap.add_argument("--label", action="append", required=True)
    # the case kind routes the case to its fix routine: code_bug for orrery's vitest profile (fix.yaml),
    # code_bug_python for podcast_scraper's pytest profile (fix-podcast.yaml)
    ap.add_argument("--kind", default="code_bug")
    a = ap.parse_args(argv)
    api = os.environ.get("GITHUB_API", "https://api.github.com").rstrip("/")
    token = None
    if os.environ.get("GITHUB_TOKEN_FILE"):
        with open(os.environ["GITHUB_TOKEN_FILE"]) as f:
            token = f.read().strip()
    per_repo = []
    for repo in a.repo:
        issues = open_issues(api, repo, a.label, token)
        per_repo.append((repo, issues))
        print(f"BUGFIX INTAKE {repo}: {len(issues)} open issues with {', '.join(a.label)}")
    total = sum(len(i) for _, i in per_repo)
    if total > MAX_DETECTIONS:
        # leaving some out would clear their cases: fail instead, and say why
        raise IntakeError(f"{total} open issues, more than the {MAX_DETECTIONS} a run may report: narrow the labels or the repos")
    # the whole report must fit flotillad's limit: each detection gets an equal share of it, its body cut to fit
    limit = min(MAX_DATA, (MAX_REPORT - REPORT_ROOM) // max(1, total) - 400)
    while True:
        found = [d for repo, issues in per_repo for d in detections(repo, issues, a.kind, limit)]
        report = json.dumps({"outcome": "detections", "detections": found})
        if len(report.encode()) <= MAX_REPORT:
            break
        if limit <= MIN_DATA:
            # never send a report flotillad refuses: that clears nothing but opens nothing either, silently
            raise IntakeError(f"{total} detections do not fit flotillad's {MAX_REPORT}-byte report even with bodies cut")
        limit = max(MIN_DATA, int(limit * 0.85))
    with open(os.environ["FLOTILLA_PLAN_FILE"], "w") as f:
        f.write(report)
    print(f"BUGFIX INTAKE detections={len(found)}")


if __name__ == "__main__":
    try:
        main()
    except (IntakeError, KeyError, OSError) as e:
        print(f"BUGFIX INTAKE failed: {e}", file=sys.stderr)
        sys.exit(2)
