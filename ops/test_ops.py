"""Tests for the ops routines against the repo's own registry declaration (host/registry.json).

  cd ops && python3 -m unittest test_ops
"""
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
os.environ["FLOTILLA_REGISTRY"] = os.path.join(HERE, "..", "host", "registry.json")
import common  # noqa: E402
import backup_monitor  # noqa: E402
import rerun_backup  # noqa: E402

common.REGISTRY = os.environ["FLOTILLA_REGISTRY"]
CASE = {"id": "case-1", "kind": "failed_backup", "target": "host:mini"}


class TestRegistryAndCode(unittest.TestCase):
    def test_every_precondition_name_is_implemented(self):
        """The registry's declaration and the routine's code agree (RFC-0006 §10 step 4)."""
        a = common.action(common.RERUN)
        for c in a["pre"] + a.get("pre_shadow", []):
            name, _ = common.parse(c)
            self.assertIn(name, rerun_backup.CONDITIONS, f"{c} is declared but not implemented")

    def test_thresholds_come_from_the_registry(self):
        a = common.action(common.RERUN)
        self.assertEqual(backup_monitor.threshold_hours(a, "shadow"), 20)
        self.assertEqual(backup_monitor.threshold_hours(a, "propose"), 26)
        self.assertEqual(backup_monitor.threshold_hours(a, "live"), 26)


class TestMonitor(unittest.TestCase):
    def test_detect(self):
        self.assertEqual(backup_monitor.detect(11.5, 20, "host:mini"), [])
        self.assertEqual(backup_monitor.detect(20.0, 20, "host:mini"), [], "at the threshold, not above it")
        d = backup_monitor.detect(20.4, 20, "host:mini")
        self.assertEqual([(x["kind"], x["target"]) for x in d], [("failed_backup", "host:mini")])
        self.assertIn("20.4h", d[0]["detail"])
        self.assertIn("30 days", backup_monitor.detect(None, 20, "host:mini")[0]["detail"])


class TestDecide(unittest.TestCase):
    def setUp(self):
        self.a = common.action(common.RERUN)

    def test_plan_in_shadow_above_20h(self):
        out = rerun_backup.decide(CASE, self.a, "shadow", 20.4)
        self.assertEqual((out["outcome"], out["action"], out["params"]), ("plan", common.RERUN, {"host": "mini"}))
        self.assertIn("backup_age_above(20h): holds", out["detail"])
        self.assertIn("not_running: the host's to check", out["detail"])

    def test_resolved_when_it_no_longer_holds(self):
        self.assertEqual(rerun_backup.decide(CASE, self.a, "shadow", 3.0)["outcome"], "resolved_itself")
        # 21h is above shadow's 20h but under propose's 26h
        self.assertEqual(rerun_backup.decide(CASE, self.a, "propose", 21.0)["outcome"], "resolved_itself")
        self.assertEqual(rerun_backup.decide(CASE, self.a, "propose", 27.0)["outcome"], "plan")

    def test_no_backup_at_all_plans(self):
        self.assertEqual(rerun_backup.decide(CASE, self.a, "shadow", None)["outcome"], "plan")

    def test_escalates_what_it_cannot_act_on(self):
        self.assertEqual(rerun_backup.decide({"kind": "memory_growth", "target": "host:mini"}, self.a, "shadow", 30)["outcome"], "escalate")
        self.assertEqual(rerun_backup.decide({"kind": "failed_backup", "target": "container:x"}, self.a, "shadow", 30)["outcome"], "escalate")
        odd = dict(self.a, pre_shadow=["backup_age_above(20h)", "disk_free_above(10)"])
        self.assertEqual(rerun_backup.decide(CASE, odd, "shadow", 30)["outcome"], "escalate")


class VM(BaseHTTPRequestHandler):
    value = "73800"  # 20.5h in seconds; None answers an empty result

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        result = [] if VM.value is None else [{"metric": {}, "value": [0, VM.value]}]
        body = json.dumps({"status": "success", "data": {"resultType": "vector", "result": result}}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


class TestRuns(unittest.TestCase):
    """The two scripts as the worker runs them: environment in, plan file out."""

    @classmethod
    def setUpClass(cls):
        cls.srv = HTTPServer(("127.0.0.1", 0), VM)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def run_script(self, name, extra=None):
        plan = os.path.join(tempfile.mkdtemp(), "plan.json")
        env = {"PATH": os.environ["PATH"], "VM_URL": f"http://127.0.0.1:{self.srv.server_port}", "BACKUP_INSTANCE": "homelab",
               "BACKUP_TARGET": "host:mini", "FLOTILLA_PLAN_FILE": plan, "FLEETD_STAGE": "shadow",
               "FLOTILLA_REGISTRY": common.REGISTRY, **(extra or {})}
        p = subprocess.run([sys.executable, os.path.join(HERE, name)], env=env, capture_output=True, text=True, timeout=30)
        out = None
        if os.path.exists(plan):
            with open(plan) as f:
                out = json.load(f)
        return p, out

    def test_monitor_reports(self):
        VM.value = "73800"
        p, out = self.run_script("backup_monitor.py")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(out["outcome"], "detections")
        self.assertEqual(len(out["detections"]), 1)
        self.assertIn("BACKUP MONITOR age=20.5h threshold=20h", p.stdout)
        VM.value = "3600"
        _, out = self.run_script("backup_monitor.py")
        self.assertEqual(out, {"outcome": "detections", "detections": []})

    def test_rerun_plans(self):
        VM.value = "73800"
        p, out = self.run_script("rerun_backup.py", {"FLOTILLA_CASE": json.dumps(CASE)})
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(out["outcome"], "plan")

    def test_missing_config_fails_loudly(self):
        p, out = self.run_script("backup_monitor.py", {"BACKUP_INSTANCE": ""})
        self.assertEqual(p.returncode, 2)
        self.assertIn("BACKUP_INSTANCE is not set", p.stderr)
        self.assertIsNone(out, "no report from a misconfigured run")
        p, _ = self.run_script("backup_monitor.py", {"BACKUP_INSTANCE": 'x"} or vector(1) #'})
        self.assertEqual(p.returncode, 2, "a label value that would change the query is refused")


if __name__ == "__main__":
    unittest.main()
