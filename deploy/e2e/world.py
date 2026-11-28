"""The outside world the example fleets read, for the e2e and `make demo`: one TLS server that answers as
every host the examples' own specs name (Docker network aliases), with a certificate from the stack's own
CA, so the examples run unchanged against it.

  www.example.com, docs.example.com, shop.example.org   the sites/site-check example's sites
  errors.example.com, grafana.example.com               the triage example's sources (GlitchTip, Grafana)
  api.github.com, pypi.org, api.osv.dev                 the deps example's sources; GitHub also for triage and bugfix/intake
  ci-stats.example.com, slo-stats.example.com           the perf example's sources (RFC-0017): a CI step ranking and service levels
  health.example.com                                    the batch example's health route (flotilla.health/1, all up) and run outcome

Some answers are wrong on purpose, so each example has something to find: a page with no description, a
link that answers 404, the certificate (its validity is the stack's choice: run.sh makes it short), a
pinned package with a known vulnerability and one behind its latest release. Prints one line per
request, for the e2e to read.

  python3 world.py CERT KEY
"""
import base64
import json
import ssl
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PAGE = """<!doctype html><html><head><title>{title}</title>{desc}
<link rel="canonical" href="{url}">{extra}</head><body><h1>{title}</h1>{body}</body></html>"""


def page(url, title, desc="A page of the example shop.", extra="", body=""):
    d = f'<meta name="description" content="{desc}">' if desc else ""
    return PAGE.format(title=title, desc=d, url=url, extra=extra, body=body)


SHOP = "https://www.example.com"
DOCS = "https://docs.example.com"
LD = '<script type="application/ld+json">{"@type": "Store", "name": "Example Shop"}</script>'
ALT = lambda path: (f'<link rel="alternate" hreflang="en" href="{SHOP}{path}">'  # noqa: E731
                    f'<link rel="alternate" hreflang="de" href="{SHOP}/de{path}">')
SITES = {
    "www.example.com": {
        "/robots.txt": "User-agent: *\nAllow: /\n\nSitemap: " + SHOP + "/sitemap.xml\n",
        "/sitemap.xml": ('<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">'
                         + "".join(f"<url><loc>{SHOP}{p}</loc></url>" for p in
                                   ("/", "/products", "/products/kettle", "/guides/brewing", "/about", "/de/"))
                         + "</urlset>"),
        "/": page(SHOP + "/", "Example Shop: kettles and teapots", extra=LD + ALT("/"),
                  body='<a href="/products">Products</a> <a href="/about">About</a> <a href="/old-offers">Offers</a>'),
        "/products": page(SHOP + "/products", "All products", extra=ALT("/products"), body='<a href="/products/kettle">Kettle</a>'),
        "/products/kettle": page(SHOP + "/products/kettle", "The steel kettle", extra=LD + ALT("/products/kettle")),
        "/guides/brewing": page(SHOP + "/guides/brewing", "How to brew tea", extra=ALT("/guides/brewing")),
        "/about": page(SHOP + "/about", "About Example Shop", desc="", extra=ALT("/about")),   # no description: a finding
        "/de/": page(SHOP + "/de/", "Example Shop: Wasserkocher", extra=ALT("/")),
        "/de/products": page(SHOP + "/de/products", "Alle Produkte", extra=ALT("/products")),
        "/de/about": page(SHOP + "/de/about", "Über Example Shop", extra=ALT("/about")),
        # /old-offers is linked from the home page and answers 404: a broken link
    },
    "docs.example.com": {
        "/robots.txt": "User-agent: *\nAllow: /\n",
        "/sitemap.xml": ('<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">'
                         f"<url><loc>{DOCS}/</loc></url><url><loc>{DOCS}/guide/</loc></url></urlset>"),
        "/": page(DOCS + "/", "Example Shop docs", desc="How to use the shop's API.", body='<a href="/guide/">Guide</a>'),
        "/guide/": page(DOCS + "/guide/", "Getting started", desc="The first steps with the shop's API."),
    },
    "shop.example.org": {},   # the retired host: every page answers 404, as it should
}

# The triage example's sources: one unresolved error in the shop (GlitchTip) and one firing alert on the api
# (Grafana's Alertmanager). The scripted model (stub.py) files the first and escalates the second.
GLITCHTIP_PROJECTS = [{"id": 1, "slug": "shop"}, {"id": 2, "slug": "api"}]
GLITCHTIP_ISSUES = [{
    "id": "101", "shortId": "SHOP-12", "title": "TypeError: Cannot read properties of undefined (reading 'price')",
    "culprit": "cart/total.js in sumLines", "status": "unresolved", "level": "error", "count": 42,
    "firstSeen": "2026-11-17T08:00:00Z", "lastSeen": "2026-11-17T09:30:00Z", "project": {"id": 1, "slug": "shop"},
    "metadata": {"type": "TypeError", "value": "Cannot read properties of undefined (reading 'price')"},
}]
ALERTS = [{
    "labels": {"alertname": "ApiErrorRateHigh", "instance": "prod-api", "severity": "warning"},
    "annotations": {"summary": "5xx responses above 2% for 10 minutes on the api"},
    "status": {"state": "active"}, "startsAt": "2026-11-17T09:00:00Z", "fingerprint": "a1b2c3d4e5f60718",
}]

# The bugfix example's intake: one open bug issue in the shop, as triage would have filed it.
BUG_ISSUES = [{
    "number": 7, "title": "Cart total crashes on a line without a price", "state": "open",
    "html_url": "https://github.com/example/shop/issues/7", "updated_at": "2026-11-17T09:40:00Z",
    "labels": [{"name": "bug"}],
    "body": "sumLines in cart/total.js throws TypeError: Cannot read properties of undefined (reading 'price') "
            "when a cart line has no price.\n\nAcceptance: sumLines skips or rejects a line without a price "
            "instead of throwing (code-invariant).",
}]

# The performance watch's sources (RFC-0017, fleets/perf): a ranked list of slow CI steps with the windows it covers, and the service
# levels of the runners with the collector's own freshness row (fresh: the watch compares). The first run is a baseline.
CI_STATS = {"steps": {
    "7d": [{"id": f"e2e-step-{i}", "repo": "example/shop", "workflow": "CI", "job": "test", "step": f"Step {i}", "lost": 1000 - 100 * i,
            "x3": {"med": 100, "n": 5}, "gh": {"med": 20, "n": 9}, "x3Prev": None} for i in range(6)],
    "yesterday": [{"id": "e2e-step-0", "repo": "example/shop", "workflow": "CI", "job": "test", "step": "Step 0", "lost": 150,
                   "x3": {"med": 100, "n": 1}, "gh": {"med": 20, "n": 2}, "x3Prev": None}],
    "windows": {p: {"from": "2026-11-10T00:00:00Z", "to": "2026-11-17T00:00:00Z", "prevFrom": "2026-11-03T00:00:00Z",
                    "prevTo": "2026-11-10T00:00:00Z", "prevHeld": True} for p in ("7d", "yesterday")}}}
SLO_STATS = {"rows": [
    {"id": "ci-runners--online", "service": "CI runners", "sli": "online", "objective": 0.99,
     "ratio": {"28d": 0.97, "7d": 0.97, "1d": 0.97}, "state": "breached"},
    {"id": "ci-collector--fresh-within-15-min", "service": "CI collector", "sli": "fresh within 15 min", "objective": 0.99,
     "ratio": {"28d": 1, "7d": 1, "1d": 1}, "state": "meeting"}]}

# The health example's route (docs/wip/health-contract.md, flotilla.health/1): every component up, the capability ready. The batch example's
# probe reads it as an installation's system that is healthy, so no case opens and the no-effect run of the probe stays clean. RUN_OUTCOME
# is the T5 shape the re-run's check reads; no real producer serves it yet.
HEALTH = {"contract": "flotilla.health/1", "status": "up", "checked_at": "2026-11-28T10:00:00Z", "version": "sha-e2e",
          "components": [{"name": n, "kind": "service", "status": "up", "since": "2026-11-27T03:10:00Z", "reason": "", "latency_ms": 12,
                          "checked_at": "2026-11-28T10:00:00Z"} for n in ("model-prod", "asr", "database")],
          "capabilities": {"scheduled-run": {"ready": True, "needs": ["model-prod", "asr", "database"], "blocked_by": []}}}
RUN_OUTCOME = {"run_id": "run-e2e", "trigger": "on_demand", "status": "finished", "started_at": "2026-11-28T01:05:00Z",
               "finished_at": "2026-11-28T02:40:00Z", "episodes": {"processed": 3, "failed": [], "skipped": 9}}

SHA = "4f2c1d0e9b8a7f6e5d4c3b2a19087f6e5d4c3b2a"
FILES = {
    ("example/shop", "requirements.txt"): "flask==3.0.0\nrequests==2.31.0\nidna>=3.0\n",
    ("example/api", "requirements/base.txt"): "fastapi==0.110.0\npydantic==2.9.2\n",
}
LATEST = {"flask": "3.0.3", "requests": "2.32.3", "fastapi": "0.115.6", "pydantic": "2.9.2"}
VULNS = {("requests", "2.31.0"): ["GHSA-9wx4-h78v-vm56"]}


class H(BaseHTTPRequestHandler):
    def send(self, code, body, ctype="text/html; charset=utf-8"):
        b = body.encode() if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def json(self, obj, code=200):
        self.send(code, json.dumps(obj), "application/json")

    def host(self):
        return (self.headers.get("Host") or "").split(":")[0].lower()

    def do_GET(self):
        host, path = self.host(), self.path.split("?")[0]
        print(f"world GET {host}{self.path}", flush=True)
        if host in SITES:
            body = SITES[host].get(path)
            if body is None:
                return self.send(404, "not found")
            ctype = "application/xml" if path.endswith(".xml") else "text/plain" if path.endswith(".txt") else "text/html; charset=utf-8"
            return self.send(200, body, ctype)
        if host == "ci-stats.example.com" and path == "/ci.json":
            return self.json(CI_STATS)
        if host == "slo-stats.example.com" and path == "/slo.json":
            return self.json(SLO_STATS)
        if host == "health.example.com" and path == "/health/ready":
            return self.json(HEALTH)
        if host == "health.example.com" and path == "/runs/latest":
            return self.json(RUN_OUTCOME)
        if host == "errors.example.com":   # GlitchTip's API
            if path.endswith("/projects/"):
                return self.json(GLITCHTIP_PROJECTS)
            if path.endswith("/issues/"):
                return self.json(GLITCHTIP_ISSUES)
            if "/events/latest/" in path:
                return self.json({"entries": [], "tags": [], "message": GLITCHTIP_ISSUES[0]["title"]})
            return self.json([])
        if host == "grafana.example.com" and path == "/api/alertmanager/grafana/api/v2/alerts":
            return self.json(ALERTS)
        if host == "api.github.com" and path == "/search/issues":
            return self.json({"total_count": 0, "items": []})
        if host == "api.github.com" and path == "/repos/example/shop/issues":   # bugfix/intake's open bug issues
            return self.json(BUG_ISSUES if "labels=bug" in self.path else [])
        if host == "api.github.com":
            parts = path.strip("/").split("/")   # repos/OWNER/NAME/commits/HEAD | repos/OWNER/NAME/contents/PATH...
            if len(parts) >= 5 and parts[0] == "repos":
                repo = "/".join(parts[1:3])
                if parts[3] == "commits" and parts[4] == "HEAD" and any(r == repo for r, _ in FILES):
                    return self.json({"sha": SHA})
                if parts[3] == "contents":
                    text = FILES.get((repo, "/".join(parts[4:])))
                    if text is not None:
                        return self.json({"encoding": "base64", "content": base64.b64encode(text.encode()).decode()})
            return self.json({"message": "Not Found"}, 404)
        if host == "pypi.org" and path.startswith("/pypi/") and path.endswith("/json"):
            name = path.split("/")[2]
            if name in LATEST:
                v = LATEST[name]
                return self.json({"info": {"version": v}, "releases": {v: [{"yanked": False}], "1.0.0": [{"yanked": False}]}})
            return self.json({"message": "Not Found"}, 404)
        self.send(404, "no such host here")

    def record_write(self, method, host, body):
        """Every write is one structured line: who sent it (the proxy's address: the live proxy and a shadow's have their own), to what, and
        whether it carried a credential. The e2e's no-effect test counts them per proxy."""
        auth = "yes" if self.headers.get("Authorization") else "no"
        print(f"world WRITE {method} {host}{self.path} client={self.client_address[0]} auth={auth} body_bytes={len(body)}", flush=True)

    def do_PUT(self):
        self.record_write("PUT", self.host(), self.rfile.read(int(self.headers.get("Content-Length") or 0)))
        self.send(404, "no such endpoint here")

    def do_PATCH(self):
        self.record_write("PATCH", self.host(), self.rfile.read(int(self.headers.get("Content-Length") or 0)))
        self.send(404, "no such endpoint here")

    def do_DELETE(self):
        self.record_write("DELETE", self.host(), b"")
        self.send(404, "no such endpoint here")

    def do_POST(self):
        host = self.host()
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        print(f"world POST {host}{self.path}", flush=True)
        if not (host == "api.github.com" and self.path == "/graphql") and not (host == "api.osv.dev" and self.path == "/v1/querybatch"):
            self.record_write("POST", host, body)   # a read made as a POST (GraphQL queries, OSV's batch) is not a write
        if host == "api.github.com" and self.path == "/graphql":   # triage's sync and stale reads: nothing open
            return self.json({"data": {"repository": None, "search": {"nodes": []}}})
        if host == "api.osv.dev" and self.path == "/v1/querybatch":
            qs = json.loads(body or b"{}").get("queries", [])
            return self.json({"results": [{"vulns": [{"id": i} for i in VULNS.get((q["package"]["name"], q["version"]), [])]}
                                          for q in qs]})
        self.send(404, "no such endpoint here")

    def log_message(self, *a):
        pass


def main(cert, key):
    srv = ThreadingHTTPServer(("0.0.0.0", 443), H)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(cert, key)
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    print("world listening on 443", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
