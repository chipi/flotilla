"""python3 test_shadow_filter.py: nothing a shadow version of a routine emits ever reaches triage (v2 step 2, docs/wip/v2-design.md
section 4, layer 3).

A shadow run runs on production with no effect on the system. Its errors carry the tag shadow=1 and live in a GlitchTip project of
their own (<name>-shadow) when the installation gave them one, its log lines carry shadow=true, and its metrics are labelled
shadow="1" under a "<fleet>/<routine>@v<n>" key. Triage's readers leave all of them out, and the filing guard refuses one that
arrives anyway, so a candidate version's failure is never a live signal and never a GitHub issue.
"""
import os  # noqa: E402
os.environ.setdefault("SF_INSTALLATION", os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                                      "testdata", "installation.json"))  # example facts
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import correlate  # noqa: E402
import filing  # noqa: E402
import sources  # noqa: E402


def _issue(short, slug, **extra):
    return dict({"status": "unresolved", "shortId": short, "id": 1, "title": "TypeError: x", "culprit": "src/a.py",
                 "metadata": {}, "project": {"slug": slug, "id": 6}, "firstSeen": "2026-11-19T00:00:00Z"}, **extra)


class PollDropsShadowIssues(unittest.TestCase):
    def poll(self, issues):
        old = sources._gt
        sources._gt = lambda path: [{"id": 6, "slug": "shop"}] if "/projects/" in path else issues
        try:
            return sources.glitchtip_unresolved(limit=10)
        finally:
            sources._gt = old

    def test_a_shadow_project_issue_is_dropped(self):
        got = self.poll([_issue("SHOP-1", "shop"), _issue("SHOP-SHADOW-2", "shop-shadow")])
        self.assertEqual([i["shortId"] for i in got], ["SHOP-1"])

    def test_an_issue_tagged_shadow_is_dropped_in_either_tag_form(self):
        got = self.poll([_issue("SHOP-1", "shop"),
                         _issue("SHOP-2", "shop", tags=[{"key": "shadow", "value": "1"}]),
                         _issue("SHOP-3", "shop", tags={"shadow": "1", "routine_version": "2"}),
                         _issue("SHOP-4", "shop", tags=[{"key": "environment", "value": "operations"}])])
        self.assertEqual([i["shortId"] for i in got], ["SHOP-1", "SHOP-4"])

    def test_a_project_that_only_mentions_shadow_is_kept(self):
        got = self.poll([_issue("SHADOWFAX-1", "shadowfax"), _issue("SHOP-SHADOW-BOX", "shop-shadow-box")])
        self.assertEqual(len(got), 2)

    def test_the_slug_check_is_case_blind(self):
        self.assertTrue(sources._is_shadow_issue(_issue("X-1", "Shop-SHADOW")))


class FiringAlertsDropShadowSeries(unittest.TestCase):
    def alerts(self, items):
        olds = (sources.get_json, sources.config.GRAFANA_TOKEN)
        sources.get_json = lambda *a, **k: items
        sources.config.GRAFANA_TOKEN = "t"
        try:
            return sources.firing_alerts()
        finally:
            sources.get_json, sources.config.GRAFANA_TOKEN = olds

    def alert(self, **labels):
        return {"status": {"state": "active"}, "labels": dict({"alertname": "Run failed"}, **labels), "fingerprint": "f" + str(len(labels))}

    def test_an_alert_on_a_shadow_series_is_dropped(self):
        got = self.alerts([self.alert(routine="triage/cycle"), self.alert(routine="triage/cycle@v2", shadow="1"),
                           self.alert(routine="bugfix/fix@v3"), self.alert(shadow="true")])
        self.assertEqual([a["labels"].get("routine") for a in got], ["triage/cycle"])


class FilingRefusesAShadowSignal(unittest.TestCase):
    def sig(self, fp, project, **labels):
        return {"fingerprint": fp, "source": "glitchtip", "alertname": "TypeError", "labels": dict({"project": project}, **labels), "raw": {}}

    def test_which_signals_are_shadow(self):
        yes = [self.sig("glitchtip:SHOP-SHADOW-4", "shop-shadow"), self.sig("glitchtip:SHOP-9", "shop", shadow="1"),
               self.sig("glitchtip:SHOP-9", "shop", shadow="true"), self.sig("glitchtip:SHOP-9", "shop", routine="triage/cycle@v2"),
               self.sig("glitchtip:API-SHADOW-1", "")]
        no = [self.sig("glitchtip:SHOP-PROD-25", "shop-prod"), self.sig("glitchtip:SHOP-1O", "shop"),
              self.sig("glitchtip:SHADOWFAX-3", "shadowfax"), self.sig("grafana:abc", ""), self.sig("glitchtip:SHOP-1", "shop", shadow="0")]
        for s in yes:
            self.assertTrue(filing.is_shadow_signal(s), s)
        for s in no:
            self.assertFalse(filing.is_shadow_signal(s), s)

    def test_a_shadow_error_is_refused_and_nothing_is_written(self):
        tmp = tempfile.NamedTemporaryFile("w", suffix=".tsv", delete=False)
        tmp.close()
        saved = {n: getattr(filing, n) for n in ("FILED", "_gh")}
        posts = []
        filing.FILED = tmp.name
        filing._gh = lambda m, p, payload=None: posts.append((m, p)) or {"number": 1, "html_url": "u"}
        try:
            out = filing.file_or_update(self.sig("glitchtip:SHOP-SHADOW-1", "shop-shadow"),
                                        {"title": "t", "body": "b", "labels": ["bug"]}, "bug")
            self.assertTrue(out.startswith("REFUSED"), out)
            self.assertIn("shadow", out)
            self.assertEqual(posts, [])
            with open(tmp.name) as f:
                self.assertEqual(f.read(), "")
        finally:
            for n, v in saved.items():
                setattr(filing, n, v)
            os.unlink(tmp.name)


class LogReadsLeaveShadowLinesOut(unittest.TestCase):
    def test_the_filter_is_added_before_the_first_pipe(self):
        self.assertEqual(correlate.live_only("app:shop AND _time:6h | sort by (_time) desc | limit 15"),
                         "(app:shop AND _time:6h) -shadow:true | sort by (_time) desc | limit 15")
        self.assertEqual(correlate.live_only("job:x"), "(job:x) -shadow:true")

    def test_an_or_cannot_escape_the_exclusion(self):
        self.assertEqual(correlate.live_only("a:1 OR b:2 | limit 5"), "(a:1 OR b:2) -shadow:true | limit 5")

    def test_it_is_idempotent(self):
        q = correlate.live_only("app:shop | limit 1")
        self.assertEqual(correlate.live_only(q), q)

    def test_every_log_read_goes_through_it(self):
        sent = []
        old = correlate.post_form_text
        correlate.post_form_text = lambda url, form, headers=None, timeout=12: sent.append(form["query"]) or ""
        try:
            correlate.vl_query("app:shop AND _time:24h | sort by (_time) desc | limit 25")
            import probes
            probes._service_logs({"labels": {"project": "shop"}}, {"service": "shop", "window": "6h"})
        finally:
            correlate.post_form_text = old
        self.assertEqual(len(sent), 2)
        for q in sent:
            self.assertIn("-shadow:true", q)
            self.assertLess(q.index("-shadow:true"), q.index("| "), q)


if __name__ == "__main__":
    unittest.main(verbosity=2)
