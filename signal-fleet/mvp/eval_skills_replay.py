"""Replay for RFC-0002 §9 step 3: the playbook and the class hints as files and rule-loaded skills.

  python3 eval_skills_replay.py CAPTURE_DIR [CAPTURE_DIR ...] [--baseline FILE]

The old code is triage.py at BASE_REV (or --baseline FILE where there is no git, as in a worker
container); the new is the working tree. Nothing is sent anywhere.

1. History: for every capture with a recorded first model turn (`exact`, since 2026-11-09), the new
   code's system and user messages for the capture's signal equal the recorded request's, byte for byte.
2. Old against new: the system text and PROMPT_SHA; the user message for a signal of each class, of no
   class, and with SF_NO_CLASS_HINT; with the rules from default-rules.json and from FLOTILLA_SKILLS
   (the spec's rules as flotillad hands them).
3. Recording: a hinted signal records its skill (name, layer, version, sha256) in `exact`.

Exit 0 when everything matches, 1 on any difference, 2 when no recorded turn was found. Captures hold
production data: run it on the host that has them; it prints counts, never content.
"""
import argparse
import glob
import importlib.util
import json
import os
import subprocess
import sys
import tempfile

os.environ.setdefault("OPENROUTER_API_KEY", "replay-dummy")
os.environ.setdefault("SF_OBSERV_DISABLED", "1")
os.environ.pop("SF_NO_CLASS_HINT", None)
os.environ.pop("FLOTILLA_SKILLS", None)

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
BASE_REV = "fafff9b"   # flotilla main before step 3

import config  # noqa: E402

SIGNALS = {
    "external-transient": {"source": "glitchtip", "alertname": "deepgram request timed out", "labels": {}},
    "external": {"source": "glitchtip", "alertname": "BadRequest", "summary": "content_policy violation", "labels": {}},
    "environment": {"source": "grafana", "alertname": "write failed", "summary": "No space left on device", "labels": {}},
    "recoverable": {"source": "glitchtip", "alertname": "RecoverableSummarizationError", "labels": {"x": "1"}},
    "none": {"source": "grafana", "alertname": "Something odd", "summary": "plain", "labels": {"a": "b"}},
}
SPEC_RULES = {"rule": [{"when": {"class": c}, "load": [f"hint-{c}"]}
                       for c in ("external-transient", "external", "environment", "recoverable")]}


def _load(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def baseline(path=None):
    if path:
        return _load(path, "triage_baseline")
    src = subprocess.run(["git", "-C", HERE, "show", f"{BASE_REV}:signal-fleet/mvp/triage.py"],
                         capture_output=True, text=True, check=True).stdout
    tmp = tempfile.NamedTemporaryFile("w", suffix=".py", delete=False, dir=HERE, prefix="_triage_base_")
    tmp.write(src)
    tmp.close()
    try:
        return _load(tmp.name, "triage_baseline")
    finally:
        os.unlink(tmp.name)


def history(new, dirs):
    n, bad = 0, []
    for d in dirs:
        for path in sorted(glob.glob(os.path.join(d, "*.json"))):
            try:
                cap = json.load(open(path))
            except (OSError, ValueError):
                continue
            turns = (cap.get("exact") or {}).get("turns") or []
            first = next((t for t in turns if t.get("request")), None)
            if first is None or not isinstance(cap.get("signal"), dict):
                continue
            rec = json.loads(first["request"])["messages"][:2]
            n += 1
            got = [{"role": "system", "content": new._sys()},
                   {"role": "user", "content": new._signal_intro(cap["signal"])}]
            if got != rec:
                which = "system" if got[0] != rec[0] else "user"
                bad.append(f"{os.path.basename(path)}: the {which} message differs from the recorded request")
    return n, bad


def old_vs_new(old, new):
    bad = []
    if old._sys() != new._sys():
        bad.append("system text differs")
    if old.PROMPT_SHA != new.PROMPT_SHA:
        bad.append(f"PROMPT_SHA {old.PROMPT_SHA} -> {new.PROMPT_SHA}")
    cases = 0
    for env in ({}, {"FLOTILLA_SKILLS": json.dumps(SPEC_RULES)}, {"SF_NO_CLASS_HINT": "1"}):
        os.environ.update(env)
        try:
            for cls, sig in SIGNALS.items():
                cases += 1
                if old.signal_class(sig) != (None if cls == "none" else cls):
                    bad.append(f"fixture {cls} is not of its class")
                if old._signal_intro(dict(sig)) != new._signal_intro(dict(sig)):
                    bad.append(f"user message differs: class {cls}, env {sorted(env)}")
        finally:
            for k in env:
                os.environ.pop(k, None)
    return cases, bad


def recording(new):
    config.CAPTURE_PROBES = True
    try:
        new._exact_start()
        new._signal_intro(dict(SIGNALS["environment"]))
        got = new._EXACT.get("skills")
    finally:
        config.CAPTURE_PROBES = False
    ok = (len(got or []) == 1 and got[0]["name"] == "hint-environment" and got[0]["layer"] == "fleet"
          and len(got[0]["sha256"]) == 64)
    return [] if ok else [f"the hinted signal's exact.skills is {got!r}"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--baseline")
    a = ap.parse_args()
    old = baseline(a.baseline)
    new = _load(os.path.join(HERE, "triage.py"), "triage_new")
    n, bad = history(new, a.dirs)
    print(f"history: {n} recorded first requests rebuilt, {len(bad)} differences")
    cases, vbad = old_vs_new(old, new)
    print(f"old against new: system text, PROMPT_SHA {new.PROMPT_SHA}, {cases} user messages, {len(vbad)} differences")
    rbad = recording(new)
    print(f"recording: {'ok' if not rbad else 'FAIL'}")
    for b in (bad + vbad + rbad)[:20]:
        print("  DIFF", b)
    if n == 0:
        print("SKILLS REPLAY: NO DATA (no recorded request found)")
        return 2
    ok = not (bad or vbad or rbad)
    print(f"SKILLS REPLAY: {'PASS' if ok else 'FAIL'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
