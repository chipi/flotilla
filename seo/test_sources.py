"""seo/traffic and seo/search-console against recorded API answers: the drop rule's edges, opportunities,
the index check, the shared property, and the token (signed by a real openssl with a throwaway key)."""
import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
import search_console  # noqa: E402
import traffic  # noqa: E402

SITE = {"name": "ex", "base": "https://www.example.org", "pages": ["/", "/a"], "umami": ["w1"], "search_console": "sc-domain:example.org"}


class FakeUmami:
    def __init__(self, visits, before, search, search_before):
        self.v = (visits, before, search, search_before)
        self.calls = []

    def get(self, path, **q):
        self.calls.append((path, q))
        if path.endswith("/stats"):
            return {"visits": self.v[0], "comparison": {"visits": self.v[1]}}
        refs = [c for c in self.calls if c[0].endswith("/metrics")]
        n = self.v[2] if len(refs) % 2 == 1 else self.v[3]  # traffic.week asks for this week first
        return [{"x": "google.com", "y": n}, {"x": "github.com", "y": 99}, {"x": "news.ycombinator.com", "y": 5}]


class SitesFile(unittest.TestCase):
    def test_the_spec_names_the_file_and_the_code_names_no_fleet(self):
        env = os.environ.pop("SEO_SITES", None)
        try:
            self.assertEqual(common.sites_path(["--sites", "/f/sites/sites.json"]), "/f/sites/sites.json")
            with self.assertRaises(common.Error):
                common.sites_path([])
            os.environ["SEO_SITES"] = "/env/sites.json"
            self.assertEqual(common.sites_path([]), "/env/sites.json")
            self.assertEqual(common.sites_path(["--sites", "/spec.json"]), "/spec.json")
        finally:
            os.environ.pop("SEO_SITES", None)
            if env is not None:
                os.environ["SEO_SITES"] = env


class Traffic(unittest.TestCase):
    def test_the_drop_rule(self):
        self.assertTrue(common.dropped(34, 50), "more than 30 % below 50")
        self.assertFalse(common.dropped(35, 50), "exactly 30 % below is not a drop")
        self.assertFalse(common.dropped(0, 49), "under the minimum the week before")

    def test_a_drop_is_a_detection_and_the_week_is_the_summary(self):
        found, summary = traffic.run([SITE], FakeUmami(100, 200, 20, 60), 10**13)
        self.assertEqual({d["target"] for d in found}, {"site:visits/www.example.org/", "site:search-visits/www.example.org/"})
        self.assertTrue(all(d["kind"] == "seo_traffic_drop" for d in found))
        self.assertIn("100 visits (-50% on the week before), 20 from search (-67%)", summary[0])

    def test_no_drop_no_detection_and_only_search_referrers_count(self):
        found, summary = traffic.run([SITE], FakeUmami(200, 200, 60, 60), 10**13)
        self.assertEqual(found, [])
        self.assertIn("60 from search", summary[0], "github.com and news.ycombinator.com are not search engines")


class FakeConsole:
    def __init__(self, clicks, before, rows, verdicts):
        self.c, self.rows, self.verdicts, self.queries = (clicks, before), rows, verdicts, []

    def query(self, prop, start, end, dims, host, limit=1000):
        self.queries.append((prop, start, end, tuple(dims), host))
        if dims:
            return self.rows
        n = self.c[0] if len([q for q in self.queries if not q[3]]) % 2 == 1 else self.c[1]
        return [{"clicks": n, "impressions": n * 10}]

    def inspect(self, prop, url):
        return self.verdicts.get(url, ("PASS", "Submitted and indexed"))


ROWS = [{"keys": ["hill sphere", "https://www.example.org/ko/hill"], "position": 10.0, "impressions": 365, "clicks": 1, "ctr": 0.003},
        {"keys": ["too far", "https://www.example.org/x"], "position": 21.0, "impressions": 900, "clicks": 0, "ctr": 0},
        {"keys": ["too few", "https://www.example.org/y"], "position": 9.0, "impressions": 99, "clicks": 0, "ctr": 0},
        {"keys": ["top", "https://www.example.org/z"], "position": 3.0, "impressions": 5000, "clicks": 300, "ctr": 0.06}]


class SearchConsole(unittest.TestCase):
    def test_opportunities_drops_and_the_index(self):
        c = FakeConsole(20, 100, ROWS, {"https://www.example.org/a": ("NEUTRAL", "URL is unknown to Google")})
        found, summary = search_console.run([SITE], c, 1_800_000_000)
        by = {d["target"]: d for d in found}
        self.assertEqual(set(by), {"site:clicks/www.example.org/", "site:opportunity/www.example.org/ko/hill/hill-sphere",
                                   "site:index/www.example.org/a"})
        self.assertEqual(by["site:opportunity/www.example.org/ko/hill/hill-sphere"]["kind"], "seo_opportunity")
        self.assertIn("URL is unknown to Google", by["site:index/www.example.org/a"]["title"])
        self.assertTrue(all(q[4] == "www.example.org" for q in c.queries), "every query is filtered to this site's host")
        self.assertIn("20 clicks (-80%)", summary[0])

    def test_the_window_ends_three_days_back(self):
        c = FakeConsole(0, 0, [], {})
        search_console.run([SITE], c, 1_800_000_000)
        self.assertEqual(c.queries[0][2], search_console.day(1_800_000_000 - 3 * 86400))

    def test_the_token_is_a_jwt_openssl_signed(self):
        with tempfile.TemporaryDirectory() as d:
            key = os.path.join(d, "k.pem")
            subprocess.run(["openssl", "genrsa", "-out", key, "2048"], check=True, capture_output=True)
            sa = os.path.join(d, "sa.json")
            with open(key) as f:
                pem = f.read()
            with open(sa, "w") as f:
                json.dump({"client_email": "x@y.iam.gserviceaccount.com", "token_uri": "https://oauth2.example/token",
                           "private_key": pem}, f)
            seen = {}

            def http(url, form=None, **_):
                seen.update(url=url, form=form)
                return {"access_token": "ya29.test"}
            self.assertEqual(search_console.token(sa, http, now=1_800_000_000), "ya29.test")
            head, claims, sig = seen["form"]["assertion"].split(".")
            pad = lambda s: s + "=" * (-len(s) % 4)  # noqa: E731
            c = json.loads(search_console.base64.urlsafe_b64decode(pad(claims)))
            self.assertEqual((c["iss"], c["scope"], c["exp"] - c["iat"]), ("x@y.iam.gserviceaccount.com", search_console.SCOPE, 600))
            pub = os.path.join(d, "pub.pem")
            subprocess.run(["openssl", "rsa", "-in", key, "-pubout", "-out", pub], check=True, capture_output=True)
            sigf = os.path.join(d, "sig")
            with open(sigf, "wb") as f:
                f.write(search_console.base64.urlsafe_b64decode(pad(sig)))
            v = subprocess.run(["openssl", "dgst", "-sha256", "-verify", pub, "-signature", sigf], input=f"{head}.{claims}".encode(),
                               capture_output=True)
            self.assertEqual(v.returncode, 0, v.stdout + v.stderr)


if __name__ == "__main__":
    unittest.main()
