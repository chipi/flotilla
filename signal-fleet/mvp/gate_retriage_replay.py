"""Replay gate: a dismissed GlitchTip error that recurs is re-triaged at once, not after RETRIAGE_HOURS.

Why (fleet-eval-data projects/signal-fleet-retriage, 2026-11-12): before 2026-11-07, 49 of 64 dismissals
that came back were overturned at their re-triage, a median of 85 hours after the dismissal; none of the
issues those re-triages filed was closed as not planned.

Input, frozen by the fleet session:
  reference-retriage/dispositions-2026-11-07.tsv.gz   the fleet's disposition ledger
  reference-retriage/filed-2026-11-06.tsv             the filed-ledger
Only events before 2026-11-07T00:00Z are replayed: the tuning side (SPLIT.md).

Every historical GlitchTip recurrence (one per fingerprint and count change) goes through the REAL
orchestrator.run_glitchtip of
  baseline  orchestrator.py at BASE_REV (git)
  fix       the working tree
with that fingerprint's ledger and the filed-ledger as of the event, the clock at the event, triage and
recurrence writes recorded, never sent. Pass:
  - baseline: every event is a recurrence row, as history recorded (faithful), except the rollup
    promotions gate_promotion_replay.py expects (PROMOTED), which the baseline re-triages;
  - fix: an event changes exactly when its last decision was a dismissal that was not itself a
    dismissal's re-triage less than RETRIAGE_HOURS after it (the rule, computed here from the
    ledger, not from the code), and then only recurrence -> triage; every other event acts as before.

  python3 gate_retriage_replay.py     # exit 1 on any failure
"""
import contextlib
import gzip
import importlib.util
import io
import os
import re
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
import evaldata  # noqa: E402  (the private corpora, SF_EVAL_DATA)

HERE = os.path.dirname(os.path.abspath(__file__))
DISP = evaldata.path("reference-retriage", "dispositions-2026-11-07.tsv.gz")
FILED = evaldata.path("reference-retriage", "filed-2026-11-06.tsv")
BASE_REV = "d161f83"   # main before this change
CUT = datetime(2026, 11, 7, tzinfo=timezone.utc)   # the held-out split: test side from here on
WINDOW_H = 24.0        # RETRIAGE_HOURS as it was set in the cycles the corpus records
_TMP = tempfile.mkdtemp(prefix="sf-retriage-replay-")
os.environ["SF_FILED_LEDGER"] = os.path.join(_TMP, "filed.tsv")
os.environ["SF_LEDGER"] = os.path.join(_TMP, "dispositions.tsv")
os.environ.setdefault("SF_OBSERV_DISABLED", "1")
os.environ.setdefault("GITHUB_TOKEN", "replay-never-sent")
os.environ["SF_RETRIAGE_HOURS"] = str(WINDOW_H)
sys.path.insert(0, HERE)


# The rollup promotions gate_promotion_replay.py expects: history recorded a recurrence, because
# _promote_now came after (2026-10-17); the baseline re-triages them. The only allowed unfaithful events.
PROMOTED = {("glitchtip:PODCAST-PIPELINE-4N", n) for n in (2, 3, 4, 5, 6)} | {
    ("glitchtip:PODCAST-PIPELINE-4P", 2), ("glitchtip:PODCAST-PIPELINE-4P", 3),
    ("glitchtip:PODCAST-PIPELINE-4R", 2), ("glitchtip:PODCAST-PIPELINE-53", 2),
    ("glitchtip:PODCAST-PIPELINE-93", 2), ("glitchtip:PODCAST-PIPELINE-93", 3),
    ("glitchtip:PLAYER-1I", 2)}


def _ts(v):
    m = re.match(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)?", (v or "").strip())
    if not m:
        return None
    tz = m.group(3) or "+00:00"
    return datetime.fromisoformat(f"{m.group(1)}.{(m.group(2) or '0').ljust(6, '0')[:6]}"
                                  f"{'+00:00' if tz == 'Z' else tz}")


def _load(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _baseline():
    src = subprocess.run(["git", "-C", HERE, "show", f"{BASE_REV}:signal-fleet/mvp/orchestrator.py"],
                         capture_output=True, text=True, check=True).stdout
    tmp = tempfile.NamedTemporaryFile("w", suffix=".py", delete=False, dir=HERE, prefix="_orch_base_")
    tmp.write(src)
    tmp.close()
    try:
        return _load(tmp.name, "orchestrator_baseline")
    finally:
        os.unlink(tmp.name)


def _outcome(mod, header, fp_rows, filed_hdr, filed_rows, ev):
    import actions, config, filing, sources
    with open(os.environ["SF_LEDGER"], "w") as f:
        f.write(header + "\n" + "".join(r + "\n" for r in fp_rows))
    with open(os.environ["SF_FILED_LEDGER"], "w") as f:
        f.write(filed_hdr + "\n" + "".join(r + "\n" for r in filed_rows))
    config.LEDGER = os.environ["SF_LEDGER"]
    config.RETRIAGE_HOURS = WINDOW_H
    filing.FILED = os.environ["SF_FILED_LEDGER"]
    seen = []
    sources.glitchtip_unresolved = lambda limit=10: [{"replay": True}]
    sources.to_error_signal = lambda issue: dict(ev["signal"])
    sources.attach_backend = lambda sig: None
    actions.record_recurrence = lambda sig, base: seen.append("recurrence")
    actions.act = lambda *a, **k: None
    mod._triage_or_defer = lambda sig: seen.append("triage")
    mod._hours_since = lambda ts: (ev["at"] - _ts(ts)).total_seconds() / 3600
    with contextlib.redirect_stdout(io.StringIO()):
        mod.run_glitchtip(limit=10, dry_run=True)
    return seen[0] if seen else "skip"


def _expected(cols, fp_rows):
    """The rule, from the ledger rows alone: the last decision is a dismissal, and not a dismissal
    re-triaged less than WINDOW_H after an earlier dismissal."""
    di, ti = cols.index("disposition"), cols.index("ts")
    dec = [r.split("\t") for r in fp_rows]
    dec = [p for p in dec if len(p) > di and p[di] != "recurrence"]
    if not dec or dec[-1][di] != "dismiss":
        return False
    if len(dec) >= 2 and dec[-2][di] == "dismiss":
        return (_ts(dec[-1][ti]) - _ts(dec[-2][ti])).total_seconds() / 3600 >= WINDOW_H
    return True


def main():
    lines = gzip.open(DISP, "rt").read().splitlines()
    header, rows = lines[0], lines[1:]
    cols = header.split("\t")
    ti, oi, fi, di, ri, si = (cols.index(c) for c in ("ts", "occurrence_id", "fingerprint", "disposition",
                                                      "reason", "source"))
    filed = open(FILED).read().splitlines()
    filed_hdr, filed_rows = filed[0], [r for r in filed[1:] if r]
    filed_at = filed_hdr.split("\t").index("filed_at")
    by_fp, events, seen = {}, [], set()
    for idx, r in enumerate(rows):
        p = r.split("\t")
        if len(p) <= ri:
            continue
        by_fp.setdefault(p[fi], []).append(idx)
        if p[di] != "recurrence" or p[si] != "glitchtip":
            continue
        at = _ts(p[ti])
        m = re.search(r"count (\d+) -> (\d+)", p[ri])
        if not at or at >= CUT or not m or (p[fi], int(m.group(2))) in seen:
            continue
        seen.add((p[fi], int(m.group(2))))
        occ = p[oi]
        events.append({"idx": idx, "fp": p[fi], "count": int(m.group(2)), "at": at,
                       "signal": {"fingerprint": p[fi], "occurrence_id": occ, "source": "glitchtip",
                                  "alertname": p[cols.index("alertname")], "raw": {},
                                  "startsAt": occ.split("@", 1)[1] if "@" in occ else "",
                                  "labels": {"count": m.group(2)}}})
    if not events:
        print("RETRIAGE REPLAY: NO DATA (no recurrence before the cut)")
        return 1
    base, fix = _baseline(), _load(os.path.join(HERE, "orchestrator.py"), "orchestrator_fix")
    fails, changed, same, expected_n, capped = [], 0, 0, 0, 0
    episodes = set()
    for ev in events:
        fp_rows = [rows[i] for i in by_fp[ev["fp"]] if i < ev["idx"]]
        frows = [r for r in filed_rows if r.split("\t")[filed_at][:19] <= ev["at"].isoformat()[:19]]
        b = _outcome(base, header, fp_rows, filed_hdr, frows, ev)
        f = _outcome(fix, header, fp_rows, filed_hdr, frows, ev)
        exp = _expected(cols, fp_rows)
        last_dec = [p for p in (r.split("\t") for r in fp_rows) if p[di] != "recurrence"]
        if last_dec and last_dec[-1][di] == "dismiss" and not exp:
            capped += 1
        if b != "recurrence" and not (b == "triage" and (ev["fp"], ev["count"]) in PROMOTED):
            fails.append(f"{ev['fp']} count {ev['count']} @ {ev['at']:%Y-%m-%d %H:%M}: baseline {b}, "
                         "history recorded a recurrence")
        if exp:
            expected_n += 1
            episodes.add((ev["fp"], last_dec[-1][ti]))
            if f != "triage":
                fails.append(f"{ev['fp']} count {ev['count']}: a dismissal's recurrence, fix gives {f}")
        elif f != b:
            fails.append(f"{ev['fp']} count {ev['count']}: unexpected change {b} -> {f}")
        if f != b:
            changed += 1
        else:
            same += 1
    print(f"Re-triage replay: {len(events)} GlitchTip recurrences before {CUT:%Y-%m-%d} "
          f"(one per fingerprint and count change), {len(by_fp)} fingerprints in the ledger\n")
    print(f"  unchanged: {same}    changed recurrence -> triage: {changed} (the rule expects {expected_n})")
    print(f"  dismissals whose recurrence now re-triages at once: {len(episodes)}")
    print(f"  recurrences after a dismissal that wait, by the once-per-dismissal cap: {capped}")
    print("\n" + ("RETRIAGE REPLAY: PASS" if not fails else "RETRIAGE REPLAY: FAIL\n  " + "\n  ".join(fails[:25])))
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
