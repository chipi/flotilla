"""Replay gate: a GlitchTip project with no repository mapping is filed in the ops repo, not refused.

The bug (fleet-eval-data SILENT-FILINGS.md, 2026-11-12): repo_for gave an unmapped GlitchTip project
SF_OPS_REPO alone. It is unset on the mini, so file_or_update returned REFUSED and actions.act only
printed it: no issue, no proposal, no escalation. Unmapped grafana signals already fell back to the
ops repo; the fix gives GlitchTip projects the same fallback.

Input: reference-retriage/dispositions-2026-11-07.tsv.gz (every disposition before 2026-11-07, the
tuning side). For every file and escalate decision, repo_for runs with
  baseline  filing.py at BASE_REV
  fix       the working-tree filing.py
both with SF_OPS_REPO unset, then both with it set.
Pass:
  - with SF_OPS_REPO unset, exactly the decisions the baseline refused change, each to the ops repo;
    every other decision's repository is unchanged; at least one decision changes;
  - with SF_OPS_REPO set, nothing changes: the installation's own ops repo still wins.
Prints counts only.

  python3 gate_unmapped_project_replay.py     # exit 1 on any failure
"""
import csv
import gzip
import importlib.util
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import evaldata  # noqa: E402
import filing  # noqa: E402

BASE_REV = "15442c3"   # main before the fix
CORPUS = evaldata.path("reference-retriage", "dispositions-2026-11-07.tsv.gz")
OPS_DEFAULT = "chipi/agentic-ai-homelab"   # fixed here, not read from the code under test


def _baseline():
    src = subprocess.run(["git", "-C", HERE, "show", f"{BASE_REV}:signal-fleet/mvp/filing.py"],
                         capture_output=True, text=True, check=True).stdout
    tmp = tempfile.NamedTemporaryFile("w", suffix=".py", delete=False, dir=HERE, prefix="_filing_base_")
    tmp.write(src)
    tmp.close()
    try:
        spec = importlib.util.spec_from_file_location("filing_baseline", tmp.name)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        return mod
    finally:
        os.unlink(tmp.name)


def main():
    csv.field_size_limit(10 ** 8)
    with gzip.open(CORPUS, "rt") as f:
        rows = [r for r in csv.DictReader(f, delimiter="\t") if r["disposition"] in ("file", "escalate")]
    if not rows:
        print("FAIL: no file or escalate decisions in the corpus")
        return 1
    base, fails = _baseline(), []
    for ops in ("", "acme/ops"):
        base.OPS_REPO = filing.OPS_REPO = ops
        moved = same = wrong = 0
        for r in rows:
            sig = {"fingerprint": r["fingerprint"], "labels": {}}
            b, x = base.repo_for(sig), filing.repo_for(sig)
            if b == x:
                same += 1
            elif not b and x == (ops or OPS_DEFAULT):
                moved += 1
            else:
                wrong += 1
        label = "set" if ops else "unset"
        print(f"SF_OPS_REPO {label}: {len(rows)} decisions, {moved} refused -> ops repo, {same} unchanged, "
              f"{wrong} changed otherwise")
        if wrong:
            fails.append(f"SF_OPS_REPO {label}: {wrong} decisions changed repository")
        if ops and moved:
            fails.append("SF_OPS_REPO set: a decision moved, the installation's ops repo did not win")
        if not ops and not moved:
            fails.append("SF_OPS_REPO unset: no refused decision moved (nothing proven)")
    for f_ in fails:
        print("FAIL", f_)
    print("PASS" if not fails else f"FAIL ({len(fails)})")
    return 1 if fails else 0


if __name__ == "__main__":
    raise SystemExit(main())
