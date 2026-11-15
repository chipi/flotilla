"""seo/search-console (RFC-0012 §2): each site in Google Search Console, read as the fleet's service account
(a Restricted user on each property).

- clicks and impressions, the 7 days to 3 days ago (Search Console's data lags) against the 7 before:
  a drop by the rule in common.py is a seo_traffic_drop on site:clicks/<host>/;
- the last 28 days by query and page: a query ranking 8 to 20 with enough impressions is a
  seo_opportunity on site:opportunity/<host><path>/<query> (the 10 with most impressions per site);
- the site's fixed pages through the URL Inspection API: a page Google does not have indexed is a
  seo_not_indexed on site:index/<host><path>, with Google's coverage state.

The token: an RS256 JWT signed with openssl (the standard library cannot sign), exchanged at Google's
token endpoint for a webmasters.readonly access token. The key never leaves its file.
"""
import base64
import json
import os
import re
import subprocess
import sys
import time
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common  # noqa: E402
from site_check import target  # noqa: E402

API = "https://searchconsole.googleapis.com"
SCOPE = "https://www.googleapis.com/auth/webmasters.readonly"
LAG_DAYS = 3
OPP_PER_SITE = 10


def b64(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=")


def token(key_path, http=common.http_json, now=None):
    with open(key_path) as f:
        k = json.load(f)
    now = int(now or time.time())
    msg = b64(json.dumps({"alg": "RS256", "typ": "JWT"}).encode()) + b"." + b64(json.dumps(
        {"iss": k["client_email"], "scope": SCOPE, "aud": k["token_uri"], "iat": now, "exp": now + 600}).encode())
    # openssl reads the key from its own file (the service account file holds it as one JSON field):
    # a pipe from this process, so the key is written nowhere
    r, w = os.pipe()
    os.write(w, k["private_key"].encode())
    os.close(w)
    try:
        p = subprocess.run(["openssl", "dgst", "-sha256", "-sign", f"/dev/fd/{r}"], input=msg, capture_output=True,
                           pass_fds=(r,), timeout=30)
    finally:
        os.close(r)
    if p.returncode != 0:
        raise common.Error(f"openssl could not sign: {p.stderr[:200]!r}")
    got = http(k["token_uri"], form={"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer",
                                     "assertion": (msg + b"." + b64(p.stdout)).decode()})
    return got["access_token"]


def day(t):
    return time.strftime("%Y-%m-%d", time.gmtime(t))


class Console:
    def __init__(self, tok, http=common.http_json):
        self.tok, self.http = tok, http

    def query(self, prop, start, end, dims, host, limit=1000):
        body = {"startDate": start, "endDate": end, "dimensions": dims, "rowLimit": limit,
                # one property can hold several sites (humancopilot.eu holds www and evals): this site's pages only
                "dimensionFilterGroups": [{"filters": [{"dimension": "page", "operator": "contains", "expression": f"://{host}/"}]}]}
        url = f"{API}/webmasters/v3/sites/{urllib.parse.quote(prop, safe='')}/searchAnalytics/query"
        return self.http(url, token=self.tok, body=body).get("rows", [])

    def inspect(self, prop, url):
        r = self.http(f"{API}/v1/urlInspection/index:inspect", token=self.tok, body={"inspectionUrl": url, "siteUrl": prop})
        s = r.get("inspectionResult", {}).get("indexStatusResult", {})
        return s.get("verdict", ""), s.get("coverageState", "")


def slug(q):
    return re.sub(r"[^A-Za-z0-9.-]+", "-", q).strip("-")[:40] or "q"


def run(sites, c, now):
    found, summary = [], []
    end = now - LAG_DAYS * 86400
    for site in sites:
        prop = site.get("search_console")
        if not prop:
            continue
        host = urllib.parse.urlsplit(site["base"]).netloc
        this = c.query(prop, day(end - 6 * 86400), day(end), [], host)
        before = c.query(prop, day(end - 13 * 86400), day(end - 7 * 86400), [], host)
        clicks, imp = (int(this[0]["clicks"]), int(this[0]["impressions"])) if this else (0, 0)
        bclicks, bimp = (int(before[0]["clicks"]), int(before[0]["impressions"])) if before else (0, 0)
        summary.append(f"{host}: {clicks:,} clicks ({common.change(clicks, bclicks)}), {imp:,} impressions "
                       f"({common.change(imp, bimp)}), the 7 days to {day(end)}")
        if common.dropped(clicks, bclicks):
            found.append({"kind": "seo_traffic_drop", "target": target("clicks", site["base"] + "/"),
                          "title": f"{host}: Search clicks fell from {bclicks:,} to {clicks:,} in a week",
                          "detail": f"Search Console, the 7 days to {day(end)}: {clicks} clicks against {bclicks}",
                          "data": {"site": site["name"], "source": "search_console",
                                   "measure": {"name": "clicks", "value": clicks, "before": bclicks,
                                               "threshold": round(bclicks * (1 - common.DROP_SHARE))}}})
        rows = c.query(prop, day(end - 27 * 86400), day(end), ["query", "page"], host)
        lo, hi = common.OPP_POSITION
        opp = sorted((r for r in rows if lo <= r["position"] <= hi and r["impressions"] >= common.OPP_IMPRESSIONS),
                     key=lambda r: -r["impressions"])[:OPP_PER_SITE]
        for r in opp:
            q, page = r["keys"]
            path = urllib.parse.urlsplit(page).path or "/"
            found.append({"kind": "seo_opportunity", "target": target("opportunity", f"{site['base']}{path.rstrip('/')}/{slug(q)}"),
                          "title": f"{host}{path}: \"{q[:60]}\" ranks {r['position']:.1f} with {int(r['impressions']):,} impressions",
                          "detail": f"Search Console, 28 days to {day(end)}: position {r['position']:.1f}, "
                                    f"{int(r['impressions'])} impressions, {int(r['clicks'])} clicks, CTR {r['ctr']:.1%}",
                          "data": {"site": site["name"], "source": "search_console", "query": q, "page": page,
                                   "measure": {"name": "position", "value": round(r["position"], 1), "threshold": f"{lo:.0f}-{hi:.0f}",
                                               "impressions": int(r["impressions"]), "clicks": int(r["clicks"])}}})
        if opp:
            summary.append(f"{host}: {len(opp)} queries rank {lo:.0f}-{hi:.0f} with at least {common.OPP_IMPRESSIONS} impressions")
        for p in site.get("pages", ["/"]):
            url = site["base"] + p
            verdict, state = c.inspect(prop, url)
            if verdict != "PASS":
                found.append({"kind": "seo_not_indexed", "target": target("index", url),
                              "title": f"{host}{p}: not indexed by Google ({state or verdict or 'no answer'})",
                              "detail": f"URL Inspection: verdict {verdict!r}, coverage {state!r}",
                              "data": {"site": site["name"], "source": "search_console", "url": url,
                                       "measure": {"name": "index_verdict", "value": verdict, "threshold": "PASS", "coverage": state}}})
    return found, summary


def main():
    c = Console(token(os.environ.get("SEO_GSC_KEY", "/run/flotilla/gsc.json")))
    found, summary = run(common.sites(), c, int(time.time()))
    common.report(found, summary)
    for ln in summary:
        print("SEO SEARCH-CONSOLE", ln, flush=True)
    for d in found:
        print(f"SEO SEARCH-CONSOLE finding {d['target']}: {d['title']}", flush=True)


if __name__ == "__main__":
    try:
        main()
    except (common.Error, KeyError, OSError, ValueError) as e:
        print(f"SEO SEARCH-CONSOLE failed: {e}", file=sys.stderr)
        sys.exit(2)
