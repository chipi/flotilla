"""runtime/station_run.py: one station's episode through a harness, as a version's code asks for it (v2-design.md sections 2 and 3).

The station driver the seam end-to-end runs every harness through, and the shape of what a version's own code does with a
station: render the station's prompt from its template and slots, run it through the harness layer (runtime/adapter.py), keep the
episode, its record (runtime/episode.schema.json) and the calls-file lines of its model calls, and resume a session by the
reference the episode left. It does nothing a station's name decides: the name is a label that lands on the records.

  run_station(station, slots, run_dir=..., workdir=..., base_env=..., seq=1, resume=None, meter=None, calls_path=None, ...)

`meter`, when given, is a callable returning the gateway's spend so far (the bugfix chain's spend.Meter reads LiteLLM's
/key/info; the seam end-to-end gives a stand-in): the episode's cost is the difference over the run, labelled `gateway`; without
it the cost is the harness's own estimate, labelled as one, or unknown (adapter.episode_record).

A caller that is stopped by SIGTERM (the worker's kill) calls exit_on_sigterm() first: the harness's process runs in a session of
its own, out of reach of the worker's kill, and it is the exception that stops it (adapter.run_process kills its group).
"""
import hashlib
import json
import os
import re
import signal
import sys
import time
from dataclasses import dataclass
from typing import Optional

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adapter  # noqa: E402

SLOT = re.compile(r"\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}")


class StationError(Exception):
    """A station that cannot be run as asked: a slot its text needs and the caller did not give, an episode record the schema
    refuses. Raised before the first model call where it can be."""


@dataclass
class Station:
    """What the version's spec declares for one station, as the code receives it (FLOTILLA_STATIONS)."""
    name: str                       # the version's own name for it: a label here
    harness: str                    # an adapter's name
    model: adapter.ModelRoute
    prompt: str                     # the station's prompt template: {{slot}}s the caller fills
    tools: str = "all"              # all | read | none
    system_path: Optional[str] = None   # the system text, a file (hash-checked by the caller)
    max_turns: int = 0
    timeout: int = 600
    alias: str = ""


@dataclass
class Result:
    episode: adapter.Episode
    record: dict
    code: int
    path: str                       # the episode file, relative to the run directory
    record_path: str                # the record file, relative to the run directory
    resume_ref: str


def render(template, slots):
    """The template with each {{slot}} replaced by the caller's text. A slot the caller did not give is an error, and so is a slot
    given that the template never uses: neither is allowed to pass silently."""
    used = set(SLOT.findall(template))
    missing = sorted(used - set(slots))
    if missing:
        raise StationError(f"the text needs the slot(s) {', '.join(missing)}, which the caller did not give")
    extra = sorted(set(slots) - used)
    if extra:
        raise StationError(f"the caller gave the slot(s) {', '.join(extra)}, which the text does not use")
    return SLOT.sub(lambda m: str(slots[m.group(1)]), template)


def text_ref(name, text):
    """What a record keeps of a rendered text: its name, hash and length."""
    raw = text.encode("utf-8")
    return {"name": name, "sha256": hashlib.sha256(raw).hexdigest(), "bytes": len(raw)}


def exit_on_sigterm():
    """Turn SIGTERM into SystemExit, so a station in flight is stopped by an exception and its harness's process group is killed."""
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))


def calls_lines(ep, alias, stem, seq, now=None):
    """The calls-file lines of one episode: one per model call, in the format of the in-process client's lines (model_client)
    plus `harness` and `station` (and `texts` on the call that read them). A tool is {name, arg, ok} only: the calls file is not
    the record. Cost is not set here: the gateway's figure is the run's."""
    now = now or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    out = []
    for t in ep.turns:
        u = t.usage or {}
        tools = [{k: x[k] for k in ("name", "arg", "ok") if k in x} for x in t.tools]
        line = {"at": now, "alias": alias, "endpoint": None, "model": t.model, "deployment": None,
                "tokens_in": u.get("input"), "tokens_out": u.get("output"), "cost_usd": None, "cost_source": "",
                "latency_s": None, "status": None, "error": str(t.error)[:200] if t.error else None,
                "finish_reason": t.stop or "", "step": t.n, "decision_key": None, "role": "", "episode": ep.session_id or stem,
                "episode_seq": seq, "tools": tools, "harness": ep.harness, "station": ep.station}
        if t.texts:
            line["texts"] = [dict(x) for x in t.texts]
        out.append(line)
    return out


def _write_json(path, doc):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(doc, f)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def run_station(station, slots, *, run_dir, workdir, base_env, seq=1, resume=None, meter=None, calls_path=None,
                version=None, spec_hash=None, run=adapter.run_process):
    """Run `station` once. The prompt is its template rendered with `slots`. `resume` is the resume_ref of an earlier Result: the
    prompt is then the next message of that session (a follow-up). The episode and its record are written under
    <run_dir>/episodes/<seq>-<name>, and the calls lines appended to `calls_path` when given. Raises StationError before any model
    call for a slot problem, adapter.HarnessError for what the adapter cannot serve, adapter.ReadOnlyViolation for a read
    station that changed the workspace (with its episode written first, so the stop is on record)."""
    prompt = render(station.prompt, slots)
    h = adapter.get(station.harness)
    stem = f"{seq:03d}-{station.name}"
    texts = [text_ref("prompt", prompt)]
    if station.system_path:
        with open(station.system_path, "rb") as f:
            texts.append({"name": "system", "sha256": hashlib.sha256(f.read()).hexdigest(), "bytes": os.path.getsize(station.system_path)})
    req = adapter.Request(station=station.name, prompt=prompt, system_path=station.system_path, model=station.model,
                          tools=station.tools, resume=resume, max_turns=station.max_turns, workdir=workdir, run_dir=run_dir,
                          timeout=station.timeout, episode=stem)
    env = dict(base_env, HOME=run_dir)
    before = meter() if meter else None
    violation = None
    try:
        ep, code, _ = adapter.execute(h, req, env, run=run)
    except adapter.ReadOnlyViolation as e:
        violation, ep, code = e, e.episode, 0
    if ep.turns:
        ep.turns[0].texts = texts
    cost = {"usd": round(meter() - before, 6), "source": "gateway"} if meter else None
    record = adapter.episode_record(ep, seq=seq, version=version, spec_hash=spec_hash, cost=cost)
    errors = adapter.validate_episode(record)
    if errors:
        raise StationError(f"harness {h.name} left an episode the schema refuses: {'; '.join(errors[:3])}")
    ref = h.resume_ref(ep)
    rel = adapter.write_episode(run_dir, stem, adapter.episode_doc(ep, station.alias or station.model.alias, seq, code, ref))
    record_rel = os.path.join(adapter.EPISODES, stem + ".record.json")
    _write_json(os.path.join(run_dir, record_rel), record)
    if calls_path:
        with open(calls_path, "a") as f:
            for line in calls_lines(ep, station.alias or station.model.alias, stem, seq):
                f.write(json.dumps(line) + "\n")
    if violation:
        raise violation
    return Result(ep, record, code, rel, record_rel, ref)
