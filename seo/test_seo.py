"""seo/site-check against recorded pages: every check has a case that fires and one that does not, and no
request leaves the process."""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import site_check  # noqa: E402

B = "https://www.example.org"
GOOD = """<!doctype html><html><head><title>Topic — Example</title>
<meta name="description" content="What the page is about.">
<link rel="canonical" href="{url}">
<link rel="alternate" hreflang="en" href="{url}"><link rel="alternate" hreflang="de" href="{de}">
<script type="application/ld+json">{{"@type": "WebSite"}}</script></head><body><h1>Topic</h1></body></html>"""
ROBOTS = "User-agent: *\nAllow: /\n\nSitemap: " + B + "/sitemap.xml\n"
INDEX = """<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<sitemap><loc>""" + B + """/sitemaps/en.xml</loc></sitemap></sitemapindex>"""
URLSET = """<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>""" + B + """/</loc></url><url><loc>""" + B + """/things/b</loc></url><url><loc>""" + B + """/things/a</loc></url></urlset>"""


def page(url):
    return GOOD.format(url=url, de=B + "/de" + url[len(B):])


class Site:
    """A recorded site: url -> (status, final url, headers, body). Unknown urls answer 404."""

    def __init__(self, **over):
        self.pages = {B + "/robots.txt": (200, None, {}, ROBOTS), B + "/sitemap.xml": (200, None, {}, INDEX),
                      B + "/sitemaps/en.xml": (200, None, {}, URLSET)}
        for path in ("/", "/things/a", "/things/b", "/de/", "/de/things/a", "/de/things/b"):
            self.pages[B + path] = (200, None, {}, page(B + path))
        self.pages.update(over)
        self.asked = []

    def __call__(self, url, follow=True):
        self.asked.append(url)
        st, final, headers, body = self.pages.get(url, (404, None, {}, ""))
        return st, final or url, headers, body


SITE = {"name": "example", "base": B, "brand": "Example", "pages": ["/"], "sample_prefixes": ["/things/"],
        "json_ld": ["/", "/things/"], "hreflang": True, "locales": 2, "bots": ["Googlebot", "Baiduspider"],
        "retired": ["https://old.example.org/"]}


def run(site=SITE, **over):
    fake = Site(**over)
    fake.pages.setdefault("https://old.example.org/", (404, None, {}, ""))
    c = site_check.Check(site, fake, sleep=lambda s: None)
    summary = c.run()
    return {d["target"] for d in c.found}, c, summary, fake


class SiteCheck(unittest.TestCase):
    def test_a_good_site_has_no_finding(self):
        found, c, s, fake = run()
        self.assertEqual(found, set(), [d["detail"] for d in c.found])
        self.assertEqual((s["urls"], s["pages"]), (3, 2))
        self.assertIn(B + "/things/a", fake.asked, "the first sampled URL of the prefix, in sorted order")
        self.assertIn(B + "/de/", fake.asked, "the home page's hreflang alternates are fetched")

    def test_robots(self):
        found, *_ = run(**{B + "/robots.txt": (200, None, {}, ROBOTS + "\nUser-agent: Baiduspider\nDisallow: /\n")})
        self.assertEqual(found, {"site:robots-baiduspider/www.example.org/robots.txt"})
        found, *_ = run(**{B + "/robots.txt": (503, None, {}, "")})
        self.assertIn("site:robots/www.example.org/robots.txt", found)

    def test_sitemaps(self):
        found, *_ = run(**{B + "/sitemaps/en.xml": (500, None, {}, "")})
        self.assertIn("site:sitemap/www.example.org/sitemaps/en.xml", found)
        found, *_ = run(**{B + "/sitemaps/en.xml": (200, None, {}, "<urlset")})
        self.assertIn("site:sitemap/www.example.org/sitemaps/en.xml", found)
        empty = '<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"></urlset>'
        found, *_ = run(**{B + "/sitemaps/en.xml": (200, None, {}, empty)})
        self.assertIn("site:sitemap/www.example.org/sitemaps/en.xml", found)

    def test_status_redirect_and_noindex(self):
        found, *_ = run(**{B + "/": (500, None, {}, "")})
        self.assertIn("site:status/www.example.org/", found)
        found, *_ = run(**{B + "/": (200, "https://elsewhere.example.net/", {}, page(B + "/"))})
        self.assertIn("site:redirect/www.example.org/", found)
        found, *_ = run(**{B + "/": (200, None, {}, page(B + "/").replace("<title>", '<meta name="robots" content="noindex"><title>'))})
        self.assertIn("site:noindex/www.example.org/", found)
        found, *_ = run(**{B + "/": (200, None, {"X-Robots-Tag": "noindex"}, page(B + "/"))})
        self.assertIn("site:noindex/www.example.org/", found)

    def test_title_description_canonical(self):
        found, *_ = run(**{B + "/": (200, None, {}, page(B + "/").replace("Topic — Example", "Example"))})
        self.assertIn("site:title/www.example.org/", found, "the bare brand")
        found, *_ = run(**{B + "/": (200, None, {}, page(B + "/").replace('<meta name="description" content="What the page is about.">', ""))})
        self.assertIn("site:description/www.example.org/", found)
        found, *_ = run(**{B + "/": (200, None, {}, page(B + "/").replace(f'rel="canonical" href="{B}/"', f'rel="canonical" href="{B}/home"'))})
        self.assertIn("site:canonical/www.example.org/", found)
        found, *_ = run(**{B + "/": (200, None, {}, page(B + "/").replace(f'rel="canonical" href="{B}/"', f'rel="canonical" href="{B}"'))})
        self.assertEqual(found, set(), "an empty path is /: not a different canonical")

    def test_json_ld_where_the_site_wants_it(self):
        bad = page(B + "/things/a").replace('{"@type": "WebSite"}', '{"@type": ')
        found, *_ = run(**{B + "/things/a": (200, None, {}, bad)})
        self.assertIn("site:json-ld/www.example.org/things/a", found)
        none = page(B + "/").replace('<script type="application/ld+json">{"@type": "WebSite"}</script>', "")
        found, *_ = run(**{B + "/": (200, None, {}, none)})
        self.assertIn("site:json-ld/www.example.org/", found)
        site = dict(SITE, pages=["/", "/hub"])
        found, *_ = run(site, **{B + "/hub": (200, None, {}, none.replace(f'href="{B}/"', f'href="{B}/hub"'))})
        self.assertNotIn("site:json-ld/www.example.org/hub", found, "a hub is not a page that must carry JSON-LD")

    def test_hreflang(self):
        found, *_ = run(**{B + "/de/": (404, None, {}, "")})
        self.assertIn("site:hreflang/www.example.org/de/", found)
        one = page(B + "/").replace(f'<link rel="alternate" hreflang="de" href="{B}/de/">', "")
        found, *_ = run(**{B + "/": (200, None, {}, one)})
        self.assertIn("site:hreflang/www.example.org/", found, "fewer alternates than the site's locales")

    def test_retired_host(self):
        found, *_ = run(**{"https://old.example.org/": (200, None, {}, page("https://old.example.org/"))})
        self.assertIn("site:retired/old.example.org/", found)
        found, *_ = run(**{"https://old.example.org/": (200, None, {}, '<meta name="robots" content="noindex">')})
        self.assertEqual(found, set(), "a retired host that serves noindex is fine")

    def test_a_network_failure_is_tried_again(self):
        """No HTTP answer (a reset, a timeout) is the network, not the page: tried again, and reported only
        when it fails every time."""
        flaky = {"n": 0}

        class Flaky(Site):
            def __call__(self, url, follow=True):
                if url == B + "/de/":
                    flaky["n"] += 1
                    if flaky["n"] < 3:
                        return 0, url, {}, "connection reset by peer"
                return super().__call__(url, follow)
        fake = Flaky()
        fake.pages["https://old.example.org/"] = (404, None, {}, "")
        pauses = []
        c = site_check.Check(SITE, fake, sleep=pauses.append)
        c.run()
        self.assertEqual(c.found, [], [d["detail"] for d in c.found])
        self.assertEqual((c.retries, flaky["n"]), (2, 3))
        self.assertIn(5, pauses)
        self.assertIn(15, pauses)
        found, *_ = run(**{B + "/de/": (0, None, {}, "connection reset by peer")})
        self.assertIn("site:hreflang/www.example.org/de/", found, "a failure every time is still a finding")

    def test_targets_fit_flotillad(self):
        t = site_check.target("canonical", B + "/a path/ü?x=1#f")
        self.assertRegex(t, r"^[a-z][a-z0-9_]*:[A-Za-z0-9._/-]{1,120}$")
        self.assertLessEqual(len(site_check.target("status", B + "/" + "x" * 300)), 120)

    def test_main_writes_the_report(self):
        with tempfile.TemporaryDirectory() as d:
            plan, sites = os.path.join(d, "plan.json"), os.path.join(d, "sites.json")
            with open(sites, "w") as f:
                json.dump([SITE], f)
            fake = Site(**{B + "/": (500, None, {}, "")})
            fake.pages["https://old.example.org/"] = (404, None, {}, "")
            env = dict(os.environ)
            os.environ.update(SEO_SITES=sites, FLOTILLA_PLAN_FILE=plan)
            old = site_check.PACE, site_check.RETRY_PAUSES
            site_check.PACE, site_check.RETRY_PAUSES = 0, (0, 0)
            try:
                site_check.main(fake)
            finally:
                site_check.PACE, site_check.RETRY_PAUSES = old
                os.environ.clear()
                os.environ.update(env)
            with open(plan) as f:
                out = json.load(f)
            self.assertEqual(out["outcome"], "detections")
            self.assertIn("site:status/www.example.org/", {d["target"] for d in out["detections"]})
            self.assertTrue(all(d["kind"] == "seo_live_regression" for d in out["detections"]))


if __name__ == "__main__":
    unittest.main()
