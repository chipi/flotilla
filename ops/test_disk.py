"""python3 ops/test_disk.py: the disk step 1 prerequisites (RFC-0016 §8, §12): the free-ratio monitor (a case per instance and mountpoint, label
exclusion, hysteresis), the disk fact the acting step re-checks, a per-mountpoint metric read in the investigate station, and the verify."""
import json
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
import free_ratio  # noqa: E402
import metric_monitor  # noqa: E402
import sop_act  # noqa: E402
import sop_investigate as inv  # noqa: E402
import sops  # noqa: E402
import verify_free_ratio  # noqa: E402

common.REGISTRY = REGISTRY_FILE
ACTION = "disk.cleanup_known"
ENV = {"MONITOR_ACTION": ACTION, "MONITOR_METRIC": "node_filesystem_avail_bytes", "MONITOR_SIZE_METRIC": "node_filesystem_size_bytes",
       "MONITOR_GROUP_BY": "instance,mountpoint", "MONITOR_EXCLUDE": '{"fstype":"tmpfs|overlay"}', "MONITOR_CLEAR_AFTER": "2",
       "MONITOR_STEP": "10m"}


def labels(instance="host-a", mountpoint="/"):
    return {"instance": instance, "mountpoint": mountpoint}


def cfg(**over):
    return metric_monitor.parse_config(dict(ENV, **over), kind="disk_low")


class Expr(unittest.TestCase):
    def test_the_ratio_reads_both_metrics_and_drops_empty_filesystems(self):
        e = free_ratio.ratio_expr("node_filesystem_avail_bytes", "node_filesystem_size_bytes", {"fstype": "tmpfs|overlay"})
        self.assertEqual(e, 'last_over_time(node_filesystem_avail_bytes{fstype!~"tmpfs|overlay"}[10m]) / '
                            '(last_over_time(node_filesystem_size_bytes{fstype!~"tmpfs|overlay"}[10m]) > 0)')

    def test_a_group_is_read_by_its_labels(self):
        e = free_ratio.ratio_expr("a", "s", {}, instance="host-a", mountpoint="/data")
        self.assertIn('a{instance="host-a",mountpoint="/data"}', e)
        self.assertIn('s{instance="host-a",mountpoint="/data"}', e)

    def test_a_label_value_that_is_not_plain_is_never_a_query(self):
        for bad in ('x"}', "a b", "/a'b", "/a\\b", "", "x" * 200):
            with self.assertRaises(common.ConfigError, msg=repr(bad)):
                free_ratio.ratio_expr("a", "s", {}, instance=bad)
            if bad:  # "/" alone is the root mountpoint
                with self.assertRaises(common.ConfigError, msg=repr(bad)):
                    free_ratio.ratio_expr("a", "s", {}, instance="h", mountpoint="/" + bad)

    def test_target_encoding_round_trips_and_refuses_what_a_target_cannot_hold(self):
        for inst, mp, target in (("host-a", "/", "host:host-a/"), ("host-a", "/data/x-1.y", "host:host-a/data/x-1.y")):
            self.assertEqual(free_ratio.encode_target(labels(inst, mp)), target)
            self.assertEqual(free_ratio.decode_target(target), (inst, mp))
        for mp in ("/Volumes/Macintosh HD", "/data'", "/é", "data", "/" + "a" * 130):
            self.assertIsNone(free_ratio.encode_target(labels("h", mp)), mp)
        for t in ("host:a", "disk:a/b", "host:/b", "host:a b/c", "", None):
            self.assertIsNone(free_ratio.decode_target(t), t)

    def test_the_limit_is_the_actions_precondition_for_the_stage(self):
        a = common.action(ACTION)
        self.assertEqual(free_ratio.limit_of(a, "propose"), 0.15)
        self.assertEqual(free_ratio.limit_of(dict(a, pre_shadow=["free_ratio_below(0.2)"]), "shadow"), 0.2)
        for bad in ([], ["free_ratio_below(15%)"], ["free_ratio_below(0)"], ["free_ratio_below(1.5)"], ["backup_age_above(26h)"]):
            with self.assertRaises(common.ConfigError, msg=str(bad)):
                free_ratio.limit_of(dict(a, pre=bad), "propose")


class Config(unittest.TestCase):
    def test_the_params_as_the_generator_sets_them(self):
        c = cfg()
        self.assertEqual((c.group_by, c.clear_after, c.step, c.exclude), (["instance", "mountpoint"], 2, 600, {"fstype": "tmpfs|overlay"}))

    def test_defaults(self):
        env = {k: v for k, v in ENV.items() if k not in ("MONITOR_EXCLUDE", "MONITOR_CLEAR_AFTER", "MONITOR_STEP")}
        c = metric_monitor.parse_config(env, kind="disk_low")
        self.assertEqual((c.exclude, c.clear_after, c.step), ({}, 1, 600))

    def test_a_bad_value_is_a_config_error_never_a_query(self):
        bad = {"group_by not starting with instance": {"MONITOR_GROUP_BY": "mountpoint,instance"},
               "a group label that is not a name": {"MONITOR_GROUP_BY": "instance,mount point"},
               "clear_after 0": {"MONITOR_CLEAR_AFTER": "0"}, "clear_after text": {"MONITOR_CLEAR_AFTER": "x"},
               "clear_after too long": {"MONITOR_CLEAR_AFTER": "500"}, "a step of text": {"MONITOR_STEP": "soon"},
               "a step in hours": {"MONITOR_STEP": "2h"}, "exclude not JSON": {"MONITOR_EXCLUDE": "fstype=tmpfs"},
               "exclude a list": {"MONITOR_EXCLUDE": '["tmpfs"]'}, "exclude empty map": {"MONITOR_EXCLUDE": "{}"},
               "exclude a number": {"MONITOR_EXCLUDE": '{"fstype": 1}'},
               "exclude a quote": {"MONITOR_EXCLUDE": '{"fstype": "a\\"b"}'},
               "exclude a backslash": {"MONITOR_EXCLUDE": '{"fstype": "a\\\\b"}'},
               "exclude a single quote": {"MONITOR_EXCLUDE": '{"fstype": "a\'b"}'},
               "exclude a newline": {"MONITOR_EXCLUDE": '{"fstype": "a\\nb"}'},
               "exclude a space": {"MONITOR_EXCLUDE": '{"fstype": "a b"}'},
               "exclude a label that is not a name": {"MONITOR_EXCLUDE": '{"fs type": "tmpfs"}'},
               "exclude the name label": {"MONITOR_EXCLUDE": '{"__name__": "x"}'},
               "exclude a regex that does not compile": {"MONITOR_EXCLUDE": '{"fstype": "(tmpfs"}'},
               "exclude too long": {"MONITOR_EXCLUDE": json.dumps({"fstype": "a" * 300})},
               "a metric that is not a name": {"MONITOR_METRIC": "a b"}, "a size metric that is not a name": {"MONITOR_SIZE_METRIC": 'a"'}}
        for name, over in bad.items():
            with self.assertRaises(common.ConfigError, msg=name):
                cfg(**over)
        for k in ("MONITOR_ACTION", "MONITOR_METRIC", "MONITOR_SIZE_METRIC", "MONITOR_GROUP_BY"):
            with self.assertRaises(common.ConfigError, msg=k):
                metric_monitor.parse_config({x: v for x, v in ENV.items() if x != k}, kind="disk_low")
        with self.assertRaises(common.ConfigError):
            metric_monitor.parse_config(ENV, kind="not a kind")

    def test_a_regex_the_pipe_and_anchors_are_fine(self):
        c = cfg(MONITOR_EXCLUDE='{"fstype": "^(tmpfs|overlay|squashfs)$", "mountpoint": "/run/.*|/boot/efi"}')
        self.assertEqual(sorted(c.exclude), ["fstype", "mountpoint"])


class Run(unittest.TestCase):
    """The monitor's decision from the readings: the instant vector now, and the matrix of the last clear_after readings."""

    def run_it(self, now, history=(), **over):
        return metric_monitor.run(cfg(**over), 0.15, list(now), list(history))

    def test_a_case_per_group_below_the_limit(self):
        found, summary = self.run_it([(labels("host-a", "/"), 0.05), (labels("host-a", "/data"), 0.5), (labels("host-b", "/"), 0.1)])
        self.assertEqual([d["target"] for d in found], ["host:host-a/", "host:host-b/"])
        self.assertEqual([d["kind"] for d in found], ["disk_low"] * 2)
        d = found[0]
        self.assertEqual(d["data"]["labels"], labels("host-a", "/"))
        m = d["data"]["measure"]
        self.assertEqual((m["name"], m["value"], m["threshold"], m["source"]["action"], m["source"]["condition"]),
                         ("free_ratio", 0.05, 0.15, ACTION, "free_ratio_below"))
        self.assertIn("5%", d["title"])
        self.assertIn("host-a", d["title"])
        self.assertEqual(summary, [])

    def test_exactly_the_limit_is_not_below(self):
        self.assertEqual(self.run_it([(labels(), 0.15)])[0], [])

    def test_a_clear_group_with_no_history_reports_nothing(self):
        self.assertEqual(self.run_it([(labels(), 0.5)], [(labels(), [(0, 0.5), (600, 0.5)])])[0], [])

    def test_hysteresis_holds_a_group_until_clear_after_readings_are_clear(self):
        # clear_after 2: [low, clear] is not two clear readings yet, so it is still reported, and says it is held
        found, _ = self.run_it([(labels(), 0.4)], [(labels(), [(0, 0.05), (600, 0.4)])])
        self.assertEqual([d["target"] for d in found], ["host:host-a/"])
        self.assertTrue(found[0]["data"]["held"])
        self.assertIn("held", found[0]["detail"])
        # [clear, clear] has cleared
        self.assertEqual(self.run_it([(labels(), 0.4)], [(labels(), [(0, 0.4), (600, 0.4)])])[0], [])

    def test_clear_after_one_clears_at_the_first_clear_reading(self):
        self.assertEqual(self.run_it([(labels(), 0.4)], [(labels(), [(0, 0.05), (600, 0.4)])], MONITOR_CLEAR_AFTER="1")[0], [])

    def test_a_flap_is_one_case(self):
        # low, clear, low across three runs: every run reports the group, so flotillad keeps the one case open
        for hist in ([(0, 0.05), (600, 0.4)], [(0, 0.4), (600, 0.05)]):
            self.assertEqual(len(self.run_it([(labels(), 0.05 if hist[1][1] < 0.15 else 0.4)], [(labels(), hist)])[0]), 1)

    def test_a_group_that_vanished_is_held_while_a_low_reading_is_in_the_window(self):
        found, _ = self.run_it([], [(labels(), [(0, 0.05)])])
        self.assertEqual([d["target"] for d in found], ["host:host-a/"])
        self.assertEqual(self.run_it([], [(labels(), [(0, 0.5)])])[0], [])

    def test_a_group_that_cannot_be_a_target_is_named_in_the_summary(self):
        found, summary = self.run_it([(labels("host-a", "/Volumes/Macintosh HD"), 0.01), (labels("host-a", "/"), 0.01)])
        self.assertEqual([d["target"] for d in found], ["host:host-a/"])
        self.assertEqual(len(summary), 1)
        self.assertIn("Macintosh HD", summary[0])

    def test_no_more_than_fifty_and_the_fullest_first(self):
        now = [(labels("h", f"/m{i:02d}"), 0.001 * (i + 1)) for i in range(60)]
        found, summary = self.run_it(now)
        self.assertEqual(len(found), 50)
        self.assertEqual(found[0]["target"], "host:h/m00")
        self.assertTrue(any("60" in s and "50" in s for s in summary), summary)

    def test_a_reading_of_nothing_is_no_reading(self):
        # nan and inf never reach run: common drops them; a group with no value is simply absent
        self.assertEqual(self.run_it([])[0], [])


def vector(*rows):
    return [(m, v) for m, v in rows]


class MainRun(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.plan = os.path.join(self.d, "plan.json")
        self.queries = []

    def go(self, now_rows, hist_rows, **over):
        env = dict(ENV, **over, VM_URL="http://vm:8428", FLOTILLA_PLAN_FILE=self.plan, FLEETD_STAGE="propose")

        def qv(expr, **kw):
            self.queries.append(expr)
            return now_rows

        def qm(expr, start, end, step, **kw):
            self.queries.append((expr, start, end, step))
            return hist_rows
        with mock.patch.dict(os.environ, env, clear=False), mock.patch.object(common, "query_vector", qv), \
                mock.patch.object(common, "query_matrix", qm), mock.patch("time.time", return_value=10_000.0):
            metric_monitor.main(["--kind", "disk_low"])
        with open(self.plan) as f:
            return json.load(f)

    def test_the_report_is_detections_flotillad_reads(self):
        rep = self.go([(labels(), 0.05)], [(labels(), [(9400.0, 0.2), (10000.0, 0.05)])])
        self.assertEqual(rep["outcome"], "detections")
        self.assertEqual([d["target"] for d in rep["detections"]], ["host:host-a/"])
        expr = self.queries[0]
        self.assertTrue(expr.startswith("min by (instance, mountpoint) ("), expr)
        self.assertIn('fstype!~"tmpfs|overlay"', expr)
        # clear_after 2 at a 10m step: the history is one step back, to now
        self.assertEqual(self.queries[1][1:], (9400.0, 10000.0, 600))

    def test_clear_after_one_makes_no_history_query(self):
        self.go([(labels(), 0.5)], [], MONITOR_CLEAR_AFTER="1")
        self.assertEqual(len(self.queries), 1)

    def test_a_bad_exclude_is_exit_2_and_no_query(self):
        with mock.patch.dict(os.environ, dict(ENV, MONITOR_EXCLUDE='{"fstype": "a\'b"}', VM_URL="http://vm", FLOTILLA_PLAN_FILE=self.plan)):
            with self.assertRaises(SystemExit) as e:
                metric_monitor.entry(["--kind", "disk_low"])
        self.assertEqual(e.exception.code, 2)
        self.assertEqual(self.queries, [])


class Fact(unittest.TestCase):
    """sop_act's disk fact: the case's own mountpoint, read with fresh numbers."""

    CASE = {"id": "case-1", "kind": "disk_low", "target": "host:host-a/data"}

    def check(self, ratio, stage="propose", case=None):
        seen = []

        def read(c):
            seen.append(c)
            return ratio
        read.needs_case = True
        out = sop_act.check(common.action(ACTION), stage, {"free_ratio_below": ("free_ratio", read)}, case or self.CASE)
        return out, seen

    def test_below_the_limit_holds_and_says_what_it_read(self):
        (state, lines), seen = self.check(0.08)
        self.assertEqual(state, "holds")
        self.assertEqual(lines, ["free_ratio_below(0.15): holds (8.0% free)"])
        self.assertEqual(seen, [self.CASE])

    def test_above_the_limit_resolved_itself(self):
        (state, detail), _ = self.check(0.4)
        self.assertEqual(state, "resolved")
        self.assertIn("free_ratio_below(0.15) no longer holds", detail)

    def test_no_reading_is_unknown_never_resolved(self):
        (state, detail), _ = self.check(None)
        self.assertEqual(state, "unknown")
        self.assertIn("no reading", detail)

    def test_the_fact_reads_the_cases_instance_and_mountpoint_only(self):
        seen = []

        def q(expr):
            seen.append(expr)
            return 0.07
        with mock.patch.dict(os.environ, {}, clear=False):
            r = sop_act.disk_free_ratio(self.CASE, query=q)
        self.assertEqual(r, 0.07)
        self.assertEqual(len(seen), 1)
        self.assertIn('instance="host-a",mountpoint="/data"', seen[0])
        self.assertTrue(seen[0].startswith("min("), seen[0])

    def test_a_case_whose_target_is_not_a_mountpoint_has_no_reading(self):
        self.assertIsNone(sop_act.disk_free_ratio({"target": "host:host-a"}, query=lambda e: 0.1))
        self.assertIsNone(sop_act.disk_free_ratio({"target": "backup"}, query=lambda e: 0.1))

    def test_the_backup_fact_is_unchanged(self):
        reg = {"backup.rerun": common.action("backup.rerun")}
        state, lines = sop_act.check(reg["backup.rerun"], "propose", {"backup_age_above": ("age", lambda: 30.0)})
        self.assertEqual((state, lines[0]), ("holds", "backup_age_above(26h): holds"))

    def test_decide_re_checks_a_disk_sop_with_the_case(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        os.makedirs(os.path.join(d, "disk-low"))
        with open(os.path.join(d, "disk-low", "SKILL.md"), "w") as f:
            f.write('---\nname: disk-low\ndescription: Use when a disk is low.\n'
                    'applies_to: {"kinds":["disk_low"],"hosts":["*"],"alerts":[]}\ndiagnosis: []\nfix: [{"action":"disk.cleanup_known"}]\n'
                    'verify: {"check":"disk_free_above_threshold","within":"60m"}\nrollback: []\nrung: 2\nevidence: []\n---\nClean up.\n')
        reg = sops.load_registry([REGISTRY_FILE])

        def read(c):
            return 0.05
        read.needs_case = True
        out = sop_act.decide(self.CASE, sops.load_all(d), reg, "propose", fetch={"free_ratio_below": ("free_ratio", read)})
        self.assertEqual((out["outcome"], out["action"], out["params"]), ("plan", ACTION, {"host": "host-a/data"}), out)
        self.assertIn("free_ratio_below(0.15): holds (5.0% free)", out["detail"])


class Investigate(unittest.TestCase):
    def test_a_read_by_instance_is_the_old_expression(self):
        seen = []
        state, v, text = inv.read_metric({"name": "m", "instance": "host-a"}, lambda e: seen.append(e) or 1.0)
        self.assertEqual((state, v), ("ok", 1.0))
        self.assertEqual(seen, ['max(last_over_time(m{instance="host-a"}[1h]))'])

    def test_a_read_by_mountpoint(self):
        seen = []
        state, v, _ = inv.read_metric({"name": "m", "instance": "host-a", "mountpoint": "/data"}, lambda e: seen.append(e) or 2.0)
        self.assertEqual((state, v), ("ok", 2.0))
        self.assertEqual(seen, ['max(last_over_time(m{instance="host-a",mountpoint="/data"}[1h]))'])

    def test_a_mountpoint_that_is_not_plain_is_refused_and_never_sent(self):
        for mp in ('/a"b', "a", "/a b", "/a'b", 3, "/" + "a" * 200, "/a\\b"):
            seen = []
            state, _, _ = inv.read_metric({"name": "m", "instance": "host-a", "mountpoint": mp}, lambda e: seen.append(e) or 1.0)
            self.assertEqual((state, seen), ("refused", []), repr(mp))

    def test_the_reads_kept_carry_the_mountpoint_only_when_given(self):
        r = inv.Reads(lambda e: 3.0)
        r.tool("metric", {"name": "m", "instance": "host-a"})
        r.tool("metric", {"name": "m", "instance": "host-a", "mountpoint": "/data"})
        self.assertEqual(r.items, [{"name": "m", "instance": "host-a", "value": 3.0},
                                   {"name": "m", "instance": "host-a", "mountpoint": "/data", "value": 3.0}])
        detail = sop_act.investigated_detail(None, r.items, "x")
        self.assertEqual(sop_act.parse_investigated(detail)[1], r.items)

    def test_a_forged_mountpoint_in_a_read_is_not_a_read(self):
        bad = 'investigated: no fix | reads: {"reads":[{"name":"m","instance":"a","mountpoint":["x"],"value":1}],"refused":0,"failed":0}'
        self.assertIsNone(sop_act.parse_investigated(bad))


class Verify(unittest.TestCase):
    CASE = {"id": "case-1", "kind": "disk_low", "target": "host:host-a/data"}
    CHECK = {"verify": "disk_free_above_threshold", "action": ACTION, "since": "2026-11-27T10:00:00.5Z"}

    def decide(self, ratio, check=None, case=None):
        return verify_free_ratio.decide(check or self.CHECK, case or self.CASE, 0.15, ratio)

    def test_back_above_the_limit_is_verified(self):
        out = self.decide(0.4)
        self.assertEqual(out["outcome"], "verified")
        self.assertIn("40.0% free", out["detail"])
        self.assertIn("/data on host-a", out["detail"])

    def test_the_limit_itself_counts_as_recovered(self):
        self.assertEqual(self.decide(0.15)["outcome"], "verified")

    def test_still_low_is_not_yet(self):
        self.assertEqual(self.decide(0.05)["outcome"], "not_yet")

    def test_no_reading_is_not_yet(self):
        out = self.decide(None)
        self.assertEqual(out["outcome"], "not_yet")
        self.assertIn("no reading", out["detail"])

    def test_another_check_or_a_target_that_is_not_a_mountpoint_is_a_config_error(self):
        with self.assertRaises(common.ConfigError):
            self.decide(0.4, check=dict(self.CHECK, verify="backup_newer_than_execution"))
        with self.assertRaises(common.ConfigError):
            self.decide(0.4, case={"target": "host:host-a"})

    def test_the_check_is_the_one_the_registry_declares(self):
        self.assertEqual(common.action(ACTION)["check"]["verify"], verify_free_ratio.VERIFY)


class Registry(unittest.TestCase):
    def test_every_precondition_name_is_implemented(self):
        for c in common.action(ACTION)["pre"]:
            name, _ = common.parse(c)
            self.assertIn(name, sop_act.CONDITIONS, f"{c} is declared but not implemented")


if __name__ == "__main__":
    unittest.main()
