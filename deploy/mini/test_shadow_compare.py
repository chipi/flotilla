"""python3 deploy/mini/test_shadow_compare.py"""
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
HDR = "ts\toccurrence_id\tfingerprint\tsource\talertname\tdisposition\treason\n"


def ledger(rows):
    f = tempfile.NamedTemporaryFile("w", suffix=".tsv", delete=False)
    f.write(HDR)
    for ts, occ, disp, why in rows:
        f.write(f"{ts}\t{occ}\t{occ.split('@')[0]}\tglitchtip\ta\t{disp}\t{why}\n")
    f.close()
    return f.name


class T(unittest.TestCase):
    def run_cmp(self, live, shadow):
        out = subprocess.run([sys.executable, os.path.join(HERE, "shadow_compare.py"), live, shadow,
                              "--since", "2026-11-08T18:45:00Z", "--json"], capture_output=True, text=True, check=True)
        return json.loads(out.stdout)

    def test_compare(self):
        seed = ("2026-11-08T10:00:00+00:00", "gt:OLD@1", "file", "seed row, the same in both")
        live = ledger([seed, ("2026-11-08T19:00:00+00:00", "gt:A@1", "file", "l"),
                       ("2026-11-08T19:00:00+00:00", "gt:B@1", "dismiss", "l"),
                       ("2026-11-08T19:10:00+00:00", "gt:C@1", "file", "only live")])
        shadow = ledger([seed, ("2026-11-08T19:00:05+00:00", "gt:A@1", "file", "s"),
                         ("2026-11-08T19:00:05+00:00", "gt:B@1", "file", "s says file"),
                         ("2026-11-08T19:20:00+00:00", "gt:D@1", "escalate", "only shadow")])
        r = self.run_cmp(live, shadow)
        self.assertEqual((r["both"], r["same"], r["differ"]), (2, 1, 1), "rows before --since are the seed")
        self.assertEqual(r["differences"][0]["occurrence"], "gt:B@1")
        self.assertEqual((r["live_only"], r["shadow_only"]), (["gt:C@1"], ["gt:D@1"]))

    def test_newest_row_per_occurrence(self):
        live = ledger([("2026-11-08T19:00:00+00:00", "gt:A@1", "dismiss", "first"),
                       ("2026-11-08T19:30:00+00:00", "gt:A@1", "file", "re-triaged")])
        shadow = ledger([("2026-11-08T19:01:00+00:00", "gt:A@1", "file", "s")])
        r = self.run_cmp(live, shadow)
        self.assertEqual((r["same"], r["differ"]), (1, 0))

    def test_recurrence_row_is_not_a_decision(self):
        # PODCAST-PIPELINE-DEV-B, 2026-11-11: both filed; the shadow's later recurrence row read as "differ"
        live = ledger([("2026-11-11T15:36:18+00:00", "gt:B@1", "file", "l")])
        shadow = ledger([("2026-11-11T15:30:27+00:00", "gt:B@1", "file", "s"),
                         ("2026-11-11T16:19:17+00:00", "gt:B@1", "recurrence", "count 10 -> 16")])
        r = self.run_cmp(live, shadow)
        self.assertEqual((r["both"], r["same"], r["differ"]), (1, 1, 0))


if __name__ == "__main__":
    unittest.main(verbosity=1)
