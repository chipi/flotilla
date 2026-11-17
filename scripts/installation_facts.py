#!/usr/bin/env python3
"""No installation facts in flotilla's code (vision plan step 7): flotilla ships the platform and
example fleets; an installation's own facts (its repos, sites, hosts, addresses, user paths) live
in its private fleets repo and its host config.

Checks every tracked file except prose (docs/, *.md, the built site, llms.txt) for:
  - a GitHub slug of the owner's other repos (chipi/<repo> other than chipi/flotilla);
  - a tailnet host name (<node>.tail<hex>.ts.net) or a tailnet address (100.64.0.0/10);
  - a home path of a person (/Users/<name>, /home/<name>; the images' and CI's own users allowed);
  - with SF_FACTS_DENYLIST (a file, one word or domain per line, kept in the private fleets repo):
    any of its entries, so a host that has it checks the installation's own names too.
Placeholders pass: example/<repo>, example.com, *.test, *.invalid, documentation ranges, and a line
that says `facts: allow` (an invented value a test needs).

  python3 scripts/installation_facts.py      # exit 1, each finding as path:line: kind: text
"""
import ipaddress
import os
import re
import subprocess
import sys

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
PROSE = re.compile(r"^(docs/|site/|llms\.txt$|mkdocs\.yml$)|\.md$")
SLUG = re.compile(r"(?<![\w.-])chipi/(?!flotilla\b)[A-Za-z0-9_.-]+")
TAILNET = re.compile(r"\b[a-z0-9-]+\.tail[0-9a-f]+\.ts\.net\b")
IPV4 = re.compile(r"(?<![\d.])(\d{1,3}(?:\.\d{1,3}){3})(?![\d.])")
HOME = re.compile(r"/(?:Users|home)/([A-Za-z0-9._-]+)")
HOME_OK = {"worker", "dev", "runner", "user", "nonroot", "tester", "example", "you", "USER", "name", "w", "operator"}
CGNAT = ipaddress.ip_network("100.64.0.0/10")
# Tailscale's own fixed addresses, the same on every tailnet: not an installation's.
TAILSCALE_FIXED = {ipaddress.ip_address("100.100.100.100")}


def files():
    out = subprocess.run(["git", "-C", ROOT, "ls-files"], capture_output=True, text=True, check=True).stdout
    return [f for f in out.splitlines() if not PROSE.search(f)]


def denylist():
    path = os.environ.get("SF_FACTS_DENYLIST")
    if not path:
        return []
    with open(path) as f:
        words = [w.strip() for w in f if w.strip() and not w.startswith("#")]
    return [(w, re.compile(r"(?<![\w-])" + re.escape(w) + r"(?![\w-])", re.I)) for w in words]


def findings(text, deny):
    for n, line in enumerate(text.splitlines(), 1):
        if "facts: allow" in line:   # an invented value a test needs, said so on its own line
            continue
        for m in SLUG.finditer(line):
            yield n, "repo slug", m.group(0)
        for m in TAILNET.finditer(line):
            yield n, "tailnet name", m.group(0)
        for m in IPV4.finditer(line):
            try:
                ip = ipaddress.ip_address(m.group(1))
            except ValueError:
                continue
            if ip in CGNAT and ip not in TAILSCALE_FIXED:
                yield n, "tailnet address", m.group(1)
        for m in HOME.finditer(line):
            if m.group(1) not in HOME_OK:
                yield n, "home path", m.group(0)
        for w, rx in deny:
            if rx.search(line):
                yield n, "denylisted", w


def main():
    deny, bad = denylist(), 0
    for f in files():
        p = os.path.join(ROOT, f)
        try:
            with open(p, encoding="utf-8") as fh:
                text = fh.read()
        except (UnicodeDecodeError, FileNotFoundError, IsADirectoryError):
            continue
        for n, kind, what in findings(text, deny):
            print(f"{f}:{n}: {kind}: {what}")
            bad += 1
    print(f"installation facts: {'PASS' if not bad else f'FAIL ({bad})'}"
          f"{' (with the denylist)' if deny else ''}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
