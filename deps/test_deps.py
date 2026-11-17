"""deps/scan and deps/upgrade against recorded answers from GitHub, PyPI and OSV.dev: what is reported,
what is planned, and that the planned diff applies and changes one line only. No request leaves the
process.

  python3 deps/test_deps.py
"""
import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
import scan  # noqa: E402
import upgrade  # noqa: E402

SHA = "a" * 40
REQ = """# the app's runtime
requests==2.31.0
flask[async]==3.0.0  # web
urllib3>=1.26
idna==3.7 ; python_version >= "3.9"
"""


class FakeGH:
    def __init__(self, text=REQ, sha=SHA):
        self.text, self.sha = text, sha

    def head(self, repo):
        return self.sha

    def file(self, repo, path, ref):
        assert ref == self.sha, "a file is read at the head it was planned on"
        return self.text


def fake_http(latest=None, vulns=None, yanked=()):
    latest = latest or {"requests": "2.32.3", "flask": "3.0.0", "idna": "3.10"}
    vulns = vulns or {("requests", "2.31.0"): ["GHSA-9wx4-h78v-vm56"]}
    asked = []

    def http(url, body=None, token=None, timeout=30):
        asked.append(url)
        if url.startswith("https://pypi.org/pypi/"):
            name = url.split("/")[4]
            v = latest[name]
            return {"info": {"version": v}, "releases": {v: [{"yanked": v in yanked}], "0.1": [{"yanked": False}]}}
        if url == "https://api.osv.dev/v1/querybatch":
            return {"results": [{"vulns": [{"id": i} for i in vulns.get((q["package"]["name"], q["version"]), [])]}
                                for q in body["queries"]]}
        raise AssertionError(url)
    http.asked = asked
    return http


REPO = {"repo": "example/shop", "manifest": "requirements.txt"}


class Pins(unittest.TestCase):
    def test_only_exact_pins_are_ours(self):
        self.assertEqual([(n, v) for _, n, v in common.pins(REQ)],
                         [("requests", "2.31.0"), ("flask", "3.0.0"), ("idna", "3.7")])

    def test_the_spec_names_the_repos_file(self):
        with self.assertRaises(common.Error):
            common.repos([])
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump([{"repo": "../x", "manifest": "r.txt"}], f)
        self.addCleanup(os.unlink, f.name)
        with self.assertRaises(common.Error):
            common.repos(["--repos", f.name])


class Scan(unittest.TestCase):
    def test_a_vulnerable_pin_is_reported_an_outdated_one_only_when_asked(self):
        found, line = scan.scan(REPO, FakeGH(), fake_http())
        self.assertEqual([d["target"] for d in found], ["repo:example/shop/deps/requests"])
        d = found[0]
        self.assertEqual((d["kind"], d["data"]["current"], d["data"]["latest"], d["data"]["vulns"]),
                         ("dependency_update", "2.31.0", "2.32.3", ["GHSA-9wx4-h78v-vm56"]))
        self.assertIn("pins=3 findings=1", line)
        found, _ = scan.scan(dict(REPO, outdated=True), FakeGH(), fake_http())
        self.assertEqual(sorted(d["target"] for d in found),
                         ["repo:example/shop/deps/idna", "repo:example/shop/deps/requests"])

    def test_nothing_to_move_is_no_detection(self):
        found, _ = scan.scan(REPO, FakeGH(), fake_http(vulns={("x", "0"): ["Y"]}))
        self.assertEqual(found, [])


class Upgrade(unittest.TestCase):
    def case(self, **over):
        d = {"repo": "example/shop", "manifest": "requirements.txt", "name": "requests", "current": "2.31.0"}
        d.update(over)
        return {"id": "case-1", "kind": "dependency_update", "data": d}

    def test_plans_a_one_line_pr_whose_diff_applies(self):
        out = upgrade.decide(self.case(), FakeGH(), fake_http())
        self.assertEqual((out["outcome"], out["action"]), ("plan", "github.open_pr"), out)
        p = out["params"]
        self.assertEqual((p["repo"], p["base"]), ("example/shop", SHA))
        self.assertIn("requests: 2.31.0 -> 2.32.3 (fixes GHSA-9wx4-h78v-vm56)", p["title"])
        d = tempfile.mkdtemp()
        with open(os.path.join(d, "requirements.txt"), "w") as f:
            f.write(REQ)
        subprocess.run(["git", "init", "-q", d], check=True)
        subprocess.run(["git", "-C", d, "apply", "-"], input=p["diff"], text=True, check=True)
        with open(os.path.join(d, "requirements.txt")) as f:
            self.assertEqual(f.read(), REQ.replace("requests==2.31.0", "requests==2.32.3"))

    def test_extras_and_comments_stay(self):
        out = upgrade.decide(self.case(name="flask", current="3.0.0"), FakeGH(), fake_http(latest={"flask": "3.1.0"}))
        self.assertIn("+flask[async]==3.1.0  # web", out["params"]["diff"])

    def test_a_pin_that_moved_resolved_itself(self):
        gh = FakeGH(REQ.replace("requests==2.31.0", "requests==2.32.3"))
        self.assertEqual(upgrade.decide(self.case(), gh, fake_http())["outcome"], "resolved_itself")
        gh = FakeGH(REQ.replace("requests==2.31.0\n", ""))
        self.assertEqual(upgrade.decide(self.case(), gh, fake_http())["outcome"], "resolved_itself")

    def test_refuses_what_it_cannot_trust(self):
        self.assertEqual(upgrade.decide(self.case(), FakeGH(), fake_http(yanked=("2.32.3",)))["outcome"], "escalate")
        self.assertEqual(upgrade.decide(self.case(repo="../etc"), FakeGH(), fake_http())["outcome"], "escalate")
        self.assertEqual(upgrade.decide({"kind": "dependency_update", "data": {}}, FakeGH(), fake_http())["outcome"], "escalate")


if __name__ == "__main__":
    unittest.main()
