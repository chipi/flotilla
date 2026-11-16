"""Unit and HTTP tests for the worker (stdlib unittest; no network beyond localhost).

  python3 worker/test_worker.py
"""
import json
import os
import socket
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import flotilla_worker as fw  # noqa: E402


class TestEnv(unittest.TestCase):
    def test_built_not_inherited(self):
        d = tempfile.mkdtemp()
        f = os.path.join(d, "fleet.env")
        with open(f, "w") as fh:
            fh.write("# comment\nGITHUB_TOKEN=gh-secret\nexport SF_HOST=homelab\nQUOTED=\"q\"\n")
        environ = {"PATH": "/usr/bin", "HOME": "/home/w", "FLOTILLA_WORKER_TOKEN": "worker-secret",
                   "FLOTILLA_ENV_FILES": f, "ROUTINE_SF_HOST": "victoriametrics", "OTHER": "x"}
        env = fw.build_env({"FLEETD_STAGE": "shadow"}, environ)
        self.assertNotIn("FLOTILLA_WORKER_TOKEN", env, "the worker's token must never reach a routine")
        self.assertNotIn("OTHER", env)
        self.assertEqual(env["GITHUB_TOKEN"], "gh-secret")
        self.assertEqual(env["SF_HOST"], "victoriametrics", "ROUTINE_ overrides the env file")
        self.assertEqual(env["QUOTED"], "q")
        self.assertEqual(env["FLEETD_STAGE"], "shadow")
        self.assertEqual(env["HOME"], "/home/w")

    def test_request_wins(self):
        env = fw.build_env({"FLEETD_STAGE": "shadow"}, {"ROUTINE_FLEETD_STAGE": "live"})
        self.assertEqual(env["FLEETD_STAGE"], "shadow", "flotillad decides the stage, not the installation")


class TestResolve(unittest.TestCase):
    def test_inside(self):
        d = tempfile.mkdtemp()
        os.makedirs(os.path.join(d, "a", "b"))
        self.assertEqual(fw.resolve(d, "a/b"), os.path.realpath(os.path.join(d, "a", "b")))
        self.assertEqual(fw.resolve(d, "."), os.path.realpath(d))

    def test_escape(self):
        d = tempfile.mkdtemp()
        for rel in ("../x", "/etc", "a/../../x"):
            with self.assertRaises(ValueError, msg=rel):
                fw.resolve(d, rel)


class TestRunJob(unittest.TestCase):
    def setUp(self):
        self.code = tempfile.mkdtemp()
        self.home = tempfile.mkdtemp()
        fw.CODE_ROOT = self.code
        self.environ = {"PATH": os.environ["PATH"], "HOME": self.home}

    def req(self, command, timeout=10, spend_file=""):
        return {"run_id": "r1", "routine": "t/j", "command": command, "workdir": ".", "timeout_s": timeout,
                "spend_file": spend_file, "env": {"FLEETD_STAGE": "shadow"}}

    def test_ok_and_env(self):
        res = fw.run_job(self.req('echo stage=$FLEETD_STAGE; pwd'), self.environ)
        self.assertEqual(res["status"], "ok")
        self.assertEqual(res["exit_code"], 0)
        self.assertIn("stage=shadow", res["output_tail"])
        self.assertIn(os.path.realpath(self.code), res["output_tail"])

    def test_error(self):
        res = fw.run_job(self.req("echo boom >&2; exit 3"), self.environ)
        self.assertEqual((res["status"], res["exit_code"]), ("error", 3))
        self.assertIn("boom", res["output_tail"], "stderr is part of the output")

    def test_timeout_kills_the_group(self):
        marker = os.path.join(self.home, "child-alive")
        cmd = f"(sleep 3; touch {marker}) & sleep 30"
        t0 = time.monotonic()
        res = fw.run_job(self.req(cmd, timeout=1), self.environ)
        self.assertEqual(res["status"], "timeout")
        self.assertLess(time.monotonic() - t0, fw.KILL_GRACE_S + 5)
        time.sleep(3.5)
        self.assertFalse(os.path.exists(marker), "a child of the timed-out routine survived")

    def test_cancel_kills_the_group_and_reads_the_spend(self):
        marker = os.path.join(self.home, "child-alive")
        os.makedirs(os.path.join(self.home, "r"))
        cmd = f"echo 0.25 > $HOME/r/spend; (sleep 3; touch {marker}) & sleep 30"
        cancel = threading.Event()
        threading.Timer(0.7, cancel.set).start()
        t0 = time.monotonic()
        r = self.req(cmd, spend_file="r/spend")
        res = fw.run_job(r, self.environ, cancel=cancel)
        self.assertEqual(res["status"], "cancelled")
        self.assertAlmostEqual(res["spend_usd"], 0.25, msg="a cancelled run's spend is read as usual")
        self.assertLess(time.monotonic() - t0, fw.KILL_GRACE_S)
        time.sleep(3.5)
        self.assertFalse(os.path.exists(marker), "a child of the cancelled routine survived")

    def test_cancel_escalates_to_sigkill_after_the_grace(self):
        fw.KILL_GRACE_S = 1
        self.addCleanup(setattr, fw, "KILL_GRACE_S", 10)
        cancel = threading.Event()
        threading.Timer(0.5, cancel.set).start()
        t0 = time.monotonic()
        res = fw.run_job(self.req("trap '' TERM; sleep 30"), self.environ, cancel=cancel)
        self.assertEqual(res["status"], "cancelled")
        self.assertGreaterEqual(time.monotonic() - t0, 1.4, "the routine ignores SIGTERM: it lives until the grace is over")
        self.assertLess(time.monotonic() - t0, 8)

    def test_spend_read_and_truncated(self):
        os.makedirs(os.path.join(self.home, "r"))
        res = fw.run_job(self.req("echo 0.0123 > $HOME/r/spend", spend_file="r/spend"), self.environ)
        self.assertAlmostEqual(res["spend_usd"], 0.0123)
        with open(os.path.join(self.home, "r", "spend")) as f:
            self.assertEqual(f.read(), "", "consumed once, as fleetd did")

    def test_calls_are_returned_and_the_file_removed(self):
        cmd = ('echo \'{"alias":"a","tokens_in":3}\' >> "$FLOTILLA_CALLS_FILE"; echo not-json >> "$FLOTILLA_CALLS_FILE";'
               ' echo \'{"alias":"b"}\' >> "$FLOTILLA_CALLS_FILE"')
        res = fw.run_job(self.req(cmd), self.environ)
        self.assertEqual([c["alias"] for c in res["calls"]], ["a", "b"], "every JSON line, a bad one skipped")
        self.assertFalse(os.path.exists(fw.calls_path({"HOME": self.home}, "r1")), "consumed once")

    def test_calls_of_an_earlier_run_are_not_this_runs(self):
        with open(fw.calls_path({"HOME": self.home}, "r1"), "w") as f:
            f.write('{"alias":"stale"}\n')
        res = fw.run_job(self.req("true"), self.environ)
        self.assertEqual(res["calls"], [])

    def test_what_the_run_read_is_returned(self):
        # G7: runtime/loaded.py writes FLOTILLA_LOADED_FILE; a stale one from an earlier run is not this run's
        with open(fw.calls_path({"HOME": self.home}, "r1", "loaded"), "w") as f:
            f.write('{"kind":"stale"}\n')
        cmd = 'echo \'{"kind":"skill","trust":"source"}\' >> "$FLOTILLA_LOADED_FILE"'
        res = fw.run_job(self.req(cmd), self.environ)
        self.assertEqual([x["kind"] for x in res["loaded"]], ["skill"])
        self.assertFalse(os.path.exists(fw.calls_path({"HOME": self.home}, "r1", "loaded")), "consumed once")

    def test_spend_file_cannot_leave_home(self):
        with self.assertRaises(ValueError):
            fw.run_job(self.req("true", spend_file="../../etc/x"), self.environ)

    def planned(self, command, plan_file="plan.json"):
        r = self.req(command)
        r["plan_file"] = plan_file
        return fw.run_job(r, self.environ)

    def test_plan_returned_and_removed(self):
        res = self.planned('echo \'{"outcome": "plan", "action": "a.b"}\' > "$FLOTILLA_PLAN_FILE"')
        self.assertEqual(res["plan"], {"outcome": "plan", "action": "a.b"})
        self.assertNotIn("plan_error", res)
        self.assertFalse(os.path.exists(os.path.join(self.home, "plan.json")), "consumed once")

    def test_an_old_plan_is_never_this_runs(self):
        with open(os.path.join(self.home, "plan.json"), "w") as f:
            f.write('{"outcome": "plan", "action": "left.over"}')
        res = self.planned("true")
        self.assertNotIn("plan", res)

    def test_a_failed_runs_plan_is_not_a_decision(self):
        res = self.planned('echo \'{"outcome": "plan"}\' > "$FLOTILLA_PLAN_FILE"; exit 2')
        self.assertEqual(res["status"], "error")
        self.assertNotIn("plan", res)
        self.assertFalse(os.path.exists(os.path.join(self.home, "plan.json")))

    def test_bad_plans(self):
        for name, cmd, want in [
            ("not JSON", 'echo nope > "$FLOTILLA_PLAN_FILE"', "not JSON"),
            ("not an object", 'echo "[1]" > "$FLOTILLA_PLAN_FILE"', "not a JSON object"),
            ("too big", f'head -c {fw.PLAN_MAX + 10} /dev/zero > "$FLOTILLA_PLAN_FILE"', "over"),
        ]:
            with self.subTest(name):
                res = self.planned(cmd)
                self.assertNotIn("plan", res)
                self.assertIn(want, res.get("plan_error", ""))

    def test_plan_file_cannot_leave_home(self):
        with self.assertRaises(ValueError):
            self.planned("true", plan_file="../../etc/x")


class TestHTTP(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        os.environ["FLOTILLA_WORKER_TOKEN"] = "t" * 40
        fw.CODE_ROOT = tempfile.mkdtemp()
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        cls.port = s.getsockname()[1]
        s.close()
        cls.srv = fw.ThreadingHTTPServer(("127.0.0.1", cls.port), fw.Handler)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def call(self, path, body=None, token="t" * 40):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}{path}", data=data,
                                     headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=20) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_auth(self):
        self.assertEqual(self.call("/healthz", token="wrong")[0], 401)
        code, body = self.call("/livez", token="wrong")
        self.assertEqual((code, body), (200, {"ok": True}), "livez is the healthcheck: no token, no detail")
        code, body = self.call("/healthz")
        self.assertEqual((code, body["ok"]), (200, True))

    def test_bad_requests(self):
        self.assertEqual(self.call("/v1/runs", {"routine": "x"})[0], 400)
        self.assertEqual(self.call("/v1/runs", {"run_id": "r", "routine": "x", "command": "true", "timeout_s": 0})[0], 400)
        self.assertEqual(self.call("/v1/runs", {"run_id": "r", "routine": "x", "command": "true", "timeout_s": 5,
                                                "workdir": "../.."})[0], 400)

    def test_run_and_one_at_a_time(self):
        out = {}
        t = threading.Thread(target=lambda: out.update(r=self.call("/v1/runs", {
            "run_id": "a", "routine": "t/slow", "command": "sleep 2", "timeout_s": 10})))
        t.start()
        time.sleep(0.5)
        code, body = self.call("/v1/runs", {"run_id": "b", "routine": "t/slow", "command": "true", "timeout_s": 10})
        self.assertEqual(code, 409, body)
        t.join()
        self.assertEqual(out["r"][0], 200)
        self.assertEqual(out["r"][1]["status"], "ok")

    def start_run(self, run_id, command, routine="t/hang", timeout_s=60):
        out = {}
        t = threading.Thread(target=lambda: out.update(r=self.call("/v1/runs", {
            "run_id": run_id, "routine": routine, "command": command, "timeout_s": timeout_s})))
        t.start()
        for _ in range(100):
            if run_id in fw._cancels:
                break
            time.sleep(0.05)
        self.assertIn(run_id, fw._cancels, "the worker never took the run")
        return t, out

    def test_cancel_ends_a_running_run_as_cancelled(self):
        marker = os.path.join(fw.CODE_ROOT, "child-alive")
        t, out = self.start_run("c1", f"(sleep 3; touch {marker}) & sleep 30")
        time.sleep(0.3)
        t0 = time.monotonic()
        code, body = self.call("/v1/runs/c1/cancel", {})
        self.assertEqual((code, body["ok"]), (200, True))
        t.join(20)
        self.assertEqual(out["r"][0], 200)
        self.assertEqual(out["r"][1]["status"], "cancelled")
        self.assertLess(time.monotonic() - t0, fw.KILL_GRACE_S, "SIGTERM ends it: no wait for the grace")
        time.sleep(3.5)
        self.assertFalse(os.path.exists(marker), "the cancel did not reach the run's process group")
        self.assertEqual(self.call("/healthz")[1]["running"], [], "the routine is free again")
        self.assertEqual(self.call("/v1/runs/c1/cancel", {})[0], 404, "answered: no longer here")

    def test_cancel_unknown_run_and_auth(self):
        self.assertEqual(self.call("/v1/runs/nope/cancel", {})[0], 404)
        self.assertEqual(self.call("/v1/runs//cancel", {})[0], 404)
        self.assertEqual(self.call("/v1/runs/a/b/cancel", {})[0], 404)
        t, out = self.start_run("c2", "sleep 30")
        self.assertEqual(self.call("/v1/runs/c2/cancel", {}, token="wrong")[0], 401, "the worker's token, like every endpoint")
        self.assertTrue(t.is_alive(), "a refused cancel stopped the run")
        self.assertEqual(self.call("/v1/runs/c2/cancel", {})[0], 200)
        t.join(20)

    def test_cancel_is_idempotent_while_the_run_ends(self):
        fw.KILL_GRACE_S = 2
        self.addCleanup(setattr, fw, "KILL_GRACE_S", 10)
        t, out = self.start_run("c3", "trap '' TERM; sleep 30")
        self.assertEqual(self.call("/v1/runs/c3/cancel", {})[0], 200)
        self.assertEqual(self.call("/v1/runs/c3/cancel", {})[0], 200, "twice while the run is ending")
        t.join(20)
        self.assertEqual(out["r"][1]["status"], "cancelled")


if __name__ == "__main__":
    unittest.main(verbosity=1)
