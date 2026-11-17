"""The outside world the example fleets read, for the e2e and `make demo`: one TLS server that answers as
every host the examples' own specs name (Docker network aliases), with a certificate from the stack's own
CA, so the examples run unchanged against it.

  www.example.com, docs.example.com, shop.example.org   the sites/site-check example's sites
  api.github.com, pypi.org, api.osv.dev                 the deps example's sources

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

    def do_POST(self):
        host = self.host()
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        print(f"world POST {host}{self.path}", flush=True)
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
