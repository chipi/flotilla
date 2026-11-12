"""Offline checks of the replay entry point's contract (replay_api.py, ADR-0013): synthetic worlds only, no network,
no production data. The equivalence with the evaluation project's own replay on real history is proven on its side
(fleet-eval-data), not here.

  SF_OBSERV_DISABLED=1 python3 test_replay_api.py     # non-zero on failure
"""
import datetime
import os
import unittest

os.environ.setdefault("SF_OBSERV_DISABLED", "1")

import callrec
import config
import filing
import replay_api as ra
import stale

NOW = "2026-10-02T08:00:00Z"
SIG = {"fingerprint": "glitchtip:ORRERY-PROD-9", "source": "glitchtip",
       "alertname": "TypeError: x is undefined", "labels": {}}


class GitHub:
    """A stand-in for the caller's gh: answers reads from `issues`, records every call, and answers a create with
    `next_number`."""

    def __init__(self, issues=None, next_number=101):
        self.issues = issues or {}
        self.calls = []
        self.next_number = next_number

    def __call__(self, method, path, payload=None):
        self.calls.append((method, path, payload))
        if method == "POST" and path.endswith("/issues"):
            return {"number": self.next_number, "html_url": f"https://example.invalid/{self.next_number}"}
        if method == "GET" and "/search/issues" in path:
            return {"items": [i for i in self.issues.values() if i.get("state") == "open"]}
        if method == "GET" and "/comments" in path:
            return []
        if method == "GET" and "/milestones" in path:
            return []
        if method == "GET" and "/issues/" in path:
            n = path.rsplit("/", 1)[-1]
            return self.issues.get(n, {"state": "open", "labels": [], "closed_at": None})
        return {}


class EntryPointContract(unittest.TestCase):
    def setUp(self):
        self.real = {k: getattr(filing, k) for k in ("FILED", "_gh", "_now", "_glitchtip_note",
                                                     "_grafana_annotation", "normalized_key", "repo_for")}
        self.env = {k: os.environ.get(k) for k in ("SF_LEDGER_BACKEND", "SF_STALE_NUDGE")}
        self.capture = config.CAPTURE_PROBES

    def assertRestored(self):
        for k, v in self.real.items():
            self.assertIs(getattr(filing, k), v, f"filing.{k} not restored")
        for k, v in self.env.items():
            self.assertEqual(os.environ.get(k), v, f"{k} not restored")
        self.assertEqual(config.CAPTURE_PROBES, self.capture)

    def test_a_new_signal_is_filed_through_the_callers_gh(self):
        gh = GitHub(next_number=7)
        with ra.World(now=NOW, github=gh) as w:
            out = ra.file(w, SIG, {"title": "TypeError", "body": "b"}, "bug")
        self.assertEqual(out["api"], ra.API_VERSION)
        self.assertTrue(out["result"].startswith("FILED"), out["result"])
        self.assertIn(("POST", "/repos/chipi/orrery/issues"), [(m, p) for m, p, _ in gh.calls])
        self.assertEqual([x["path"] for x in out["writes"] if x["path"].endswith("/issues")], ["/repos/chipi/orrery/issues"])
        row = next(r for r in out["ledger"] if r["fingerprint"] == SIG["fingerprint"])
        self.assertEqual(row["issue"], "7")                       # the caller's answer, read back
        self.assertEqual(row["filed_at"][:19], NOW[:19])          # the world's clock
        self.assertEqual(out["record"]["result"], out["result"])  # the decision's record, in memory
        self.assertRestored()

    def test_reads_are_not_writes(self):
        gh = GitHub()
        with ra.World(now=NOW, github=gh) as w:
            out = ra.file(w, SIG, {"title": "t", "body": "b"}, "bug")
        self.assertTrue(gh.calls)
        self.assertTrue(all(x["method"] != "GET" for x in out["writes"]))
        self.assertFalse(any(p.startswith("/graphql") for p in (x["path"] for x in out["writes"])))

    def test_the_ledger_carries_forward_on_one_world(self):
        gh = GitHub(issues={"7": {"state": "open", "labels": [], "closed_at": None}}, next_number=7)
        with ra.World(now=NOW, github=gh) as w:
            ra.file(w, SIG, {"title": "t", "body": "b"}, "bug")
            w.now = ra._when("2026-10-03T09:00:00Z")
            out = ra.file(w, SIG, {"title": "t", "body": "b"}, "bug")
        self.assertTrue(out["result"].startswith("COMMENTED"), out["result"])

    def test_recorded_keys_route_the_decision(self):
        gh = GitHub()
        keys = {"norm_key": "n1", "group_key": "", "alert_key": "a1", "repo": "acme/other"}
        with ra.World(now=NOW, github=gh, keys=keys) as w:
            out = ra.file(w, SIG, {"title": "t", "body": "b"}, "bug")
        self.assertIn("/repos/acme/other/issues", [x["path"] for x in out["writes"]])
        row = out["ledger"][-1]
        self.assertEqual((row["norm_key"], row["alert_key"]), ("n1", "a1"))
        self.assertRestored()

    def test_an_exception_restores_everything(self):
        def broken(method, path, payload=None):
            if method == "POST":
                raise RuntimeError("GitHub down")
            return GitHub()(method, path, payload)
        with ra.World(now=NOW, github=broken) as w:
            with self.assertRaises(RuntimeError):
                ra.file(w, SIG, {"title": "t", "body": "b"}, "bug")
        self.assertRestored()
        with ra.World(now=NOW, github=GitHub()) as w:     # the next call is not blocked by the failed one
            self.assertTrue(ra.file(w, SIG, {"title": "t", "body": "b"}, "bug")["result"].startswith("FILED"))

    def test_one_call_at_a_time(self):
        gh = GitHub()
        with ra.World(now=NOW, github=gh) as w:
            inner = {}

            def reentrant(method, path, payload=None):
                if "result" not in inner:
                    inner["result"] = "tried"
                    with self.assertRaises(RuntimeError):
                        ra.file(w, SIG, {"title": "t", "body": "b"}, "bug")
                return gh(method, path, payload)
            w.github = reentrant
            ra.file(w, SIG, {"title": "t", "body": "b"}, "bug")
        self.assertEqual(inner.get("result"), "tried")
        self.assertRestored()

    def test_escalate(self):
        gh = GitHub(next_number=9)
        with ra.World(now=NOW, github=gh) as w:
            out = ra.escalate(w, SIG, {"question": "is this real?", "reason": "unclear"})
        self.assertTrue(out["result"].startswith("FILED"), out["result"])
        body = next(x["payload"]["body"] for x in out["writes"] if x["path"].endswith("/issues"))
        self.assertIn("is this real?", body)
        self.assertRestored()

    def test_a_world_needs_github(self):
        with self.assertRaises(ValueError):
            ra.World(now=NOW)


class Sweep(unittest.TestCase):
    def world(self, gh, seen_days_ago):
        now = datetime.datetime(2026, 10, 20, 8, tzinfo=datetime.timezone.utc)
        ledger = [{"fingerprint": SIG["fingerprint"], "repo": "chipi/orrery", "issue": "5", "group_key": "",
                   "filed_at": "2026-09-01T00:00:00+00:00", "last_comment_day": "", "norm_key": "", "alert_key": ""}]
        occ = [{"ts": (now - datetime.timedelta(days=seen_days_ago)).isoformat(), "fingerprint": SIG["fingerprint"],
                "source": "glitchtip", "alertname": SIG["alertname"]}]
        return ra.World(now=now, ledger=ledger, github=gh, keys={"alert_key": "never-used"}), occ

    def gh(self):
        return GitHub(issues={"5": {"number": 5, "state": "open", "labels": [{"name": filing.FILED_LABEL}],
                                    "closed_at": None}})

    def test_a_quiet_issue_is_nudged(self):
        gh = self.gh()
        w, occ = self.world(gh, seen_days_ago=10)
        nudge_env = os.environ.get("SF_STALE_NUDGE")
        with w:
            out = ra.sweep(w, occ, firing=[])
        self.assertIn("chipi/orrery#5", out["result"])
        self.assertIn("/repos/chipi/orrery/issues/5/comments", [x["path"] for x in out["writes"]])
        self.assertEqual(out["record"]["summary"], out["result"])
        self.assertEqual(os.environ.get("SF_STALE_NUDGE"), nudge_env)   # forced on only inside the call

    def test_quiet_days_is_honoured(self):
        gh = self.gh()
        w, occ = self.world(gh, seen_days_ago=10)
        with w:
            out = ra.sweep(w, occ, firing=[], quiet_days=14)
        self.assertNotIn("chipi/orrery#5", out["result"])
        self.assertEqual(out["writes"], [])
        self.assertIs(stale.is_stale.__module__, "stale")         # restored

    def test_a_firing_alert_is_seen_now(self):
        gh = self.gh()
        w, occ = self.world(gh, seen_days_ago=30)
        with w:
            out = ra.sweep(w, occ, firing=[SIG["alertname"]])
        self.assertEqual(out["writes"], [])

    def test_the_sweep_ignores_recorded_keys_and_writes_no_file(self):
        gh = self.gh()
        w, occ = self.world(gh, seen_days_ago=10)
        before = callrec.write_sweep
        with w:
            ra.sweep(w, occ, firing=[])
        self.assertIs(callrec.write_sweep, before)
        self.assertIs(filing.alert_key.__module__, "filing")


class Gates(unittest.TestCase):
    EDGE = {"fingerprint": "grafana:edge1", "source": "grafana", "alertname": "Edge 5xx burst on example.invalid",
            "labels": {}, "startsAt": "2026-10-26T13:46:30Z"}
    LOOK = {"kind": "restarts_near", "args": {"instance": "prod-podcast", "at": "2026-10-26T13:46:30Z"}}
    LOGS = [{"probe": "service_logs", "args": {}, "result": "200 OK for the last hour"}]

    def run_gates(self, signal, decision, trace=(), gates=None):
        import guards
        import triage
        before = (guards.restarts_near, triage._EXACT)
        with ra.World(now=NOW, github=GitHub()) as w:
            out = ra.gates(w, signal, decision, trace=trace, gates=gates if gates is not None else {"lookups": []})
        self.assertIs(guards.restarts_near, before[0])
        self.assertIs(triage._EXACT, before[1])
        return out

    def test_a_file_without_cited_intent_is_escalated(self):
        model = {"disposition": "file", "reason": "r", "file": {"acceptance": [{"text": "works"}]}}
        out = self.run_gates(SIG, model)
        self.assertEqual(out["decision"]["disposition"], "escalate")
        self.assertIn("i:downgraded", out["result"])
        self.assertEqual(out["model_decision"], model)        # the model's own, untouched
        self.assertEqual(model["disposition"], "file")        # the caller's dict is not changed

    def test_a_dismiss_with_corroboration_passes(self):
        out = self.run_gates(SIG, {"disposition": "dismiss", "reason": "r", "dismissal_evidence": "logs clean"},
                             trace=self.LOGS)
        self.assertEqual(out["decision"]["disposition"], "dismiss")
        self.assertIn("d:passed", out["result"])

    def test_the_edge_gate_reads_the_recorded_lookup(self):
        dismiss = {"disposition": "dismiss", "reason": "r", "dismissal_evidence": "logs clean"}
        quiet = self.run_gates(self.EDGE, dict(dismiss), self.LOGS, {"lookups": [dict(self.LOOK, result=[])]})
        self.assertIn("e:passed", quiet["result"])
        self.assertEqual(quiet["record"]["lookups"], [dict(self.LOOK, result=[])])
        hit = self.run_gates(self.EDGE, dict(dismiss), self.LOGS, {"lookups": [dict(self.LOOK, result=["player-api"])]})
        self.assertEqual(hit["decision"]["disposition"], "escalate")
        self.assertIn("e:downgraded (restart nearby)", hit["result"])
        err = self.run_gates(self.EDGE, dict(dismiss), self.LOGS, {"lookups": [dict(self.LOOK, error="timeout")]})
        self.assertIn("e:downgraded (lookup failed)", err["result"])

    def test_a_missing_recorded_lookup_is_refused(self):
        dismiss = {"disposition": "dismiss", "reason": "r", "dismissal_evidence": "logs clean"}
        with self.assertRaises(ra.LookupMissing):
            self.run_gates(self.EDGE, dismiss, self.LOGS, {"lookups": []})
        with ra.World(now=NOW, github=GitHub()) as w:         # and the lock is free again
            ra.gates(w, SIG, {"disposition": "dismiss", "reason": "r"}, gates={"lookups": []})


if __name__ == "__main__":
    unittest.main()
