"""The business examples (crm, campaigns, outbound, renewals) against their seed data in fleets/: what each
monitor reports, what each acting routine plans or refuses, the executor's hash check and re-checks, and
each check before and after its action. Each fleet also runs end to end through the scripts' entry points
(monitor, acting routine, executor, check) on a copy of its seed data.

  python3 business/test_business.py
"""
import copy
import datetime
import importlib.util
import json
import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import apply  # noqa: E402
import campaigns  # noqa: E402
import crm  # noqa: E402
import outbound  # noqa: E402
import renewals  # noqa: E402
import store  # noqa: E402

FLEETS = os.path.join(HERE, "..", "fleets")
TODAY = datetime.date(2026, 11, 17)


def seed(fleet, name):
    with open(os.path.join(FLEETS, fleet, "seed", name + ".json")) as f:
        return json.load(f)


class Data(unittest.TestCase):
    """Each test gets its own copy of the fleet's seed data as BIZ_DATA."""

    fleet = ""

    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir)
        self.plan = os.path.join(self.dir, "plan.json")
        self.env = {"BIZ_DATA": os.path.join(self.dir, "data"), "BIZ_SEED": os.path.join(FLEETS, self.fleet, "seed"),
                    "FLOTILLA_PLAN_FILE": self.plan}
        self.old = {k: os.environ.get(k) for k in list(self.env) + ["FLOTILLA_CASE", "FLOTILLA_PLAN", "FLOTILLA_CHECK"]}
        os.environ.update(self.env)
        self.addCleanup(self.restore)

    def restore(self):
        for k, v in self.old.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def approve(self, case, out):
        """The plan as flotillad hands it to the executor once approved: with its hash."""
        return {"id": 1, "action": out["action"], "params": out["params"],
                "plan_hash": apply.plan_hash(case["id"], out["action"], out["params"])}

    def execute(self, case, out):
        os.environ["FLOTILLA_CASE"] = json.dumps(case)
        os.environ["FLOTILLA_PLAN"] = json.dumps(self.approve(case, out))
        return apply.main(["execute"])

    def check(self, out, verify):
        os.environ["FLOTILLA_CHECK"] = json.dumps({"verify": verify, "action": out["action"], "params": out["params"]})
        return apply.main(["check"])

    def act(self, module, verb, det):
        case = {"id": "case-" + det["target"].replace(":", "-").replace("/", "-"), "kind": det["kind"], "target": det["target"],
                "data": det["data"]}
        os.environ["FLOTILLA_CASE"] = json.dumps(case)
        return case, module.main([verb])


class Hash(unittest.TestCase):
    def test_the_plan_hash_is_open_prs(self):
        """open_pr.py's hash is checked against flotillad's; this executor's must be the same function."""
        spec = importlib.util.spec_from_file_location("open_pr", os.path.join(HERE, "..", "bugfix", "open_pr.py"))
        op = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(op)
        for params in ({"a": "1", "b": "x<y>&z"}, {"body": "line two é", "to": "a@b.example"}, {}):
            self.assertEqual(apply.plan_hash("case-1", "email.send", params), op.plan_hash("case-1", "email.send", params))


class CRM(Data):
    fleet = "crm"

    def test_scan_finds_fields_and_duplicates_and_skips_locked(self):
        got = {d["target"]: d["data"] for d in crm.scan(seed("crm", "contacts"))}
        self.assertEqual(got["crm:contact/c-101/email"]["to"], "ana.ruiz@example.org")
        self.assertEqual(got["crm:contact/c-101/country"]["to"], "ES")
        self.assertEqual(got["crm:contact/c-103/merge"], {"op": "merge", "keep": "c-102", "drop": "c-103", "email": "ben@example.net"})
        self.assertNotIn("crm:contact/c-104/country", got, "a field a person edited is not proposed")
        self.assertNotIn("crm:contact/c-105/email", got)

    def test_fix_refuses_what_it_may_not_do(self):
        cs = seed("crm", "contacts")
        locked = {"id": "x", "kind": "crm_fix", "data": {"op": "update", "contact": "c-104", "field": "country"}}
        self.assertEqual(crm.decide(locked, cs)["outcome"], "escalate")
        other = {"id": "x", "kind": "crm_fix", "data": {"op": "update", "contact": "c-101", "field": "name"}}
        self.assertEqual(crm.decide(other, cs)["outcome"], "escalate")
        cs2 = copy.deepcopy(cs)
        store.by_id(cs2)["c-103"]["email"] = "ben.ode@example.net"
        merge = {"id": "x", "kind": "crm_fix", "data": {"op": "merge", "keep": "c-102", "drop": "c-103"}}
        self.assertEqual(crm.decide(merge, cs2)["outcome"], "escalate", "emails differ now: no merge")
        done = {"id": "x", "kind": "crm_fix", "data": {"op": "update", "contact": "c-105", "field": "email"}}
        self.assertEqual(crm.decide(done, cs)["outcome"], "resolved_itself")

    def test_end_to_end_update_and_merge(self):
        dets = {d["target"]: d for d in crm.main(["scan"])}
        case, out = self.act(crm, "fix", dets["crm:contact/c-101/email"])
        self.assertEqual((out["action"], out["params"]["to"]), ("crm.update_field", "ana.ruiz@example.org"))
        self.assertEqual(self.check(out, "crm_field_set")["outcome"], "not_yet")
        self.assertEqual(self.execute(case, out)["outcome"], "executed")
        self.assertEqual(self.check(out, "crm_field_set")["outcome"], "verified")
        self.assertEqual(self.execute(case, out)["outcome"], "escalate", "a second run finds the value changed: nothing done twice")
        case, out = self.act(crm, "fix", dets["crm:contact/c-103/merge"])
        self.assertEqual((out["action"], out["params"]), ("crm.merge", {"keep": "c-102", "drop": "c-103"}))
        self.assertEqual(self.execute(case, out)["outcome"], "executed")
        self.assertEqual(self.check(out, "crm_merged")["outcome"], "verified")
        kept = store.by_id(store.load("contacts"))["c-102"]
        self.assertEqual(kept["phone"], "+49 30 1234567", "the dropped record's fields fill the kept one's gaps")
        self.assertEqual([m["id"] for m in store.load("merged")], ["c-103"])

    def test_the_executor_refuses_a_plan_that_was_not_approved(self):
        det = {d["target"]: d for d in crm.main(["scan"])}["crm:contact/c-101/email"]
        case, out = self.act(crm, "fix", det)
        plan = self.approve(case, out)
        plan["params"] = dict(plan["params"], to="attacker@example.org")
        os.environ["FLOTILLA_PLAN"] = json.dumps(plan)
        self.assertEqual(apply.main(["execute"])["outcome"], "escalate")
        self.assertEqual(store.by_id(store.load("contacts"))["c-101"]["email"], "Ana.Ruiz@Example.org ")


class Campaigns(Data):
    fleet = "campaigns"

    def test_the_checks_decide(self):
        cs, landing, cp, footer = (seed("campaigns", n) for n in ("campaigns", "landing_pages", "copy", "sender"))
        self.assertEqual(campaigns.checks(store.by_id(cs)["k-201"], set(landing), cp, footer), [])
        got = {k for k, _ in campaigns.checks(store.by_id(cs)["k-202"], set(landing), cp, footer)}
        self.assertEqual(got, {"utm", "merge", "copy"})
        bad = dict(store.by_id(cs)["k-201"], body=store.by_id(cs)["k-201"]["body"].replace("www.example.com/sale", "evil.example/sale"))
        self.assertIn("links", {k for k, _ in campaigns.checks(bad, set(landing), cp, footer)})
        nofoot = dict(store.by_id(cs)["k-201"], body="https://www.example.com/sale?utm_source=a&utm_medium=b&utm_campaign=spring-sale")
        self.assertIn("footer", {k for k, _ in campaigns.checks(nofoot, set(landing), cp, footer)})

    def test_end_to_end_launch_and_hold(self):
        dets = {d["target"]: d for d in campaigns.main(["scan"])}
        self.assertEqual(sorted(dets), ["campaign:k-201", "campaign:k-202"], "only the ready ones")
        case, out = self.act(campaigns, "decide", dets["campaign:k-201"])
        self.assertEqual(out["action"], "campaign.launch")
        self.assertEqual(self.execute(case, out)["outcome"], "executed")
        self.assertEqual(self.check(out, "campaign_status")["outcome"], "verified")
        case, out = self.act(campaigns, "decide", dets["campaign:k-202"])
        self.assertEqual(out["action"], "campaign.hold")
        self.assertIn("unfilled merge field {{first_name}}", out["params"]["reason"])
        self.assertEqual(self.execute(case, out)["outcome"], "executed")
        self.assertEqual(self.check(out, "campaign_status")["outcome"], "verified")
        self.assertEqual(campaigns.main(["scan"]), [], "nothing is ready any more")


class Outbound(Data):
    fleet = "outbound"

    def decide(self, pid, outbox=()):
        return outbound.decide({"id": "x", "kind": "prospect_new", "data": {"prospect": pid}}, seed("outbound", "prospects"),
                               seed("outbound", "enrichment"), seed("outbound", "suppression"), list(outbox),
                               seed("outbound", "sender"), today="2026-11-17")

    def test_every_personal_sentence_cites_a_fact(self):
        out = self.decide("p-301")
        body = out["params"]["body"]
        self.assertIn("I saw Northwind Analytics is hiring for two data engineers.  [fact f-1]", body)
        self.assertIn("[fact f-2]", body)
        self.assertEqual(out["params"]["facts"], "f-1,f-2")
        out = self.decide("p-304")
        self.assertNotIn("funding", out["params"]["body"], "a fact of a kind with no sentence is never used")
        self.assertEqual(out["params"]["facts"], "f-4")

    def test_the_gates(self):
        self.assertIn("do-not-contact", self.decide("p-303")["detail"])
        self.assertIn("lawful basis", self.decide("p-302")["detail"])
        self.assertEqual(self.decide("p-304")["action"], "email.send", "an EU contact with a recorded basis")
        full = [{"prospect": f"z-{i}", "sent": "2026-11-17T09:00:00Z"} for i in range(outbound.DAILY_CAP)]
        self.assertIn("cap", self.decide("p-301", full)["detail"])
        self.assertEqual(self.decide("p-301", full[:-1])["action"], "email.send")

    def test_end_to_end_send(self):
        dets = {d["target"]: d for d in outbound.main(["scan"])}
        case, out = self.act(outbound, "draft", dets["prospect:p-301"])
        self.assertEqual(out["action"], "email.send")
        self.assertEqual(self.check(out, "email_in_outbox")["outcome"], "not_yet")
        self.assertEqual(self.execute(case, out)["outcome"], "executed")
        self.assertEqual(self.check(out, "email_in_outbox")["outcome"], "verified")
        self.assertEqual(self.execute(case, out)["outcome"], "escalate", "one first touch, never two")
        self.assertNotIn("prospect:p-301", {d["target"] for d in outbound.main(["scan"])})

    def test_the_executor_rechecks_suppression(self):
        dets = {d["target"]: d for d in outbound.main(["scan"])}
        case, out = self.act(outbound, "draft", dets["prospect:p-301"])
        store.save("suppression", store.load("suppression") + ["riley@northwind.example"])
        self.assertEqual(self.execute(case, out)["outcome"], "escalate", "opted out between the plan and the send")
        self.assertEqual(store.load("outbox", []), [])


class Renewals(Data):
    fleet = "renewals"

    def test_the_score_and_the_window(self):
        found = renewals.scan(seed("renewals", "accounts"), TODAY)
        self.assertEqual([d["target"] for d in found], ["account:a-401"], "a-403 is risky but renews outside 60 days")
        self.assertEqual(found[0]["data"]["score"], 70)

    def test_the_replay_over_past_renewals(self):
        """What the score would have said: 3 of 4 churners caught, 1 false alarm, h-6 missed (seats down 37 %
        alone scores 40). A change to the score is read against these numbers before it ships."""
        self.assertEqual(renewals.replay(seed("renewals", "history")),
                         {"renewals": 8, "churned": 4, "flagged": 4, "caught": 3, "false_alarms": 1, "missed": ["h-6"]})

    def test_end_to_end_flag(self):
        dets = renewals.main(["scan"], today=TODAY)
        os.environ["FLOTILLA_CASE"] = json.dumps({"id": "case-r", "kind": dets[0]["kind"], "data": dets[0]["data"]})
        out = renewals.main(["flag"], today=TODAY)
        self.assertEqual((out["action"], out["params"]["risk"]), ("crm.flag_account", "high"))
        self.assertIn("decide the outreach yourself", out["params"]["note"])
        case = {"id": "case-r"}
        self.assertEqual(self.execute(case, out)["outcome"], "executed")
        self.assertEqual(self.check(out, "account_flagged")["outcome"], "verified")


class Registry(unittest.TestCase):
    def test_every_action_planned_here_is_declared_and_each_check_is_implemented(self):
        with open(os.path.join(HERE, "..", "host", "registry.json")) as f:
            reg = {a["name"]: a for a in json.load(f)["actions"]}
        for name in apply.ACTIONS:
            self.assertIn(name, reg)
            self.assertEqual(reg[name]["executor"], "routine", name)
        old = os.environ.get("BIZ_DATA")
        with tempfile.TemporaryDirectory() as d:
            os.environ["BIZ_DATA"] = d
            try:
                for name in ("contacts", "campaigns", "accounts", "outbox"):
                    store.save(name, [])
                for n in apply.ACTIONS:
                    v = reg[n]["check"]["verify"]
                    self.assertEqual(apply.check({"verify": v, "action": n, "params": {}})["outcome"], "not_yet", v)
                self.assertEqual(apply.check({"verify": "no_such_check", "params": {}})["outcome"], "failed")
            finally:
                if old is None:
                    os.environ.pop("BIZ_DATA", None)
                else:
                    os.environ["BIZ_DATA"] = old
        self.assertEqual({reg[n]["reversible"] for n in ("crm.merge", "campaign.launch", "email.send")}, {"no"},
                         "what cannot be undone is declared so: policy keeps it at propose")


if __name__ == "__main__":
    unittest.main()
