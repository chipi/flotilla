"""python3 test_probes.py: service_logs and metric reach a project's real logs and metrics.

The probes queried `app:<GlitchTip project slug>` and `job=~".*<slug>.*"`; the logs carry other labels (app=api,
player, shop, evals, websites, with `service` per surface) and the jobs other names (api, player_mcp, moss-app).
Live on 2026-11-16 (VictoriaLogs, 24 h): app:api-api, player-mcp, shop-prod, evals-inspector ... 0 lines,
while service:api-pipeline had 5,604, player-api 11,964, shop-web 47,451. A dismissal needs a usable
service_logs or metric (the dismiss_corroborated gate), so for most projects it escalated.
"""
import os  # noqa: E402
os.environ.setdefault("SF_INSTALLATION", os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                                      "testdata", "installation.json"))  # example facts
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import correlate  # noqa: E402
import probes  # noqa: E402


class Queries(unittest.TestCase):
    def setUp(self):
        self.sent = []
        self.saved = correlate.vl_query, correlate.vm_query
        correlate.vl_query = lambda q: self.sent.append(("vl", q)) or "logs"
        correlate.vm_query = lambda q: self.sent.append(("vm", q)) or {"data": {"result": [1]}}

    def tearDown(self):
        correlate.vl_query, correlate.vm_query = self.saved

    def logs(self, project, args=None):
        self.sent.clear()
        probes.run_probe({"labels": {"project": project}}, "service_logs", args or {})
        return self.sent[-1][1]

    def metric(self, project, kind="up"):
        self.sent.clear()
        probes.run_probe({"labels": {"project": project}}, "metric", {"kind": kind})
        return self.sent[-1][1]

    def test_logs_by_the_labels_the_logs_carry(self):
        cases = {"api-pipeline": "service:api-pipeline", "api-api": "service:api-api",
                 "player-api": "service:player-api", "player-mcp": "service:player-mcp",
                 "player-obs": "service:player-obs", "player": "app:player",
                 "shop-prod": "service:shop-web", "shop-lab-api": "service:shop-lab-api",
                 "shop-mcp": "service:shop-mcp", "evals-inspector": "service:evals-inspector",
                 "evals-web": "service:evals-web", "litellm-prod": "service:litellm-gateway",
                 "site-a-web": "app:websites", "site-b-web": "app:websites",
                 "shop": "app:shop", "litellm": "app:litellm"}
        for project, selector in cases.items():
            self.assertTrue(self.logs(project).startswith(selector + " AND _time:6h"), (project, self.logs(project)))

    def test_an_unknown_project_keeps_the_old_query(self):
        self.assertTrue(self.logs("chemigram").startswith("app:chemigram AND"))

    def test_the_models_own_service_argument_still_wins(self):
        self.assertTrue(self.logs("api-api", {"service": "api"}).startswith("app:api AND"))

    def test_a_slug_given_as_the_service_argument_is_mapped_too(self):
        """The model knows a project only by its GlitchTip slug, so it passes that as `service`."""
        self.assertTrue(self.logs("chemigram", {"service": "shop-prod"}).startswith("service:shop-web AND"))
        self.sent.clear()
        probes.run_probe({"labels": {"project": "x"}}, "metric", {"service": "player-mcp", "kind": "up"})
        self.assertEqual(self.sent[-1][1], 'up{job="player_mcp"}')

    def test_a_service_that_is_not_a_plain_name_queries_nothing(self):
        """The value goes into LogsQL and PromQL; quotes or pipes would change the query."""
        for bad in ('x" or 1', "a | delete", "b}[5m]"):
            self.sent.clear()
            self.assertEqual(probes.run_probe({"labels": {"project": "p"}}, "service_logs", {"service": bad}),
                             "<no service to query>", bad)
            self.assertEqual(self.sent, [], bad)

    def test_metrics_by_the_job_that_exists(self):
        cases = {"player-mcp": "player_mcp", "player-obs": "player_obs", "shop-prod": "shop",
                 "shop-lab-api": "shop", "shop-mcp": "shop", "evals-inspector": "evals-inspector",
                 "evals-mcp": "evals-mcp", "moss-model-host-transcription": "moss-app",
                 "pyannote-model-host-diarization": "pyannote-app", "delivery": "delivery"}
        for project, job in cases.items():
            self.assertEqual(self.metric(project, "up"), f'up{{job="{job}"}}', project)
        self.assertEqual(self.metric("moss-model-host-transcription", "5xx"),
                         'sum(rate(http_requests_total{job="moss-app",status=~"5.."}[5m]))')
        # an unmapped project: the old regex
        self.assertEqual(self.metric("chemigram", "up"), 'up{job=~".*chemigram.*"}')


if __name__ == "__main__":
    unittest.main()
