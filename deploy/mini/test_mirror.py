"""python3 deploy/mini/test_mirror.py: the mirror's rules, with flotillad and docker stood in for."""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mirror as m  # noqa: E402


def run(i, start, end, status="ok", stage="propose"):
    return {"id": f"triage-cycle-{i}", "started_at": start, "ended_at": end, "status": status, "stage": stage}


class T(unittest.TestCase):
    def setUp(self):
        self.dest = tempfile.mkdtemp()
        self.vol = tempfile.mkdtemp()          # what the "volume" holds
        for rel, body in {"results/dispositions.tsv": "ts\tx\n1\ta\n", "results/filed.tsv": "f\n",
                          "results/probes/20261108T190000Z__glitchtip_X-1.json": "{}",
                          "queue/bug-1.json": "{}", "export/inbox.json": '{"schema":"fleet_inbox/v1"}',
                          "frozen/ledgers/w45/filed.tsv": "MUST NOT COPY", "fleet.env.bak-1": "SECRET"}.items():
            p = os.path.join(self.vol, rel); os.makedirs(os.path.dirname(p), exist_ok=True); open(p, "w").write(body)
        os.makedirs(os.path.join(self.dest, "frozen"))
        open(os.path.join(self.dest, "frozen", "mine"), "w").write("the freeze's own")
        open(os.path.join(self.dest, "fleet-gateway.env"), "w").write("HOST SECRET")
        self.states = [{"running": False, "stage": "propose",
                        "runs": [run(2, "2026-11-08T20:00:00Z", "2026-11-08T20:01:00Z"), run(1, "2026-11-08T19:50:00Z", "2026-11-08T19:51:00Z")]}]
        self.logs = []

    def copy(self, volume, image, tmp):
        for top in m.TREES:
            src = os.path.join(self.vol, top)
            for dp, _, fs in os.walk(src):
                for f in fs:
                    rel = os.path.relpath(os.path.join(dp, f), self.vol)
                    out = os.path.join(tmp, rel); os.makedirs(os.path.dirname(out), exist_ok=True)
                    open(out, "wb").write(open(os.path.join(self.vol, rel), "rb").read())

    def go(self, states=None, dest=None, cfg=None):
        seq = list(states or self.states)
        get = lambda: seq.pop(0) if len(seq) > 1 else seq[0]
        return m.mirror(dest or self.dest, "v", "img", cfg or {}, get_state=get, do_copy=self.copy, log=self.logs.append)

    def test_copies_three_trees_only(self):
        self.assertEqual(self.go(), 0, self.logs)
        self.assertTrue(self.logs[-1].startswith("MIRROR PASS"), self.logs)
        self.assertEqual(open(os.path.join(self.dest, "results/dispositions.tsv")).read(), "ts\tx\n1\ta\n")
        self.assertTrue(os.path.exists(os.path.join(self.dest, "results/probes/20261108T190000Z__glitchtip_X-1.json")),
                        "capture names unchanged")
        self.assertEqual(open(os.path.join(self.dest, "frozen", "mine")).read(), "the freeze's own")
        self.assertFalse(os.path.exists(os.path.join(self.dest, "frozen", "ledgers")), "frozen/ is never written")
        self.assertEqual(open(os.path.join(self.dest, "fleet-gateway.env")).read(), "HOST SECRET")
        self.assertFalse(os.path.exists(os.path.join(self.dest, "fleet.env.bak-1")))

    def test_stamp_and_cycles(self):
        self.go()
        st = json.load(open(os.path.join(self.dest, "results/.mirror_stamp")))
        self.assertEqual((st["end"], st["run_id"]), ("2026-11-08T20:01:00Z", "triage-cycle-2"))
        rows = open(os.path.join(self.dest, "results/cycles.tsv")).read().splitlines()
        self.assertEqual(rows[0], "run_id\tstart_utc\tend_utc\tstatus\tstage")
        self.assertEqual([r.split("\t")[0] for r in rows[1:]], ["triage-cycle-1", "triage-cycle-2"], "oldest first")
        # nothing new: skip, and no duplicate cycle rows
        self.go()
        self.assertIn("SKIP: nothing new", self.logs[-1])
        self.assertEqual(len(open(os.path.join(self.dest, "results/cycles.tsv")).read().splitlines()), 3)

    def test_skip_while_running(self):
        self.assertEqual(self.go([dict(self.states[0], running=True)]), 0)
        self.assertIn("in flight", self.logs[-1])
        self.assertFalse(os.path.exists(os.path.join(self.dest, "results")))

    def test_race_discards_the_copy(self):
        """The fleet session's race (2026-11-08): a run starts between the check and the end of the copy."""
        later = dict(self.states[0], runs=[run(3, "2026-11-08T20:02:00Z", None, status="running")] + self.states[0]["runs"])
        self.assertEqual(self.go([self.states[0], dict(later, running=True)]), 0)
        self.assertIn("RACED", self.logs[-1])
        self.assertFalse(os.path.exists(os.path.join(self.dest, "results")), "nothing installed")
        self.assertFalse(os.path.exists(os.path.join(self.dest, "results/.mirror_stamp")), "no stamp for a raced copy")
        # a run that started AND finished during the copy is a race too
        done = dict(self.states[0], runs=[run(3, "2026-11-08T20:02:00Z", "2026-11-08T20:02:30Z")] + self.states[0]["runs"])
        self.go([self.states[0], done])
        self.assertIn("RACED", self.logs[-1])

    def test_queue_synced_results_kept(self):
        self.go()
        os.remove(os.path.join(self.vol, "queue/bug-1.json"))
        os.remove(os.path.join(self.vol, "results/filed.tsv"))
        newer = dict(self.states[0], runs=[run(3, "2026-11-08T20:10:00Z", "2026-11-08T20:11:00Z")] + self.states[0]["runs"])
        self.go([newer])
        self.assertFalse(os.path.exists(os.path.join(self.dest, "queue/bug-1.json")), "queue/ follows the volume")
        self.assertTrue(os.path.exists(os.path.join(self.dest, "results/filed.tsv")), "results/ only gains")

    def test_live_folder_guard(self):
        old = m.LIVE
        m.LIVE = os.path.realpath(self.dest)
        try:
            self.assertEqual(self.go(cfg={}), 1)
            self.assertIn("MIRROR_LIVE=1", self.logs[-1])
            self.assertIn("STOP flag", self.logs[-1])
            open(os.path.join(self.dest, "STOP"), "w").close()
            self.assertEqual(self.go([dict(self.states[0], stage="shadow")], cfg={"MIRROR_LIVE": "1"}), 1)
            self.assertIn("still in shadow", self.logs[-1])
            self.assertEqual(self.go(cfg={"MIRROR_LIVE": "1"}), 0)
            self.assertTrue(self.logs[-1].startswith("MIRROR PASS"))
        finally:
            m.LIVE = old

    def test_atomic_write_leaves_no_temp(self):
        self.go()
        left = [f for dp, _, fs in os.walk(self.dest) for f in fs if f.startswith(".mirror-")]
        self.assertEqual(left, [])


if __name__ == "__main__":
    unittest.main(verbosity=1)
