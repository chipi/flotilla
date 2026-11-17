"""seo/site-check (RFC-0012 §2): what each site in sites.json serves live, against what its build and its
search engines expect. Code only, read-only, the standard library only.

Per site: robots.txt answers and lets the named crawlers in; every sitemap answers and lists URLs; a fixed
set of pages plus one page per sampled prefix (taken from the sitemap) answers 200 on the site's own host,
is indexable, has a title, a description and a self canonical, and, where the site declares them, JSON-LD
that parses and hreflang alternates that answer; each retired host answers 404/410 or serves noindex.

Each failed check is one seo_live_regression detection on site:<check>/<host><path>; flotillad opens the
case and keeps it while the detection repeats. The run prints one line per site with its numbers.
"""
import http.client
import json
import os
import re
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import urllib.robotparser
import xml.etree.ElementTree as ET
from html.parser import HTMLParser

UA = "flotilla-seo/1 (+https://github.com/chipi/flotilla)"
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import common  # noqa: E402

MAX_BODY = 4_000_000
# A request that got no HTTP answer is tried again after these pauses: Cloudflare reset 7 of 204 of
# the host's connections to one site in one run (2026-11-15), and a reset is about the network,
# not the page. One that fails every time is still reported. PACE spaces the requests to one host.
RETRY_PAUSES = (5, 15)
PACE = 0.3
TARGET_MAX = 120
SM = "{http://www.sitemaps.org/schemas/sitemap/0.9}"


class Page(HTMLParser):
    """The head tags site-check reads."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.title, self.description, self.canonical, self.robots = None, None, None, []
        self.alternates, self.json_ld, self.h1, self.links = [], [], 0, []
        self._in_title, self._in_ld, self._buf = False, False, []

    def handle_starttag(self, tag, attrs):
        a = {k.lower(): (v or "") for k, v in attrs}
        rel = a.get("rel", "").lower().split()
        if tag == "title" and self.title is None:
            self._in_title, self._buf = True, []
        elif tag == "meta" and a.get("name", "").lower() == "description" and self.description is None:
            self.description = a.get("content", "").strip()
        elif tag == "meta" and a.get("name", "").lower() in ("robots", "googlebot"):
            self.robots.append(a.get("content", "").lower())
        elif tag == "link" and "canonical" in rel and self.canonical is None:
            self.canonical = a.get("href", "").strip()
        elif tag == "link" and "alternate" in rel and a.get("hreflang"):
            self.alternates.append((a["hreflang"], a.get("href", "").strip()))
        elif tag == "script" and a.get("type", "").lower() == "application/ld+json":
            self._in_ld, self._buf = True, []
        elif tag == "h1":
            self.h1 += 1
        elif tag == "a" and a.get("href"):
            self.links.append(a["href"].strip())

    def handle_data(self, data):
        if self._in_title or self._in_ld:
            self._buf.append(data)

    def handle_endtag(self, tag):
        if tag == "title" and self._in_title:
            self.title, self._in_title = "".join(self._buf).strip(), False
        elif tag == "script" and self._in_ld:
            self.json_ld.append("".join(self._buf))
            self._in_ld = False


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def fetch(url, follow=True, timeout=30):
    """(status, final url, headers, body text); a network failure is status 0 with the reason as the body."""
    opener = urllib.request.build_opener() if follow else urllib.request.build_opener(NoRedirect)
    req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "text/html,application/xml;q=0.9,*/*;q=0.8"})
    try:
        with opener.open(req, timeout=timeout) as r:
            return r.status, r.geturl(), r.headers, r.read(MAX_BODY).decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, url, e.headers, ""
    except (urllib.error.URLError, OSError) as e:
        return 0, url, {}, str(getattr(e, "reason", e))


def cert_expiry(host, timeout=30):
    """When the TLS certificate host serves on 443 expires (epoch seconds), through the routine's egress
    proxy when it has one (a CONNECT tunnel), as the page requests go."""
    proxy = os.environ.get("HTTPS_PROXY") or os.environ.get("https_proxy")
    ctx = ssl.create_default_context()
    if proxy:
        p = urllib.parse.urlsplit(proxy)
        conn = http.client.HTTPSConnection(p.hostname, p.port or 443, timeout=timeout, context=ctx)
        conn.set_tunnel(host, 443)
    else:
        conn = http.client.HTTPSConnection(host, 443, timeout=timeout, context=ctx)
    try:
        conn.connect()
        return ssl.cert_time_to_seconds(conn.sock.getpeercert()["notAfter"])
    finally:
        conn.close()


def norm(url):
    """For comparing canonicals: scheme and host lowercased, an empty path is /, no fragment."""
    p = urllib.parse.urlsplit(url)
    return urllib.parse.urlunsplit((p.scheme.lower(), p.netloc.lower(), p.path or "/", p.query, ""))


def target(check, url):
    p = urllib.parse.urlsplit(url)
    path = re.sub(r"[^A-Za-z0-9._/-]", "_", urllib.parse.unquote(p.path) or "/")
    t = f"site:{check}/{p.netloc.lower()}{path}"
    return t if len(t) <= TARGET_MAX else t[:TARGET_MAX]


class Check:
    def __init__(self, site, fetcher=fetch, sleep=time.sleep, certifier=cert_expiry, now=time.time):
        self.site, self.fetch, self.sleep, self.found, self.requests, self.retries = site, fetcher, sleep, [], 0, 0
        self.certifier, self.now, self.seen_links = certifier, now, []
        self.base = site["base"].rstrip("/")
        self.host = urllib.parse.urlsplit(self.base).netloc.lower()

    def get(self, url, follow=True):
        for pause in (0,) + RETRY_PAUSES:
            if pause:
                self.retries += 1
            self.sleep(pause or PACE)
            self.requests += 1
            got = self.fetch(url, follow)
            if got[0] != 0:
                return got
        return got

    def find(self, check, url, title, detail, value=None, threshold=None):
        self.found.append({"kind": "seo_live_regression", "target": target(check, url), "title": title, "detail": detail,
                           "data": {"site": self.site["name"], "url": url, "check": check,
                                    "measure": {"name": check, "value": value, "threshold": threshold}}})

    def robots(self):
        url = self.base + "/robots.txt"
        st, _, _, body = self.get(url)
        if st != 200:
            self.find("robots", url, f"{self.host}: robots.txt answers {st or 'nothing'}", f"robots.txt: {st} {body[:120]}", st, 200)
            return []
        rp = urllib.robotparser.RobotFileParser()
        rp.parse(body.splitlines())
        for bot in self.site.get("bots", ["Googlebot", "Bingbot"]):
            if not rp.can_fetch(bot, self.base + "/"):
                self.find(f"robots-{bot.lower()}", url, f"{self.host}: robots.txt keeps {bot} out",
                          f"robots.txt disallows / for {bot}", "disallowed", "allowed")
        return re.findall(r"(?im)^\s*sitemap:\s*(\S+)", body) or [self.base + "/sitemap.xml"]

    def sitemap_urls(self, sitemaps, depth=0):
        urls = []
        for sm in sitemaps:
            st, _, _, body = self.get(sm)
            if st != 200:
                self.find("sitemap", sm, f"{self.host}: a sitemap answers {st or 'nothing'}", f"{sm}: {st} {body[:120]}", st, 200)
                continue
            try:
                root = ET.fromstring(body.encode("utf-8"))
            except ET.ParseError as e:
                self.find("sitemap", sm, f"{self.host}: a sitemap does not parse", f"{sm}: {e}", "invalid", "XML")
                continue
            if root.tag == SM + "sitemapindex" and depth == 0:
                urls += self.sitemap_urls([e.text.strip() for e in root.iter(SM + "loc") if e.text], depth + 1)
                continue
            got = [e.text.strip() for e in root.iter(SM + "loc") if e.text]
            if not got:
                self.find("sitemap", sm, f"{self.host}: a sitemap lists no URLs", f"{sm}: 0 URLs", 0, 1)
            urls += got
        return urls

    def page(self, url, alternates=False):
        st, final, headers, body = self.get(url)
        if st != 200:
            self.find("status", url, f"{self.host}: {urllib.parse.urlsplit(url).path or '/'} answers {st or 'nothing'}",
                      f"{url}: {st} {body[:120] if st == 0 else ''}".strip(), st, 200)
            return
        fhost = urllib.parse.urlsplit(final).netloc.lower()
        if fhost != self.host:
            self.find("redirect", url, f"{self.host}: {url} redirects off the site", f"{url} ends at {final}", fhost, self.host)
        p = Page()
        try:
            p.feed(body)
        except Exception as e:  # a page the parser cannot read is a finding, not a crash
            self.find("html", url, f"{self.host}: a page's HTML cannot be read", f"{url}: {e}", "unparsed", "HTML")
            return
        self.seen_links += [(url, urllib.parse.urljoin(final, h)) for h in p.links]
        xr = (headers.get("X-Robots-Tag") or "").lower() if headers else ""
        if any("noindex" in r for r in p.robots) or "noindex" in xr:
            self.find("noindex", url, f"{self.host}: {urllib.parse.urlsplit(url).path or '/'} is noindex",
                      f"{url}: robots {p.robots} X-Robots-Tag {xr!r}", "noindex", "indexable")
        brand = self.site.get("brand")
        if not p.title:
            self.find("title", url, f"{self.host}: a page has no title", f"{url}: no <title>", "", "a title")
        elif brand and p.title.strip() == brand:
            self.find("title", url, f"{self.host}: a page is titled only {brand!r}", f"{url}: <title>{p.title}</title>", p.title, "more than the brand")
        if not p.description:
            self.find("description", url, f"{self.host}: a page has no description", f"{url}: no meta description", "", "a description")
        if not p.canonical:
            self.find("canonical", url, f"{self.host}: a page has no canonical", f"{url}: no rel=canonical", "", "self")
        elif norm(urllib.parse.urljoin(final, p.canonical)) != norm(final):
            self.find("canonical", url, f"{self.host}: a page's canonical points elsewhere",
                      f"{final}: canonical {p.canonical}", p.canonical, norm(final))
        if self.wants_json_ld(urllib.parse.urlsplit(final).path or "/"):
            if not p.json_ld:
                self.find("json-ld", url, f"{self.host}: a page has no JSON-LD", f"{url}: no application/ld+json", 0, 1)
            for block in p.json_ld:
                try:
                    json.loads(block)
                except ValueError as e:
                    self.find("json-ld", url, f"{self.host}: a page's JSON-LD does not parse", f"{url}: {e}", "invalid", "JSON")
                    break
        if self.site.get("hreflang"):
            want = self.site.get("locales")
            if want and len({h for h, _ in p.alternates if h != "x-default"}) < want:
                self.find("hreflang", url, f"{self.host}: a page lists fewer hreflang alternates than locales",
                          f"{url}: {len(p.alternates)} alternates", len(p.alternates), want)
            if alternates:
                for lang, href in p.alternates:
                    st2, _, _, _ = self.get(urllib.parse.urljoin(final, href))
                    if st2 != 200:
                        self.find("hreflang", href, f"{self.host}: an hreflang alternate answers {st2 or 'nothing'}",
                                  f"{url} lists {lang} {href}: {st2}", st2, 200)

    def wants_json_ld(self, path):
        """json_ld: true (every page), or the paths that must carry it: "/" is the home page alone, a path
        ending in / a prefix of the pages below it (for example the home page and the article pages, not
        the index pages that list them)."""
        want = self.site.get("json_ld")
        if want is True:
            return True
        for w in want or []:
            if path == w or (w != "/" and w.endswith("/") and path.startswith(w) and path != w):
                return True
        return False

    def retired(self):
        for url in self.site.get("retired", []):
            st, _, headers, body = self.get(url)
            if st in (404, 410):
                continue
            p = Page()
            p.feed(body or "")
            xr = (headers.get("X-Robots-Tag") or "").lower() if headers else ""
            if st == 200 and (any("noindex" in r for r in p.robots) or "noindex" in xr):
                continue
            self.find("retired", url, f"{self.host}: the retired host {urllib.parse.urlsplit(url).netloc} is indexable",
                      f"{url}: {st}, not 404/410 and not noindex", st, "404 or noindex")

    def broken_links(self):
        """links: N (opt-in): the first N distinct links on the site's own host, from the pages checked,
        answer 200 after redirects. A link off the site is not followed."""
        limit = self.site.get("links")
        if not limit:
            return
        done = set()
        for page, link in self.seen_links:
            u = urllib.parse.urlsplit(link)
            if u.scheme not in ("http", "https") or u.netloc.lower() != self.host:
                continue
            link = urllib.parse.urlunsplit((u.scheme, u.netloc, u.path or "/", u.query, ""))
            if link in done:
                continue
            if len(done) >= limit:
                break
            done.add(link)
            st, _, _, _ = self.get(link)
            if st != 200:
                self.find("link", link, f"{self.host}: a link answers {st or 'nothing'}",
                          f"{page} links to {link}: {st}", st, 200)

    def certificate(self):
        """cert_days: D (opt-in): the site's TLS certificate is valid for at least D more days."""
        want = self.site.get("cert_days")
        if not want:
            return
        url = self.base + "/"
        try:
            left = (self.certifier(self.host) - self.now()) / 86400
        except (OSError, ssl.SSLError, http.client.HTTPException, KeyError, ValueError) as e:
            self.find("certificate", url, f"{self.host}: the TLS certificate cannot be read", f"{self.host}: {e}", "unread", want)
            return
        if left < want:
            self.find("certificate", url, f"{self.host}: the TLS certificate expires in {max(left, 0):.0f} days",
                      f"{self.host}: {left:.1f} days left, the site asks for {want}", round(left, 1), want)

    def run(self):
        sitemaps = self.robots()
        urls = self.sitemap_urls(sitemaps)
        pages = [self.base + p for p in self.site.get("pages", ["/"])]
        for prefix in self.site.get("sample_prefixes", []):
            pick = sorted(u for u in urls if u.startswith(self.base + prefix) and u.rstrip("/") != (self.base + prefix).rstrip("/"))
            if pick:
                pages.append(pick[0])
        for i, u in enumerate(pages):
            self.page(u, alternates=i < len(self.site.get("pages", ["/"])))
        self.retired()
        self.broken_links()
        self.certificate()
        return {"site": self.site["name"], "urls": len(urls), "pages": len(pages), "requests": self.requests,
                "retries": self.retries, "findings": len(self.found)}


def main(fetcher=fetch):
    sites = common.sites()
    found, lines = [], []
    for site in sites:
        t = time.time()
        c = Check(site, fetcher)
        s = c.run()
        found += c.found
        lines.append(f"SEO SITE-CHECK {s['site']} urls={s['urls']} pages={s['pages']} requests={s['requests']} retries={s['retries']} "
                     f"findings={s['findings']} {time.time() - t:.0f}s")
    plan = os.environ.get("FLOTILLA_PLAN_FILE")
    if plan:
        with open(plan, "w") as f:
            json.dump({"outcome": "detections", "detections": found}, f)
    for ln in lines:
        print(ln, flush=True)
    for d in found:
        print(f"SEO SITE-CHECK finding {d['target']}: {d['detail'][:200]}", flush=True)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, common.Error) as e:
        print(f"SEO SITE-CHECK config: {e}", file=sys.stderr)
        sys.exit(2)
