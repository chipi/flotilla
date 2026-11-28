"""python3 ops/test_verify_run.py: the check routine of pipeline.rerun_scheduled (window_failed_episodes_processed), written against the
health contract's T5 shape (docs/wip/health-contract.md): a run's outcome readable with its status, the episodes processed, the failed ones
(with their stage and error type) and the skipped. T5 is not built by any producer yet: these are fixtures of the shape the contract asks
for, and the routine has not run against a real run outcome."""
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
REGISTRY_FILE = os.path.join(ROOT, "host", "registry.json")
os.environ["FLOTILLA_REGISTRY"] = REGISTRY_FILE
import common  # noqa: E402
import health  # noqa: E402
import verify_run_processed as vr  # noqa: E402

common.REGISTRY = REGISTRY_FILE
CASE = {"id": "case-1", "kind": "batch_window_failed", "target": "host:pipeline-host"}
CHECK = {"verify": "window_failed_episodes_processed", "action": "pipeline.rerun_scheduled", "since": "2026-11-28T01:00:00Z"}


def run(**over):
    r = {"run_id": "run-7", "trigger": "on_demand", "status": "finished", "started_at": "2026-11-28T01:05:00Z",
         "finished_at": "2026-11-28T02:40:00Z", "episodes": {"processed": 12, "failed": [], "skipped": 30}}
    r.update(over)
    return r


class Decide(unittest.TestCase):
    def decide(self, doc, check=None):
        return vr.decide(check or CHECK, CASE, doc)

    def test_a_finished_rerun_with_no_failed_episode_is_verified(self):
        out = self.decide(run())
        self.assertEqual(out["outcome"], "verified")
        self.assertIn("run-7", out["detail"])
        self.assertIn("12 processed", out["detail"])

    def test_a_rerun_still_going_is_not_yet(self):
        self.assertEqual(self.decide(run(status="running", finished_at=None))["outcome"], "not_yet")

    def test_failed_episodes_after_the_rerun_are_not_verified_and_say_where(self):
        out = self.decide(run(episodes={"processed": 10, "failed": [{"episode": "e-1", "stage": "transcribe", "error_type": "Timeout"}],
                                        "skipped": 30}))
        self.assertEqual(out["outcome"], "failed")
        self.assertIn("e-1", out["detail"])
        self.assertIn("transcribe", out["detail"])

    def test_a_run_that_failed_as_a_whole_is_failed(self):
        self.assertEqual(self.decide(run(status="failed"))["outcome"], "failed")

    def test_the_scheduled_run_or_one_older_than_the_action_is_not_the_rerun(self):
        self.assertEqual(self.decide(run(trigger="schedule"))["outcome"], "not_yet")
        self.assertEqual(self.decide(run(started_at="2026-11-28T00:30:00Z"))["outcome"], "not_yet")

    def test_no_outcome_a_wrong_shape_or_a_missing_field_is_never_verified(self):
        for doc in (None, [], {}, run(episodes=None), run(episodes={"processed": "12", "failed": [], "skipped": 0}),
                    run(episodes={"processed": 1, "skipped": 0}), run(status="finished", started_at="yesterday"),
                    run(episodes={"processed": 1, "failed": "none", "skipped": 0})):
            self.assertEqual(self.decide(doc)["outcome"], "not_yet", doc)

    def test_a_rerun_that_processed_nothing_is_not_verified_by_that_alone(self):
        # no episode failed and none was processed: nothing shows the window was repeated
        self.assertEqual(self.decide(run(episodes={"processed": 0, "failed": [], "skipped": 42}))["outcome"], "not_yet")

    def test_another_check_or_a_target_that_is_not_a_host_is_a_config_error(self):
        with self.assertRaises(common.ConfigError):
            self.decide(run(), check=dict(CHECK, verify="service_up_again"))
        with self.assertRaises(common.ConfigError):
            vr.decide(CHECK, {"target": "backup"}, run())

    def test_the_check_is_the_one_the_registry_declares(self):
        self.assertEqual(common.action("pipeline.rerun_scheduled")["check"]["verify"], vr.VERIFY)


class Main(unittest.TestCase):
    def go(self, fetch, check=None):
        d = tempfile.mkdtemp()
        plan = os.path.join(d, "plan.json")
        env = {"FLOTILLA_CASE": json.dumps(CASE), "FLOTILLA_CHECK": json.dumps(check or CHECK), "FLOTILLA_PLAN_FILE": plan,
               "PROCESS_SOURCE": "run_outcome", "SOURCE_RUN_OUTCOME": "https://runs.example/latest"}
        with mock.patch.dict(os.environ, env):
            vr.main(fetch=fetch)
        return json.load(open(plan))

    def test_it_reads_the_named_source_and_reports(self):
        seen = []
        out = self.go(lambda url: seen.append(url) or run())
        self.assertEqual(out["outcome"], "verified")
        self.assertEqual(seen, ["https://runs.example/latest"])

    def test_an_unreadable_source_is_not_yet_never_verified(self):
        def boom(url):
            raise health.Unreadable("could not be read")
        out = self.go(boom)
        self.assertEqual(out["outcome"], "not_yet")
        self.assertNotIn("runs.example", out["detail"])


if __name__ == "__main__":
    unittest.main()
