"""Tests for ops/ranked_watch.py (RFC-0017) against synthesized ci.json and slo.json (ops/testdata/ranked_watch, placeholder names).

  cd ops && python3 -m unittest test_ranked_watch
"""
import copy
import datetime
import json
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import common  # noqa: E402
import ranked_watch as rw  # noqa: E402

DATA = os.path.join(HERE, "testdata", "ranked_watch")


def load(name):
    with open(os.path.join(DATA, name)) as f:
        return json.load(f)


CI, SLO = load("ci.json"), load("slo.json")
FRESH = "ci-collector--fresh-within-15-min"
TAKEN = "2026-01-15T06:00:00Z"
NOW = datetime.datetime(2026, 1, 15, 6, 0, 0, tzinfo=datetime.timezone.utc)
FIXES = os.path.join(DATA, "fixes.yaml")


def cfg(**kw):
    c = {"source": "hub_ci", "list": "steps.7d", "key": "id", "cost": "lost", "top": 5, "rise": (0.5, 600),
         "signal_source": "hub_slo", "fresh": FRESH, "signal_ids": ["ci-runners--*"], "fixes": "", "unit": "seconds"}
    c.update(kw)
    return c


class Store:
    def __init__(self, snap=None):
        self.snap, self.saved = snap, []

    def load(self):
        return self.snap

    def save(self, snap):
        self.saved.append(snap)
        self.snap = snap


def build(c=None, ci=CI, slo=SLO, prev=None, fixes=None):
    return rw.build(c or cfg(), ci, slo, prev, fixes or {}, TAKEN)


def snapshot_of(c=None, ci=CI):
    return build(c, ci)[1]


def targets(report):
    return sorted(d["target"] for d in report["detections"])


class TestRank(unittest.TestCase):
    def test_top_n_by_cost_largest_first(self):
        top, _ = rw.rank(CI["steps"]["7d"], "id", "lost", 3)
        self.assertEqual([r["lost"] for r in top], [4378, 2596, 2520])

    def test_ties_break_by_key_so_a_run_is_repeatable(self):
        rows = [{"id": "b", "lost": 5}, {"id": "a", "lost": 5}, {"id": "c", "lost": 9}]
        top, _ = rw.rank(rows, "id", "lost", 3)
        self.assertEqual([r["id"] for r in top], ["c", "a", "b"])

    def test_a_row_without_a_numeric_cost_or_a_key_refuses_the_run(self):
        for rows in ([{"id": "a"}], [{"lost": 1}], [{"id": "a", "lost": "9"}], [{"id": "a", "lost": True}], ["x"]):
            with self.assertRaises(common.ConfigError, msg=repr(rows)):
                rw.rank(rows, "id", "lost", 3)

    def test_a_key_listed_twice_refuses_the_run(self):
        with self.assertRaises(common.ConfigError):
            rw.rank([{"id": "a", "lost": 1}, {"id": "a", "lost": 2}], "id", "lost", 3)

    def test_the_list_path_is_dotted(self):
        self.assertEqual(len(rw.dig(CI, "steps.7d")), 6)
        with self.assertRaises(common.ConfigError):
            rw.dig(CI, "steps.30d")


class TestBaseline(unittest.TestCase):
    def test_first_run_records_a_baseline_and_reports_no_changes(self):
        rep, snap = build()
        self.assertIn("baseline", rep["summary"][0])
        self.assertEqual([d for d in rep["detections"] if not d["target"].startswith("perf:slo/ci-runners")], [])
        self.assertEqual(list(snap["items"]), [r["id"] for r in sorted(CI["steps"]["7d"], key=lambda r: -r["lost"])[:5]])
        self.assertEqual(snap["taken"], TAKEN)
        self.assertEqual(snap["windows"]["from"], "2026-01-08T00:00:00Z")

    def test_a_breached_service_level_is_a_case_even_on_the_baseline(self):
        rep, _ = build()
        self.assertEqual(targets(rep), ["perf:slo/ci-runners--online-share-of-runners-seen-online", "perf:slo/ci-runners--queue-wait-under-10-min"])


class TestDiff(unittest.TestCase):
    def setUp(self):
        self.prev = snapshot_of()

    def test_nothing_changed_reports_no_change_cases(self):
        rep, _ = build(prev=self.prev)
        self.assertEqual([d for d in rep["detections"] if "ci-runners" not in d["target"]], [])
        self.assertIn("compared with the run of 2026-01-15", rep["summary"][0])

    def test_a_new_entry_in_the_top_is_a_case(self):
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"].append({"id": "ffffffffff01", "repo": "example/api-service", "workflow": "CI", "job": "e2e", "step": "Warm cache", "lost": 3000})
        rep, snap = build(ci=ci, prev=self.prev)
        new = [d for d in rep["detections"] if d["target"] == "perf:step/ffffffffff01"]
        self.assertEqual(len(new), 1)
        self.assertEqual(new[0]["kind"], "perf_regression")
        self.assertIn("New in the top 5", new[0]["title"])
        self.assertTrue(any("new in the top: Warm cache (example/api-service)" in l for l in rep["summary"]))
        self.assertIn("ffffffffff01", snap["items"])

    def test_a_rise_needs_both_thresholds(self):
        for lost, qualifies in ((4378 + 2200, True), (4378 + 2189, True), (4378 + 599, False)):
            ci = copy.deepcopy(CI)
            ci["steps"]["7d"][0]["lost"] = lost
            rep, _ = build(c=cfg(rise=(0.5, 600)), ci=ci, prev=self.prev)
            got = [d for d in rep["detections"] if d["target"] == "perf:step/a1b2c3d4e501"]
            self.assertEqual(bool(got), qualifies, lost)
        # past the minutes but not the ratio: 600 more on a 4378 base is 13.7 %
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"][0]["lost"] = 4378 + 700
        rep, _ = build(c=cfg(rise=(0.5, 600)), ci=ci, prev=self.prev)
        self.assertEqual([d for d in rep["detections"] if d["target"] == "perf:step/a1b2c3d4e501"], [])
        # past the ratio but not the minutes: 400 on 400 is 100 %
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"][5]["lost"] = 800
        c = cfg(top=6, rise=(0.5, 600))
        rep, _ = build(c=c, ci=ci, prev=snapshot_of(c))
        self.assertEqual([d for d in rep["detections"] if d["target"] == "perf:step/a1b2c3d4e506"], [])

    def test_a_rise_names_both_costs(self):
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"][1]["lost"] = 2596 + 3000
        rep, _ = build(ci=ci, prev=self.prev)
        d = [d for d in rep["detections"] if d["target"] == "perf:step/a1b2c3d4e502"][0]
        self.assertIn("rose from 43 min to 93 min", d["title"])
        self.assertEqual(d["data"]["measure"]["value"], 5596)
        self.assertEqual(d["data"]["measure"]["threshold"], 2596)

    def test_an_entry_that_drops_out_is_reported_and_never_a_case(self):
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"][3]["lost"] = 100  # Build image was 4th of 4; Install dependencies takes its place
        c = cfg(top=4)
        rep, _ = build(c=c, ci=ci, prev=snapshot_of(c))
        self.assertTrue(any("dropped out: Build image (example/api-service), was 30 min, now 100 s" in l for l in rep["summary"]), rep["summary"])
        self.assertNotIn("fixed?", " ".join(rep["summary"]))
        self.assertNotIn("perf:step/a1b2c3d4e504", targets(rep))

    def test_a_dropped_entry_with_a_listed_fix_is_flagged_fixed(self):
        ci = copy.deepcopy(CI)
        del ci["steps"]["7d"][0]
        fixes = {"a1b2c3d4e501": {"item": "a1b2c3d4e501", "link": "example/web-app#12", "owner": "example/web-app"}}
        rep, _ = build(ci=ci, prev=self.prev, fixes=fixes)
        line = [l for l in rep["summary"] if "dropped out" in l][0]
        self.assertIn("no longer listed", line)
        self.assertIn("fixed? a fix is listed", line)

    def test_a_renamed_step_is_one_gone_and_one_new(self):
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"][0]["id"] = "a1b2c3d4e599"
        rep, _ = build(ci=ci, prev=self.prev)
        text = " ".join(rep["summary"])
        self.assertIn("dropped out", text)
        self.assertIn("new in the top", text)
        self.assertIn("perf:step/a1b2c3d4e599", targets(rep))

    def test_a_changed_top_or_list_is_a_new_baseline_not_a_flood_of_new_entries(self):
        rep, snap = build(c=cfg(top=6), prev=self.prev)  # was 5: the sixth would be "new" against the old top
        self.assertEqual([d for d in rep["detections"] if "ci-runners" not in d["target"]], [])
        self.assertIn("the ranking changed (top 5 of steps.7d before): a new baseline for it, no changes", rep["summary"][0])
        self.assertEqual(len(snap["items"]), 6)
        rep, _ = build(c=cfg(list="steps.yesterday", top=5, signal_source=""), slo=None, prev=self.prev)
        self.assertIn("a new baseline", rep["summary"][0])

    def test_no_rise_rule_means_no_rise_cases(self):
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"][0]["lost"] = 99999
        rep, _ = build(c=cfg(rise=None), ci=ci, prev=self.prev)
        self.assertNotIn("perf:step/a1b2c3d4e501", targets(rep))


class TestWindows(unittest.TestCase):
    def test_the_windows_compared_are_named_in_the_first_line(self):
        rep, _ = build()
        self.assertIn("7d 2026-01-08 to 2026-01-15, previous window 2026-01-01 to 2026-01-08 (held)", rep["summary"][0])

    def test_a_previous_window_the_source_does_not_hold_says_so(self):
        ci = copy.deepcopy(CI)
        ci["steps"]["windows"]["7d"]["prevHeld"] = False
        self.assertIn("(not held)", build(ci=ci)[0]["summary"][0])

    def test_a_source_with_no_windows_says_so(self):
        ci = copy.deepcopy(CI)
        del ci["steps"]["windows"]
        self.assertIn("the source names no window", build(ci=ci)[0]["summary"][0])

    def test_the_daily_period_reads_its_own_window(self):
        rep, _ = build(c=cfg(list="steps.yesterday", top=2, signal_source=""), slo=None)
        self.assertIn("yesterday 2026-01-14 to 2026-01-15, previous window 2026-01-13 to 2026-01-14", rep["summary"][0])
        self.assertTrue(rep["summary"][1].startswith("[yesterday] 1."))


class TestSignals(unittest.TestCase):
    def test_each_signal_is_read_against_its_objective_and_state(self):
        rep, _ = build()
        lines = [l for l in rep["summary"] if "CI runners" in l]
        self.assertEqual(len(lines), 3)
        self.assertIn("CI runners online (share of runners seen online): 96.2% over 7d against 99.0%, breached", lines[0])
        self.assertIn("CPU headroom (throttled under 5% of periods): 97.0% over 7d against 95.0%, meeting", " ".join(lines))

    def test_a_glob_selects_rows_and_the_freshness_row_is_not_a_signal(self):
        rows = rw.signal_rows(SLO, ["ci-*"], FRESH)
        self.assertEqual(len(rows), 3)
        self.assertNotIn(FRESH, [r["id"] for r in rows])
        self.assertEqual(rw.signal_rows(SLO, ["nothing-*"], FRESH), [])

    def test_only_a_breached_row_is_a_case_and_it_carries_its_numbers(self):
        rep, _ = build()
        d = [d for d in rep["detections"] if d["target"].endswith("queue-wait-under-10-min")][0]
        self.assertEqual(d["data"]["measure"], {"name": "queue wait under 10 min", "value": 0.848, "threshold": 0.95})
        self.assertNotIn("perf:slo/ci-runners--cpu-headroom", targets(rep))

    def test_a_standing_breach_has_the_same_target_every_run(self):
        """flotillad keys a case by kind and target and attaches a repeat to the open one (store.DetectIncident), so a breach that
        stands reports the same key every run and never opens a second case; a cleared one stops being reported."""
        first, snap = build()
        second, _ = build(prev=snap)
        self.assertEqual(targets(first), targets(second))
        fixed = copy.deepcopy(SLO)
        fixed["rows"][0]["state"] = "meeting"
        third, _ = build(slo=fixed, prev=snap)
        self.assertNotIn("perf:slo/ci-runners--online-share-of-runners-seen-online", targets(third))

    def test_a_target_is_a_valid_case_target(self):
        import re
        pattern = re.compile(r"^[a-z][a-z0-9_]*:[A-Za-z0-9._/-]{1,120}$")  # dispatch.targetRe
        self.assertRegex(rw.target("step", "ab-cd/" + "x" * 90), pattern)
        for d in build()[0]["detections"]:
            self.assertRegex(d["target"], pattern)

    def swapped(self):
        """homelab's change of 2026-11-27: one CI service level is gone, and a different one (another SLI, not a rename) is new."""
        slo = copy.deepcopy(SLO)
        gone = next(r for r in slo["rows"] if r["id"] == "ci-runners--cpu-headroom")
        slo["rows"].remove(gone)
        new = copy.deepcopy(gone)
        new.update(id="ci-runners--cpu-wait-under-10-of-the-time", sli="CPU wait under 10% of the time", state="breached", ratio={"7d": 0.8})
        slo["rows"].append(new)
        return slo

    def test_a_signal_row_that_disappears_is_reported_gone_once_and_is_not_a_breach(self):
        _, snap = build()
        self.assertIn("ci-runners--cpu-headroom", snap["signals"])
        rep, snap2 = build(slo=self.swapped(), prev=snap)
        gone = [l for l in rep["summary"] if "gone" in l]
        self.assertEqual(len(gone), 1, rep["summary"])
        self.assertIn("CI runners CPU headroom (throttled under 5% of periods)", gone[0])
        self.assertNotIn("perf:slo/ci-runners--cpu-headroom", targets(rep))
        self.assertNotIn("ci-runners--cpu-headroom", snap2["signals"])
        again, _ = build(slo=self.swapped(), prev=snap2)
        self.assertEqual([l for l in again["summary"] if "gone" in l], [])

    def test_a_new_signal_row_is_read_once_with_no_history_and_the_glob_finds_it(self):
        _, snap = build()
        rep, snap2 = build(slo=self.swapped(), prev=snap)
        line = [l for l in rep["summary"] if "CPU wait under 10% of the time" in l]
        self.assertEqual(len(line), 1)
        self.assertIn("new, no history, not compared", line[0])
        self.assertIn("perf:slo/ci-runners--cpu-wait-under-10-of-the-time", targets(rep))
        self.assertIn("ci-runners--cpu-wait-under-10-of-the-time", snap2["signals"])
        again, _ = build(slo=self.swapped(), prev=snap2)
        self.assertNotIn("no history", " ".join(again["summary"]))

    def test_the_baseline_has_no_new_or_gone_signals(self):
        rep, _ = build(slo=self.swapped())
        self.assertNotIn("no history", " ".join(rep["summary"]))
        self.assertNotIn("gone", " ".join(rep["summary"]))

    def test_a_snapshot_from_before_signals_were_kept_compares_no_signals(self):
        _, snap = build()
        del snap["signals"]
        rep, _ = build(slo=self.swapped(), prev=snap)
        self.assertNotIn("no history", " ".join(rep["summary"]))
        self.assertNotIn("gone", " ".join(rep["summary"]))

    def test_a_step_without_signals_reads_none(self):
        rep, _ = build(c=cfg(signal_source="", signal_ids=[], fresh=""), slo=None)
        self.assertEqual([l for l in rep["summary"] if "against" in l], [])
        self.assertEqual(rep["detections"], [])


class TestStale(unittest.TestCase):
    def stale_slo(self, ratio=0.5):
        slo = copy.deepcopy(SLO)
        for r in slo["rows"]:
            if r["id"] == FRESH:
                r["ratio"]["1d"] = ratio
        return slo

    def test_a_stale_collector_reports_only_that_and_keeps_the_old_snapshot(self):
        rep, snap = build(slo=self.stale_slo(), prev=snapshot_of())
        self.assertIsNone(snap)
        self.assertEqual([d for d in rep["detections"] if not d["title"].startswith("carried, source stale")], [])  # see TestCarried
        self.assertEqual(len(rep["summary"]), 1)
        self.assertIn("the source is stale, nothing compared", rep["summary"][0])
        self.assertIn("50.0% of the last day, below its 99.0% objective", rep["summary"][0])

    def test_a_missing_freshness_row_is_stale(self):
        slo = copy.deepcopy(SLO)
        slo["rows"] = [r for r in slo["rows"] if r["id"] != FRESH]
        rep, snap = build(slo=slo)
        self.assertIsNone(snap)
        self.assertIn("is not in the service-level source", rep["summary"][0])

    def test_a_freshness_row_without_a_ratio_is_stale(self):
        slo = copy.deepcopy(SLO)
        for r in slo["rows"]:
            if r["id"] == FRESH:
                r["ratio"] = {}
        self.assertIn("has no 1d ratio", build(slo=slo)[0]["summary"][0])

    def test_a_fresh_collector_is_not_stale(self):
        self.assertEqual(rw.stale(SLO, FRESH), "")
        self.assertEqual(rw.stale(SLO, ""), "")


class TestTopThree(unittest.TestCase):
    def test_the_top_three_name_fix_and_owner_or_say_no_owner(self):
        fixes = {"a1b2c3d4e501": {"item": "a1b2c3d4e501", "link": "example/web-app#12", "owner": "example/web-app"},
                 "a1b2c3d4e502": {"item": "a1b2c3d4e502", "link": "example/web-app#13"}}
        rep, _ = build(fixes=fixes)
        top = [l for l in rep["summary"] if l.startswith("[7d] ") and l[5:7] in ("1.", "2.", "3.", "4.")]
        self.assertEqual(len(top), 3)
        self.assertIn("1. Restore model cache (example/web-app): 73 min; fix example/web-app#12, owner example/web-app", top[0])
        self.assertIn("fix example/web-app#13, no owner", top[1])
        self.assertIn("no known fix, no owner", top[2])

    def test_fixes_are_read_from_the_knowledge_file(self):
        fixes = rw.load_fixes(FIXES)
        self.assertEqual(fixes["a1b2c3d4e501"]["owner"], "example/web-app")
        self.assertEqual(rw.load_fixes(""), {})

    def test_the_fixes_parser_is_strict(self):
        ok = rw.parse_fixes("# c\nfixes:\n  - item: a\n    link: 'x y'\n    owner: \"o\"\n  - item: b\n")
        self.assertEqual(ok, [{"item": "a", "link": "x y", "owner": "o"}, {"item": "b"}])
        self.assertEqual(rw.parse_fixes("fixes:\n"), [])
        for bad in ("items:\n", "fixes:\n  - link: x\n", "fixes:\n  link: x\n", "fixes:\n  - item: a\n    item: b\n", "fixes:\nitem: a\n",
                    "fixes:\n  - item a\n"):
            with self.assertRaises(common.ConfigError, msg=repr(bad)):
                rw.parse_fixes(bad)


class TestReport(unittest.TestCase):
    def test_a_report_is_at_most_twenty_short_lines(self):
        rows = [{"id": f"id{i:03d}", "lost": 1000 - i, "step": "S" * 90, "repo": "example/" + "r" * 60} for i in range(40)]
        ci = {"steps": {"7d": rows}}
        prev = snapshot_of(cfg(top=30), ci)
        for r in rows[:20]:
            r["lost"] += 5000
        rep, _ = build(c=cfg(top=30, signal_source=""), ci=ci, slo=None, prev=prev)
        self.assertEqual(len(rep["summary"]), 20)
        self.assertIn("more lines", rep["summary"][-1])

    def test_a_report_is_the_monitors_shape(self):
        rep, _ = build()
        self.assertEqual(rep["outcome"], "detections")
        for d in rep["detections"]:
            self.assertEqual(set(d) - {"data"}, {"kind", "target", "detail", "title"})
            json.dumps(d)
            self.assertLess(len(json.dumps(d["data"])), 8192)


class TestRun(unittest.TestCase):
    ENV = {"SOURCE_HUB_CI": "http://hub.invalid/ci.json", "SOURCE_HUB_SLO": "http://hub.invalid/slo.json", "SOURCE_ALLOW_HTTP": "hub.invalid"}

    def setUp(self):
        self.plan = tempfile.mktemp()
        os.environ["FLOTILLA_PLAN_FILE"] = self.plan

    def tearDown(self):
        os.environ.pop("FLOTILLA_PLAN_FILE", None)
        if os.path.exists(self.plan):
            os.remove(self.plan)

    def fetch(self, url):
        return {"http://hub.invalid/ci.json": CI, "http://hub.invalid/slo.json": SLO}[url]

    def test_two_runs_baseline_then_compared_and_the_report_is_written(self):
        store = Store()
        rep, snap, mode = rw.run(cfg(), store, fetch=self.fetch, environ=self.ENV, now=NOW)
        self.assertEqual(mode, "baseline")
        with open(self.plan) as f:
            self.assertEqual(json.load(f), rep)
        self.assertEqual(len(store.saved), 1)
        rep2, _, mode2 = rw.run(cfg(), store, fetch=self.fetch, environ=self.ENV, now=NOW)
        self.assertEqual(mode2, "compared")
        self.assertEqual(len(store.saved), 2)

    def test_a_stale_run_writes_no_snapshot(self):
        slo = copy.deepcopy(SLO)
        for r in slo["rows"]:
            if r["id"] == FRESH:
                r["ratio"]["1d"] = 0.1
        store = Store(snapshot_of())
        _, snap, mode = rw.run(cfg(), store, fetch=lambda u: slo if u.endswith("slo.json") else CI, environ=self.ENV, now=NOW)
        self.assertEqual((snap, mode, store.saved), (None, "stale", []))

    def test_the_snapshot_is_not_written_when_the_report_cannot_be(self):
        os.environ["FLOTILLA_PLAN_FILE"] = "/nonexistent-dir/plan.json"
        store = Store()
        with self.assertRaises(OSError):
            rw.run(cfg(), store, fetch=self.fetch, environ=self.ENV, now=NOW)
        self.assertEqual(store.saved, [])

    def test_a_source_that_fails_fails_the_run(self):
        def boom(url):
            raise OSError("down")
        with self.assertRaises(common.ConfigError):
            rw.run(cfg(), Store(), fetch=boom, environ=self.ENV, now=NOW)
        self.assertFalse(os.path.exists(self.plan))

    def test_the_fixes_file_is_read_from_the_fleets_folder(self):
        c = rw.config({"RW_SOURCE": "hub_ci", "RW_LIST": "steps.7d", "RW_KEY": "id", "RW_COST": "lost", "RW_TOP": "5",
                       "RW_FIXES": "knowledge/fixes.yaml", "FLOTILLA_CODE_ROOT": "/opt/flotilla"}, "perf")
        self.assertEqual(c["fixes"], "/opt/flotilla/fleets/perf/knowledge/fixes.yaml")


class TestConfig(unittest.TestCase):
    BASE = {"RW_SOURCE": "hub_ci", "RW_LIST": "steps.7d", "RW_KEY": "id", "RW_COST": "lost", "RW_TOP": "15"}

    def test_the_commands_params_become_the_config(self):
        env = dict(self.BASE, RW_RISE="0.5:600", RW_UNIT="seconds",
                   RW_SIGNALS="hub_slo:" + FRESH + ":ci-runners--*,other-*", RW_FIXES="knowledge/fixes.yaml")
        c = rw.config(env, "perf")
        self.assertEqual((c["top"], c["rise"], c["unit"]), (15, (0.5, 600), "seconds"))
        self.assertEqual((c["signal_source"], c["fresh"], c["signal_ids"]), ("hub_slo", FRESH, ["ci-runners--*", "other-*"]))

    def test_signals_may_be_a_freshness_row_alone(self):
        c = rw.config(dict(self.BASE, RW_SIGNALS="hub_slo:" + FRESH), "perf")
        self.assertEqual((c["signal_source"], c["fresh"], c["signal_ids"]), ("hub_slo", FRESH, []))

    def test_the_optional_params_may_be_absent(self):
        c = rw.config(self.BASE, "perf")
        self.assertEqual((c["rise"], c["signal_source"], c["signal_ids"], c["fixes"]), (None, "", [], ""))

    def test_a_bad_value_is_a_config_error(self):
        for k, v in (("RW_TOP", "0"), ("RW_TOP", "x"), ("RW_RISE", "50%"), ("RW_SIGNALS", "a"), ("RW_SIGNALS", ":b:c"),("RW_FIXES", "../x.yaml"),
                     ("RW_FIXES", "/etc/passwd"), ("RW_LIST", "")):
            with self.assertRaises(common.ConfigError, msg=f"{k}={v}"):
                rw.config(dict(self.BASE, **{k: v}), "perf")

    def test_a_source_is_a_name_resolved_in_the_workers_environment(self):
        self.assertEqual(rw.source_url("hub_ci", {"SOURCE_HUB_CI": "https://hub.invalid/ci.json"}), "https://hub.invalid/ci.json")
        for name, env in (("hub_ci", {}), ("hub_ci", {"SOURCE_HUB_CI": "ftp://x"}), ("Hub", {}), ("https://hub.invalid/ci.json", {}), ("", {})):
            with self.assertRaises(common.ConfigError, msg=name):
                rw.source_url(name, env)


class TestLedgerStore(unittest.TestCase):
    class Client:
        def __init__(self):
            self.rows = []

        def filings(self, keys=None):
            return [r for r in self.rows if r["fingerprint"] == keys["fingerprint"]][-1:]

        def write_filing(self, row):
            assert all(isinstance(v, str) for v in row.values()), "a ledger row is strings"
            self.rows.append(row)

    def test_a_snapshot_round_trips_through_the_routines_ledger(self):
        s = rw.LedgerStore(self.Client())
        self.assertIsNone(s.load())
        snap = snapshot_of()
        s.save(snap)
        self.assertEqual(s.load(), snap)
        snap2 = dict(snap, taken="2026-01-16T06:00:00Z")
        s.save(snap2)
        self.assertEqual(s.load()["taken"], "2026-01-16T06:00:00Z")


class TestCostText(unittest.TestCase):
    def test_durations(self):
        self.assertEqual([rw.cost_text(v, "seconds") for v in (45, 119, 120, 4378, 7200)], ["45 s", "119 s", "2 min", "73 min", "2.0 h"])
        self.assertEqual(rw.cost_text(12.5, ""), "12.5")


class TestCarried(unittest.TestCase):
    """A stale run reports no findings of its own, but the cases already open must not close (dispatch clears an incident whose key a
    successful run no longer reports) and reopen when the source is fresh again."""

    def stale_slo(self):
        slo = copy.deepcopy(SLO)
        for r in slo["rows"]:
            if r["id"] == FRESH:
                r["ratio"]["1d"] = 0.2
        return slo

    def test_an_open_breach_survives_a_stale_run_and_is_still_one_case_after_it(self):
        first, snap = build()
        stale, kept = build(slo=self.stale_slo(), prev=snap)
        self.assertIsNone(kept)
        self.assertEqual(targets(stale), targets(first))
        for d in stale["detections"]:
            self.assertTrue(d["title"].startswith("carried, source stale: "), d["title"])
        self.assertIn("the source is stale", stale["summary"][0])
        again, _ = build(prev=snap)
        self.assertEqual(targets(again), targets(first))

    def test_a_new_entrys_case_is_carried_too(self):
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"].append({"id": "ffffffffff01", "repo": "example/api-service", "step": "Warm cache", "lost": 3000})
        _, base = build()
        _, snap = build(ci=ci, prev=base)
        stale, _ = build(slo=self.stale_slo(), prev=snap)
        self.assertIn("perf:step/ffffffffff01", targets(stale))

    def test_nothing_to_carry_without_a_usable_snapshot(self):
        for prev in (None, "garbage", {"items": 1}):
            rep, snap = build(slo=self.stale_slo(), prev=prev)
            self.assertEqual((rep["detections"], snap), ([], None))

    def test_the_carried_list_is_in_the_snapshot_and_bounded(self):
        _, snap = build()
        self.assertEqual(sorted(d["target"] for d in snap["carry"]), targets(build()[0]))
        self.assertLess(len(json.dumps(snap)), rw.MAX_SNAPSHOT)


class TestSnapshotShape(unittest.TestCase):
    def test_an_unreadable_snapshot_is_no_baseline_with_a_line_saying_so(self):
        good = snapshot_of()
        for name, prev in (("text", "x"), ("list", []), ("no items", {"taken": "t", "top": 5, "list": "steps.7d"}),
                           ("items not a map", dict(good, items=[1])), ("a cost that is text", dict(good, items={"a": {"cost": "9"}})),
                           ("no taken", {k: v for k, v in good.items() if k != "taken"}), ("signals a list", dict(good, signals=[1])),
                           ("carry not detections", dict(good, carry=[1])), ("an infinite cost", dict(good, items={"a": {"cost": float("inf")}}))):
            rep, snap = build(prev=prev)
            self.assertIn("the previous snapshot is unreadable", rep["summary"][0], name)
            self.assertIn("a new baseline", rep["summary"][0], name)
            self.assertIsNotNone(snap, name)

    def test_a_snapshot_with_bad_json_loads_as_unreadable_not_as_a_crash(self):
        class C:
            def filings(self, keys=None):
                return [{"fingerprint": "snapshot", "data": "{not json"}]
        self.assertEqual(rw.LedgerStore(C()).load(), rw.UNREADABLE)


class TestSourceShape(unittest.TestCase):
    def test_non_finite_and_non_dict_values_are_config_errors(self):
        for rows in ([{"id": "a", "lost": float("inf")}], [{"id": "a", "lost": float("nan")}], [{"id": "a", "lost": float("-inf")}]):
            with self.assertRaises(common.ConfigError):
                rw.rank(rows, "id", "lost", 3)
        for bad in ({"rows": [1]}, {"rows": "x"}, {"rows": [{"id": "a", "ratio": 5}]}, [1], {"rows": {"a": 1}}):
            with self.assertRaises(common.ConfigError, msg=repr(bad)):
                build(slo=dict(bad) if isinstance(bad, dict) else bad)
        with self.assertRaises(common.ConfigError):
            build(ci=[1])
        with self.assertRaises(common.ConfigError):
            build(ci={"steps": {"7d": {"a": 1}}})
        with self.assertRaises(common.ConfigError):
            build(ci={"steps": {"7d": "x"}})

    def test_a_signals_glob_over_the_cap_is_refused(self):
        slo = copy.deepcopy(SLO)
        slo["rows"] += [dict(slo["rows"][0], id=f"ci-runners--x{i}") for i in range(rw.MAX_SIGNALS + 1)]
        with self.assertRaises(common.ConfigError):
            build(slo=slo)

    def test_a_snapshot_over_the_ledger_cap_is_refused(self):
        rows = [{"id": f"id{i}", "lost": 10000 - i, "step": "S" * 90} for i in range(rw.MAX_TOP)]
        with self.assertRaises(common.ConfigError):
            build(c=cfg(top=rw.MAX_TOP + 1, signal_source=""), ci={"steps": {"7d": rows + rows[:1]}}, slo=None)


class TestTargets(unittest.TestCase):
    def test_steps_and_service_levels_have_their_own_namespaces(self):
        self.assertEqual(rw.target("step", "abc"), "perf:step/abc")
        self.assertEqual(rw.target("slo", "abc"), "perf:slo/abc")
        rep, _ = build()
        for d in rep["detections"]:
            self.assertTrue(d["target"].startswith("perf:slo/"))

    def test_an_id_that_sanitising_would_change_is_skipped_with_a_line_never_merged(self):
        self.assertIsNone(rw.target("step", "a b"))
        self.assertIsNone(rw.target("step", "é"))
        self.assertIsNone(rw.target("step", "x" * 200))
        self.assertIsNone(rw.target("step", ""))
        _, base = build()
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"].append({"id": "bad id!", "repo": "example/a", "step": "Odd", "lost": 9000})
        rep, _ = build(ci=ci, prev=base)
        self.assertEqual([d for d in rep["detections"] if "bad" in d["target"]], [])
        self.assertTrue(any("no case" in l and "bad id!" in l for l in rep["summary"]), rep["summary"])

    def test_a_long_label_is_capped_in_lines_and_titles(self):
        _, base = build()
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"].append({"id": "long01", "repo": "example/" + "r" * 300, "step": "S" * 300, "lost": 9000})
        rep, _ = build(ci=ci, prev=base)
        d = [d for d in rep["detections"] if d["target"] == "perf:step/long01"][0]
        self.assertLessEqual(len(d["title"]), 200)
        self.assertLessEqual(max(len(l) for l in rep["summary"]), 200)


class TestFixedQuestion(unittest.TestCase):
    FIXES = {"a1b2c3d4e504": {"item": "a1b2c3d4e504", "link": "x", "owner": "o"}}

    def drop(self, lost):
        c = cfg(top=4)
        prev = snapshot_of(c)
        ci = copy.deepcopy(CI)
        ci["steps"]["7d"][3]["lost"] = lost  # Build image, 1800 before
        ci["steps"]["7d"].append({"id": "ffffffffff09", "repo": "example/a", "step": "New", "lost": 2100})  # pushes it out of the top 4
        rep, _ = build(c=c, ci=ci, prev=prev, fixes=self.FIXES)
        return [l for l in rep["summary"] if "dropped out" in l][0]

    def test_out_of_the_top_but_not_cheaper_is_not_fixed(self):
        self.assertNotIn("fixed?", self.drop(1800))
        self.assertNotIn("fixed?", self.drop(1950))

    def test_cheaper_or_gone_is_fixed(self):
        self.assertIn("fixed?", self.drop(1000))
        self.assertIn("fixed?", self.drop(100))
        c = cfg(top=4)
        prev = snapshot_of(c)
        ci = copy.deepcopy(CI)
        del ci["steps"]["7d"][3]
        rep, _ = build(c=c, ci=ci, prev=prev, fixes=self.FIXES)
        self.assertIn("fixed?", [l for l in rep["summary"] if "dropped out" in l][0])


class TestFixesFileRequired(unittest.TestCase):
    def test_a_fixes_file_the_step_names_but_is_missing_is_a_config_error(self):
        with self.assertRaises(common.ConfigError):
            rw.load_fixes(os.path.join(DATA, "no-such.yaml"))

    def test_the_operators_fixes_file_parses_as_written(self):
        text = ("# c\nfixes:\n  # c2\n  - item: 85e0e1c670df\n    link: https://example.invalid/pull/56\n    owner: example/web-app\n"
                "  - item: b12e4f869391\n    link: example/ops RFC-0011 item 24 (no PR yet)\n    owner: ingestion\n")
        f = rw.parse_fixes(text)
        self.assertEqual(f[0]["link"], "https://example.invalid/pull/56")
        self.assertEqual(f[1]["link"], "example/ops RFC-0011 item 24 (no PR yet)")


class _Handler:
    pass


class TestFetch(unittest.TestCase):
    def serve(self, handler_body):
        import http.server
        import threading

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                handler_body(self)

            def log_message(self, *a):
                pass

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        self.addCleanup(srv.server_close)
        os.environ["no_proxy"] = "127.0.0.1"
        return f"http://127.0.0.1:{srv.server_address[1]}/secret-path.json"

    def test_a_good_body_is_json(self):
        def ok(h):
            h.send_response(200)
            h.end_headers()
            h.wfile.write(b'{"a": 1}')
        self.assertEqual(rw.fetch_json(self.serve(ok)), {"a": 1})

    def test_a_non_json_body_is_a_config_error_that_does_not_name_the_url(self):
        def html(h):
            h.send_response(200)
            h.end_headers()
            h.wfile.write(b"<html>")
        url = self.serve(html)
        with self.assertRaises(common.ConfigError) as e:
            rw.fetch_json(url)
        self.assertNotIn("secret-path", str(e.exception))
        self.assertNotIn("127.0.0.1", str(e.exception))

    def test_a_refused_connection_does_not_name_the_url(self):
        with self.assertRaises(common.ConfigError) as e:
            rw.fetch_json("http://127.0.0.1:1/secret-path.json")
        self.assertNotIn("secret-path", str(e.exception))

    def test_a_cross_host_redirect_is_refused_and_a_same_host_one_is_not(self):
        h = rw.SameHostRedirect()
        import urllib.request
        req = urllib.request.Request("https://a.example.com/x")
        with self.assertRaises(common.ConfigError):
            h.redirect_request(req, None, 302, "Found", {}, "https://b.example.com/y")
        self.assertIsNotNone(h.redirect_request(req, None, 302, "Found", {}, "https://a.example.com/y"))
        with self.assertRaises(common.ConfigError):
            h.redirect_request(req, None, 302, "Found", {}, "http://a.example.com/y")  # a downgrade is a new scheme

    def test_the_whole_fetch_has_a_deadline_not_just_each_read(self):
        import time

        def dribble(h):
            h.send_response(200)
            h.end_headers()
            for _ in range(40):
                h.wfile.write(b" ")
                h.wfile.flush()
                time.sleep(0.1)
        t = time.monotonic()
        with self.assertRaises(common.ConfigError):
            rw.fetch_json(self.serve(dribble), deadline=0.6)
        self.assertLess(time.monotonic() - t, 3)

    def test_http_needs_the_test_allowlist(self):
        self.assertEqual(rw.source_url("hub_ci", {"SOURCE_HUB_CI": "https://h.example.com/ci.json"}), "https://h.example.com/ci.json")
        with self.assertRaises(common.ConfigError):
            rw.source_url("hub_ci", {"SOURCE_HUB_CI": "http://h.example.com/ci.json"})
        env = {"SOURCE_HUB_CI": "http://h.example.com/ci.json", "SOURCE_ALLOW_HTTP": "x.example.com, h.example.com"}
        self.assertEqual(rw.source_url("hub_ci", env), "http://h.example.com/ci.json")

    def test_a_failing_source_is_a_config_error_naming_the_source_not_the_url(self):
        def boom(url):
            raise OSError("down " + url)
        with self.assertRaises(common.ConfigError) as e:
            rw.run(cfg(), Store(), fetch=boom, environ=TestRun.ENV, now=NOW)
        self.assertIn("hub_ci", str(e.exception))
        self.assertNotIn("hub.invalid", str(e.exception))


if __name__ == "__main__":
    unittest.main()
