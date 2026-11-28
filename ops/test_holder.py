"""python3 ops/test_holder.py: the DGX case made holder-aware (the operator's decision of 2026-11-28) and the readiness fact for the
scheduled run's re-run:

  a job HOLDS the host (robotics with its trainer, code, a judging sweep)  -> wait: dgx.wait_for_holder, no mode switch
  a mode LEFT BEHIND (non-prod, nothing holds it)                           -> dgx.gpu_mode_prod
  anything unknown                                                          -> never the switch (robotics: the wait; elsewhere: escalate)
  capability_ready(<capability>): one fresh read of the health route; not ready blocks the re-run, unknown never resolves it
"""
import os
import shutil
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
import dgx_holder  # noqa: E402
import health  # noqa: E402
import sop_act  # noqa: E402
import sops  # noqa: E402

common.REGISTRY = REGISTRY_FILE
NAN = float("nan")
NOT_SET = dgx_holder.NOT_CONFIGURED


class Classify(unittest.TestCase):
    def state(self, *a, **kw):
        return dgx_holder.classify(*a, **kw)[0]

    def test_robotics_with_its_trainer_up_is_held(self):
        self.assertEqual(self.state("robotics", 2.0, True), "held")
        self.assertEqual(self.state("robotics", 0.0, True), "held")
        self.assertEqual(self.state("robotics", 3.0, NOT_SET), "held")

    def test_robotics_with_an_unknown_trainer_reading_is_held_never_free(self):
        self.assertEqual(self.state("robotics", None, None), "held")
        self.assertEqual(self.state("robotics", 0.0, None), "held")
        self.assertEqual(self.state("robotics", None, NOT_SET), "held")
        self.assertEqual(self.state("robotics", None, False), "held")
        self.assertEqual(self.state("robotics", NAN, NOT_SET), "held")

    def test_robotics_is_free_only_when_every_reading_says_nothing_runs(self):
        self.assertEqual(self.state("robotics", 0.0, False), "free")
        self.assertEqual(self.state("robotics", 0.0, NOT_SET), "free")

    def test_another_mode_is_held_while_a_process_holds_the_gpu_and_free_when_none_does(self):
        for mode in ("code", "free", "sweep"):
            self.assertEqual(self.state(mode, 1.0, NOT_SET), "held", mode)
            self.assertEqual(self.state(mode, 0.0, NOT_SET), "free", mode)

    def test_another_mode_with_no_process_reading_is_unknown(self):
        self.assertIsNone(self.state("code", None, NOT_SET))
        self.assertIsNone(self.state("code", NAN, NOT_SET))

    def test_no_mode_is_unknown_and_prod_is_not_a_mode_to_switch_from(self):
        self.assertIsNone(self.state(None, 0.0, False))
        self.assertIsNone(self.state("prod", 0.0, NOT_SET))

    def test_the_names_of_the_modes_are_the_installations(self):
        self.assertEqual(self.state("train", 0.0, True, trainer_mode="train"), "held")
        self.assertIsNone(self.state("main", 0.0, NOT_SET, prod_mode="main"))


class Read(unittest.TestCase):
    CASE = {"id": "c", "kind": "dgx_not_serving", "target": "host:host-a"}
    ENV = {"HOLDER_MODE_METRIC": "gpu_mode", "HOLDER_MODE_LABEL": "mode", "HOLDER_PROCESS_METRIC": "gpu_processes"}

    def read(self, modes, procs, container=None, env=None, case=None):
        asked = []

        def qv(expr, **kw):
            asked.append(expr)
            return modes

        def q(expr, **kw):
            asked.append(expr)
            return container if "container" in expr else procs
        e = dict(self.ENV, **(env or {}))
        out = dgx_holder.read(case or self.CASE, e, query=q, query_vector=qv)
        return out, asked

    def test_it_reads_the_mode_and_the_processes_of_the_cases_instance(self):
        (state, why), asked = self.read([({"instance": "host-a", "mode": "code"}, 1.0)], 0.0)
        self.assertEqual(state, "free")
        self.assertEqual(asked[0], 'max by (instance, mode) (last_over_time(gpu_mode[10m]) == 1)')
        self.assertEqual(asked[1], 'max(last_over_time(gpu_processes{instance="host-a"}[10m]))')
        self.assertIn("code", why)

    def test_the_container_signal_is_read_when_one_is_named(self):
        (state, why), asked = self.read([({"instance": "host-a", "mode": "robotics"}, 1.0)], 0.0, container=1.0,
                                        env={"HOLDER_CONTAINER_METRIC": "trainer_container_up"})
        self.assertEqual(state, "held")
        self.assertIn('min(last_over_time(trainer_container_up{instance="host-a"}[10m]))', asked)

    def test_robotics_with_no_reading_of_the_trainer_is_held(self):
        (state, _), _ = self.read([({"instance": "host-a", "mode": "robotics"}, 1.0)], None, container=None,
                                  env={"HOLDER_CONTAINER_METRIC": "trainer_container_up"})
        self.assertEqual(state, "held")

    def test_another_instances_mode_is_not_the_cases(self):
        (state, _), _ = self.read([({"instance": "host-b", "mode": "code"}, 1.0)], 0.0)
        self.assertIsNone(state)

    def test_two_modes_at_once_or_none_is_unknown(self):
        two = [({"instance": "host-a", "mode": "code"}, 1.0), ({"instance": "host-a", "mode": "robotics"}, 1.0)]
        self.assertIsNone(self.read(two, 0.0)[0][0])
        self.assertIsNone(self.read([], 0.0)[0][0])

    def test_a_nan_reading_is_unknown(self):
        self.assertIsNone(self.read([({"instance": "host-a", "mode": "code"}, 1.0)], NAN)[0][0])
        self.assertIsNone(self.read([({"instance": "host-a", "mode": "code"}, NAN)], 0.0)[0][0])

    def test_a_case_that_is_not_on_a_host_has_no_reading(self):
        self.assertIsNone(self.read([({"instance": "host-a", "mode": "code"}, 1.0)], 0.0, case={"target": "backup"})[0][0])

    def test_a_metric_that_is_not_plain_is_never_a_query(self):
        for bad in ('x"}', "a b", ""):
            with self.assertRaises(common.ConfigError):
                self.read([], 0.0, env={"HOLDER_PROCESS_METRIC": bad})

    def test_a_query_that_fails_fails_the_read(self):
        def boom(expr, **kw):
            raise OSError("connection refused")
        with self.assertRaises(OSError):
            dgx_holder.read(self.CASE, self.ENV, query=boom, query_vector=boom)


def sop_dir(fixes, verify="service_up_again", kind="dgx_not_serving"):
    d = tempfile.mkdtemp()
    os.makedirs(os.path.join(d, "dgx-not-prod"))
    with open(os.path.join(d, "dgx-not-prod", "SKILL.md"), "w") as f:
        f.write('---\nname: dgx-not-prod\ndescription: Use when the prod model server is down.\n'
                f'applies_to: {{"kinds":["{kind}"],"hosts":["*"],"alerts":[]}}\ndiagnosis: []\n'
                f'fix: [{",".join(chr(123) + chr(34) + "action" + chr(34) + ":" + chr(34) + a + chr(34) + chr(125) for a in fixes)}]\n'
                f'verify: {{"check":"{verify}","within":"30m"}}\nrollback: []\nrung: 2\nevidence: []\n---\nWait or swap.\n')
    return d


def reader(value, calls=None, **flags):
    def r(*a):
        if calls is not None:
            calls.append(a)
        return value
    for k, v in dict({"needs_case": True}, **flags).items():
        setattr(r, k, v)
    return r


class Decide(unittest.TestCase):
    CASE = {"id": "case-1", "kind": "dgx_not_serving", "target": "host:host-a"}
    FIXES = ["dgx.wait_for_holder", "dgx.gpu_mode_prod"]

    def setUp(self):
        self.reg = sops.load_registry([REGISTRY_FILE])
        self.dirs = []

    def sops(self, fixes, **kw):
        d = sop_dir(fixes, **kw)
        self.dirs.append(d)
        return sops.load_all(d, record=False)

    def tearDown(self):
        for d in self.dirs:
            shutil.rmtree(d, True)

    def decide(self, holder, service=0.0, fixes=None, alternatives=True, calls=None):
        fetch = {"service_down": ("service_up", reader(service, needs_arg=True)),
                 "holder_active": ("holder", reader(holder, calls)), "no_holder_active": ("holder", reader(holder, calls))}
        return sop_act.decide(self.CASE, self.sops(fixes or self.FIXES), self.reg, "propose", fetch, alternatives=alternatives)

    def test_a_job_that_holds_the_host_plans_the_wait_and_never_the_switch(self):
        out = self.decide("held (mode robotics: trainer up)")
        self.assertEqual((out["outcome"], out["action"]), ("plan", "dgx.wait_for_holder"), out)
        self.assertIn("holder_active: holds (held (mode robotics: trainer up))", out["detail"])

    def test_a_mode_left_behind_plans_the_switch(self):
        out = self.decide("free (mode code: no GPU process)")
        self.assertEqual((out["outcome"], out["action"], out["params"]), ("plan", "dgx.gpu_mode_prod", {"host": "host-a"}), out)
        self.assertIn("fix[1] dgx.gpu_mode_prod", out["detail"])
        self.assertIn("fix[0] dgx.wait_for_holder skipped", out["detail"])

    def test_no_reading_of_the_holder_is_unknown_never_the_switch(self):
        out = self.decide(None)
        self.assertEqual(out["outcome"], "escalate", out)
        self.assertNotIn("action", out)

    def test_a_service_that_serves_again_resolved_itself(self):
        self.assertEqual(self.decide("free (mode code)", service=1.0)["outcome"], "resolved_itself")

    def test_a_service_with_no_reading_is_unknown_not_resolved(self):
        self.assertEqual(self.decide("held (x)", service=None)["outcome"], "escalate")

    def test_the_holder_is_read_once_for_every_alternative(self):
        calls = []
        self.decide("free (mode code)", calls=calls)
        self.assertEqual(len(calls), 1)

    def test_the_order_is_the_sops_first_whose_preconditions_hold(self):
        out = self.decide("held (x)", fixes=list(reversed(self.FIXES)))
        self.assertEqual(out["action"], "dgx.wait_for_holder", out)   # the switch's precondition does not hold; the wait's does

    def test_without_the_option_two_fixes_are_still_refused_as_before(self):
        out = self.decide("held (x)", alternatives=False)
        self.assertEqual(out["outcome"], "escalate")
        self.assertIn("2 fix steps", out["detail"])

    def test_alternatives_must_share_the_one_check_the_sop_verifies(self):
        out = self.decide("held (x)", fixes=["dgx.wait_for_holder", "pipeline.rerun_scheduled"])
        self.assertEqual(out["outcome"], "escalate")

    def test_robotics_with_an_unreadable_trainer_waits_through_the_real_reader(self):
        env = {"HOLDER_MODE_METRIC": "gpu_mode", "HOLDER_MODE_LABEL": "mode", "HOLDER_PROCESS_METRIC": "gpu_processes",
               "HOLDER_CONTAINER_METRIC": "trainer_container_up"}
        with mock.patch.dict(os.environ, env), \
                mock.patch.object(common, "query_vector", lambda e, **kw: [({"instance": "host-a", "mode": "robotics"}, 1.0)]), \
                mock.patch.object(common, "query", lambda e, **kw: None):
            fetch = dict(sop_act.FACTS, service_down=("service_up", reader(0.0, needs_arg=True)))
            out = sop_act.decide(self.CASE, self.sops(self.FIXES), self.reg, "propose", fetch, alternatives=True)
        self.assertEqual((out["outcome"], out["action"]), ("plan", "dgx.wait_for_holder"), out)

    def test_the_switch_precondition_holds_only_for_a_free_reading(self):
        for value, expect in (("free (x)", "holds"), ("held (x)", "resolved"), (None, "unknown")):
            fetch = {"service_down": ("service_up", reader(0.0, needs_arg=True)), "no_holder_active": ("holder", reader(value))}
            state, _ = sop_act.check(common.action("dgx.gpu_mode_prod"), "propose", fetch, self.CASE)
            self.assertEqual(state, expect, value)


class Capability(unittest.TestCase):
    CASE = {"id": "case-1", "kind": "batch_window_failed", "target": "host:pipeline-host"}
    ENV = {"HEALTH_SOURCE": "health_example", "SOURCE_HEALTH_EXAMPLE": "https://health.example/health/ready"}

    def body(self, ready):
        comps = [{"name": n, "status": "up" if ready else ("down" if n == "model-prod" else "up")} for n in ("model-prod", "asr", "database")]
        return {"contract": "flotilla.health/1", "status": "up" if ready else "down", "checked_at": "2026-11-28T10:00:00Z", "components": comps,
                "capabilities": {"scheduled-run": {"ready": ready, "needs": ["model-prod", "asr", "database"],
                                                   "blocked_by": [] if ready else ["model-prod"]}}}

    def decide(self, fetch):
        d = sop_dir(["pipeline.rerun_scheduled"], verify="window_failed_episodes_processed", kind="batch_window_failed")
        self.addCleanup(shutil.rmtree, d, True)
        with mock.patch.dict(os.environ, self.ENV):
            return sop_act.decide(self.CASE, sops.load_all(d, record=False), sops.load_registry([REGISTRY_FILE]), "propose",
                                  health_fetch=fetch)

    def test_a_ready_capability_plans_the_rerun(self):
        out = self.decide(lambda url: self.body(True))
        self.assertEqual((out["outcome"], out["action"], out["params"]), ("plan", "pipeline.rerun_scheduled", {"host": "pipeline-host"}), out)
        self.assertIn("capability_ready(scheduled-run): holds", out["detail"])

    def test_a_capability_that_is_not_ready_blocks_the_rerun(self):
        out = self.decide(lambda url: self.body(False))
        self.assertNotEqual(out["outcome"], "plan", out)
        self.assertNotIn("action", out)
        self.assertEqual(out["outcome"], "resolved_itself")

    def test_a_route_that_cannot_be_read_is_unknown_never_resolved_never_planned(self):
        def boom(url):
            raise health.Unreadable("could not be read")
        out = self.decide(boom)
        self.assertEqual(out["outcome"], "escalate", out)

    def test_a_refused_body_is_unknown_too(self):
        self.assertEqual(self.decide(lambda url: {"contract": "other/1"})["outcome"], "escalate")

    def test_a_capability_the_body_does_not_name_is_unknown(self):
        b = self.body(True)
        b["capabilities"] = {}
        self.assertEqual(self.decide(lambda url: b)["outcome"], "escalate")

    def test_the_source_must_be_configured(self):
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("HEALTH_SOURCE", None)
            with self.assertRaises(common.ConfigError):
                sop_act.capability_reading(self.CASE, "scheduled-run")


class Registry(unittest.TestCase):
    def test_every_precondition_name_the_new_actions_use_is_implemented(self):
        for name in ("dgx.gpu_mode_prod", "dgx.wait_for_holder", "pipeline.rerun_scheduled"):
            for c in common.action(name)["pre"]:
                self.assertIn(common.parse(c)[0], sop_act.CONDITIONS, f"{name}: {c} is declared but not implemented")

    def test_the_wait_and_the_switch_share_the_check_the_sop_verifies(self):
        self.assertEqual(common.action("dgx.wait_for_holder")["check"], common.action("dgx.gpu_mode_prod")["check"])

    def test_the_wait_is_record_only_and_the_rerun_is_medium_risk(self):
        self.assertEqual(common.action("dgx.wait_for_holder")["executor"], "record")
        self.assertEqual(common.action("pipeline.rerun_scheduled")["risk"], "medium")


if __name__ == "__main__":
    unittest.main()
