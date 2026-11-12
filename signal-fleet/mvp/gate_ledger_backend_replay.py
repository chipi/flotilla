"""Replay for RFC-0002 §9 step 5: dedup through flotillad's ledger (SF_LEDGER_BACKEND=api) against filed.tsv.

  python3 gate_ledger_backend_replay.py FILED_TSV [DISPOSITIONS_TSV]   # a real ledger, copied (read-only use)
  python3 gate_ledger_backend_replay.py --synthetic N                    # N random filings

Run inside a worker, as a routine runs (FLOTILLA_API_URL, FLOTILLA_API_TOKEN_FILE): it works in fresh
namespaces of its own (replay-<random>), never the routine's.

1. Lookup: the ledger is imported, then for every row's keys, and for crossed variants (a row's fingerprint
   with another's norm key, unknown fingerprints, single keys), filing.ledger_lookup runs with each backend
   under the same open-state stand-in. Same row, same _dim, same open-state calls in the same order.
   filing._ledger_rows() with no keys returns the same rows.
2. Writes: on a copy of the ledgers, random upserts (new fingerprints, rewrites with changed keys) and
   decision and recurrence appends with the api backend; afterwards both TSVs rendered from the store equal
   the files byte for byte (ledger_sync's check), and each backend's lookups agree again.

Prints counts only. Exit 0 when everything agrees, 1 otherwise.
"""
import argparse
import os
import random
import shutil
import sys
import tempfile
import uuid

os.environ.setdefault("SF_OBSERV_DISABLED", "1")
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import actions  # noqa: E402
import config  # noqa: E402
import filing  # noqa: E402
import ledger_sync  # noqa: E402

KEYS = ("fingerprint", "group_key", "norm_key", "alert_key")


def synthetic(path, n, seed=11):
    r = random.Random(seed)
    with open(path, "w") as f:
        f.write("\t".join(filing.FILED_COLS) + "\n")
        for i in range(n):
            group = (f"{filing.ROLLUP_PREFIX}b{r.randrange(5)}" if r.random() < .1 else
                     f"g{r.randrange(n // 8 + 1)}" if r.random() < .3 else "")
            cols = [f"glitchtip:X-{i}", "o/r", str(1000 + i), group,
                    "2026-11-01T00:00:00Z", "", f"n{r.randrange(n // 4 + 1)}" if r.random() < .8 else "",
                    f"a{r.randrange(n // 10 + 1)}" if r.random() < .5 else ""]
            f.write("\t".join(cols) + "\n")


def refuses_short_rows(work):
    """An old 6-column row (before norm_key) must not be imported: the store keeps every column."""
    path = os.path.join(work, "short.tsv")
    with open(path, "w") as f:
        f.write("\t".join(filing.FILED_COLS) + "\nglitchtip:S-1\to/r\t1\t\t2026-01-01\t\n")
    keep, filing.FILED = filing.FILED, path
    use("api", f"replay-{uuid.uuid4().hex[:12]}")
    try:
        return ledger_sync.main(["import"]) == 1
    finally:
        filing.FILED = keep


def use(backend, ns):
    os.environ["SF_LEDGER_BACKEND"] = backend
    os.environ["FLOTILLA_ROUTINE"] = ns
    filing._LEDGER_CLIENT = None


def lookups(rows, r):
    qs = []
    for x in rows:
        qs.append({k: x.get(k, "") for k in KEYS})
    for _ in range(len(rows)):
        a, b = r.choice(rows), r.choice(rows)
        qs.append({"fingerprint": a.get("fingerprint", ""), "group_key": "", "norm_key": b.get("norm_key", ""),
                   "alert_key": r.choice(rows).get("alert_key", "")})
        qs.append({"fingerprint": "glitchtip:NOPE-" + str(r.random()), "group_key": a.get("group_key", ""),
                   "norm_key": "", "alert_key": ""})
        qs.append({"fingerprint": "grafana:none", "group_key": "", "norm_key": "", "alert_key": b.get("alert_key", "")})
    return qs


def run_lookups(qs, ns):
    out = {}
    for backend in ("tsv", "api"):
        use(backend, ns)
        res = []
        for q in qs:
            calls = []

            def is_open(repo, issue, calls=calls):
                calls.append((repo, issue))
                return int(issue) % 3 != 0
            hit = filing.ledger_lookup(q["fingerprint"], q["group_key"], q["norm_key"], q["alert_key"], is_open=is_open)
            res.append((hit, tuple(calls)))
        # the rollup bucket's row (filing._file_rollup), for every bucket and one with no row
        res += [("rollup", b, filing._rollup_row(f"{filing.ROLLUP_PREFIX}b{b}")) for b in range(6)]
        out[backend] = (res, filing._ledger_rows())
    return out


def compare(out, label):
    bad = []
    t_res, t_all = out["tsv"]
    a_res, a_all = out["api"]
    if t_all != a_all:
        bad.append(f"{label}: _ledger_rows() differs ({len(t_all)} against {len(a_all)})")
    n = sum(1 for t, a in zip(t_res, a_res) if t != a)
    if n:
        bad.append(f"{label}: {n} of {len(t_res)} lookups differ")
    return bad


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("filed", nargs="?")
    ap.add_argument("dispositions", nargs="?")
    ap.add_argument("--synthetic", type=int, default=0)
    a = ap.parse_args()
    work = tempfile.mkdtemp(prefix="ledger-replay-")
    try:
        filed, disp = os.path.join(work, "filed.tsv"), os.path.join(work, "dispositions.tsv")
        if a.synthetic:
            synthetic(filed, a.synthetic)
        else:
            shutil.copyfile(a.filed, filed)
        if a.dispositions:
            shutil.copyfile(a.dispositions, disp)
        else:
            with open(disp, "w") as f:
                f.write("\t".join(actions.LEDGER_COLS) + "\n")
        filing.FILED, config.LEDGER = filed, disp
        ns = f"replay-{uuid.uuid4().hex[:12]}"
        use("api", ns)
        if ledger_sync.main(["import"]) != 0:
            print("LEDGER BACKEND REPLAY: FAIL (the import)")
            return 1
        r = random.Random(5)
        rows = filing._tsv_rows()
        qs = lookups(rows, r)
        bad = compare(run_lookups(qs, ns), "imported")
        print(f"lookup: {len(rows)} filings, {len(qs)} lookups per backend, {len(bad)} differences")
        # writes with the api backend: the store and the files must stay equal
        use("api", ns)
        for i in range(max(50, len(rows) // 5)):
            if rows and r.random() < .5:
                e = dict(r.choice(rows))
                e["last_comment_day"] = f"2026-11-{r.randrange(1, 29):02d}"
                if r.random() < .3:
                    e["norm_key"] = f"n-new-{r.randrange(20)}"
            else:
                e = {"fingerprint": f"grafana:new-{i}", "repo": "o/r", "issue": str(5000 + i), "group_key": "",
                     "filed_at": "2026-11-12T00:00:00Z", "last_comment_day": "", "norm_key": f"n-new-{r.randrange(20)}",
                     "alert_key": ""}
            filing.ledger_upsert(e)
            sig = {"occurrence_id": f"o{i}", "fingerprint": e["fingerprint"], "source": "replay", "alertname": "x",
                   "labels": {}}
            disp_kind = "recurrence" if r.random() < .6 else "dismiss"
            actions.ledger_append(sig, {"disposition": disp_kind, "reason": "replay", "_meta": {}})
        check = ledger_sync.main(["check"])
        rows2 = filing._tsv_rows()
        bad += compare(run_lookups(lookups(rows2, r), ns), "after writes")
        if check != 0:
            bad.append("the TSVs rendered from the store differ from the files after the writes")
        if not refuses_short_rows(work):
            bad.append("an import of a ledger with a short row was not refused")
        use("api", f"replay-{uuid.uuid4().hex[:12]}")   # a namespace never imported
        try:
            filing._ledger_rows({"fingerprint": "x"})
            bad.append("a lookup on an empty namespace was not refused (it would read as nothing ever filed)")
        except filing._ledger_module().LedgerUnavailable:
            pass
        print(f"writes: {len(rows2)} filings after the upserts, {len(bad)} differences in total")
        for b in bad:
            print("  DIFF", b)
        print(f"LEDGER BACKEND REPLAY: {'PASS' if not bad else 'FAIL'}")
        return 0 if not bad else 1
    finally:
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
