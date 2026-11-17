"""What the deps routines share: the repos file, JSON over HTTP, pinned Python requirements, PyPI and
OSV.dev. Python stdlib only.

A repo in the fleet folder's repos file (the spec names it: --repos <path>):
  {"repo": "owner/name", "manifest": "requirements.txt", "outdated": false}
`outdated: true` also reports pins behind the latest release; vulnerable pins are always reported.
GITHUB_TOKEN, when set, is sent to GitHub's API (a private repo, or a higher rate limit).
"""
import base64
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

UA = "flotilla-deps/1 (+https://github.com/chipi/flotilla)"
KIND = "dependency_update"
PIN = re.compile(r"^(?P<name>[A-Za-z0-9][A-Za-z0-9._-]*)(?P<extras>\[[^\]]*\])?==(?P<version>[A-Za-z0-9.+!_-]+)\s*(?P<rest>(?:;[^#]*)?(?:#.*)?)$")
# owner/name; neither part starts with a dot, so ".." and "../x" are not repos
REPO = re.compile(r"^[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$")


class Error(Exception):
    """A source that cannot be read: the run fails, so no case is made from a missing answer."""


def http_json(url, body=None, token=None, timeout=30):
    headers = {"User-Agent": UA, "Accept": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    try:
        with urllib.request.urlopen(urllib.request.Request(url, data=data, headers=headers), timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        raise Error(f"{url}: HTTP {e.code}") from e
    except (urllib.error.URLError, OSError, ValueError) as e:
        raise Error(f"{url}: {getattr(e, 'reason', e)}") from e


def arg(argv, name):
    if name in argv and argv.index(name) + 1 < len(argv):
        return argv[argv.index(name) + 1]
    return ""


def repos(argv=None):
    path = arg(sys.argv[1:] if argv is None else argv, "--repos")
    if not path:
        raise Error("no repos file: the routine's spec passes --repos <path>")
    with open(path) as f:
        got = json.load(f)
    for r in got:
        if not REPO.match(r.get("repo", "")) or not r.get("manifest"):
            raise Error(f"{path}: each repo needs repo (owner/name) and manifest: {r}")
    return got


def norm(name):
    """PEP 503's normalized name, for comparing and for PyPI's URL."""
    return re.sub(r"[-_.]+", "-", name).lower()


def pins(text):
    """The exactly pinned requirements: [(line number, name, version)]. Anything else is not ours to move."""
    out = []
    for n, line in enumerate(text.splitlines()):
        m = PIN.match(line.strip())
        if m:
            out.append((n, m["name"], m["version"]))
    return out


class GitHub:
    def __init__(self, http=http_json, token=None):
        self.http, self.token = http, token if token is not None else os.environ.get("GITHUB_TOKEN", "")

    def head(self, repo):
        """The default branch's head commit."""
        return self.http(f"https://api.github.com/repos/{repo}/commits/HEAD", token=self.token)["sha"]

    def file(self, repo, path, ref):
        got = self.http(f"https://api.github.com/repos/{repo}/contents/{urllib.parse.quote(path)}?ref={ref}", token=self.token)
        if got.get("encoding") != "base64":
            raise Error(f"{repo}/{path}: not a file")
        return base64.b64decode(got["content"]).decode("utf-8")


def latest(name, http=http_json):
    """PyPI's latest release that is not yanked, and the set of every released version."""
    got = http(f"https://pypi.org/pypi/{norm(name)}/json")
    releases = {v for v, files in (got.get("releases") or {}).items() if files and not all(f.get("yanked") for f in files)}
    return got["info"]["version"], releases


def vulns(packages, http=http_json):
    """OSV.dev's known vulnerabilities for each (name, version): {(name, version): [ids]}."""
    if not packages:
        return {}
    q = {"queries": [{"package": {"name": n, "ecosystem": "PyPI"}, "version": v} for n, v in packages]}
    got = http("https://api.osv.dev/v1/querybatch", body=q)
    res = got.get("results") or []
    if len(res) != len(packages):
        raise Error(f"OSV answered {len(res)} results for {len(packages)} queries")
    return {p: sorted(v["id"] for v in (r.get("vulns") or [])) for p, r in zip(packages, res)}


def report(obj):
    plan = os.environ.get("FLOTILLA_PLAN_FILE")
    if plan:
        with open(plan, "w") as f:
            json.dump(obj, f)
