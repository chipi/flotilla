#!/usr/bin/env python3
"""Tests of the e2e's own helpers: make_shadows.py, extend_state.py, make_probes.py and the matrix's report (python3 deploy/e2e/test_e2e_helpers.py).

The e2e uses them on a live stack; these run on a folder, so a helper that does nothing (and so lets a check pass for nothing) is caught here.
"""
import hashlib
import json
import os
import re
import shutil
import sys
import tempfile
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
sys.path.insert(0, HERE)
os.environ.setdefault("WORK", tempfile.gettempdir())
os.environ.setdefault("ROOT", ROOT)
import extend_state  # noqa: E402
import make_probes  # noqa: E402
import make_shadows  # noqa: E402


def write(path, text=""):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


def manifest_hash(folder):
    """The version's identity as flotillad computes it: over the sorted (path, sha256) pairs of the files under it."""
    pairs = []
    for root, _, names in os.walk(folder):
        for n in names:
            p = os.path.join(root, n)
            pairs.append((os.path.relpath(p, folder), hashlib.sha256(open(p, "rb").read()).hexdigest()))
    return hashlib.sha256(json.dumps(sorted(pairs)).encode()).hexdigest()


class MakeShadows(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.specs = self.tmp.name
        for fleet, routine, worker in (("a", "r1", "ops-read"), ("a", "r2", "triage"), ("a", "r3", "ops-read"), ("e2e", "r4", "ops-read")):
            write(f"{self.specs}/{fleet}/{routine}/routine.yaml", f"fleet: {fleet}\nroutine: {routine}\nworker: {worker}\n")
            write(f"{self.specs}/{fleet}/{routine}/v1/spec.yaml", "run:\n  command: true\n")
            write(f"{self.specs}/{fleet}/{routine}/v1/text.md", "a text\n")
        write(f"{self.specs}/a/r3/v2/spec.yaml", "run:\n  command: 'true'\n")

    def test_only_the_routines_of_the_named_workers_without_a_v2_outside_the_e2e_fleet(self):
        self.assertEqual(make_shadows.generate(self.specs, {"ops-read"}), ["a/r1"])
        self.assertFalse(os.path.exists(f"{self.specs}/a/r2/v2"), "another worker's routine")
        self.assertEqual(os.listdir(f"{self.specs}/a/r3/v2"), ["spec.yaml"], "a routine with a v2 of its own is left alone")
        self.assertFalse(os.path.exists(f"{self.specs}/e2e/r4/v2"), "the e2e's own fleet")

    def test_the_copy_is_v1s_files_plus_a_marker_so_the_hash_differs(self):
        make_shadows.generate(self.specs, {"ops-read"})
        v1, v2 = f"{self.specs}/a/r1/v1", f"{self.specs}/a/r1/v2"
        self.assertEqual(open(f"{v1}/spec.yaml").read(), open(f"{v2}/spec.yaml").read())
        self.assertEqual(open(f"{v1}/text.md").read(), open(f"{v2}/text.md").read())
        self.assertTrue(os.path.exists(f"{v2}/{make_shadows.MARKER}"))
        self.assertNotEqual(manifest_hash(v1), manifest_hash(v2))
        os.remove(f"{v2}/{make_shadows.MARKER}")
        self.assertEqual(manifest_hash(v1), manifest_hash(v2), "without the marker the copy would be v1 itself")

    def test_a_second_run_makes_nothing(self):
        make_shadows.generate(self.specs, {"ops-read"})
        self.assertEqual(make_shadows.generate(self.specs, {"ops-read"}), [])

    def test_the_shipped_examples_give_the_matrix_its_routines(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        shutil.copytree(os.path.join(ROOT, "fleets"), d, dirs_exist_ok=True)
        made = make_shadows.generate(d, {"ops-read"})
        self.assertGreaterEqual(len(made), 20, made)
        for r in made:
            self.assertTrue(os.path.isdir(f"{d}/{r}/v1") and os.path.isdir(f"{d}/{r}/v2"), r)
            self.assertIn("worker: ops-read", open(f"{d}/{r}/routine.yaml").read())


class ExtendState(unittest.TestCase):
    def test_json_and_tsv_stores_gain_a_key_and_a_column_and_keep_what_they_had(self):
        with tempfile.TemporaryDirectory() as d:
            write(f"{d}/contacts.json", json.dumps([{"id": "c1", "email": "a@x"}, {"id": "c2", "email": "b@x"}]))
            write(f"{d}/campaigns.json", json.dumps({"items": [{"id": "k1"}], "name": "n"}))
            write(f"{d}/results/filed.tsv", "fingerprint\trepo\nf1\to/r\n")
            write(f"{d}/notes.txt", "not a store\n")
            self.assertEqual(extend_state.main(["x", d]), 0)
            contacts = json.load(open(f"{d}/contacts.json"))
            self.assertEqual([c["id"] for c in contacts], ["c1", "c2"])
            self.assertTrue(all(c[extend_state.KEY] == extend_state.VALUE and "email" in c for c in contacts))
            camp = json.load(open(f"{d}/campaigns.json"))
            self.assertEqual(camp["name"], "n")
            self.assertEqual(camp[extend_state.KEY], extend_state.VALUE)
            self.assertEqual(camp["items"][0][extend_state.KEY], extend_state.VALUE)
            self.assertEqual(open(f"{d}/results/filed.tsv").read(), f"fingerprint\trepo\t{extend_state.KEY}\nf1\to/r\t{extend_state.VALUE}\n")
            self.assertEqual(open(f"{d}/notes.txt").read(), "not a store\n")

    def test_it_says_how_many_it_changed_and_an_empty_folder_says_zero(self):
        with tempfile.TemporaryDirectory() as d:
            write(f"{d}/a.json", json.dumps([{"id": 1}, {"id": 2}, {"id": 3}]))
            import io
            from contextlib import redirect_stdout
            out = io.StringIO()
            with redirect_stdout(out):
                extend_state.main(["x", d])
            self.assertEqual(out.getvalue().strip(), "EXTENDED files 1 records 3")
        with tempfile.TemporaryDirectory() as d, __import__("contextlib").redirect_stdout(__import__("io").StringIO()) as out:
            extend_state.main(["x", d])
        self.assertEqual(out.getvalue().strip(), "EXTENDED files 0 records 0")


class MakeProbes(unittest.TestCase):
    def test_a_probe_per_worker_class_with_the_workers_own_token_path(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertEqual(make_probes.main(["x", d, "ops-read", "remote-e2e:/remote/worker.token"]), 0)
            a = open(f"{d}/e2e/token-probe-ops-read.yaml").read()
            b = open(f"{d}/e2e/token-probe-remote-e2e.yaml").read()
            self.assertIn("worker: ops-read", a)
            self.assertIn('wt_path = "/run/flotilla/worker.token"', a)
            self.assertIn("worker: remote-e2e", b)
            self.assertIn('wt_path = "/remote/worker.token"', b)
            for text in (a, b):
                self.assertIn("enabled: false", text)

    def test_the_probe_program_compiles_and_checks_what_it_claims(self):
        with tempfile.TemporaryDirectory() as d:
            make_probes.main(["x", d, "bugfix"])
            text = open(f"{d}/e2e/token-probe-bugfix.yaml").read()
        body = re.search(r"python3 - <<'PY'\n(.*?)\n    PY\n", text, re.S).group(1)
        code = textwrap.dedent(body)
        compile(code, "probe", "exec")
        for need in ("!= 401", "!= 200", "0o600", ".flotilla-token-", "someone-elses", "ns=bugfix/fix"):
            self.assertIn(need, code, f"the probe does not check {need}")


class MatrixDecisions(unittest.TestCase):
    def test_only_a_shadows_own_decision_counts_not_that_it_ran(self):
        import sqlite3
        import matrix
        con = sqlite3.connect(":memory:")
        con.execute("CREATE TABLE shadow_records (routine TEXT, kind TEXT, payload TEXT)")
        for r, kind, payload in (("a/b@v2", "run.started", "{}"), ("a/b@v2", "run.ended", "{}"), ("a/b@v2", "report.refused", "{}"),
                                 ("a/b@v2", "decision", '{"outcome": "none"}')):
            con.execute("INSERT INTO shadow_records VALUES (?, ?, ?)", (r, kind, payload))
        self.assertEqual(matrix.decisions(con, "a/b"), 0, "a shadow that ran and decided nothing")
        con.execute("INSERT INTO shadow_records VALUES ('a/b@v2', 'report', '{}')")
        con.execute("INSERT INTO shadow_records VALUES ('a/b@v2', 'decision', '{\"outcome\": \"propose\"}')")
        con.execute("INSERT INTO shadow_records VALUES ('other/c@v2', 'report', '{}')")
        self.assertEqual(matrix.decisions(con, "a/b"), 2)


class MatrixReport(unittest.TestCase):
    def test_the_summary_counts_cells_and_a_fail_is_exit_1(self):
        import io
        from contextlib import redirect_stdout
        import matrix
        matrix.cells[:] = [("a/b", "versions", "PASS", ""), ("a/b", "shadow", "SKIP", "no live run"), ("a/b", "unchanged", "FAIL", "its rows in the store changed")]
        out = io.StringIO()
        with redirect_stdout(out):
            rc = matrix.report(["a/b"])
        self.assertEqual(rc, 1)
        self.assertIn("NOEFFECT PASS 1 / SKIP 1 (no live run) / FAIL 1", out.getvalue())
        self.assertIn("unchanged FAIL: its rows in the store changed", out.getvalue())
        matrix.cells[:] = [("a/b", "versions", "PASS", "")]
        with redirect_stdout(io.StringIO()):
            self.assertEqual(matrix.report(["a/b"]), 0)


if __name__ == "__main__":
    unittest.main()
