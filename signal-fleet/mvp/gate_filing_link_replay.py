"""Replay gate: a fingerprint matched on another key gets a linked row, and no decision changes.

The bug (fleet-eval-data SILENT-FILINGS.md, 2026-11-12): a signal matched to an issue on
group_key, norm_key or alert_key (a comment, a same-day dedup, a reopen, a mute) never got a ledger
row of its own. 265 of 1,117 file decisions before 2026-11-07 left no row, so the stale check and
the GlitchTip close sync never linked them to the issue that tracked them.

Input: reference-filing/replay-2026-09-30.json (the corpus of gate_filing_replay: every filed row
with its alert, and each issue's real open and close times). Every historical filing is replayed
in order through file_or_update, in ONE ledger that only the code under test writes, with each
issue open or closed as it was at that moment, the filing's own time as `now`, and a created issue
given the number history gave it. Two arms of the same code: SF_FILING_LINK=0 (no links, as before
the fix) and SF_FILING_LINK=1.

Pass:
  1. every decision (its result line and every GitHub call with its payload) is the same in both
     arms, including every decision made after a link exists;
  2. the link arm's ledger is the base arm's plus exactly the expected linked rows: one per
     fingerprint whose latest decision matched an issue on another key, naming the issue that
     decision acted on, `linked_via` the key; a fingerprint that later got its own issue has none;
  3. at least one link is written, and at least one through a fuzzy key (norm_key or alert_key);
  4. a mute never spreads through a link: once the issue a fuzzy-linked fingerprint points at is
     muted, that fingerprint's next occurrence decides as in the base arm and is not MUTED.

  python3 gate_filing_link_replay.py              # exit 1 on any failure
  python3 gate_filing_link_replay.py --emit F     # also write arm 0's decisions to F
  python3 gate_filing_link_replay.py --code DIR   # import filing from DIR (e.g. the commit before
                                                  # the fix: its --emit output must equal arm 0's)
"""
import json
import os
import re
import sys
import tempfile

if "--code" in sys.argv:
    sys.path.insert(0, os.path.abspath(sys.argv[sys.argv.index("--code") + 1]))
import filing  # noqa: E402
import evaldata  # noqa: E402

CORPUS = evaldata.path("reference-filing", "replay-2026-09-30.json")
LINKS = ("group_key", "norm_key", "alert_key")   # fixed here, not read from the code under test
ACTED = ("COMMENTED", "DEDUP", "REOPENED", "MUTED")
REAL = (filing.FILED, filing._gh, filing.issue_state, filing._now, filing.ledger_lookup)


def _ts(s):
    d = filing.datetime.fromisoformat(s.replace("Z", "+00:00"))
    return d if d.tzinfo else d.replace(tzinfo=filing.timezone.utc)


class World:
    """One evolving ledger and GitHub as it stood at each filing; swaps filing's I/O for fakes."""

    def __init__(self, corpus):
        self.corpus, self.at, self.number, self.muted = corpus, None, None, set()
        self.calls, self.matched = [], []
        tmp = tempfile.NamedTemporaryFile("w", suffix=".tsv", delete=False)
        tmp.close()
        self.path = tmp.name
        self._install()
        filing._ledger_write([])

    def _install(self):
        """A world's fakes go in before each of its calls: two worlds share filing's globals."""
        lookup = REAL[4]

        def spy(*a, **k):
            r = lookup(*a, **k)
            self.matched.append(r)
            return r
        filing.FILED, filing._gh, filing.issue_state = self.path, self._gh, self._state
        filing._now, filing.ledger_lookup = (lambda: _ts(self.at)), spy

    def _state(self, repo, num):
        t = self.corpus["issues"].get(f"{repo}#{num}", {})
        closed = t.get("closed")
        is_open = bool(t) and t["created"] <= self.at and (not closed or closed > self.at)
        labels = [filing.MUTE_LABEL] if (repo, str(num)) in self.muted else []
        return {"state": "open" if is_open else "closed", "labels": labels,
                "closed_at": _ts(closed) if closed and not is_open else None, "url": ""}

    def _gh(self, method, path, payload=None):
        self.calls.append([method, path, payload])
        if method == "POST" and path.endswith("/issues"):
            return {"number": self.number, "html_url": "replay"}
        if method == "GET" and "/search/issues" in path:
            return {"items": []}
        if method == "GET" and "/milestones" in path:
            return []
        return {}

    def decide(self, row, at=None):
        self.at, self.number = at or row["filed_at"], row["issue"]
        self.calls, self.matched = [], []
        self._install()
        signal = {"fingerprint": row["fingerprint"], "source": row["source"],
                  "alertname": row["alertname"], "labels": {}}
        out = filing.file_or_update(signal, {"title": row["alertname"] or "replayed signal",
                                             "body": "replay"}, "bug")
        return {"out": out, "calls": self.calls,
                "dim": next((m["_dim"] for m in self.matched if m), None),
                "matched_fp": next((m["fingerprint"] for m in self.matched if m), None)}

    def ledger(self):
        self._install()
        return {r["fingerprint"]: r for r in filing._tsv_rows()}

    def close(self):
        (filing.FILED, filing._gh, filing.issue_state, filing._now, filing.ledger_lookup) = REAL
        os.unlink(self.path)


def run_arm(corpus, rows, link):
    os.environ["SF_FILING_LINK"] = "1" if link else "0"
    w = World(corpus)
    try:
        decisions = [w.decide(r) for r in rows]
        return decisions, w.ledger(), w
    except BaseException:
        w.close()
        raise


def _target(out):
    m = re.search(r"([\w.-]+/[\w.-]+)#(\w+)", out)
    return (m.group(1), m.group(2)) if m else None


def expected_links(rows, decisions):
    want = {}
    for row, d in zip(rows, decisions):
        fp, word = row["fingerprint"], d["out"].split(":")[0]
        if word in ACTED and d["dim"] in LINKS and d["matched_fp"] != fp:
            want[fp] = (*_target(d["out"]), d["dim"])
        elif word in ("FILED", "ROLLED-UP", "ROLLUP-OPENED"):
            want.pop(fp, None)                # a row of its own replaces the link
    return want


def main():
    with open(CORPUS) as f:
        corpus = json.load(f)
    rows = sorted(corpus["rows"], key=lambda r: r["filed_at"])
    if not rows:
        print("FAIL: the corpus is empty")
        return 1
    fails = []
    base, base_ledger, wb = run_arm(corpus, rows, link=False)
    if "--emit" in sys.argv:
        with open(sys.argv[sys.argv.index("--emit") + 1], "w") as f:
            json.dump([[d["out"], d["calls"]] for d in base], f)
    if not hasattr(filing, "LINK_DIMS"):
        wb.close()
        print(f"base code: {len(base)} decisions emitted (no linking in this code)")
        return 0
    fix, fix_ledger, wf = run_arm(corpus, rows, link=True)

    # 1. no decision changes
    diff = [i for i, (b, x) in enumerate(zip(base, fix)) if (b["out"], b["calls"]) != (x["out"], x["calls"])]
    for i in diff[:5]:
        fails.append(f"decision {i} changed: {base[i]['out'][:70]!r} -> {fix[i]['out'][:70]!r}")
    words = {}
    for d in base:
        words[d["out"].split(":")[0]] = words.get(d["out"].split(":")[0], 0) + 1
    print(f"1. {len(rows)} filings replayed in order: {len(diff)} decisions differ between the arms "
          f"({', '.join(f'{k} {v}' for k, v in sorted(words.items()))})")

    # 2. the ledger: the base's rows plus exactly the expected links
    want = expected_links(rows, fix)
    got = {fp: (r["repo"], str(r["issue"]), r["linked_via"]) for fp, r in fix_ledger.items() if r.get("linked_via")}
    own = {fp: r for fp, r in fix_ledger.items() if not r.get("linked_via")}
    if own != base_ledger:
        changed = sorted(set(own) ^ set(base_ledger) | {k for k in own if own.get(k) != base_ledger.get(k)})
        fails.append(f"{len(changed)} non-linked rows differ from the base arm's (first index: "
                     f"{rows.index(next(r for r in rows if r['fingerprint'] in changed)) if changed else '-'})")
    if got != {fp: (repo, str(n), dim) for fp, (repo, n, dim) in want.items()}:
        fails.append(f"links: expected {len(want)}, written {len(got)}, "
                     f"{len(set(want) ^ set(got))} fingerprints differ")
    by_dim = {}
    for _, _, dim in got.values():
        by_dim[dim] = by_dim.get(dim, 0) + 1
    print(f"2. ledger: {len(own)} rows as in the base arm, {len(got)} linked "
          f"({', '.join(f'{k} {v}' for k, v in sorted(by_dim.items())) or 'none'}); expected {len(want)}")

    # 3. something was linked, and through a fuzzy key
    fuzzy = sorted(fp for fp, (_, _, dim) in got.items() if dim in ("norm_key", "alert_key"))
    if not got or not fuzzy:
        fails.append(f"no {'fuzzy ' if got else ''}link written: the replay proves nothing")

    # 4. the mute case, after the whole history, in both arms
    if fuzzy:
        fp = fuzzy[0]
        row = next(r for r in rows if r["fingerprint"] == fp)
        repo, issue, _ = got[fp]
        last = max(rows[-1]["filed_at"], max(v.get("closed") or v["created"] for v in corpus["issues"].values()))
        corpus["issues"][f"{repo}#{issue}"] = {"created": rows[0]["filed_at"], "closed": None}
        res = []
        for w, link in ((wb, "0"), (wf, "1")):
            os.environ["SF_FILING_LINK"] = link
            w.muted = {(repo, issue)}
            res.append(w.decide(row, at=last)["out"])
        if res[0] != res[1] or res[1].startswith("MUTED"):
            fails.append(f"mute case: base {res[0][:60]!r}, link arm {res[1][:60]!r}")
        print(f"4. mute case (a fuzzy-linked fingerprint whose issue is then muted): base "
              f"{res[0].split(':')[0]}, link arm {res[1].split(':')[0]}")
    wb.close()
    wf.close()
    os.environ.pop("SF_FILING_LINK", None)

    for f_ in fails:
        print("FAIL", f_)
    print("PASS" if not fails else f"FAIL ({len(fails)})")
    return 1 if fails else 0


if __name__ == "__main__":
    raise SystemExit(main())
