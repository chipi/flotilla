"""python3 ops/test_case_review.py: the post-incident review (ADR-0016) and the process-change routine.

The replay is the first shadow case, case-20261114-6983646f65 (2026-11-14 23:45Z to 2026-11-15 03:54Z).
The public repo holds no production cases, so the test builds its timeline from its numbers: opened at
20.1 h, a detection absorbed every 10 minutes up to 24.1 h, cleared at 03:54Z. With FLOTILLA_EVAL_DATA set
(the installation's research repo's projects/flotilla-case-review) it also replays the case itself, frozen from
the host. Either way each detection gets the measure the new monitor would have sent: the age and
threshold from its own words ("backup 20.1h old, above 20h") and the cycle measured on the host on
2026-11-15 (14 successes, median 24.02 h).
"""
import copy
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import case_review as cr  # noqa: E402
import process_change as pc  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
CYCLE = 24.02
SOURCE = {"file": "host/registry.json", "action": "backup.rerun", "key": "pre_shadow",
          "condition": "backup_age_above"}
NOW = datetime(2026, 11, 15, 9, 0, tzinfo=timezone.utc)


CASE_ID = "case-20261114-6983646f65"


def built_case():
    """The case's timeline as the host recorded it, from its numbers."""
    events = [{"kind": "case.detected", "actor": "ops/backup-monitor", "detail": "run r0: backup 20.1h old, above 20h"},
              {"kind": "case.handed_over", "actor": "ops/backup-monitor", "by": "detector", "from": "detected", "to": "handed_over"},
              {"kind": "case.planned", "actor": "ops/rerun-backup", "by": "act", "from": "handed_over", "to": "planned"},
              {"kind": "case.closed", "actor": "flotillad", "by": "policy", "from": "planned", "to": "closed",
               "detail": "shadow_plan_recorded: plan 1 backup.rerun: shadow at shadow, would propose"}]
    age = 20.3
    while age <= 24.15:
        events.append({"kind": "signal.absorbed", "actor": "ops/backup-monitor", "detail": f"run r: backup {age:.1f}h old, above 20h"})
        age += 10 / 60
    events.append({"kind": "incident.cleared", "actor": "ops/backup-monitor", "detail": "the monitor no longer reports it"})
    return {"case": {"id": CASE_ID, "kind": "failed_backup", "target": "host:backup-01", "status": "closed",
                     "resolution": "plan_recorded", "opened_at": "2026-11-14T23:45:03Z",
                     "closed_at": "2026-11-14T23:45:19Z"}, "events": events, "plans": []}


def real_case():
    d = os.environ.get("FLOTILLA_EVAL_DATA")
    path = os.path.join(d, "projects", "flotilla-case-review", CASE_ID + ".json") if d else ""
    if path and os.path.exists(path):
        with open(path) as f:
            c = json.load(f)
    else:
        c = built_case()
    for e in c["events"]:
        m = re.search(r"backup ([0-9.]+)h old, above ([0-9]+)h", e.get("detail") or "")
        if m and e["kind"] in ("case.detected", "signal.absorbed"):
            e["data"] = json.dumps({"measure": {"name": "backup_age_hours", "value": float(m.group(1)),
                                                "threshold": int(m.group(2)), "cycle_hours": CYCLE, "source": SOURCE}})
    return c


class FakeAPI:
    def __init__(self, cases):
        self.by_id = {c["case"]["id"]: c for c in cases}
        self.notes = []

    def cases(self, **q):
        return [c["case"] for c in self.by_id.values()]

    def case(self, case_id):
        return self.by_id[case_id]

    def propose_note(self, case_id, text):
        self.notes.append((case_id, text))


def synth(case_id, kind="failed_backup", target="host:backup-01", resolution="resolved_itself", closed="2026-11-14T10:00:00Z",
          events=None, **extra):
    c = {"id": case_id, "kind": kind, "target": target, "status": "closed", "resolution": resolution,
         "opened_at": "2026-11-14T08:00:00Z", "closed_at": closed}
    c.update(extra)
    return {"case": c, "events": events or [{"kind": "case.detected", "detail": "x"}, {"kind": "case.closed", "detail": "y"}]}


class BelowTheCycle(unittest.TestCase):
    def test_the_first_shadow_case_proposes_25h(self):
        """The vision plan's done check: last night's case gives a process_change proposing 25 h, with
        its evidence."""
        det, notes = cr.review(FakeAPI([real_case()]), NOW)
        self.assertEqual(len(det), 1, det)
        d = det[0]
        p = d["data"]["proposal"]
        self.assertEqual((d["kind"], d["target"]), ("process_change", "repo:host/registry.json/backup.rerun/pre_shadow"))
        self.assertEqual((p["rule"], p["from"], p["to"]), ("below_cycle", "backup_age_above(20h)", "backup_age_above(25h)"))
        ev = " ".join(p["evidence"])
        for want in ("case-20261114-6983646f65", "opened at 20.1h", "the highest seen 24.1h", "24.02h"):
            self.assertIn(want, ev)
        self.assertEqual(notes, [])

    def test_a_threshold_above_the_cycle_gives_nothing(self):
        """Today's 25 h threshold: a healthy night opens no case, and a case that did open says nothing."""
        c = real_case()
        for e in c["events"]:
            if e.get("data"):
                m = json.loads(e["data"])
                m["measure"]["threshold"] = 25
                e["data"] = json.dumps(m)
        self.assertEqual(cr.review(FakeAPI([c]), NOW)[0], [])

    def test_a_really_late_backup_gives_nothing(self):
        """A backup 30 h late is not a threshold below the cycle: the case stays a case."""
        c = real_case()
        last = [e for e in c["events"] if e.get("data")][-1]
        m = json.loads(last["data"])
        m["measure"]["value"] = 30.0
        last["data"] = json.dumps(m)
        self.assertEqual(cr.review(FakeAPI([c]), NOW)[0], [])

    def test_a_fixed_case_is_not_evidence(self):
        c = real_case()
        c["events"].insert(5, {"kind": "case.executing", "detail": "approved"})
        self.assertEqual(cr.review(FakeAPI([c]), NOW)[0], [])

    def test_settled_proposals_are_not_made_again(self):
        """Once a process change on the target closed after the evidence (applied, or no longer needed),
        or a person dismissed one in the last 30 days, the same proposal waits."""
        target = "repo:host/registry.json/backup.rerun/pre_shadow"
        for res, closed in (("verified", "2026-11-15T08:00:00Z"), ("resolved_itself", "2026-11-15T08:00:00Z"),
                            ("operator_resolved", "2026-11-01T00:00:00Z")):
            pcase = synth("case-pc", kind="process_change", target=target, resolution=res, closed=closed)
            self.assertEqual(cr.review(FakeAPI([real_case(), pcase]), NOW)[0], [], res)
        old = synth("case-pc", kind="process_change", target=target, resolution="operator_resolved", closed="2026-10-01T00:00:00Z")
        self.assertEqual(len(cr.review(FakeAPI([real_case(), old]), NOW)[0]), 1, "a dismissal 45 days ago no longer holds it")


class OtherRules(unittest.TestCase):
    def test_self_clearing_three_times(self):
        cs = [synth(f"case-s{i}", closed=f"2026-11-1{i}T10:00:00Z") for i in range(2, 5)]
        det, _ = cr.review(FakeAPI(cs), NOW)
        self.assertEqual([d["data"]["proposal"]["rule"] for d in det], ["self_clearing"])
        self.assertEqual(len(cr.review(FakeAPI(cs[:2]), NOW)[0]), 0, "twice is not a pattern")

    def test_fix_not_holding(self):
        prev = synth("case-v", resolution="verified", closed="2026-11-10T00:00:00Z")
        cs = [prev] + [synth(f"case-r{i}", recurrence_of="case-v", resolution="operator_resolved",
                             opened_at="2026-11-1{}T00:00:00Z".format(i), closed=f"2026-11-1{i}T10:00:00Z") for i in (2, 3)]
        det, _ = cr.review(FakeAPI(cs), NOW)
        self.assertIn("fix_not_holding", [d["data"]["proposal"]["rule"] for d in det])
        # after a case that only cleared itself, coming back is not a fix failing
        cs[0]["case"]["resolution"] = "resolved_itself"
        det, _ = cr.review(FakeAPI(cs), NOW)
        self.assertNotIn("fix_not_holding", [d["data"]["proposal"]["rule"] for d in det])

    def test_notes_from_a_wrong_verdict_and_a_rejection(self):
        ev = [{"kind": "case.detected", "detail": "x"},
              {"kind": "feedback.wrong", "actor": "user:marko", "detail": "the diagnosis was wrong"},
              {"kind": "case.escalated", "actor": "user:marko", "from": "awaiting_approval", "detail": "rejected: the job was running"},
              {"kind": "case.closed", "detail": "y"}]
        c = synth("case-n", resolution="operator_resolved", events=ev)
        _, notes = cr.review(FakeAPI([c]), NOW)
        rules = sorted(n[1] for n in notes)
        self.assertEqual(rules, ["refused_plan", "wrong_diagnosis"])
        self.assertIn("the job was running", [n for n in notes if n[1] == "refused_plan"][0][2])
        # proposed once: the note.proposed events on the case stop a second proposal
        ev += [{"kind": "note.proposed", "detail": "note 1: (case-review: wrong_diagnosis) …"},
               {"kind": "note.proposed", "detail": "note 2: (case-review: refused_plan) …"}]
        self.assertEqual(cr.review(FakeAPI([synth("case-n", resolution="operator_resolved", events=ev)]), NOW)[1], [])


class ProcessChange(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.root)
        os.makedirs(os.path.join(self.root, "host"))
        shutil.copy(os.path.join(HERE, "..", "host", "registry.json"), os.path.join(self.root, "host", "registry.json"))
        self.path = os.path.join(self.root, "host", "registry.json")
        # the registry on disk, the repo that holds it, its path there (the spec's PROCESS_CHANGE_*)
        self.place = (self.path, "example/fleets", "host/registry.json")

    def case(self, frm, to):
        det, _ = cr.review(FakeAPI([real_case()]), NOW)
        p = copy.deepcopy(det[0]["data"]["proposal"])
        p["from"], p["to"] = frm, to
        return {"id": "case-pc", "kind": "process_change", "target": det[0]["target"], "data": {"proposal": p}}

    def set_shadow(self, cond):
        """The registry as it was with another shadow threshold, edited as text so its layout stays."""
        path = os.path.join(self.root, "host", "registry.json")
        with open(path) as f:
            text = f.read()
        old = '"pre_shadow": ["backup_age_above(25h)", "not_running"]'
        self.assertEqual(text.count(old), 1, "the registry's pre_shadow line moved: update this test")
        with open(path, "w") as f:
            f.write(text.replace(old, '"pre_shadow": [' + json.dumps(cond) + ', "not_running"]'))

    def test_plans_a_pr_whose_diff_applies(self):
        """RFC-0010 §3: the change as a PR on the repo that holds the registry, based on the image's commit
        (no .commit beside it); the diff applies to the registry and changes only that entry."""
        self.set_shadow("backup_age_above(20h)")
        out = pc.decide(self.case("backup_age_above(20h)", "backup_age_above(25h)"), self.place, version="sha-abc1234")
        self.assertEqual((out["outcome"], out["action"]), ("plan", "github.open_pr"), out)
        prm = out["params"]
        self.assertEqual((prm["repo"], prm["base"]), ("example/fleets", "abc1234"))
        self.assertIn("backup_age_above(20h) -> backup_age_above(25h)", prm["title"])
        self.assertIn("24.02h", prm["body"])
        self.assertIn('-      "pre_shadow": ["backup_age_above(20h)", "not_running"],', prm["diff"])
        self.assertIn('+      "pre_shadow": ["backup_age_above(25h)", "not_running"],', prm["diff"])
        subprocess.run(["git", "init", "-q", self.root], check=True)
        subprocess.run(["git", "-C", self.root, "apply", "-"], input=prm["diff"], text=True, check=True)
        with open(os.path.join(self.root, "host", "registry.json")) as f:
            after = json.load(f)
        with open(os.path.join(HERE, "..", "host", "registry.json")) as f:
            want = json.load(f)
        self.assertEqual(after, want, "the diff, applied, gives back today's registry and nothing else")
        for k, v in prm.items():
            self.assertLessEqual(len(v), {"repo": 40, "base": 40, "title": 200, "body": 8000, "diff": 16000}[k], k)

    def test_by_hand_without_a_commit_or_a_line(self):
        """A dev build has no commit to base a PR on, and a key it cannot find on one line cannot become a
        diff: both stay repo.process_change, applied by hand, with the change text."""
        self.set_shadow("backup_age_above(20h)")
        c = self.case("backup_age_above(20h)", "backup_age_above(25h)")
        for version in ("dev", ""):
            out = pc.decide(c, self.place, version=version)
            self.assertEqual((out["action"], out["params"]["file"], out["params"]["rule"]), ("repo.process_change", "host/registry.json", "below_cycle"))
            ch = out["params"]["change"]
            self.assertIn('- "backup_age_above(20h)"', ch)
            self.assertIn('+ "backup_age_above(25h)"', ch)
            self.assertIn("24.02h", ch)
            self.assertLessEqual(len(ch), pc.CHANGE_MAX)
            self.assertIn("a dev build", out["detail"])
        self.assertEqual(pc.registry_diff(self.path, "host/registry.json", "backup.rerun", "pre", "backup_age_above(20h)",
                                          "backup_age_above(25h)"), "", "pre has 26h, not 20h: no diff")

    def test_already_changed_resolves(self):
        """The registry today has 25 h: the proposal from the 20 h days is already made."""
        out = pc.decide(self.case("backup_age_above(20h)", "backup_age_above(25h)"), self.place, version="sha-abc1234")
        self.assertEqual(out["outcome"], "resolved_itself", out)

    def test_refuses_what_it_cannot_read(self):
        self.assertEqual(pc.decide({"kind": "process_change", "data": {}}, self.place)["outcome"], "escalate")
        bad = self.case("backup_age_above(20h)", "backup_age_above(25h)")
        bad["data"]["proposal"]["file"] = "../../etc/passwd"
        self.assertEqual(pc.decide(bad, self.place)["outcome"], "escalate")

    def test_an_installations_own_registry_goes_to_its_own_repo(self):
        """An installation's registry in its fleets folder: the PR goes to the repo the spec names, on the
        file's path there, based on the commit deploy_fleets.sh exported (.commit beside it), not the image's."""
        self.set_shadow("backup_age_above(20h)")
        with open(os.path.join(self.root, "host", ".commit"), "w") as f:
            f.write("0123456789abcdef0123456789abcdef01234567\n")
        out = pc.decide(self.case("backup_age_above(20h)", "backup_age_above(25h)"),
                        (self.path, "example/our-fleets", "fleets/registry.json"), version="sha-abc1234")
        prm = out["params"]
        self.assertEqual((out["action"], prm["repo"], prm["base"]),
                         ("github.open_pr", "example/our-fleets", "0123456789abcdef0123456789abcdef01234567"), out)
        self.assertIn("--- a/fleets/registry.json", prm["diff"])
        self.assertIn("+++ b/fleets/registry.json", prm["diff"])

    def test_no_repo_named_is_by_hand(self):
        """A spec that names no repo never gets a PR on a repo the code chose: the change is by hand."""
        self.set_shadow("backup_age_above(20h)")
        for place in ((self.path, "", ""), (self.path, "example/fleets", "")):
            out = pc.decide(self.case("backup_age_above(20h)", "backup_age_above(25h)"), place, version="sha-abc1234")
            self.assertEqual(out["action"], "repo.process_change", out)
            self.assertIn("PROCESS_CHANGE_REPO", out["detail"])


if __name__ == "__main__":
    unittest.main()
