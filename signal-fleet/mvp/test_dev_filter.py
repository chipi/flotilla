"""python3 test_dev_filter.py: an error from a dev environment never becomes a `bug` on main (fleet-eval-data
ROADMAP T0.1).

A dev build runs a worktree's unmerged code (orrery's GlitchTip project orrery-dev: environment=dev, a `worktree`
tag, no release). Its errors were filed in chipi/orrery as `bug` + actionable and routed to bugfix, which fixed main,
where the code does not exist: both wrong fixes of phase A (#651, #659) came from there. The signal still gets
filed, labelled as dev, without `bug`, the actionable marker or the routing note.
"""
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import filing  # noqa: E402


def _sig(fp, project):
    return {"fingerprint": fp, "source": "glitchtip", "alertname": "TypeError: m.x is not a function",
            "labels": {"project": project, "level": "error", "culprit": "src/a.svelte", "count": "5"},
            "raw": {"metadata": {"type": "TypeError"}, "culprit": "src/a.svelte", "count": "5", "userCount": 1}}


class DevEnvironment(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.NamedTemporaryFile("w", suffix=".tsv", delete=False)
        tmp.close()
        self.path = tmp.name
        self.saved = {n: getattr(filing, n) for n in ("FILED", "_gh", "_related_issues", "_ensure_labels",
                                                      "_milestone_number", "_glitchtip_note", "_grafana_annotation")}
        filing.FILED = self.path
        self.posts = []

        def gh(m, p, payload=None):
            if m == "POST" and p.endswith("/issues"):
                self.posts.append(payload)
            return {"number": 99, "html_url": "u/99"}
        filing._gh = gh
        filing._related_issues = lambda repo, signal: []
        filing._ensure_labels = lambda repo, labels: None
        filing._milestone_number = lambda repo, title: None
        filing._glitchtip_note = filing._grafana_annotation = lambda *a, **k: None

    def tearDown(self):
        for n, v in self.saved.items():
            setattr(filing, n, v)
        os.unlink(self.path)

    def file(self, fp, project):
        out = filing.file_or_update(_sig(fp, project), {"title": "t", "body": "b", "labels": ["bug", "area:x"]}, "bug")
        self.assertTrue(out.startswith("CREATED") or out.startswith("FILED"), out)
        return self.posts[-1]

    def test_a_dev_error_is_filed_as_dev_not_as_a_bug(self):
        p = self.file("glitchtip:ORRERY-DEV-T", "orrery-dev")
        self.assertIn(filing.DEV_LABEL, p["labels"])
        self.assertNotIn("bug", p["labels"])
        self.assertNotIn(filing.ACTIONABLE_LABEL, p["labels"])
        self.assertNotIn("routable", p["body"])

    def test_a_production_error_is_unchanged(self):
        p = self.file("glitchtip:ORRERY-PROD-25", "orrery-prod")
        self.assertIn("bug", p["labels"])
        self.assertIn(filing.ACTIONABLE_LABEL, p["labels"])
        self.assertIn("routable", p["body"])
        self.assertNotIn(filing.DEV_LABEL, p["labels"])

    def test_which_signals_are_dev(self):
        yes = [("glitchtip:ORRERY-DEV-13", "orrery-dev"), ("glitchtip:PODCAST-PIPELINE-DEV-1I", "podcast-pipeline-dev"),
               ("glitchtip:PLAYER-DEV-4", "player-dev")]
        no = [("glitchtip:ORRERY-PROD-25", "orrery-prod"), ("glitchtip:ORRERY-1O", "orrery"),
              ("glitchtip:DEVWHATEVEROPS-WEB-3", "devwhateverops-web"), ("grafana:abc", "")]
        for fp, project in yes:
            self.assertTrue(filing.is_dev_signal(_sig(fp, project)), fp)
        for fp, project in no:
            self.assertFalse(filing.is_dev_signal(_sig(fp, project)), fp)


    def test_a_grafana_alert_is_never_dev(self):
        # a decision, not a side effect: dev environments run no Grafana alerting (homelab review of T0.1),
        # so only GlitchTip signals are classed dev, even when an alert's project label ends in -dev
        sig = _sig("grafana:abc123", "orrery-dev")
        sig["source"] = "grafana"
        self.assertFalse(filing.is_dev_signal(sig))

if __name__ == "__main__":
    unittest.main()
