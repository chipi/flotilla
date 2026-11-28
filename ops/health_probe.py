"""ops/health-probe (block health-probe@1): the code-only monitor of a system's health route (docs/wip/health-contract.md,
flotilla.health/1). Each run reads the route once, takes one reading of each thing it watches (a capability's `ready`, or a component's
status) and reports one detection per thing, so flotillad opens a case and clears it when a run no longer reports it. It plans nothing and
reaches nothing but the route, through the worker's egress proxy.

  action       the registry action whose precondition names what is watched: `capability_ready(<capability>)` or `component_up(<name>)`.
               The condition has one home, the one the acting step re-checks with a fresh read. Everything it names must be watched
  capability   the capability to watch (exactly one of capability and components)
  components   the components to watch, one case each (a, b)
  instance     the case's host: the target is host:<instance>, and /<component> for a component's case
  report       down (default): a detection while it has read not up for `readings` readings in a row, held until `clear_after` readings in
               a row read up. recovered, the readiness gate: after an outage of `readings` readings, a detection once it has read up for
               `clear_after` readings in a row, for at most MAX_RECOVERED_RUNS runs (the act step plans the re-run on it)
  readings, clear_after   counts of this probe's own runs: the step's `every` is the interval between them

Unknown is not up and not down: a capability or component the body does not name, a component that says `unknown`, breaks both streaks, so
it never opens a case and never clears or recovers one. A route that cannot be read, or whose body is not the contract, fails the run:
nothing is reported, the snapshot is kept and no case is cleared on the strength of a broken read.

State is the probe's own snapshot in the routine's ledger namespace (the newest filing under the fingerprint "snapshot", as ranked-watch
keeps its own): the route has no history, and the producer's `since` says when a status began but not how long a case was open. It is
written after the report, so a run that fails to write it fails. An unreadable snapshot fails the run (starting over would clear cases).

Environment (the block's params, set on the run's command by the generator, and the worker's):
  PROBE_ACTION, PROBE_SOURCE (the route's NAME: its URL is SOURCE_<NAME>, host config), PROBE_CAPABILITY or PROBE_COMPONENTS (a, b),
  PROBE_INSTANCE, PROBE_REPORT, PROBE_READINGS, PROBE_CLEAR_AFTER, HEALTH_ALLOW_HTTP, FLOTILLA_PLAN_FILE. --kind is the flow's case kind.
"""
import datetime
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "runtime"))
import common  # noqa: E402
import free_ratio  # noqa: E402
import health  # noqa: E402
import service_up  # noqa: E402

FINGERPRINT = "snapshot"
UNREADABLE = "unreadable snapshot"   # what a stored snapshot that is not JSON loads as
KIND_RE = re.compile(r"^[a-z][a-z0-9_]{0,39}$")
MAX_COUNT = 100
MAX_DETECTIONS = 50
MAX_RECOVERED_RUNS = 12      # two hours at a 10m step: the gate reports a recovery this long, then waits for the next outage
MAX_TEXT = 200
CAPABILITY, COMPONENT = "capability_ready", "component_up"
UP, DOWN = 1.0, 0.0


class Config:
    def __init__(self, **kw):
        self.__dict__.update(kw)


def clean(text, limit=MAX_TEXT):
    """A producer's words, as a case may carry them: one line, no angle brackets, no control characters, cut."""
    t = "".join(c if c.isprintable() else " " for c in str(text or ""))
    t = " ".join(t.split()).replace("<", "‹").replace(">", "›")
    return t if len(t) <= limit else t[:limit - 1] + "…"


def parse_config(environ, kind):
    def val(name):
        return (environ.get(name) or "").strip()

    def count(name, default):
        v = val(name) or default
        if not v.isdigit() or not 1 <= int(v) <= MAX_COUNT:
            raise common.ConfigError(f"{name} is a count from 1 to {MAX_COUNT}, not {v[:20]!r}")
        return int(v)

    def names(raw, what):
        out = [n.strip() for n in raw.split(",")]
        if not all(health.NAME_RE.match(n) for n in out) or len(set(out)) != len(out):
            raise common.ConfigError(f"{what} is distinct kebab-case names, not {raw[:60]!r}")
        return out
    if not KIND_RE.match(kind or ""):
        raise common.ConfigError(f"--kind {str(kind)[:40]!r} is not a case kind")
    action, source = val("PROBE_ACTION"), val("PROBE_SOURCE")
    if not action:
        raise common.ConfigError("PROBE_ACTION is not set")
    if not health.SOURCE_RE.match(source):
        raise common.ConfigError(f"PROBE_SOURCE {source[:40]!r}: a source name such as pipeline_health")
    cap, comps = val("PROBE_CAPABILITY"), val("PROBE_COMPONENTS")
    if bool(cap) == bool(comps):
        raise common.ConfigError("a probe watches a capability (PROBE_CAPABILITY) or components (PROBE_COMPONENTS), one of them")
    capability = names(cap, "PROBE_CAPABILITY")[0] if cap else None
    if cap and "," in cap:
        raise common.ConfigError("PROBE_CAPABILITY is one capability")
    instance = val("PROBE_INSTANCE")
    if not free_ratio.INSTANCE_RE.match(instance):
        raise common.ConfigError(f"PROBE_INSTANCE {instance[:40]!r}: letters, digits, dot, dash and underscore, no port")
    report = val("PROBE_REPORT") or "down"
    if report not in ("down", "recovered"):
        raise common.ConfigError(f"PROBE_REPORT is down or recovered, not {report[:20]!r}")
    return Config(kind=kind, action=action, source=source, capability=capability, components=names(comps, "PROBE_COMPONENTS") if comps else [],
                  instance=instance, report=report, readings=count("PROBE_READINGS", "2"), clear_after=count("PROBE_CLEAR_AFTER", "2"))


def check_watched(cfg, action):
    """What the action's preconditions name is what the probe watches: a flow that watches another thing would open cases the acting step
    can only call resolved."""
    want = CAPABILITY if cfg.capability else COMPONENT
    named = []
    for c in common.preconditions(action, "propose"):
        name, arg = common.parse(c)
        if name == want:
            named.append(arg)
    if not named:
        raise common.ConfigError(f"{cfg.action} has no {want} precondition for the probe to read")
    watched = [cfg.capability] if cfg.capability else cfg.components
    missing = [n for n in named if n not in watched]
    if missing:
        raise common.ConfigError(f"{cfg.action} names {', '.join(str(m) for m in missing)} in {want}, which the probe does not watch")


def fresh():
    return {"opened": False, "down": 0, "up": 0, "armed": False, "reported": 0}


def advance(st, r, need_down, need_up):
    """The state after one reading (1 up, 0 down, None unknown): the same rule as the service monitor's scan. A group opens at need_down
    down readings in a row and closes at need_up up readings in a row; an unknown reading counts for neither and breaks both streaks."""
    st = dict(st)
    if r is None:
        st["down"] = st["up"] = 0
    elif r < 1:
        st["down"], st["up"] = st["down"] + 1, 0
    else:
        st["down"], st["up"] = 0, st["up"] + 1
    if st["down"] >= need_down:
        st["opened"] = True
    elif st["opened"] and st["up"] >= need_up:
        st["opened"] = False
    return st


def readings_of(cfg, body):
    """{key: (reading, what it is, the producer's facts)}: one reading per watched thing, 1 up, 0 down, None unknown."""
    out = {}
    if cfg.capability:
        ready = health.capability_ready(body, cfg.capability)
        blockers = []
        for n in health.blocked_by(body, cfg.capability) or [n for n in health.needs(body, cfg.capability) if not health.component_up(body, n)]:
            blockers.append({"name": n, "status": health.component_status(body, n), "reason": clean(health.component_reason(body, n), 120),
                             "since": health.component_since(body, n)})
        facts = {"needs": health.needs(body, cfg.capability), "blocked_by": [b["name"] for b in blockers], "blockers": blockers,
                 "reason": blockers[0]["reason"] if blockers else "", "since": min((b["since"] for b in blockers if b["since"]), default=None)}
        out["capability:" + cfg.capability] = (None if ready is None else UP if ready else DOWN, cfg.capability, facts)
    for n in cfg.components:
        s = health.component_status(body, n)
        facts = {"status": s, "reason": clean(health.component_reason(body, n), 120), "since": health.component_since(body, n)}
        out["component:" + n] = (None if s == "unknown" else DOWN if s == "down" else UP, n, facts)
    return out


def target_of(cfg, name):
    if cfg.capability:
        return service_up.encode_target(["instance"], {"instance": cfg.instance})
    return service_up.encode_target(["instance", "component"], {"instance": cfg.instance, "component": name})


def build(cfg, body, prev, taken):
    """(the run's report, the snapshot to keep). Pure: no network, no store. prev is the stored snapshot or None for a first run."""
    if prev == UNREADABLE:
        raise common.ConfigError("the previous snapshot is unreadable: the run fails rather than start over and clear cases")
    old = (prev or {}).get("items") or {}
    seen = readings_of(cfg, body)
    items, found, summary = {}, [], []
    for key in sorted(seen):
        r, name, facts = seen[key]
        st = advance({**fresh(), **(old.get(key) or {})}, r, cfg.readings, cfg.clear_after)
        report, held = False, False
        if r is None:
            summary.append(f"{key.partition(':')[0]} {name} is unknown in the route's body: not up, not down; no case opened or cleared on it")
        if cfg.report == "down":
            report, held = st["opened"], st["down"] < cfg.readings
        else:
            if st["down"] >= cfg.readings:
                st["armed"], st["reported"] = True, 0
            if st["armed"] and r == UP and st["up"] >= cfg.clear_after:
                if st["reported"] < MAX_RECOVERED_RUNS:
                    st["reported"] += 1
                    report = True
                else:
                    st["armed"] = False
        items[key] = st
        if not report:
            continue
        target = target_of(cfg, name)
        if target is None:
            raise RuntimeError(f"{name} on {cfg.instance} cannot make a case target: the run fails rather than leave it unreported")
        found.append(detection(cfg, key, name, r, st, held, facts, target, taken))
    if len(found) > MAX_DETECTIONS:
        summary.insert(0, f"{len(found)} things are reported: the first {MAX_DETECTIONS} are kept")
        found = found[:MAX_DETECTIONS]
    snap = {"taken": taken, "items": items}
    rep = {"outcome": "detections", "detections": found}
    if summary:
        rep["summary"] = [clean(s) for s in summary[:20]]
    return rep, snap


def detection(cfg, key, name, r, st, held, facts, target, taken):
    what = "capability" if cfg.capability else "component"
    inst = cfg.instance
    if cfg.report == "recovered":
        title = f"{name} is ready again on {inst}: the window's run can be repeated"
        detail = (f"{name} read ready for the last {st['up']} readings after an outage of {cfg.readings} readings or more "
                  f"(the report lasts at most {MAX_RECOVERED_RUNS} runs)")
    else:
        blockers = facts.get("blocked_by") or []
        title = (f"{name} is not ready on {inst}" + (f": blocked by {', '.join(blockers)}" if blockers else "") if cfg.capability
                 else f"{name} is {facts['status']} on {inst}") + (", not yet clear" if held else "")
        if not held:
            detail = f"{what} {name} read not up for the last {st['down']} readings (a detection needs {cfg.readings})"
        elif r is None:
            detail = f"{what} {name} has no reading now (unknown is not up): held until {cfg.clear_after} readings in a row read up"
        elif r < 1:
            detail = f"{what} {name} read not up again: held until {cfg.clear_after} readings in a row read up"
        else:
            detail = f"{what} {name} reads up again: held until {cfg.clear_after} readings in a row read up"
    cond = CAPABILITY if cfg.capability else COMPONENT
    measure = {"name": cond, "value": 1 if r == UP else 0, "threshold": 1,
               "source": {"file": "host/registry.json", "action": cfg.action, "key": "pre", "condition": cond}}
    data = {"measure": measure, "labels": {"instance": inst, what: name}, "held": held, "report": cfg.report,
            "readings": {"down_in_a_row": st["down"], "up_in_a_row": st["up"], "needed": cfg.readings, "clear_after": cfg.clear_after},
            "health": facts}
    return {"kind": cfg.kind, "target": target, "title": clean(title), "detail": clean(detail, 400), "data": data}


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


def run(cfg, store, fetch=health.fetch, environ=None, now=None):
    environ = os.environ if environ is None else environ
    check_watched(cfg, common.action(cfg.action))
    body = health.read(cfg.source, environ, fetch)
    taken = now or datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    prev = store.load()
    report, snap = build(cfg, body, prev, taken)
    common.report(report)
    store.save(snap)
    return report, snap, "baseline" if prev is None else "compared"


def main(argv):
    kind = argv[argv.index("--kind") + 1] if "--kind" in argv and argv.index("--kind") + 1 < len(argv) else ""
    cfg = parse_config(os.environ, kind)
    report, snap, mode = run(cfg, LedgerStore())
    print(f"HEALTH PROBE {cfg.source} {mode} watched={len(snap['items'])} detections={len(report['detections'])}"
          + "".join(f"\nHEALTH PROBE {s}" for s in report.get("summary", [])))


def entry(argv):
    try:
        main(argv)
    except common.ConfigError as e:
        print(f"HEALTH PROBE config: {e}", file=sys.stderr)
        sys.exit(2)


if __name__ == "__main__":
    entry(sys.argv[1:])
