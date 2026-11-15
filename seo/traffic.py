"""seo/traffic (RFC-0012 §2): each site's visits and its visits from search engines in Umami, the last 7
days against the 7 before. A drop by the rule in common.py is one seo_traffic_drop detection on
site:visits/<host>/ or site:search-visits/<host>/. The week's numbers are the run's summary, for the
brief's Search section. Read only, as the view-only Umami user (team seo-read).
"""
import os
import re
import sys
import time
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
from site_check import target  # noqa: E402

DAY_MS = 86_400_000
SEARCH = re.compile(r"(^|\.)(google|bing|duckduckgo|yahoo|baidu|yandex|ecosia|startpage|qwant|naver|seznam|brave)\.", re.I)


class Umami:
    def __init__(self, url, user, password, http=common.http_json):
        self.url, self.http = url.rstrip("/"), http
        self.token = http(self.url + "/api/auth/login", body={"username": user, "password": password})["token"]

    def get(self, path, **q):
        return self.http(f"{self.url}{path}?{urllib.parse.urlencode(q)}", token=self.token)


def week(u, wid, end):
    """(visits, visits the week before, search visits, search visits the week before) for one website."""
    start, prev = end - 7 * DAY_MS, end - 14 * DAY_MS
    st = u.get(f"/api/websites/{wid}/stats", startAt=start, endAt=end)
    before = st.get("comparison") or u.get(f"/api/websites/{wid}/stats", startAt=prev, endAt=start)

    def search(a, b):
        rows = u.get(f"/api/websites/{wid}/metrics", type="referrer", startAt=a, endAt=b, limit=500)
        return sum(int(r.get("y", 0)) for r in rows if SEARCH.search(str(r.get("x", ""))))
    return int(st.get("visits", 0)), int(before.get("visits", 0)), search(start, end), search(prev, start)


def run(sites, u, end):
    found, summary = [], []
    for site in sites:
        if not site.get("umami"):
            continue
        host = urllib.parse.urlsplit(site["base"]).netloc
        v = vb = s = sb = 0
        for wid in site["umami"]:
            a, b, c, d = week(u, wid, end)
            v, vb, s, sb = v + a, vb + b, s + c, sb + d
        summary.append(f"{host}: {v:,} visits ({common.change(v, vb)} on the week before), "
                       f"{s:,} from search ({common.change(s, sb)})")
        for check, now, before, what in (("visits", v, vb, "visits"), ("search-visits", s, sb, "visits from search engines")):
            if common.dropped(now, before):
                found.append({"kind": "seo_traffic_drop", "target": target(check, site["base"] + "/"),
                              "title": f"{host}: {what} fell from {before:,} to {now:,} in a week",
                              "detail": f"Umami, the 7 days to {time.strftime('%Y-%m-%d', time.gmtime(end / 1000))}: {now} against {before}",
                              "data": {"site": site["name"], "source": "umami",
                                       "measure": {"name": check, "value": now, "threshold": round(before * (1 - common.DROP_SHARE)),
                                                   "before": before, "rule": f"more than {common.DROP_SHARE:.0%} below, from at least {common.DROP_MIN}"}}})
    return found, summary


def main():
    e = common.env_file(os.environ.get("SEO_UMAMI_ENV", "/run/flotilla/umami.env"))
    u = Umami(os.environ.get("SEO_UMAMI_URL", e.get("UMAMI_URL", "")), e["UMAMI_USERNAME"], e["UMAMI_PASSWORD"])
    found, summary = run(common.sites(), u, int(time.time() * 1000))
    common.report(found, summary)
    for ln in summary:
        print("SEO TRAFFIC", ln, flush=True)
    for d in found:
        print(f"SEO TRAFFIC finding {d['target']}: {d['title']}", flush=True)


if __name__ == "__main__":
    try:
        main()
    except (common.Error, KeyError, OSError) as e:
        print(f"SEO TRAFFIC failed: {e}", file=sys.stderr)
        sys.exit(2)
