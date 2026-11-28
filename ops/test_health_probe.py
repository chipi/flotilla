"""python3 ops/test_health_probe.py: the health probe (block health-probe@1, docs/wip/health-contract.md): readings before a detection,
hysteresis, the recovery gate, an unknown reading that never opens and never clears, a read that fails the run."""
import json
import os
import random
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(ROOT, "runtime"))
REGISTRY_FILE = os.path.join(ROOT, "host", "registry.json")
os.environ["FLOTILLA_REGISTRY"] = REGISTRY_FILE
import common  # noqa: E402
import health  # noqa: E402
import health_probe as hp  # noqa: E402
import service_monitor  # noqa: E402

common.REGISTRY = REGISTRY_FILE
ACTION = "pipeline.rerun_scheduled"
NOW = "2026-11-28T10:00:00Z"
ENV = {"PROBE_ACTION": ACTION, "PROBE_SOURCE": "health_example", "PROBE_CAPABILITY": "scheduled-run", "PROBE_INSTANCE": "pipeline-host",
       "PROBE_READINGS": "2", "PROBE_CLEAR_AFTER": "2", "SOURCE_HEALTH_EXAMPLE": "https://health.example/health/ready"}


def comp(name, status="up", reason="", since="2026-11-27T03:10:00Z"):
    return {"name": name, "kind": "model", "status": status, "since": since, "reason": reason, "latency_ms": None, "checked_at": NOW}


def body(model="up", ready=None, reason="", asr="up", cap="scheduled-run"):
    ready = (model == "up" and asr == "up") if ready is None else ready
    blocked = [n for n, s in (("model-prod", model), ("asr", asr)) if s != "up"]
    return health.parse({"contract": "flotilla.health/1", "status": "up" if ready else "down", "checked_at": NOW, "version": "sha-1234567",
                         "components": [comp("model-prod", model, reason), comp("asr", asr)],
                         "capabilities": {cap: {"ready": ready, "needs": ["model-prod", "asr"], "blocked_by": [] if ready else blocked}}})


def cfg(**over):
    return hp.parse_config(dict(ENV, **over), kind="batch_window_failed")


class Mem:
    def __init__(self, snap=None):
        self.snap, self.saved = snap, 0

    def load(self):
        return self.snap

    def save(self, snap):
        self.snap, self.saved = snap, self.saved + 1


def feed(c, bodies, store=None):
    """Run the probe once per body; the reports, in order."""
    store = store or Mem()
    out = []
    for b in bodies:
        rep, snap = hp.build(c, b, store.load(), NOW)
        store.save(snap)
        out.append(rep["detections"])
    return out


class Advance(unittest.TestCase):
    def test_it_is_the_service_monitors_scan(self):
        rnd = random.Random(7)
        for _ in range(300):
            seq = [rnd.choice([1.0, 0.0, None]) for _ in range(rnd.randint(1, 14))]
            nd, nu = rnd.randint(1, 3), rnd.randint(1, 3)
            st = hp.fresh()
            for r in seq:
                st = hp.advance(st, r, nd, nu)
            self.assertEqual((st["opened"], st["down"]), service_monitor.scan(seq, nd, nu), (seq, nd, nu))


class Down(unittest.TestCase):
    def c(self, **over):
        return cfg(PROBE_REPORT="down", **over)

    def test_nothing_is_reported_before_n_readings(self):
        self.assertEqual(feed(self.c(), [body("down")])[0], [])

    def test_a_capability_not_ready_for_n_readings_is_one_detection_with_the_producers_facts(self):
        found = feed(self.c(), [body("down", reason="connection refused")] * 2)[1]
        self.assertEqual(len(found), 1)
        d = found[0]
        self.assertEqual((d["kind"], d["target"]), ("batch_window_failed", "host:pipeline-host"))
        self.assertEqual(d["data"]["health"]["blocked_by"], ["model-prod"])
        self.assertEqual(d["data"]["health"]["since"], "2026-11-27T03:10:00Z")
        self.assertEqual(d["data"]["health"]["reason"], "connection refused")
        self.assertIn("scheduled-run", d["title"])

    def test_it_is_held_until_clear_after_ready_readings_in_a_row(self):
        got = feed(self.c(), [body("down")] * 2 + [body("up")] + [body("up")])
        self.assertEqual([len(g) for g in got], [0, 1, 1, 0])
        self.assertTrue(got[2][0]["data"]["held"])

    def test_up_with_busy_in_its_reason_is_up(self):
        got = feed(self.c(), [body("up", reason="busy: one job running")] * 3)
        self.assertEqual([len(g) for g in got], [0, 0, 0])

    def test_an_unknown_reading_never_opens_and_never_clears(self):
        c = self.c()
        # never opens: capability absent from the body is unknown
        absent = body("up", cap="another-capability")
        self.assertEqual([len(g) for g in feed(c, [absent] * 4)], [0, 0, 0, 0])
        # never clears: opened, then three unknown readings
        got = feed(c, [body("down")] * 2 + [absent] * 3)
        self.assertEqual([len(g) for g in got], [0, 1, 1, 1, 1])

    def test_a_flap_after_it_closed_opens_a_new_one(self):
        got = feed(self.c(), [body("down")] * 2 + [body("up")] * 2 + [body("down")] + [body("down")])
        self.assertEqual([len(g) for g in got], [0, 1, 1, 0, 0, 1])

    def test_components_are_one_case_each(self):
        c = cfg(PROBE_REPORT="down", PROBE_CAPABILITY="", PROBE_COMPONENTS="model-prod,asr", PROBE_ACTION="dgx.wait_for_holder")
        with self.assertRaises(common.ConfigError):   # that action names no component of the route
            hp.check_watched(c, common.action("dgx.wait_for_holder"))
        two = body("down", asr="down")
        found = feed(c, [two] * 2)[1]
        self.assertEqual(sorted(d["target"] for d in found), ["host:pipeline-host/asr", "host:pipeline-host/model-prod"])


class Recovered(unittest.TestCase):
    def c(self, **over):
        return cfg(PROBE_REPORT="recovered", **over)

    def test_a_capability_that_was_never_blocked_reports_nothing(self):
        self.assertEqual([len(g) for g in feed(self.c(), [body("up")] * 5)], [0] * 5)

    def test_a_blip_shorter_than_readings_is_not_an_outage(self):
        self.assertEqual([len(g) for g in feed(self.c(), [body("down"), body("up"), body("up"), body("up")])], [0, 0, 0, 0])

    def test_ready_for_clear_after_readings_after_an_outage_is_one_detection(self):
        got = feed(self.c(), [body("down")] * 2 + [body("up")] * 3)
        self.assertEqual([len(g) for g in got], [0, 0, 0, 1, 1])
        d = got[3][0]
        self.assertEqual((d["kind"], d["target"]), ("batch_window_failed", "host:pipeline-host"))
        self.assertEqual(d["data"]["report"], "recovered")
        self.assertEqual(d["data"]["measure"]["value"], 1)

    def test_an_unknown_reading_never_reports_a_recovery(self):
        absent = body("up", cap="another-capability")
        got = feed(self.c(), [body("down")] * 2 + [body("up"), absent, body("up")])
        self.assertEqual([len(g) for g in got], [0, 0, 0, 0, 0])

    def test_it_stops_after_a_bounded_number_of_runs_and_waits_for_the_next_outage(self):
        got = feed(self.c(), [body("down")] * 2 + [body("up")] * (hp.MAX_RECOVERED_RUNS + 6) + [body("down")] * 2 + [body("up")] * 2)
        n = [len(g) for g in got]
        self.assertEqual(sum(n[2:2 + hp.MAX_RECOVERED_RUNS + 6]), hp.MAX_RECOVERED_RUNS)
        self.assertEqual(n[-1], 1)

    def test_a_new_outage_while_reported_ends_the_report(self):
        got = feed(self.c(), [body("down")] * 2 + [body("up")] * 2 + [body("down")])
        self.assertEqual([len(g) for g in got], [0, 0, 0, 1, 0])


class Failures(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.plan = os.path.join(self.d, "plan.json")
        self.env = dict(ENV, FLOTILLA_PLAN_FILE=self.plan, PROBE_REPORT="down")

    def run_probe(self, fetch, store):
        c = hp.parse_config(self.env, "batch_window_failed")
        with mock.patch.dict(os.environ, {"FLOTILLA_PLAN_FILE": self.plan}):
            return hp.run(c, store, fetch=fetch, environ=self.env, now=NOW)

    def test_a_read_that_fails_fails_the_run_reports_nothing_and_keeps_the_snapshot(self):
        store = Mem({"taken": NOW, "items": {}})
        before = json.dumps(store.snap)

        def boom(url):
            raise health.Unreadable("the health route could not be read (ConnectionRefusedError)")
        with self.assertRaises(common.ConfigError):
            self.run_probe(boom, store)
        self.assertFalse(os.path.exists(self.plan))
        self.assertEqual((store.saved, json.dumps(store.snap)), (0, before))

    def test_a_refused_body_fails_the_run_and_clears_no_case(self):
        store = Mem()

        def other(url):
            return {"contract": "something/else"}
        with self.assertRaises(common.ConfigError):
            self.run_probe(other, store)
        self.assertFalse(os.path.exists(self.plan))
        self.assertEqual(store.saved, 0)

    def test_an_unreadable_snapshot_fails_the_run_rather_than_start_over(self):
        store = Mem(hp.UNREADABLE)
        with self.assertRaises(common.ConfigError):
            self.run_probe(lambda url: body("down"), store)
        self.assertFalse(os.path.exists(self.plan))

    def test_a_good_read_writes_the_report_then_the_snapshot(self):
        store = Mem()
        rep, snap, _ = self.run_probe(lambda url: body("down"), store)
        self.assertEqual(json.load(open(self.plan))["outcome"], "detections")
        self.assertEqual(store.saved, 1)
        self.assertIn("capability:scheduled-run", snap["items"])

    def test_the_url_is_not_in_a_failure(self):
        def boom(url):
            raise RuntimeError(url)
        try:
            self.run_probe(boom, Mem())
        except common.ConfigError as e:
            self.assertNotIn("health.example", str(e))


class Config(unittest.TestCase):
    def test_a_probe_watches_a_capability_or_components_never_neither_never_both(self):
        for over in ({"PROBE_CAPABILITY": ""}, {"PROBE_COMPONENTS": "asr"}):
            with self.assertRaises(common.ConfigError):
                cfg(**over)

    def test_what_the_action_names_must_be_watched(self):
        c = cfg(PROBE_CAPABILITY="another-capability")
        with self.assertRaises(common.ConfigError):
            hp.check_watched(c, common.action(ACTION))
        hp.check_watched(cfg(), common.action(ACTION))

    def test_counts_and_names_are_checked(self):
        for over in ({"PROBE_READINGS": "0"}, {"PROBE_CLEAR_AFTER": "x"}, {"PROBE_INSTANCE": "host:port"}, {"PROBE_REPORT": "both"},
                     {"PROBE_CAPABILITY": "Bad Name"}, {"PROBE_SOURCE": "Bad"}):
            with self.assertRaises(common.ConfigError, msg=str(over)):
                cfg(**over)
        with self.assertRaises(common.ConfigError):
            hp.parse_config(ENV, kind="Not A Kind")


class RegistryHome(unittest.TestCase):
    def test_the_rerun_action_names_the_capability_and_the_check_the_flow_reads(self):
        a = common.action(ACTION)
        self.assertEqual(a["pre"], ["capability_ready(scheduled-run)"])
        self.assertEqual(a["check"]["verify"], "window_failed_episodes_processed")
        self.assertEqual(a["executor"], "record")
        self.assertEqual(a["risk"], "medium")


if __name__ == "__main__":
    unittest.main()
