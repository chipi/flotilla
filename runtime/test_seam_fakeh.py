"""python3 runtime/test_seam_fakeh.py: the seam end-to-end over the fake harness that speaks to a model (fakeh.FAKE_HTTP_CLI), so CI,
which has no pi, runs every seam of runtime/HARNESS_SEAMS.md through the station driver, the adapter layer and the runner: the
prompt on stdin, the tools values, the turn cap and timeout, a session continued by its reference, the calls lines, the gateway's cost,
the proxy that sees nothing, the isolation in the run's folder. What it cannot show is pi: that is test_seam_pi.py, on a machine
that has it. A harness that cannot enforce read-only is checked here too (the runner's own check of the workspace)."""
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import adapter  # noqa: E402
import fakeh  # noqa: E402
import seam_e2e as se  # noqa: E402


class FakeSeams(se.SeamSuite):
    NAME = "fakehttp"
    READ_TOOLS = ("find", "grep", "ls", "read")
    WRITE_TOOLS = ("bash", "edit", "write")
    SESSIONS = "fakeh/sessions/*.json"

    @classmethod
    def unavailable(cls):
        return ""

    @classmethod
    def prepare(cls):
        class Holder:
            def addCleanup(self, fn, *a, **k):
                cls.addClassCleanup(fn, *a, **k)
        cls.fake, cls.cli = fakeh.install_fake_http(Holder())

    @classmethod
    def env(cls, proxy):
        return {"HTTP_PROXY": proxy.url, "HTTPS_PROXY": proxy.url, "NO_PROXY": "127.0.0.1,localhost,::1"}


class FakeSeamsForAHarnessThatCannotEnforceReadOnly(unittest.TestCase):
    """S6 as the runner enforces it: the CLI offers every tool whatever it was told, a read station writes, and the run is stopped
    with a clear message (and its episode and record are on disk first)."""

    def test_a_write_by_a_read_station_is_a_fatal_stop_with_the_episode_kept(self):
        import json
        import shutil
        import tempfile
        import station_run
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        h, _ = fakeh.install_fake_http(self, enforces_readonly=False, ignores_readonly=True)
        w = se.World(d, "ro", se.Script())
        self.addCleanup(w.close)
        st = station_run.Station(name="reader", harness="fakehttp", model=w.route("flash"), prompt="WRITE it anyway", tools="read",
                                 system_path=w.system, alias="flash", timeout=60)
        base = dict(os.environ, HOME=w.home)
        with self.assertRaises(adapter.ReadOnlyViolation) as cm:
            station_run.run_station(st, {}, run_dir=w.run_dir, workdir=w.workdir, base_env=base, calls_path=w.calls_path)
        self.assertEqual(cm.exception.changed, ["src/a.js"])
        self.assertIn("cannot enforce it", str(cm.exception))
        with open(os.path.join(w.workdir, "src", "a.js")) as f:
            self.assertIn("=> 2", f.read(), "the CLI did write: that is what the check caught")
        stem = os.path.join(w.run_dir, "episodes", "001-reader")
        self.assertTrue(os.path.exists(stem + ".json") and os.path.exists(stem + ".record.json"), "the stop is on record")
        with open(stem + ".record.json") as f:
            self.assertEqual(json.load(f)["station"], "reader")
        self.assertTrue(w.calls, "and so are its model calls")

    def test_the_same_run_on_a_harness_that_enforces_it_changes_nothing(self):
        import shutil
        import tempfile
        import station_run
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        fakeh.install_fake_http(self, enforces_readonly=True)
        w = se.World(d, "ro", se.Script())
        self.addCleanup(w.close)
        st = station_run.Station(name="reader", harness="fakehttp", model=w.route("flash"), prompt="WRITE it anyway", tools="read",
                                 system_path=w.system, alias="flash", timeout=60)
        station_run.run_station(st, {}, run_dir=w.run_dir, workdir=w.workdir, base_env=dict(os.environ, HOME=w.home))
        with open(os.path.join(w.workdir, "src", "a.js")) as f:
            self.assertIn("=> 1", f.read(), "the CLI refused the tool it did not offer")


if __name__ == "__main__":
    unittest.main()
