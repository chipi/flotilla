"""python3 bugfix/test_bugfix.py: the bugfix fleet's routines (RFC-0010)."""
import http.server
import json
import os
import sys
import tempfile
import threading
import unittest
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import intake  # noqa: E402
import pi_config  # noqa: E402

MODELS = {
    "endpoints": {
        "gateway": {"base_url": "http://litellm:4000/v1", "kind": "litellm", "key_env": "OPENROUTER_API_KEY"},
        "gateway-bugfix": {"base_url": "http://litellm:4000/v1", "kind": "litellm", "key_file": "/run/flotilla/model.key"},
    },
    "aliases": {
        "triage-flash": {"endpoint": "gateway", "model": "fleet-triage-flash"},
        "bugfix-flash": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-flash"},
        "bugfix-reviewer": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-reviewer"},
    },
}


class PiConfig(unittest.TestCase):
    def test_only_the_named_aliases_and_the_key_by_name(self):
        """T4: pi sees only the routine's aliases; the key is named (a file to cat), never written."""
        out = pi_config.models_json(MODELS, ["bugfix-flash", "bugfix-reviewer"])
        self.assertEqual(list(out["providers"]), ["flotilla-gateway-bugfix"])
        p = out["providers"]["flotilla-gateway-bugfix"]
        self.assertEqual((p["baseUrl"], p["api"], p["apiKey"]), ("http://litellm:4000/v1", "openai-completions", "!cat /run/flotilla/model.key"))
        self.assertEqual([m["id"] for m in p["models"]], ["fleet-bugfix-flash", "fleet-bugfix-reviewer"])
        self.assertNotIn("fleet-triage-flash", json.dumps(out))
        env = pi_config.models_json(MODELS, ["triage-flash"])["providers"]["flotilla-gateway"]
        self.assertEqual(env["apiKey"], "$OPENROUTER_API_KEY")
        self.assertEqual(pi_config.pick(MODELS, "bugfix-flash"), ("flotilla-gateway-bugfix", "fleet-bugfix-flash"))

    def test_the_upstream_model_s_limits_and_prices(self):
        """pi gets the context window, output limit and prices of the model LiteLLM routes to."""
        meta = {"deepseek/deepseek-v4-flash": {"contextWindow": 1048575, "maxTokens": 4096, "cost": {"input": 0.098, "output": 0.196}}}
        out = pi_config.models_json(MODELS, ["bugfix-flash", "bugfix-reviewer"], meta, {"fleet-bugfix-flash": "deepseek/deepseek-v4-flash"})
        flash, rev = out["providers"]["flotilla-gateway-bugfix"]["models"]
        self.assertEqual((flash["id"], flash["contextWindow"], flash["maxTokens"], flash["cost"]["output"]),
                         ("fleet-bugfix-flash", 1048575, 4096, 0.196))
        self.assertEqual(rev, {"id": "fleet-bugfix-reviewer"}, "no upstream named: pi's defaults")

    def test_the_reply_limit_has_a_floor(self):
        """pi's catalog gives deepseek-v4-flash maxTokens 4096 while the pinned providers allow 131072 and more; flash
        spent all 4096 on reasoning and was cut off before its answer in 5 eval runs of 2026-11-16 (a verdict cut off
        that way ends the case "no verdict"). A floor raises the limit; a catalog's higher limit stays."""
        meta = {"deepseek/deepseek-v4-flash": {"contextWindow": 1048575, "maxTokens": 4096},
                "z-ai/glm-5.2": {"contextWindow": 1048576, "maxTokens": 131072}}
        up = {"fleet-bugfix-flash": "deepseek/deepseek-v4-flash", "fleet-bugfix-reviewer": "z-ai/glm-5.2"}
        flash, rev = pi_config.models_json(MODELS, ["bugfix-flash", "bugfix-reviewer"], meta, up,
                                           min_output=32768)["providers"]["flotilla-gateway-bugfix"]["models"]
        self.assertEqual((flash["maxTokens"], rev["maxTokens"]), (32768, 131072))
        bare = pi_config.models_json(MODELS, ["bugfix-flash"], {}, {}, min_output=32768)
        self.assertEqual(bare["providers"]["flotilla-gateway-bugfix"]["models"][0]["maxTokens"], 32768)

    def test_unknown_alias_and_missing_section(self):
        with self.assertRaises(pi_config.ConfigError):
            pi_config.models_json(MODELS, ["nope"])
        with self.assertRaises(pi_config.ConfigError):
            pi_config.write(tempfile.mkdtemp(), ["bugfix-flash"], environ={})

    def test_written_private(self):
        d = tempfile.mkdtemp()
        pi_config.write(d, ["bugfix-flash"], environ={"FLOTILLA_MODELS": json.dumps(MODELS)}, meta={})
        path = os.path.join(d, "models.json")
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
        with open(path) as f:
            self.assertIn("fleet-bugfix-flash", f.read())


def issue(n, title, labels, body="steps to reproduce", pr=False):
    it = {"number": n, "title": title, "html_url": f"https://github.com/o/r/issues/{n}", "labels": [{"name": x} for x in labels],
          "updated_at": "2026-11-15T10:00:00Z", "body": body}
    if pr:
        it["pull_request"] = {"url": "x"}
    return it


class FakeGitHub:
    """A local GitHub issues API: GET /repos/<repo>/issues?labels=<one label> answers from `issues`."""

    def __init__(self, issues, status=200):
        self.issues, self.status, self.seen = issues, status, []
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                u = urllib.parse.urlparse(self.path)
                q = urllib.parse.parse_qs(u.query)
                outer.seen.append((u.path, q.get("labels", [""])[0], q.get("state", [""])[0], self.headers.get("Authorization")))
                if outer.status != 200:
                    self.send_response(outer.status)
                    self.end_headers()
                    return
                label = q.get("labels", [""])[0]
                body = json.dumps([i for i in outer.issues if label in [x["name"] for x in i["labels"]]]).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"

    def close(self):
        self.srv.shutdown()


class Intake(unittest.TestCase):
    def run_intake(self, gh, *args, token=None):
        plan = os.path.join(tempfile.mkdtemp(), "plan.json")
        env = {"FLOTILLA_PLAN_FILE": plan, "GITHUB_API": gh.url}
        if token:
            tf = os.path.join(tempfile.mkdtemp(), "t")
            with open(tf, "w") as f:
                f.write(token + "\n")
            env["GITHUB_TOKEN_FILE"] = tf
        old = dict(os.environ)
        os.environ.update(env)
        try:
            intake.main(list(args))
        finally:
            os.environ.clear()
            os.environ.update(old)
        with open(plan) as f:
            return json.load(f)

    def test_the_case_kind_is_per_routine(self):
        # ROADMAP B8: podcast_scraper's issues go to the pytest profile (fleets/bugfix/fix-podcast.yaml), whose
        # routine takes a case kind of its own; orrery keeps code_bug, the default
        gh = FakeGitHub([issue(31, "ffprobe chokes on non-UTF-8", ["bug"])])
        self.addCleanup(gh.close)
        out = self.run_intake(gh, "--repo", "chipi/podcast_scraper", "--label", "bug", "--kind", "code_bug_python")
        self.assertEqual([(d["kind"], d["target"]) for d in out["detections"]],
                         [("code_bug_python", "repo:chipi/podcast_scraper/issues/31")])
        out = self.run_intake(gh, "--repo", "chipi/podcast_scraper", "--label", "bug")
        self.assertEqual([d["kind"] for d in out["detections"]], ["code_bug"])

    def test_one_case_per_issue_either_label_no_prs(self):
        gh = FakeGitHub([issue(12, "the orbit is wrong", ["bug"]), issue(15, "crash on /fly", ["triage-fleet/routed:bugfix", "bug"]),
                         issue(16, "a PR", ["bug"], pr=True), issue(17, "routed only", ["triage-fleet/routed:bugfix"])])
        self.addCleanup(gh.close)
        out = self.run_intake(gh, "--repo", "chipi/orrery", "--label", "bug", "--label", "triage-fleet/routed:bugfix", token="tok-1")
        self.assertEqual(out["outcome"], "detections")
        got = {d["target"]: d for d in out["detections"]}
        self.assertEqual(sorted(got), ["repo:chipi/orrery/issues/12", "repo:chipi/orrery/issues/15", "repo:chipi/orrery/issues/17"])
        d = got["repo:chipi/orrery/issues/15"]
        self.assertEqual((d["kind"], d["title"]), ("code_bug", "orrery #15: crash on /fly"))
        self.assertEqual(d["data"]["issue"]["number"], 15)
        self.assertEqual(d["data"]["issue"]["labels"], ["bug", "triage-fleet/routed:bugfix"])
        self.assertTrue(all(s[2] == "open" and s[3] == "Bearer tok-1" for s in gh.seen), gh.seen)

    def test_an_issue_filed_from_a_dev_signal_is_not_a_case(self):
        """Issues triage filed from dev signals carry `bug`; their body names the signal
        (actions.py: `fp: `glitchtip:ORRERY-DEV-13``), as #651 and #659 do. A production fingerprint stays a case."""
        dev = "**Signal:** `TypeError: m.x is not a function` (source: glitchtip, fp: `glitchtip:ORRERY-DEV-13`)"
        prod = "**Signal:** `TypeError: m.x is not a function` (source: glitchtip, fp: `glitchtip:ORRERY-PROD-25`)"
        gh = FakeGitHub([issue(14, "prod", ["bug"], body=prod), issue(15, "dev", ["bug"], body=dev)])
        self.addCleanup(gh.close)
        out = self.run_intake(gh, "--repo", "chipi/orrery", "--label", "bug")
        self.assertEqual([d["target"] for d in out["detections"]], ["repo:chipi/orrery/issues/14"])

    def test_the_whole_report_stays_under_flotillads_limit(self):
        # B8 (2026-11-16): podcast_scraper's 35 open bug issues at up to 7,500 bytes each made a report over the
        # 64 KiB flotillad accepts (internal/envelope MaxBytes): refused, no case opened, the run still "ok"
        big = [issue(n, f"bug {n}", ["bug"], body="x" * 9000) for n in range(1, 51)]
        gh = FakeGitHub(big)
        self.addCleanup(gh.close)
        out = self.run_intake(gh, "--repo", "chipi/podcast_scraper", "--label", "bug")
        self.assertEqual(len(out["detections"]), 50)
        self.assertLess(len(json.dumps(out).encode()), 64 * 1024)
        self.assertTrue(all(d["data"]["issue"]["body_cut"] for d in out["detections"]))
        # a handful of short issues keeps its bodies whole
        small = [issue(n, f"bug {n}", ["bug"], body="short body") for n in range(1, 6)]
        gh2 = FakeGitHub(small)
        self.addCleanup(gh2.close)
        out = self.run_intake(gh2, "--repo", "chipi/orrery", "--label", "bug")
        self.assertEqual({d["data"]["issue"]["body"] for d in out["detections"]}, {"short body"})

    def test_long_titles_shrink_the_bodies_until_the_report_fits(self):
        # a title sits in the detection and in its data, outside the per-detection estimate: the shares must shrink
        long_title = "t" * 300
        gh = FakeGitHub([issue(n, long_title, ["bug"], body="x" * 9000) for n in range(1, 51)])
        self.addCleanup(gh.close)
        out = self.run_intake(gh, "--repo", "chipi/podcast_scraper", "--label", "bug")
        self.assertEqual(len(out["detections"]), 50)
        self.assertLess(len(json.dumps(out).encode()), 64 * 1024)

    def test_a_report_that_cannot_fit_fails_the_run(self):
        # never hand flotillad a report it refuses (it opens nothing and says nothing). With titles capped at
        # TITLE_BYTES a real report always fits, so the limit is lowered here to reach the guard
        gh = FakeGitHub([issue(n, "t" * 1500, ["bug"], body="x") for n in range(1, 51)])
        self.addCleanup(gh.close)
        old = intake.MAX_REPORT
        intake.MAX_REPORT = 20000
        try:
            with self.assertRaises(intake.IntakeError):
                self.run_intake(gh, "--repo", "chipi/podcast_scraper", "--label", "bug")
        finally:
            intake.MAX_REPORT = old

    def test_fifty_long_non_ascii_titles_still_fit(self):
        # homelab's review (2026-11-16): JSON's \\uXXXX escapes and the title in two places could fill the report even
        # with the bodies cut away, and the whole intake failed
        gh = FakeGitHub([issue(n, "é" * 256, ["bug"], body="ü" * 9000) for n in range(1, 51)])
        self.addCleanup(gh.close)
        out = self.run_intake(gh, "--repo", "chipi/podcast_scraper", "--label", "bug")
        self.assertEqual(len(out["detections"]), 50)
        for d in out["detections"]:
            self.assertLessEqual(len(d["data"]["issue"]["title"].encode()), intake.TITLE_BYTES)
            self.assertLessEqual(len(d["title"].encode()), intake.TITLE_BYTES)

    def test_an_issue_s_data_does_not_depend_on_how_many_issues_are_open(self):
        # flotillad counts a repeat detection by its data (13c46f6); a cut that depended on the number of open issues
        # changed every case's data whenever one issue opened or closed (the advisor's review of flotilla, 2026-11-16)
        def data_of(n_open):
            gh = FakeGitHub([issue(n, "t", ["bug"], body="x" * 9000) for n in range(1, n_open + 1)])
            self.addCleanup(gh.close)
            out = self.run_intake(gh, "--repo", "chipi/podcast_scraper", "--label", "bug")
            return next(d["data"] for d in out["detections"] if d["target"].endswith("/issues/1"))
        self.assertEqual(data_of(3), data_of(30))

    def test_a_long_body_is_cut_to_fit(self):
        gh = FakeGitHub([issue(3, "long", ["bug"], body="x" * 40000)])
        self.addCleanup(gh.close)
        d = self.run_intake(gh, "--repo", "chipi/orrery", "--label", "bug")["detections"][0]
        self.assertTrue(d["data"]["issue"]["body_cut"])
        self.assertLessEqual(len(json.dumps(d["data"]).encode()), intake.DATA_SHARE)
        self.assertGreater(len(d["data"]["issue"]["body"]), 400, "a fixed share still carries the start of the body")

    def test_a_failed_read_or_too_many_fails_the_run(self):
        """A run that could not see the issues, or would have to leave some out, reports nothing: either
        would clear open cases."""
        gh = FakeGitHub([], status=403)
        self.addCleanup(gh.close)
        with self.assertRaises(intake.IntakeError):
            self.run_intake(gh, "--repo", "chipi/orrery", "--label", "bug")
        many = FakeGitHub([issue(n, "b", ["bug"]) for n in range(1, intake.MAX_DETECTIONS + 2)])
        self.addCleanup(many.close)
        with self.assertRaises(intake.IntakeError):
            self.run_intake(many, "--repo", "chipi/orrery", "--label", "bug")


if __name__ == "__main__":
    unittest.main()
