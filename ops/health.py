"""ops/health: what the routines share to read a system's health route (docs/wip/health-contract.md, contract flotilla.health/1).

A route is named, never spelled in a spec: the source's NAME is a block param, its URL is host config (SOURCE_<NAME> in the worker's
environment, ROUTINE_SOURCE_<NAME> in the host's), reached through the worker's egress proxy like every read, over https unless the host is
named in HEALTH_ALLOW_HTTP (a test's). The fetch keeps ranked-watch's hardening: a whole-fetch deadline, a size cap, no redirect to
another host or scheme, and no message that names the URL (it is host config and may carry a credential). A 503 is read for its body
(H7: the body is the answer, the code is not).

A body that is not the contract, or has the wrong shape, is refused, and a refused body is unknown, never up. So is a component or a
capability the body does not name. Only `status: up` is up, whatever its reason says: a server busy with a long job reports up with
`busy` in its reason (H11) and is up. Python stdlib only.
"""
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request

import common

CONTRACT = "flotilla.health/1"
STATUSES = ("up", "degraded", "down", "unknown")
MAX_BYTES = 1 << 20            # a health body is small: the producer's route answers in 5 s with a list of components
MAX_ITEMS = 500
NAME_RE = re.compile(r"^[a-z][a-z0-9-]{0,63}$")          # H6: kebab-case, never renamed
SOURCE_RE = re.compile(r"^[a-z][a-z0-9_]{0,31}$")


class Unreadable(common.ConfigError):
    """The route could not be read, or its body is not the contract: the caller has no answer, which is not "up"."""


class Refused(Unreadable):
    """The body was read and is not flotilla.health/1, or is not shaped as it."""


def source_url(name, environ=None):
    environ = os.environ if environ is None else environ
    if not SOURCE_RE.match(name or ""):
        raise common.ConfigError(f"health source {name!r}: a name such as pipeline_health")
    url = environ.get("SOURCE_" + name.upper(), "").strip()
    if not url:
        raise common.ConfigError(f"health source {name}: SOURCE_{name.upper()} is not set (host config: ROUTINE_SOURCE_{name.upper()})")
    parts = urllib.parse.urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise common.ConfigError(f"health source {name}: SOURCE_{name.upper()} is not an http or https URL")
    allowed = [h.strip() for h in environ.get("HEALTH_ALLOW_HTTP", "").split(",") if h.strip()]
    if parts.scheme == "http" and parts.hostname not in allowed:
        raise common.ConfigError(f"health source {name}: SOURCE_{name.upper()} is http; a health route is read over https")
    return url


class SameHostRedirect(urllib.request.HTTPRedirectHandler):
    """A route may redirect within its own host and scheme, never elsewhere."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        a, b = urllib.parse.urlsplit(req.full_url), urllib.parse.urlsplit(newurl)
        if (a.scheme, a.hostname, a.port) != (b.scheme, b.hostname, b.port):
            raise Unreadable("the health route redirected to another host or scheme; refused")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def _slurp(r, end, deadline):
    chunks, n = [], 0
    while True:
        if time.monotonic() > end:
            raise Unreadable(f"the health route did not finish within {deadline}s")
        c = r.read1(65536)
        if not c:
            return b"".join(chunks)
        chunks.append(c)
        n += len(c)
        if n > MAX_BYTES:
            raise Unreadable(f"the health route's body is over {MAX_BYTES} bytes")


def fetch(url, timeout=10, deadline=30):
    """The route's validated body. `timeout` bounds each socket operation and `deadline` the whole fetch. Every failure is Unreadable and
    none names the URL."""
    return parse_bytes(fetch_raw(url, timeout, deadline))


def fetch_raw(url, timeout=10, deadline=30):
    """The route's bytes, the same fetch for any JSON route of the same worker (the run outcome the re-run's check reads)."""
    end = time.monotonic() + deadline
    try:
        try:
            with urllib.request.build_opener(SameHostRedirect).open(url, timeout=timeout) as r:
                raw = _slurp(r, end, deadline)
        except urllib.error.HTTPError as e:
            if e.code != 503:
                raise Unreadable(f"the health route answered HTTP {e.code}") from None
            with e:
                raw = _slurp(e, end, deadline)   # H7: a 503 carries the same body
        return raw
    except Unreadable:
        raise
    except Exception as e:  # noqa: BLE001: a refused connection, a timeout, a TLS error
        raise Unreadable(f"the health route could not be read ({type(e).__name__})") from None


def _strs(v):
    return isinstance(v, list) and len(v) <= MAX_ITEMS and all(isinstance(x, str) and NAME_RE.match(x) for x in v)


def parse_bytes(raw):
    try:
        obj = json.loads(raw)
    except ValueError:
        raise Refused("the health route's body is not JSON (or was cut short)") from None
    return parse(obj)


def parse(obj):
    """The body, when it is flotilla.health/1 and shaped as H1 and H5 say; Refused otherwise. Nothing is repaired."""
    def no(why):
        raise Refused(f"the health route's body is refused: {why}")
    if not isinstance(obj, dict):
        no("not a JSON object")
    if obj.get("contract") != CONTRACT:
        no(f"its contract is not {CONTRACT}")
    if obj.get("status") not in STATUSES:
        no("status is not up, degraded, down or unknown")
    comps = obj.get("components")
    if not isinstance(comps, list) or len(comps) > MAX_ITEMS:
        no("components is not a list")
    seen = set()
    for c in comps:
        if not isinstance(c, dict) or not isinstance(c.get("name"), str) or not NAME_RE.match(c["name"]):
            no("a component has no kebab-case name")
        if c.get("status") not in STATUSES:
            no(f"component {c['name']}: status is not up, degraded, down or unknown")
        if c["name"] in seen:
            no(f"component {c['name']} is listed twice")
        seen.add(c["name"])
    caps = obj.get("capabilities")
    if not isinstance(caps, dict) or len(caps) > MAX_ITEMS:
        no("capabilities is not an object")
    for name, cap in caps.items():
        if not NAME_RE.match(name) or not isinstance(cap, dict):
            no(f"capability {str(name)[:40]!r} is not a kebab-case name with an object")
        if not isinstance(cap.get("ready"), bool) or not _strs(cap.get("needs")) or not _strs(cap.get("blocked_by")):
            no(f"capability {name}: ready is true or false, needs and blocked_by are lists of component names")
        if cap["ready"] and cap["blocked_by"]:
            no(f"capability {name} is ready and blocked at once")
    return obj


def read(name, environ=None, fetch=fetch):
    """The named route's body, or Unreadable. A source that is not configured is a ConfigError (the flow's mistake, not an answer)."""
    url = source_url(name, environ)
    try:
        return parse(fetch(url))
    except common.ConfigError:
        raise
    except Exception as e:  # noqa: BLE001: the message of a failing fetch may carry the URL, which is host config
        raise Unreadable(f"health source {name}: could not be read ({type(e).__name__})") from None


def fact_body(name, environ=None, fetch=fetch):
    """The body for a fact read: None when it cannot be read or is refused (unknown), never a guess. An unconfigured source still raises."""
    try:
        return read(name, environ, fetch)
    except Unreadable:
        return None


def _component(body, name):
    for c in body["components"]:
        if c["name"] == name:
            return c
    return None


def component_status(body, name):
    """up, degraded, down or unknown. A component the body does not name is unknown."""
    c = _component(body, name)
    return c["status"] if c else "unknown"


def component_up(body, name):
    return component_status(body, name) == "up"


def component_since(body, name):
    c = _component(body, name)
    s = c.get("since") if c else None
    return s if isinstance(s, str) else None


def component_reason(body, name):
    c = _component(body, name)
    r = c.get("reason") if c else None
    return r if isinstance(r, str) else ""


def capability_ready(body, name):
    """True, False, or None (unknown): the capability the body does not name is unknown. The producer's `ready` is trusted only as far as
    its own components show: ready with a need that is not up is False, and ready with a need the body does not list is unknown."""
    cap = body["capabilities"].get(name)
    if cap is None:
        return None
    if not cap["ready"]:
        return False
    states = [component_status(body, n) if _component(body, n) else None for n in cap["needs"]]
    if any(s is None for s in states):
        return None
    return all(s == "up" for s in states)


def blocked_by(body, name):
    cap = body["capabilities"].get(name)
    return list(cap["blocked_by"]) if cap else []


def needs(body, name):
    cap = body["capabilities"].get(name)
    return list(cap["needs"]) if cap else []
