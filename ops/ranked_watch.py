"""ops/ranked-watch: the performance watch of RFC-0017. Code only: no model, nothing outward, nothing written outside flotilla.

Each run reads one JSON source (a ranked list of items with a cost, and optionally the health signals of a service-level file),
ranks the top N by cost, compares them with the previous run's snapshot and reports:

  the brief   up to 20 lines for the routine's brief_section: the windows compared, the top 3 with their known fix and owner (or
              "no owner"), the changes (new, rose, dropped out, "fixed?" when a fix is listed), each signal against its objective
  the cases   one perf_regression detection per new top entry, per rise past both thresholds, and per breached service level.
              The target is perf:<id>, so a standing breach attaches to its open case and is not reopened each run.

A stale collector (its freshness row under its objective over the last day, or missing) is reported alone: the snapshot is kept
and nothing is compared. The first run records a baseline and reports no changes.

The snapshot is the routine's own ledger row (runtime/ledger.py, the namespace flotillad gives the run): the newest filing under
the fingerprint "snapshot". It is written after the report, so a run that fails to write it fails and its report is dropped.

Environment (the step's `with:` values, set on the command by the generator, and the worker's):
  RW_SOURCE    the source's NAME; its URL is SOURCE_<NAME> in the worker's environment (host config: ROUTINE_SOURCE_<NAME>), reached
               through the worker's egress proxy
  RW_LIST      the dotted path of the ranked list in the source (steps.7d)
  RW_KEY       the field that names an item (id)
  RW_COST      the field to rank by (lost)
  RW_TOP       how many to rank
  RW_RISE      ratio:at_least (0.5:600): a rise counts when it is at least that share of the previous cost and that many units
  RW_SIGNALS   source:fresh[:ids], the service-level source's name, the freshness row's id, and the comma-separated row ids to read
  RW_FIXES     the knowledge file of known fixes, under fleets/<fleet>/
  RW_UNIT      seconds: costs are shown as durations
  --fleet      the fleet the step belongs to
"""
import argparse
import datetime
import fnmatch
import json
import math
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "runtime"))
import common  # noqa: E402

KIND = "perf_regression"
MAX_LINES = 20
MAX_BYTES = 16 << 20
MAX_SNAPSHOT = 900_000   # under the ledger's 1 MiB row
MAX_SIGNALS = 50
MAX_TOP = 200
MAX_LABEL = 100
MAX_TITLE = 200
FINGERPRINT = "snapshot"
UNREADABLE = "unreadable snapshot"   # what a stored snapshot that is not JSON loads as
TARGET_OK = re.compile(r"[A-Za-z0-9._/-]{1,100}")


def source_url(name, environ=None):
    environ = os.environ if environ is None else environ
    if not re.fullmatch(r"[a-z][a-z0-9_]{0,31}", name or ""):
        raise common.ConfigError(f"source {name!r}: a name such as hub_ci")
    url = environ.get("SOURCE_" + name.upper(), "").strip()
    if not url:
        raise common.ConfigError(f"source {name}: SOURCE_{name.upper()} is not set (host config: ROUTINE_SOURCE_{name.upper()})")
    parts = urllib.parse.urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise common.ConfigError(f"source {name}: SOURCE_{name.upper()} is not an http or https URL")
    # plain http only for the hosts a test names in SOURCE_ALLOW_HTTP: a source is read over TLS
    allowed = [h.strip() for h in environ.get("SOURCE_ALLOW_HTTP", "").split(",") if h.strip()]
    if parts.scheme == "http" and parts.hostname not in allowed:
        raise common.ConfigError(f"source {name}: SOURCE_{name.upper()} is http; a source is read over https")
    return url


class SameHostRedirect(urllib.request.HTTPRedirectHandler):
    """A source may redirect within its own host and scheme, never elsewhere."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        a, b = urllib.parse.urlsplit(req.full_url), urllib.parse.urlsplit(newurl)
        if (a.scheme, a.hostname, a.port) != (b.scheme, b.hostname, b.port):
            raise common.ConfigError("the source redirected to another host or scheme; refused")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def fetch_json(url, timeout=10, deadline=30):
    """The source's JSON. `timeout` bounds each socket operation and `deadline` the whole fetch. Every failure is a ConfigError
    that never names the URL (host config's, and it may carry a credential)."""
    end = time.monotonic() + deadline
    try:
        with urllib.request.build_opener(SameHostRedirect).open(url, timeout=timeout) as r:
            chunks, n = [], 0
            while True:
                if time.monotonic() > end:
                    raise common.ConfigError(f"the source did not finish within {deadline}s")
                c = r.read1(65536)
                if not c:
                    break
                chunks.append(c)
                n += len(c)
                if n > MAX_BYTES:
                    raise common.ConfigError(f"the source is over {MAX_BYTES} bytes")
        return json.loads(b"".join(chunks))
    except common.ConfigError:
        raise
    except urllib.error.HTTPError as e:
        raise common.ConfigError(f"the source answered HTTP {e.code}") from None
    except ValueError:
        raise common.ConfigError("the source's body is not JSON (or was cut short)") from None
    except Exception as e:  # noqa: BLE001: a refused connection, a timeout, a TLS error
        raise common.ConfigError(f"the source could not be read ({type(e).__name__})") from None


def dig(doc, path):
    cur = doc
    for part in path.split("."):
        if not isinstance(cur, dict) or part not in cur:
            raise common.ConfigError(f"path {path}: no {part!r} in the source")
        cur = cur[part]
    return cur


def parse_fixes(text):
    """The known fixes: `fixes:` and a list of flat maps, `- item: <key>` then `  link: ...` and `  owner: ...`. A restricted YAML, so the
    worker needs no parser: whole-line comments, blank lines and quoted or plain scalars, nothing nested."""
    out, cur, seen_head = [], None, False
    for n, raw in enumerate(text.splitlines(), 1):
        line = "" if raw.lstrip().startswith("#") else raw.rstrip()
        if not line.strip():
            continue
        if not seen_head:
            if line.strip() != "fixes:":
                raise common.ConfigError(f"fixes line {n}: the file starts with 'fixes:'")
            seen_head = True
            continue
        m = re.fullmatch(r"(\s*)(- )?([a-z_]+):\s*(.*)", line)
        if not m or not m.group(1) and not m.group(2):
            raise common.ConfigError(f"fixes line {n}: '- item: <key>' or '  link: <text>', not {raw.strip()!r}")
        val = m.group(4).strip()
        if len(val) >= 2 and val[0] == val[-1] and val[0] in "\"'":
            val = val[1:-1]
        if m.group(2):
            cur = {}
            out.append(cur)
        elif cur is None:
            raise common.ConfigError(f"fixes line {n}: a key before the first '- '")
        if m.group(3) in cur:
            raise common.ConfigError(f"fixes line {n}: {m.group(3)} twice in one fix")
        cur[m.group(3)] = val
    for i, f in enumerate(out, 1):
        if not f.get("item"):
            raise common.ConfigError(f"fix {i}: no item (the key of the entry it targets)")
    return out


def load_fixes(path):
    if not path:
        return {}
    try:
        with open(path) as f:
            text = f.read()
    except FileNotFoundError:
        raise common.ConfigError(f"the fixes file the step names is missing ({os.path.basename(path)}): ship it, even empty") from None
    return {f["item"]: f for f in parse_fixes(text)}


def fix_text(fix):
    if not fix:
        return "no known fix, no owner"
    link = fix.get("link") or "a fix is listed"
    return f"fix {link}, " + (f"owner {fix['owner']}" if fix.get("owner") else "no owner")


def cost_text(v, unit):
    if unit == "seconds":
        s = int(round(v))
        if s >= 7200:
            return f"{s / 3600:.1f} h"
        if s >= 120:
            return f"{s / 60:.0f} min"
        return f"{s} s"
    return f"{v:g}"


def finite(x):
    return isinstance(x, (int, float)) and not isinstance(x, bool) and math.isfinite(x)


def clip(s, n):
    s = str(s)
    return s if len(s) <= n else s[:n - 1] + "\u2026"


def rank(rows, key, cost, top):
    """The top N rows by cost, largest first; a row with no key or no finite numeric cost is the source's fault and refuses the run."""
    if not isinstance(rows, list):
        raise common.ConfigError("the list path does not lead to a list")
    items = []
    for i, r in enumerate(rows):
        if not isinstance(r, dict) or not r.get(key) or not finite(r.get(cost)):
            raise common.ConfigError(f"row {i}: needs a {key} and a finite numeric {cost}")
        items.append(r)
    keys = [str(r[key]) for r in items]
    if len(set(keys)) != len(keys):
        raise common.ConfigError(f"the source lists a {key} twice")
    items.sort(key=lambda r: (-r[cost], str(r[key])))
    return items[:top], {str(r[key]): r for r in items}


def label(row, key):
    name = next((row[k] for k in ("name", "title", "step") if isinstance(row.get(k), str) and row[k]), str(row[key]))
    where = row.get("repo") if isinstance(row.get("repo"), str) else ""
    return clip(f"{name} ({where})" if where else name, MAX_LABEL)


def windows(doc, list_path):
    """steps.windows.<period> for steps.<period>: the period's bounds and the previous window's, as the source says them."""
    head, _, period = list_path.rpartition(".")
    try:
        w = dig(doc, f"{head}.windows.{period}" if head else f"windows.{period}")
    except common.ConfigError:
        return None
    return w if isinstance(w, dict) else None


def day(s):
    return str(s)[:10] if s else "?"


def windows_text(w, period):
    if not w:
        return f"{period}: the source names no window"
    held = {True: "held", False: "not held"}.get(w.get("prevHeld"), "unknown")
    return f"{period} {day(w.get('from'))} to {day(w.get('to'))}, previous window {day(w.get('prevFrom'))} to {day(w.get('prevTo'))} ({held})"


def stale(slo, fresh_id):
    """Why the collector's freshness row says the source is not current, or "" when it is. The 1d ratio is the freshest window the row has."""
    if not fresh_id:
        return ""
    rows = {r.get("id"): r for r in slo.get("rows", []) if isinstance(r, dict)}
    row = rows.get(fresh_id)
    if row is None:
        return f"the freshness row {fresh_id} is not in the service-level source"
    ratio = (row.get("ratio") or {}).get("1d")
    obj = row.get("objective")
    if not isinstance(ratio, (int, float)) or not isinstance(obj, (int, float)):
        return f"the freshness row {fresh_id} has no 1d ratio and objective"
    if ratio < obj:
        return f"the collector was fresh {ratio:.1%} of the last day, below its {obj:.1%} objective ({fresh_id})"
    return ""


def check_slo(slo):
    """The service-level source's shape, or a ConfigError: a dict with a list of dict rows whose ratio, when given, is a dict."""
    if not isinstance(slo, dict) or not isinstance(slo.get("rows"), list):
        raise common.ConfigError("the service-level source is not {rows: [...]}")
    for i, r in enumerate(slo["rows"]):
        if not isinstance(r, dict) or not (r.get("ratio") is None or isinstance(r.get("ratio"), dict)):
            raise common.ConfigError(f"service-level row {i}: not a map with a map for its ratio")


def signal_rows(slo, ids, skip):
    out = []
    for r in slo.get("rows", []):
        rid = r.get("id")
        if isinstance(rid, str) and rid != skip and any(fnmatch.fnmatchcase(rid, p) for p in ids):
            out.append(r)
    if len(out) > MAX_SIGNALS:
        raise common.ConfigError(f"the signal ids match {len(out)} rows, over {MAX_SIGNALS}: narrow them")
    return out


def pct(x):
    return f"{x:.1%}" if isinstance(x, (int, float)) else "?"


def signal_name(r):
    return " ".join(x for x in (r.get("service"), r.get("sli")) if x) or r.get("id")


def signal_line(r):
    ratio = r.get("ratio") or {}
    shown = next(((w, ratio[w]) for w in ("7d", "28d", "1d") if isinstance(ratio.get(w), (int, float))), ("", None))
    name = signal_name(r)
    return f"{name}: {pct(shown[1])} over {shown[0] or 'no window'} against {pct(r.get('objective'))}, {r.get('state', 'unknown')}"


def target(kind, rid):
    """The case target of a step ("step") or a service level ("slo"): their own namespaces, and None for an id that is not a plain
    target as it stands (flotillad's rule, with room for the namespace), so two ids are never merged by cleaning."""
    rid = str(rid)
    return f"perf:{kind}/{rid}" if TARGET_OK.fullmatch(rid) else None


def clean_snapshot(prev):
    """(the snapshot, "") when the stored one has the shape a run reads, (None, why) when it has not; None is no snapshot at all."""
    if prev is None:
        return None, ""
    def bad(why):
        return None, why
    if not isinstance(prev, dict):
        return bad("not a snapshot")
    if not (isinstance(prev.get("taken"), str) and isinstance(prev.get("list"), str) and isinstance(prev.get("top"), int)):
        return bad("no taken, list or top")
    items = prev.get("items")
    if not isinstance(items, dict) or not all(isinstance(v, dict) and finite(v.get("cost")) for v in items.values()):
        return bad("items are not a map of costs")
    if "signals" in prev and not (isinstance(prev["signals"], dict) and all(isinstance(v, str) for v in prev["signals"].values())):
        return bad("signals are not a map of names")
    carry = prev.get("carry", [])
    if not (isinstance(carry, list) and all(isinstance(d, dict) and all(isinstance(d.get(k), str) for k in ("kind", "target", "title", "detail"))
                                            for d in carry)):
        return bad("carried detections are not detections")
    return prev, ""


def compare(top, prev, key, cost, rise, fixes, everything):
    """(new, rose, dropped) against the previous snapshot's top: a baseline (prev None) has no changes. A dropped entry is "fixed?" only
    when a fix is listed and its cost now is below its cost before, or the source no longer lists it: leaving the top N because others
    grew is not a fix."""
    if prev is None:
        return [], [], []
    old = prev["items"]
    new = [r for r in top if str(r[key]) not in old]
    rose = []
    if rise:
        ratio, at_least = rise
        for r in top:
            o = old.get(str(r[key]))
            if o is None:
                continue
            d = r[cost] - o["cost"]
            if d >= at_least and d >= ratio * o["cost"]:
                rose.append((r, o["cost"]))
    now = {str(r[key]) for r in top}
    dropped = [(k, o) for k, o in old.items() if k not in now]
    dropped.sort(key=lambda t: -t[1]["cost"])
    out = []
    for k, o in dropped:
        row = everything.get(k)
        out.append((k, o, row, k in fixes and (row is None or row[cost] < o["cost"])))
    return new, rose, out


def build(cfg, doc, slo, prev, fixes, taken):
    """The run's report and the snapshot to keep: (report dict, snapshot dict or None). Pure: no network, no store."""
    period = cfg["list"].rpartition(".")[2]
    w = windows(doc, cfg["list"])
    tag = f"[{period}]"
    if slo is not None:
        check_slo(slo)
    prev, unreadable = clean_snapshot(prev)
    why = stale(slo, cfg["fresh"]) if slo is not None else ""
    if why:
        # Nothing is compared and no snapshot is written, but the cases already open are carried: a report that left them out would
        # close them (flotillad clears an incident its monitor stops reporting) and the next fresh run would open them again.
        carried = [dict(d, title=clip("carried, source stale: " + d["title"], MAX_TITLE)) for d in (prev or {}).get("carry", [])]
        return {"outcome": "detections", "detections": carried,
                "summary": [clip(f"{tag} the source is stale, nothing compared ({len(carried)} open cases carried): {why}", MAX_TITLE)]}, None
    unit = cfg["unit"]
    key, cost = cfg["key"], cfg["cost"]
    if cfg["top"] > MAX_TOP:
        raise common.ConfigError(f"top {cfg['top']}: at most {MAX_TOP}")
    top, everything = rank(dig(doc, cfg["list"]), key, cost, cfg["top"])
    # a ranking that is not the previous run's (another N or list) has nothing to compare with: every entry would be "new"
    same = prev is not None and prev.get("top") == cfg["top"] and prev.get("list") == cfg["list"]
    new, rose, dropped = compare(top, prev if same else None, key, cost, cfg["rise"], fixes, everything)
    found, lines = [], []
    head = f"{tag} top {len(top)} by {cost}, {windows_text(w, period)}"
    if same:
        head += f"; compared with the run of {day(prev['taken'])}"
    elif unreadable:
        head += f"; the previous snapshot is unreadable ({unreadable}): a new baseline, no changes"
    elif prev:
        head += f"; the ranking changed (top {prev.get('top')} of {prev.get('list')} before): a new baseline for it, no changes"
    else:
        head += "; the first run: a baseline, no changes"
    lines.append(head)

    def case(kind, rid, title, detail, measure):
        t = target(kind, rid)
        if t is None:
            lines.append(f"{tag} {str(rid)[:40]!r} is not a plain id (letters, digits . _ / -, at most 100): no case for it")
            return
        found.append({"kind": KIND, "target": t, "detail": clip(detail, MAX_TITLE), "title": clip(title, MAX_TITLE), "data": {"measure": measure}})

    for i, r in enumerate(top[:3], 1):
        lines.append(f"{tag} {i}. {label(r, key)}: {cost_text(r[cost], unit)}; {fix_text(fixes.get(str(r[key])))}")
    for r in new:
        lines.append(f"{tag} new in the top: {label(r, key)}, {cost_text(r[cost], unit)}")
        case("step", r[key], f"New in the top {cfg['top']}: {label(r, key)} costs {cost_text(r[cost], unit)}",
             f"new entry, {cost} {r[cost]:g}", {"name": cost, "value": r[cost], "threshold": 0})
    for r, was in rose:
        lines.append(f"{tag} rose: {label(r, key)}, {cost_text(was, unit)} to {cost_text(r[cost], unit)}")
        case("step", r[key], f"{label(r, key)} rose from {cost_text(was, unit)} to {cost_text(r[cost], unit)}",
             f"rose from {was:g} to {r[cost]:g}", {"name": cost, "value": r[cost], "threshold": was})
    for k, o, row, fixed in dropped:
        where = f"now {cost_text(row[cost], unit)}" if row is not None else "no longer listed"
        lines.append(f"{tag} dropped out: {clip(o.get('label', k), MAX_LABEL)}, was {cost_text(o['cost'], unit)}, {where}"
                     + ("; fixed? a fix is listed, confirm and remove it" if fixed else ""))
    seen = {}
    if slo is not None and cfg["signal_ids"]:
        # a snapshot from before signals were kept has no list to compare: nothing is new or gone then
        old = prev.get("signals") if prev else None
        for r in signal_rows(slo, cfg["signal_ids"], cfg["fresh"]):
            seen[str(r["id"])] = signal_name(r)
            fresh_row = old is not None and r["id"] not in old
            lines.append(f"{tag} {signal_line(r)}" + ("; new, no history, not compared" if fresh_row else ""))
            if r.get("state") == "breached":
                case("slo", r["id"], f"Service level breached: {signal_line(r)}", signal_line(r),
                     {"name": str(r.get("sli") or r["id"]), "value": (r.get("ratio") or {}).get("7d"), "threshold": r.get("objective")})
        for rid, name in sorted((old or {}).items()):
            if rid not in seen:
                lines.append(f"{tag} signal gone: {name} is no longer in the source")
    summary = [clip(l, MAX_TITLE) for l in lines[:MAX_LINES]]
    if len(lines) > MAX_LINES:
        summary[-1] = f"{tag} and {len(lines) - MAX_LINES + 1} more lines (the run's detections have them)"
    snap = {"taken": taken, "list": cfg["list"], "top": cfg["top"], "windows": w, "signals": seen, "carry": found,
            "items": {str(r[key]): {"cost": r[cost], "label": label(r, key)} for r in top}}
    if len(json.dumps(snap)) > MAX_SNAPSHOT:
        raise common.ConfigError(f"the snapshot would be over {MAX_SNAPSHOT} bytes (the ledger's row limit): lower top or narrow the labels")
    return {"outcome": "detections", "detections": found, "summary": summary}, snap


def parse_rise(s):
    if not s:
        return None
    m = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?):([0-9]+)", s)
    if not m:
        raise common.ConfigError(f"RW_RISE {s!r}: ratio:at_least such as 0.5:600")
    return float(m.group(1)), int(m.group(2))


def parse_signals(s):
    if not s:
        return None
    parts = s.split(":", 2)
    if len(parts) < 2 or not parts[0]:
        raise common.ConfigError(f"RW_SIGNALS {s!r}: source:fresh or source:fresh:ids")
    return parts[0], parts[1], [i for i in (parts[2] if len(parts) == 3 else "").split(",") if i]


def config(environ, fleet):
    top = environ.get("RW_TOP", "").strip()
    if not top.isdigit() or int(top) < 1:
        raise common.ConfigError(f"RW_TOP {top!r}: a number from 1")
    sig = parse_signals(environ.get("RW_SIGNALS", "").strip())
    fixes = environ.get("RW_FIXES", "").strip()
    if fixes and (".." in fixes.split("/") or fixes.startswith("/")):
        raise common.ConfigError(f"RW_FIXES {fixes!r}: a path inside the fleet's folder")
    root = environ.get("FLOTILLA_CODE_ROOT") or os.path.dirname(HERE)
    def need(name):
        if not environ.get(name, "").strip():
            raise common.ConfigError(f"{name} is not set")
        return environ[name].strip()

    return {"source": need("RW_SOURCE"), "list": need("RW_LIST"), "key": need("RW_KEY"), "cost": need("RW_COST"),
            "top": int(top), "rise": parse_rise(environ.get("RW_RISE", "").strip()),
            "signal_source": sig[0] if sig else "", "fresh": sig[1] if sig else "", "signal_ids": sig[2] if sig else [],
            "fixes": os.path.join(root, "fleets", fleet, fixes) if fixes else "", "unit": environ.get("RW_UNIT", "").strip()}


class LedgerStore:
    """The previous run's snapshot, in the routine's own ledger namespace (runtime/ledger.py): the newest filing under one fingerprint."""

    def __init__(self, client=None):
        if client is None:
            import ledger
            client = ledger.from_env()
        self.client = client

    def load(self):
        rows = self.client.filings({"fingerprint": FINGERPRINT})
        if not rows:
            return None
        try:
            return json.loads(rows[-1]["data"])
        except (ValueError, KeyError, TypeError):
            return UNREADABLE

    def save(self, snap):
        self.client.write_filing({"fingerprint": FINGERPRINT, "taken": snap["taken"], "data": json.dumps(snap, sort_keys=True)})


def read_source(name, fetch, environ):
    try:
        return fetch(source_url(name, environ))
    except common.ConfigError as e:
        raise common.ConfigError(f"source {name}: {e}") from None
    except Exception as e:  # noqa: BLE001: the message of a failing fetch may carry the URL, which is host config
        raise common.ConfigError(f"source {name}: could not be read ({type(e).__name__})") from None


def run(cfg, store, fetch=fetch_json, environ=None, now=None):
    environ = os.environ if environ is None else environ
    doc = read_source(cfg["source"], fetch, environ)
    slo = read_source(cfg["signal_source"], fetch, environ) if cfg["signal_source"] else None
    taken = (now or datetime.datetime.now(datetime.timezone.utc)).strftime("%Y-%m-%dT%H:%M:%SZ")
    prev = store.load()
    report, snap = build(cfg, doc, slo, prev, load_fixes(cfg["fixes"]), taken)
    common.report(report)
    if snap is not None:
        store.save(snap)
    return report, snap, "stale" if snap is None else "baseline" if prev is None else "compared"


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--fleet", required=True)
    a = ap.parse_args(argv)
    cfg = config(os.environ, a.fleet)
    report, snap, mode = run(cfg, LedgerStore())
    print(f"RANKED WATCH {cfg['list']} {mode} items={len((snap or {}).get('items', {}))} detections={len(report['detections'])}")


if __name__ == "__main__":
    try:
        main()
    except common.ConfigError as e:
        print(f"RANKED WATCH config: {e}", file=sys.stderr)
        sys.exit(2)
