"""python3 bugfix/test_open_pr.py: bugfix/open-pr and bugfix/pr-check (RFC-0010 §3) against a fake GitHub
API and a local git remote. Needs git, node and openssl (the test App key is made here)."""
import base64
import http.server
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import open_pr  # noqa: E402
import pr_check  # noqa: E402

CASE = {"id": "case-20261115-abc", "kind": "code_bug", "target": "repo:chipi/orrery/issues/12"}


def git(*a, cwd):
    return subprocess.run(["git", *a], cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()


class FakeGitHub:
    """The calls open-pr and pr-check make: the installation token, the repo, and pulls (create, list)."""

    def __init__(self, refuse_pr=False):
        self.calls, self.pulls, self.refuse_pr = [], [], refuse_pr
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def reply(self, code, obj):
                b = json.dumps(obj).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b)

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
                auth = self.headers.get("Authorization", "")
                outer.calls.append(("POST", self.path, auth, body))
                if self.path == "/app/installations/77/access_tokens":
                    jwt = auth.split(" ", 1)[1]
                    claims = json.loads(base64.urlsafe_b64decode(jwt.split(".")[1] + "=="))
                    if len(jwt.split(".")) != 3 or claims.get("iss") != "4242":
                        return self.reply(401, {})
                    return self.reply(201, {"token": "fake-tok"})
                if self.path == "/repos/chipi/orrery/pulls":
                    if outer.refuse_pr or auth != "Bearer fake-tok":
                        return self.reply(422, {"message": "no"})
                    pr = {"number": 7, "html_url": "https://github.com/chipi/orrery/pull/7", "head": body["head"], "base": body["base"],
                          "draft": body.get("draft"), "state": "open", "merged_at": None, "title": body["title"]}
                    outer.pulls.append(pr)
                    return self.reply(201, pr)
                self.reply(404, {})

            def do_GET(self):
                u = urllib.parse.urlparse(self.path)
                outer.calls.append(("GET", self.path, self.headers.get("Authorization", ""), None))
                if u.path == "/repos/chipi/orrery":
                    return self.reply(200, {"default_branch": "main"})
                if u.path == "/repos/chipi/orrery/pulls":
                    head = urllib.parse.parse_qs(u.query).get("head", [""])[0]
                    return self.reply(200, [p for p in outer.pulls if "chipi:" + p["head"] == head])
                self.reply(404, {})

            def log_message(self, *a):
                pass

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"


class Fixture:
    def __init__(self, refuse_pr=False):
        self.d = tempfile.mkdtemp()
        src = os.path.join(self.d, "src")
        os.makedirs(src)
        with open(os.path.join(src, "a.js"), "w") as f:
            f.write("module.exports = () => 1;\n")
        git("init", "-q", "-b", "main", cwd=src)
        git("-c", "user.email=t@t", "-c", "user.name=t", "add", "-A", cwd=src)
        git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base", cwd=src)
        self.remote = os.path.join(self.d, "remotes", "chipi", "orrery")
        os.makedirs(os.path.dirname(self.remote))
        git("clone", "-q", "--bare", src, self.remote, cwd=self.d)
        git("config", "uploadpack.allowReachableSHA1InWant", "true", cwd=self.remote)  # GitHub serves a commit by its sha
        git("config", "uploadpack.allowfilter", "true", cwd=self.remote)               # and --filter
        self.base = git("rev-parse", "main", cwd=self.remote)
        self.key = os.path.join(self.d, "app.pem")
        subprocess.run(["openssl", "genrsa", "-out", self.key, "2048"], check=True, capture_output=True)
        self.gh = FakeGitHub(refuse_pr)
        self.env = dict(os.environ, GITHUB_API=self.gh.url, GIT_REMOTE_BASE="file://" + os.path.join(self.d, "remotes"),
                        GITHUB_APP_ID="4242", GITHUB_APP_INSTALLATION_ID="77", GITHUB_APP_KEY_FILE=self.key,
                        GIT_ALLOW_PROTOCOL="file", FLOTILLA_PLAN_FILE=os.path.join(self.d, "plan.json"), BUGFIX_SCRATCH=self.d)
        self.diff = ("diff --git a/a.js b/a.js\n--- a/a.js\n+++ b/a.js\n@@ -1 +1 @@\n-module.exports = () => 1;\n+module.exports = () => 2;\n"
                     "diff --git a/a.test.js b/a.test.js\nnew file mode 100644\n--- /dev/null\n+++ b/a.test.js\n@@ -0,0 +1 @@\n+expect value 2\n")

    def plan(self, **over):
        prm = {"repo": "chipi/orrery", "base": self.base, "title": "fix #12: value is 1", "body": "why <b>&</b>", "diff": self.diff}
        prm.update(over)
        return {"id": 3, "action": "github.open_pr", "params": prm, "plan_hash": open_pr.plan_hash(CASE["id"], "github.open_pr", prm)}

    def run(self, plan):
        env = dict(self.env, FLOTILLA_CASE=json.dumps(CASE), FLOTILLA_PLAN=json.dumps(plan))
        return open_pr.main(env)

    def close(self):
        self.gh.srv.shutdown()
        shutil.rmtree(self.d, ignore_errors=True)


class OpenPR(unittest.TestCase):
    def fixture(self, **kw):
        f = Fixture(**kw)
        self.addCleanup(f.close)
        return f

    def test_the_approved_diff_is_what_is_pushed_as_a_draft(self):
        f = self.fixture()
        out = f.run(f.plan())
        self.assertEqual((out["outcome"], out["ref"]), ("executed", "chipi/orrery#7"), out)
        branch = "flotilla/" + CASE["id"]
        pushed = git("diff", f.base, branch, cwd=f.remote)
        self.assertIn("+module.exports = () => 2;", pushed)
        self.assertIn("+expect value 2", pushed)
        self.assertEqual(git("rev-parse", branch + "^", cwd=f.remote), f.base, "one commit on the base")
        pr = f.gh.pulls[0]
        self.assertEqual((pr["head"], pr["base"], pr["draft"], pr["title"]), (branch, "main", True, "fix #12: value is 1"))
        self.assertFalse([c for c in f.gh.calls if "merge" in c[1]], "nothing merges")
        self.assertNotIn("fake-tok", git("config", "--list", cwd=f.remote))
        self.assertEqual(os.listdir(f.d).count("plan.json"), 1)
        self.assertEqual(sorted(x for x in os.listdir(f.d) if x.startswith("tmp")), [], "its work directory is deleted")

    def test_a_changed_plan_is_refused_by_its_hash(self):
        f = self.fixture()
        p = f.plan()
        p["params"]["diff"] = p["params"]["diff"].replace("() => 2", "() => 3")
        out = f.run(p)
        self.assertEqual(out["outcome"], "escalate")
        self.assertIn("not the approved one", out["detail"])
        self.assertEqual(f.gh.calls, [], "nothing was asked of GitHub")

    def test_a_missing_base_or_a_diff_that_does_not_apply_escalates(self):
        f = self.fixture()
        out = f.run(f.plan(base="0123456789abcdef0123456789abcdef01234567"))
        self.assertEqual(out["outcome"], "escalate")
        self.assertIn("is gone", out["detail"])
        g = self.fixture()
        out = g.run(g.plan(diff=g.diff.replace("-module.exports = () => 1;", "-module.exports = () => 9;")))
        self.assertIn("does not apply", out["detail"])
        self.assertEqual(git("branch", "--list", cwd=g.remote), "* main", "nothing pushed")

    def test_an_existing_branch_and_a_refused_pr(self):
        f = self.fixture()
        self.assertEqual(f.run(f.plan())["outcome"], "executed")
        again = f.run(f.plan())
        self.assertIn("already exists", again["detail"])
        g = self.fixture(refuse_pr=True)
        out = g.run(g.plan())
        self.assertEqual(out["outcome"], "escalate")
        self.assertIn("is pushed, but GitHub refused the PR", out["detail"])

    def test_no_app_configured(self):
        f = self.fixture()
        f.env.pop("GITHUB_APP_ID")
        out = f.run(f.plan())
        self.assertIn("no GitHub App configured", out["detail"])


class PRCheck(unittest.TestCase):
    def test_open_merged_closed(self):
        f = Fixture()
        self.addCleanup(f.close)
        env = dict(f.env, FLOTILLA_CASE=json.dumps(CASE), FLOTILLA_CHECK=json.dumps({"verify": "pr_resolved", "params": {"repo": "chipi/orrery"}}))
        self.assertEqual(pr_check.main(env)["outcome"], "not_yet")
        f.run(f.plan())
        out = pr_check.main(env)
        self.assertEqual(out["outcome"], "not_yet")
        self.assertIn("open (draft)", out["detail"])
        f.gh.pulls[0].update(merged_at="2026-11-16T09:00:00Z", merge_commit_sha="f" * 40, state="closed")
        self.assertEqual(pr_check.main(env)["outcome"], "verified")
        f.gh.pulls[0].update(merged_at=None, closed_at="2026-11-16T09:00:00Z")
        out = pr_check.main(env)
        self.assertEqual(out["outcome"], "failed")
        self.assertIn("closed without merging", out["detail"])


class Hash(unittest.TestCase):
    def test_the_go_vector(self):
        """internal/policy/hash_vector_test.go asserts the same value for policy.Plan.Hash."""
        p = {"repo": "chipi/orrery", "base": "abc1234", "title": "fix #12: a < b && c > d — été",
             "body": "line 1\nline \"2\"\t\\   \U0001F680", "diff": "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-<a href=\"x\">&amp;</a>\n+ok\n"}
        self.assertEqual(open_pr.plan_hash("case-20261115-abc", "github.open_pr", p),
                         "9a41a15da7f89b53fda5fe9ac8777d8f798af5011944cd4eb4c6e8295d63b7f1")


if __name__ == "__main__":
    unittest.main()
