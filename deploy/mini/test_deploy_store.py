#!/usr/bin/env python3
"""Tests for deploy.sh's handling of the store: the snapshot before every `up`, and the rollback that
cannot run an older flotillad on a newer store.

deploy.sh runs for real against a throwaway checkout with the stub engine of test_triage_hooks.py. The state
volume is a real SQLite file and the snapshot/restore helper (store_snapshot.py) is the real one; only
docker and flotillad are stubs. Two schema generations exist as commits: A (store.go with 2 schema steps)
and B (3 steps). B's stub binary migrates the store to schema 3 when it starts; A refuses a store above 2.
"""
import os
import re
import shutil
import sqlite3
import subprocess
import unittest

import test_triage_hooks as th

GO_STORE = "package store\n\nvar schema = []string{\n%s}\n"


class StoreBase(th.DeployBase):
    def setUp(self):
        super().setUp()
        self.A = self.commit_store(2)
        self.B = self.commit_store(3)
        self.a, self.b = "sha-" + self.A[:7], "sha-" + self.B[:7]
        with open(os.path.join(self.home, "deployed.tsv"), "w") as f:
            f.write(f"2026-11-08T18:45:35Z\t{self.a}\t{self.A}\tpass\n")
        th.git(self.repo, "checkout", "-q", "--detach", self.A)
        self.db = os.path.join(self.vols, "flotilla-state", "flotilla.db")
        os.makedirs(os.path.dirname(self.db))
        con = sqlite3.connect(self.db)
        con.executescript("CREATE TABLE schema_version (n INTEGER NOT NULL); INSERT INTO schema_version VALUES (1);"
                          "INSERT INTO schema_version VALUES (2); CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('before')")
        con.commit(); con.close()
        self.cfg_tag(self.a, max=2); self.cfg_tag(self.b, max=3, migrate=3)
        self.backups = os.path.join(self.home, "backups")

    def commit_store(self, steps):
        path = os.path.join(self.repo, "internal", "store")
        os.makedirs(path, exist_ok=True)
        with open(os.path.join(path, "store.go"), "w") as f:
            f.write(GO_STORE % "".join(f"\t`CREATE TABLE s{i} (x INTEGER);`,\n" for i in range(steps)))
        th.git(self.repo, "add", "-A"); th.git(self.repo, "commit", "-qm", f"store {steps}")
        return th.git(self.repo, "rev-parse", "HEAD")

    def cfg_tag(self, tag, **kv):
        for k, v in kv.items():
            with open(os.path.join(self.stub, f"{tag}.{k}"), "w") as f:
                f.write(str(v))

    def schema_and_rows(self, path=None):
        con = sqlite3.connect(path or self.db)
        try:
            return (con.execute("SELECT MAX(n) FROM schema_version").fetchone()[0],
                    [r[0] for r in con.execute("SELECT v FROM t ORDER BY rowid")])
        finally:
            con.close()

    def snaps(self):
        if not os.path.isdir(self.backups):
            return []
        return sorted(d for d in os.listdir(self.backups) if re.match(r"sha-[0-9a-f]{7}-\d{8}T\d{6}Z", d))

    def docker_calls(self):
        with open(os.path.join(self.tmp, "docker_calls")) as f:
            return f.read().splitlines()

    def running(self):
        with open(os.path.join(self.stub, "running")) as f:
            return f.read().strip()


class SnapshotTest(StoreBase):
    def test_snapshot_is_taken_before_up_and_tagged_with_its_schema(self):
        r = self.deploy(self.b)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        calls = self.docker_calls()
        backup = calls.index("helper backup")
        up = next(i for i, l in enumerate(calls) if " up -d " in l)
        self.assertLess(backup, up, "the snapshot is taken before up")
        (name,) = self.snaps()
        self.assertRegex(name, rf"^{self.b}-\d{{8}}T\d{{6}}Z$")
        snap = os.path.join(self.backups, name)
        with open(os.path.join(snap, "SCHEMA")) as f:
            self.assertEqual(f.read().strip(), "2")
        # the snapshot is the store as it was before B's binary moved it to schema 3
        self.assertEqual(self.schema_and_rows(os.path.join(snap, "flotilla.db")), (2, ["before"]))
        self.assertEqual(self.schema_and_rows(), (3, ["before", "after-migration"]))
        self.assertIn("snapshot: ", r.stdout)

    def test_a_store_that_cannot_be_read_stops_the_deploy_before_up(self):
        with open(self.db, "wb") as f:
            f.write(b"this is not a database" * 100)
        r = self.deploy(self.b)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("nothing was stopped", r.stdout)
        self.assertFalse([l for l in self.docker_calls() if " up -d " in l])
        self.assertEqual(th.git(self.repo, "rev-parse", "HEAD"), self.A)

    def test_a_snapshot_that_fails_stops_the_deploy_before_up(self):
        r = self.deploy(self.b, env=dict(self.env, STUB_FAIL_BACKUP="1"))
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("snapshot of the store failed", r.stdout)
        self.assertFalse([l for l in self.docker_calls() if " up -d " in l])
        self.assertEqual(self.snaps(), [])
        self.assertFalse([d for d in os.listdir(self.backups) if d.endswith(".partial")], "no half-written snapshot is left")
        self.assertEqual(th.git(self.repo, "rev-parse", "HEAD"), self.A)

    def test_no_store_yet_is_a_first_install_not_a_failure(self):
        os.remove(self.db)
        r = self.deploy(self.b)
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("nothing to snapshot", r.stdout)
        self.assertEqual(self.snaps(), [])

    def test_only_the_newest_snapshots_of_this_script_are_kept(self):
        os.makedirs(os.path.join(self.backups, "sha-0000000-20200101T000000Z"))
        os.makedirs(os.path.join(self.backups, "somebody-elses-folder"))
        env = dict(self.env, FLOTILLA_BACKUPS_KEEP="3")
        for _ in range(4):
            self.assertEqual(self.deploy(self.b, env=env).returncode, 0)
        names = self.snaps()
        self.assertEqual(len(names), 3, names)
        self.assertNotIn("sha-0000000-20200101T000000Z", names, "the oldest went first")
        self.assertTrue(os.path.isdir(os.path.join(self.backups, "somebody-elses-folder")), "only this script's own are deleted")
        # two deploys in one second must not overwrite each other: the last three are all distinct deploys
        self.assertEqual(len(set(names)), 3)


class RollbackTest(StoreBase):
    def deploy_b_and_roll_back(self, env=None):
        self.assertEqual(self.deploy(self.b).returncode, 0)
        self.assertEqual(self.schema_and_rows(), (3, ["before", "after-migration"]))
        return self.deploy("--rollback", env=env)

    def check_restored(self, r):
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.schema_and_rows(), (2, ["before"]), "the store is the snapshot; the row written after it is gone")
        self.assertEqual(self.running(), self.a)
        self.assertIn("restored the store from", r.stdout)
        self.assertIn("rows written after it are lost", r.stdout)
        self.assertIn(f"rolled back to {self.a} (running and healthy)", r.stdout)
        self.assertTrue(r.stdout.rstrip().endswith(f"DEPLOY PASS ({self.a})"), r.stdout[-200:])
        # what was there before the restore is kept too: a snapshot of the schema-3 store
        schemas = []
        for d in self.snaps():
            with open(os.path.join(self.backups, d, "SCHEMA")) as f:
                schemas.append(f.read().strip())
        self.assertIn("3", schemas)
        calls = self.docker_calls()
        n = len(calls) - 1 - calls[::-1].index("helper restore")
        before = calls[:n]
        self.assertTrue(any(" stop " in l for l in before[len(before) - 6:]), "flotillad is stopped before the restore")
        self.assertTrue(any(" up -d " in l for l in calls[n:]), "up comes after the restore")

    def test_rollback_restores_the_snapshot_the_older_tag_accepts(self):
        r = self.deploy_b_and_roll_back()
        self.assertIn("newer than this flotillad (2)", r.stdout, "-check refused the store")
        self.check_restored(r)
        self.assertEqual(self.deploy_rows()[-1][1], self.a)

    def test_rollback_to_a_tag_whose_check_does_not_look_at_the_store(self):
        """A tag older than the store check: its -check passes and it would fail at up. The schema steps in its
        source say it cannot run the store."""
        self.cfg_tag(self.a, nocheck=1)
        r = self.deploy_b_and_roll_back()
        self.assertNotIn("newer than this flotillad", r.stdout)
        self.check_restored(r)

    def deploy_rows(self):
        with open(os.path.join(self.home, "deployed.tsv")) as f:
            return [l.split("\t") for l in f.read().splitlines()]

    def test_rollback_with_no_fitting_snapshot_changes_nothing(self):
        # the only snapshot is of a schema-3 store (a later deploy's), which tag A cannot run
        self.assertEqual(self.deploy(self.b).returncode, 0)
        (only,) = self.snaps()
        snap = os.path.join(self.backups, only)
        shutil.copy(self.db, os.path.join(snap, "flotilla.db"))
        with open(os.path.join(snap, "SCHEMA"), "w") as f:
            f.write("3\n")
        kept = self.snaps()
        calls_before, rows_before = self.docker_calls(), self.deploy_rows()
        db_before = open(self.db, "rb").read()

        r = self.deploy("--rollback")
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("no snapshot with schema <= 2", r.stdout)
        self.assertIn("nothing was stopped, restored or changed", r.stdout)
        self.assertNotIn("rolled back", r.stdout)
        self.assertEqual(self.snaps(), kept, "no new snapshot either")
        self.assertEqual(open(self.db, "rb").read(), db_before)
        self.assertEqual(self.running(), self.b)
        self.assertEqual(th.git(self.repo, "rev-parse", "HEAD"), self.B)
        self.assertEqual(self.deploy_rows(), rows_before, "no row in deployed.tsv")
        new = self.docker_calls()[len(calls_before):]
        self.assertFalse([l for l in new if " stop " in l or " up " in l or l.startswith("helper restore")], new)

    def test_a_rollback_target_that_is_not_healthy_is_never_reported_as_rolled_back(self):
        self.cfg_tag(self.b, migrate=2)   # B starts without moving the store: the rollback needs no restore
        self.assertEqual(self.deploy(self.b).returncode, 0)
        self.cfg_tag(self.a, unhealthy=1)
        r = self.deploy("--rollback")
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertNotIn("rolled back", r.stdout)
        self.assertIn(f"DEPLOY FAIL (rollback to {self.a})", r.stdout)
        self.assertEqual(self.running(), self.b, "the newer tag is running again, and the message says so")
        self.assertIn(f"{self.b} is running and healthy again", r.stdout)

    def test_a_failed_deploy_that_moved_the_store_gets_the_old_store_back_with_the_old_tag(self):
        self.cfg_tag(self.b, unhealthy=1)   # B migrates to schema 3, then is unhealthy
        r = self.deploy(self.b)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertEqual(self.schema_and_rows(), (2, ["before"]))
        self.assertEqual(self.running(), self.a)
        self.assertIn(f"rolled back to {self.a} (running and healthy)", r.stdout)
        self.assertIn("restored the store from", r.stdout)

    def test_a_failed_deploy_whose_old_tag_cannot_run_the_store_does_not_claim_a_rollback(self):
        self.cfg_tag(self.b, unhealthy=1)
        self.cfg_tag(self.a, unhealthy=1)
        r = self.deploy(self.b)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertNotIn("rolled back", r.stdout)
        self.assertIn(f"{self.a} is NOT running and healthy", r.stdout)

    def test_a_forward_deploy_of_a_tag_older_than_the_store_is_refused_before_anything_stops(self):
        self.assertEqual(self.deploy(self.b).returncode, 0)
        calls_before = len(self.docker_calls())
        r = self.deploy(self.a)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("deploy.sh --rollback restores a snapshot", r.stdout)
        self.assertIn("nothing was stopped", r.stdout)
        self.assertFalse([l for l in self.docker_calls()[calls_before:] if " stop " in l or " up " in l])
        self.assertEqual(self.schema_and_rows(), (3, ["before", "after-migration"]))

    def test_a_forward_deploy_onto_a_newer_store_is_refused_for_a_tag_without_the_store_check_too(self):
        self.cfg_tag(self.a, nocheck=1)   # A's -check passes; its source says it knows 2 schema steps
        self.assertEqual(self.deploy(self.b).returncode, 0)
        calls_before = len(self.docker_calls())
        r = self.deploy(self.a)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertNotIn("newer than this flotillad", r.stdout)
        self.assertIn("the store is at schema 3 and %s runs up to 2" % self.a, r.stdout)
        self.assertFalse([l for l in self.docker_calls()[calls_before:] if " stop " in l or " up " in l])
        self.assertEqual(self.running(), self.b)


if __name__ == "__main__":
    unittest.main(verbosity=2)
