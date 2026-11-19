"""Unit and HTTP tests for the worker (stdlib unittest; no network beyond localhost).

  python3 worker/test_worker.py
"""
import json
import os
import socket
import sys
import shutil
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
        self.addCleanup(shutil.rmtree, d, True)
        f = os.path.join(d, "fleet.env")
        with open(f, "w") as fh:
            fh.write("# comment\nGITHUB_TOKEN=gh-secret\nexport SF_HOST=host-01\nQUOTED=\"q\"\n")
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
        self.addCleanup(shutil.rmtree, d, True)
        os.makedirs(os.path.join(d, "a", "b"))
        self.assertEqual(fw.resolve(d, "a/b"), os.path.realpath(os.path.join(d, "a", "b")))
        self.assertEqual(fw.resolve(d, "."), os.path.realpath(d))

    def test_escape(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        for rel in ("../x", "/etc", "a/../../x"):
            with self.assertRaises(ValueError, msg=rel):
                fw.resolve(d, rel)


class TestRunJob(unittest.TestCase):
    def setUp(self):
        self.code = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.code, True)
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home, True)
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

    def test_the_run_is_told_its_deadline(self):
        before = time.time()
        res = fw.run_job(self.req("echo deadline=$FLOTILLA_RUN_DEADLINE", timeout=100), self.environ)
        deadline = float(res["output_tail"].split("deadline=")[1])
        self.assertTrue(before + 99 <= deadline <= time.time() + 100, deadline)

    def test_spend_read_and_truncated(self):
        os.makedirs(os.path.join(self.home, "r"))
        res = fw.run_job(self.req("echo 0.0123 > $HOME/r/spend", spend_file="r/spend"), self.environ)
        self.assertAlmostEqual(res["spend_usd"], 0.0123)
        with open(os.path.join(self.home, "r", "spend")) as f:
            self.assertEqual(f.read(), "", "consumed once, as fleetd did")
        self.assertTrue(res["spend_reported"])

    def test_no_spend_report_is_not_a_reported_zero(self):
        # the Fable advisor's review (2026-11-16): "no report" and "$0" were the same number
        res = fw.run_job(self.req("true", spend_file="r/spend"), self.environ)
        self.assertEqual((res["spend_usd"], res["spend_reported"]), (0.0, False))
        os.makedirs(os.path.join(self.home, "r"), exist_ok=True)
        res = fw.run_job(self.req("echo 0 > $HOME/r/spend", spend_file="r/spend"), self.environ)
        self.assertEqual((res["spend_usd"], res["spend_reported"]), (0.0, True))
        res = fw.run_job(self.req("true"), self.environ)   # no spend file named: nothing was due
        self.assertTrue(res["spend_reported"])

    def test_calls_are_returned_and_the_file_removed(self):
        cmd = ('echo \'{"alias":"a","tokens_in":3}\' >> "$FLOTILLA_CALLS_FILE"; echo not-json >> "$FLOTILLA_CALLS_FILE";'
               ' echo \'{"alias":"b"}\' >> "$FLOTILLA_CALLS_FILE"')
        res = fw.run_job(self.req(cmd), self.environ)
        self.assertEqual([c["alias"] for c in res["calls"]], ["a", "b"], "every JSON line, a bad one skipped")
        self.assertFalse(os.path.exists(fw.calls_path({"HOME": self.home}, "r1")), "consumed once")

    def test_the_cap_is_5000_and_the_lines_over_it_are_counted(self):
        self.assertEqual(fw.CALLS_MAX, 5000)
        path = os.path.join(self.home, "calls.jsonl")
        with open(path, "w") as f:
            for i in range(8):
                f.write(json.dumps({"alias": f"a{i}"}) + "\n")
                if i == 2:
                    f.write("not json\n[1]\n")   # neither is a line, nor counted as dropped
        kept, dropped = fw.read_calls_counted(path, 5)
        self.assertEqual([c["alias"] for c in kept], ["a0", "a1", "a2", "a3", "a4"], "the first lines are the ones kept")
        self.assertEqual(dropped, 3)
        self.assertFalse(os.path.exists(path), "consumed once")
        with open(path, "w") as f:
            f.write('{"alias":"x"}\n')
        self.assertEqual(fw.read_calls_counted(path, 5), ([{"alias": "x"}], 0))
        self.assertEqual(fw.read_calls_counted(os.path.join(self.home, "missing"), 5), ([], 0))

    def test_a_run_over_the_cap_reports_calls_dropped(self):
        old, fw.CALLS_MAX = fw.CALLS_MAX, 3
        try:
            res = fw.run_job(self.req("for i in 1 2 3 4 5; do echo '{\"alias\":\"a'$i'\"}' >> \"$FLOTILLA_CALLS_FILE\"; done"), self.environ)
        finally:
            fw.CALLS_MAX = old
        self.assertEqual(len(res["calls"]), 3)
        self.assertEqual(res["calls_dropped"], 2)
        res = fw.run_job(self.req("true"), self.environ)
        self.assertNotIn("calls_dropped", res)

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


class mode:
    """FLOTILLA_WORKER_MODE for a block: the HTTP server in these tests reads the process environment per request."""

    def __init__(self, value):
        self.value = value

    def __enter__(self):
        self.old = os.environ.get("FLOTILLA_WORKER_MODE")
        os.environ["FLOTILLA_WORKER_MODE"] = self.value

    def __exit__(self, *a):
        if self.old is None:
            os.environ.pop("FLOTILLA_WORKER_MODE", None)
        else:
            os.environ["FLOTILLA_WORKER_MODE"] = self.old


class TestRunToken(unittest.TestCase):
    """A run's own token for flotillad: a 0600 file in the run's temp directory, named by FLOTILLA_API_TOKEN_FILE, removed after."""

    TOKEN = "run-token-" + "0123456789abcdef" * 4

    def setUp(self):
        self.code = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.code, True)
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home, True)
        fw.CODE_ROOT = self.code
        self.environ = {"PATH": os.environ["PATH"], "HOME": self.home,
                        "ROUTINE_FLOTILLA_API_TOKEN_FILE": "/run/flotilla/worker.token"}

    def req(self, command, token=TOKEN, run_id="r-tok", timeout=10):
        r = {"run_id": run_id, "routine": "t/j", "command": command, "workdir": ".", "timeout_s": timeout, "env": {}}
        if token is not None:
            r["api_token"] = token
        return r

    def path(self, run_id="r-tok"):
        return fw.token_path({}, run_id)

    def test_the_run_reads_its_own_token_from_a_0600_file(self):
        res = fw.run_job(self.req('stat -c "mode=%a" "$FLOTILLA_API_TOKEN_FILE"; printf "got=%s\n" "$(cat "$FLOTILLA_API_TOKEN_FILE")"'), self.environ)
        self.assertEqual(res["status"], "ok", res)
        self.assertIn("mode=600", res["output_tail"])
        self.assertIn("got=" + self.TOKEN, res["output_tail"])

    def test_it_replaces_the_worker_token_the_installation_names(self):
        res = fw.run_job(self.req('echo file=$FLOTILLA_API_TOKEN_FILE'), self.environ)
        self.assertIn("file=" + self.path(), res["output_tail"])
        self.assertNotIn("/run/flotilla/worker.token", res["output_tail"])

    def test_without_a_token_the_installations_file_stays(self):
        res = fw.run_job(self.req('echo file=$FLOTILLA_API_TOKEN_FILE', token=None), self.environ)
        self.assertIn("file=/run/flotilla/worker.token", res["output_tail"])
        self.assertFalse(os.path.exists(self.path()))

    def test_the_token_is_not_in_the_environment(self):
        res = fw.run_job(self.req("env"), self.environ)
        self.assertNotIn(self.TOKEN, res["output_tail"], "only the file holds it")

    def test_the_file_is_removed_after_every_ending(self):
        for name, cmd, status in [("ok", "true", "ok"), ("error", "exit 3", "error")]:
            with self.subTest(name):
                res = fw.run_job(self.req(cmd), self.environ)
                self.assertEqual(res["status"], status)
                self.assertFalse(os.path.exists(self.path()), "the token file outlives the run")

    def test_the_file_is_removed_after_a_timeout(self):
        res = fw.run_job(self.req("sleep 30", timeout=0.5), self.environ)
        self.assertEqual(res["status"], "timeout")
        self.assertFalse(os.path.exists(self.path()))

    def test_the_file_is_removed_when_the_run_cannot_start(self):
        r = self.req("true")
        r["workdir"] = "../../escape"
        with self.assertRaises(ValueError):
            fw.run_job(r, self.environ)
        self.assertFalse(os.path.exists(self.path()), "a request refused after the token was written leaves no file")

    def test_a_stale_file_is_replaced(self):
        with open(self.path(), "w") as f:
            f.write("an earlier run's token")
        os.chmod(self.path(), 0o644)
        res = fw.run_job(self.req('stat -c "%a" "$FLOTILLA_API_TOKEN_FILE"; cat "$FLOTILLA_API_TOKEN_FILE"'), self.environ)
        self.assertIn("600", res["output_tail"])
        self.assertIn(self.TOKEN, res["output_tail"])
        self.assertNotIn("earlier", res["output_tail"])

    def test_a_symlink_in_its_place_is_not_followed(self):
        victim = os.path.join(self.home, "victim")
        with open(victim, "w") as f:
            f.write("keep me")
        os.symlink(victim, self.path())
        try:
            res = fw.run_job(self.req("true"), self.environ)
            self.assertEqual(res["status"], "ok")
            with open(victim) as f:
                self.assertEqual(f.read(), "keep me", "the token was written through a symlink")
        finally:
            if os.path.islink(self.path()):
                os.remove(self.path())

    def test_two_runs_have_two_files(self):
        self.assertNotEqual(self.path("r-a"), self.path("r-b"))

    def test_the_run_id_cannot_steer_the_path(self):
        p = fw.token_path({"TMPDIR": self.home}, "../../etc/passwd")
        self.assertEqual(os.path.dirname(p), self.home)
        self.assertTrue(os.path.basename(p).startswith(".flotilla-token-"))
        self.assertNotIn(os.sep, os.path.basename(p))


class TestShadowLogMarks(unittest.TestCase):
    """Every log line of a shadow run carries shadow=true and routine_version; a live run's lines are as they were."""

    def setUp(self):
        self.code = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.code, True)
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home, True)
        fw.CODE_ROOT = self.code
        self.environ = {"PATH": os.environ["PATH"], "HOME": self.home}
        self.shadow_environ = dict(self.environ, FLOTILLA_WORKER_MODE="shadow")

    def lines(self, req):
        import contextlib
        import io
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            res = fw.run_job(req, self.shadow_environ if req.get("shadow") else self.environ)
        return res, [json.loads(ln) for ln in buf.getvalue().splitlines() if ln.startswith("{")]

    def req(self, command, shadow=False, run_id="r-sh"):
        r = {"run_id": run_id, "routine": "t/j", "command": command, "workdir": ".", "timeout_s": 10,
             "env": {"FLOTILLA_ROUTINE_VERSION": "2"}}
        if shadow:
            r["shadow"] = True
        return r

    def test_a_shadow_runs_output_lines_are_marked(self):
        res, lines = self.lines(self.req("echo one; echo two", shadow=True))
        out = [ln for ln in lines if ln["msg"] == "run output"]
        self.assertEqual([ln["line"] for ln in out], ["one", "two"])
        for ln in out:
            self.assertIs(ln["shadow"], True)
            self.assertEqual(ln["routine_version"], 2)

    def test_a_live_runs_lines_are_unchanged(self):
        res, lines = self.lines(self.req("echo one"))
        self.assertTrue(lines)
        for ln in lines:
            self.assertNotIn("shadow", ln)
            self.assertNotIn("routine_version", ln)

    def test_marks_are_gone_when_the_run_ends(self):
        self.lines(self.req("true", shadow=True))
        self.assertNotIn("r-sh", fw._marks)
        fw.log("later", run_id="r-sh")   # nothing of the finished run lingers

    def test_a_missing_version_still_marks_the_shadow(self):
        r = self.req("echo x", shadow=True)
        r["env"] = {}
        _, lines = self.lines(r)
        out = [ln for ln in lines if ln["msg"] == "run output"]
        self.assertIs(out[0]["shadow"], True)
        self.assertNotIn("routine_version", out[0])

    def test_run_marks(self):
        self.assertEqual(fw.run_marks({}), {})
        self.assertEqual(fw.run_marks({"shadow": False, "env": {"FLOTILLA_ROUTINE_VERSION": "3"}}), {})
        self.assertEqual(fw.run_marks({"shadow": True, "env": {"FLOTILLA_ROUTINE_VERSION": "3"}}), {"shadow": True, "routine_version": 3})
        self.assertEqual(fw.run_marks({"shadow": True, "env": {"FLOTILLA_ROUTINE_VERSION": "x"}}), {"shadow": True})


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
        self.assertEqual(body["caps"], ["run_token"], "flotillad's run_tokens: required needs the worker to say it carries a run token")

    def test_bad_requests(self):
        self.assertEqual(self.call("/v1/runs", {"routine": "x"})[0], 400)
        self.assertEqual(self.call("/v1/runs", {"run_id": "r", "routine": "x", "command": "true", "timeout_s": 0})[0], 400)
        self.assertEqual(self.call("/v1/runs", {"run_id": "r", "routine": "x", "command": "true", "timeout_s": 5,
                                                "workdir": "../.."})[0], 400)

    def test_a_bad_api_token_or_shadow_flag_is_refused(self):
        base = {"run_id": "r", "routine": "x", "command": "true", "timeout_s": 5}
        for name, extra in [("short", {"api_token": "abc"}), ("not a string", {"api_token": 12345678901234567890}),
                            ("a path", {"api_token": "../../etc/passwd/../../xxxxxxxx"}), ("empty", {"api_token": ""}),
                            ("shadow not a bool", {"shadow": "yes"})]:
            with self.subTest(name):
                self.assertEqual(self.call("/v1/runs", dict(base, **extra))[0], 400)

    def test_a_run_with_a_token_gets_the_file_and_the_token_is_not_logged(self):
        import contextlib
        import io
        tok = "f" * 64
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            code, body = self.call("/v1/runs", {"run_id": "tk1", "routine": "t/tok", "command": 'test -s "$FLOTILLA_API_TOKEN_FILE"',
                                               "timeout_s": 10, "api_token": tok})
        self.assertEqual((code, body["status"]), (200, "ok"), body)
        self.assertNotIn(tok, buf.getvalue(), "the worker's log must never hold a run's token")
        self.assertFalse(os.path.exists(fw.token_path({}, "tk1")))

    def test_a_shadow_runs_start_and_end_lines_are_marked_and_a_failure_keeps_its_level(self):
        import contextlib
        import io
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            with mode("shadow"):
                code, body = self.call("/v1/runs", {"run_id": "sh1", "routine": "t/shadow", "command": "exit 3", "timeout_s": 10,
                                                   "shadow": True, "env": {"FLOTILLA_ROUTINE_VERSION": "2"}})
            code2, _ = self.call("/v1/runs", {"run_id": "lv1", "routine": "t/live", "command": "exit 3", "timeout_s": 10})
        self.assertEqual((code, body["status"], code2), (200, "error", 200))
        lines = [json.loads(ln) for ln in buf.getvalue().splitlines() if ln.startswith("{")]
        mine = [ln for ln in lines if ln.get("run_id") == "sh1"]
        self.assertEqual({ln["msg"] for ln in mine}, {"run start", "run end"})
        for ln in mine:
            self.assertIs(ln["shadow"], True)
            self.assertEqual(ln["routine_version"], 2)
        end = next(ln for ln in mine if ln["msg"] == "run end")
        live_end = next(ln for ln in lines if ln.get("run_id") == "lv1" and ln["msg"] == "run end")
        self.assertEqual(end["level"], live_end["level"], "a shadow failure logs at the level a live one does")
        self.assertEqual(end["level"], "warning")
        self.assertNotIn("shadow", live_end)

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



def write_file(path, text, mode=0o600):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)
    os.chmod(path, mode)


class TestEnvSplit(unittest.TestCase):
    """The worker's env files are split into the credentials that can only read and the ones that can write (v2 step 2): a live
    run gets both, a shadow run the read ones, and a shadow worker cannot be configured to reach a write credential."""

    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.legacy = os.path.join(self.d, "fleet.env")
        self.read = os.path.join(self.d, "read.env")
        self.write = os.path.join(self.d, "write.env")
        write_file(self.legacy, "LEGACY_KEY=legacy-value\n")
        write_file(self.read, "READ_TOKEN=read-value\nSHARED_URL=http://svc\n")
        write_file(self.write, "GH_TOKEN=write-secret-1\nPUSH_KEY=write-secret-2\n")
        self.live = {"PATH": "/usr/bin", "HOME": "/home/w", "FLOTILLA_ENV_FILES": self.legacy,
                     "FLOTILLA_READ_ENV_FILES": self.read, "FLOTILLA_WRITE_ENV_FILES": self.write}
        self.shadow = {"PATH": "/usr/bin", "HOME": "/home/w", "FLOTILLA_WORKER_MODE": "shadow", "FLOTILLA_READ_ENV_FILES": self.read,
                       "FLOTILLA_WRITE_ENV_FILES": os.path.join(self.d, "not-mounted.env")}

    def test_a_live_run_gets_all_three(self):
        env = fw.build_env({}, self.live)
        for k in ("LEGACY_KEY", "READ_TOKEN", "GH_TOKEN", "PUSH_KEY", "SHARED_URL"):
            self.assertIn(k, env)

    def test_a_shadow_run_gets_the_read_files_only(self):
        env = fw.build_env({}, self.shadow)
        self.assertEqual((env["READ_TOKEN"], env["SHARED_URL"]), ("read-value", "http://svc"))
        for k in ("GH_TOKEN", "PUSH_KEY", "LEGACY_KEY"):
            self.assertNotIn(k, env)

    def test_a_shadow_worker_with_the_unsplit_env_files_is_refused(self):
        bad = dict(self.shadow, FLOTILLA_ENV_FILES=self.legacy)
        with self.assertRaises(fw.Refused) as cm:
            fw.build_env({}, bad)
        self.assertIn("FLOTILLA_READ_ENV_FILES", str(cm.exception))
        with self.assertRaises(fw.Refused):
            fw.check_start(bad)

    def test_a_shadow_worker_that_can_read_a_write_file_does_not_start(self):
        bad = dict(self.shadow, FLOTILLA_WRITE_ENV_FILES=self.write)
        with self.assertRaises(fw.Refused) as cm:
            fw.check_start(bad)
        self.assertIn(self.write, str(cm.exception))
        self.assertNotIn("write-secret", str(cm.exception), "the refusal names files, never values")
        fw.check_start(self.shadow)   # the write file is named but not there: fine
        fw.check_start(self.live)     # a live worker has them all

    def test_a_bad_mode_is_refused(self):
        for v in ("Shadow", "candidate", "both"):
            with self.assertRaises(ValueError):
                fw.worker_mode({"FLOTILLA_WORKER_MODE": v})
        self.assertEqual(fw.worker_mode({}), "live")
        self.assertEqual(fw.worker_mode({"FLOTILLA_WORKER_MODE": ""}), "live")

    def test_a_shadow_run_with_a_write_credential_in_its_environment_is_refused(self):
        base = {"run_id": "s1", "routine": "t/j", "command": "true", "workdir": ".", "timeout_s": 5, "shadow": True}
        # A shadow worker cannot read the write files, so it is told the names (FLOTILLA_WRITE_KEYS); a key of a write file that
        # IS reachable counts too. The credential can come in through the request's env or a ROUTINE_ override.
        named = dict(self.shadow, FLOTILLA_WRITE_KEYS="GH_TOKEN, PUSH_KEY ,DEPLOY_KEY")
        cases = {
            "the request": (dict(base, env={"GH_TOKEN": "smuggled-value"}), named, "GH_TOKEN"),
            "a ROUTINE_ override": (dict(base), dict(named, ROUTINE_PUSH_KEY="override-value"), "PUSH_KEY"),
            "a key named with spaces": (dict(base, env={"DEPLOY_KEY": "named-value"}), named, "DEPLOY_KEY"),
            "a write file that is there": (dict(base, env={"GH_TOKEN": "x-value"}), dict(self.shadow, FLOTILLA_WRITE_ENV_FILES=self.write), "GH_TOKEN"),
        }
        for name, (req, environ, key) in cases.items():
            with self.subTest(name):
                with self.assertRaises(fw.Refused) as cm:
                    fw.prepare_env(req, environ)
                msg = str(cm.exception)
                self.assertIn("write credentials", msg)
                self.assertIn(key, msg)
                self.assertNotIn("-value", msg, "names, never values")
        # with nothing configured the built-in names still refuse: the refusal does not depend on FLOTILLA_WRITE_KEYS being set
        for key in ("GH_TOKEN", "GITHUB_TOKEN", "GLITCHTIP_WRITE_TOKEN", "GRAFANA_ADMIN_PASSWORD", "SLACK_BOT_TOKEN"):
            with self.subTest("built in " + key):
                with self.assertRaises(fw.Refused) as cm:
                    fw.prepare_env(dict(base, env={key: "smuggled-value"}), self.shadow)
                self.assertIn(key, str(cm.exception))
        # a name that is neither built in nor configured is not refused: the check is only as good as its names
        fw.prepare_env(dict(base, env={"SOMETHING_ELSE": "x"}), self.shadow)

    def test_the_write_keys_are_names_only(self):
        self.assertEqual(fw.write_keys(self.live), set(fw.DEFAULT_WRITE_KEYS) | {"GH_TOKEN", "PUSH_KEY"})
        self.assertEqual(fw.write_keys(dict(self.shadow, FLOTILLA_WRITE_KEYS="A,B")), set(fw.DEFAULT_WRITE_KEYS) | {"A", "B"})
        self.assertEqual(fw.write_keys({}), set(fw.DEFAULT_WRITE_KEYS))

    def test_a_clean_shadow_run_is_prepared(self):
        req = {"run_id": "s1", "routine": "t/j", "command": "true", "workdir": ".", "timeout_s": 5, "shadow": True,
               "env": {"FLEETD_STAGE": "live", "FOO": "bar"}}
        env = fw.prepare_env(req, self.shadow)
        self.assertEqual((env["FLOTILLA_SHADOW"], env["FLEETD_STAGE"], env["FOO"]), ("1", "shadow", "bar"),
                         "the stage is shadow whatever the request said")
        self.assertEqual(env["READ_TOKEN"], "read-value")


class TestModes(unittest.TestCase):
    """A shadow run only on a shadow worker, a live run only on a live one."""

    def req(self, shadow=False, run_id="m1"):
        r = {"run_id": run_id, "routine": "t/j", "command": "true", "workdir": ".", "timeout_s": 5}
        if shadow:
            r["shadow"] = True
        return r

    def test_the_mismatches_are_refused(self):
        live, shadow = {"HOME": "/h"}, {"HOME": "/h", "FLOTILLA_WORKER_MODE": "shadow"}
        with self.assertRaises(fw.Refused) as cm:
            fw.check_mode(self.req(shadow=True), live)
        self.assertIn("not a shadow worker", str(cm.exception))
        with self.assertRaises(fw.Refused) as cm:
            fw.check_mode(self.req(), shadow)
        self.assertIn("shadow runs only", str(cm.exception))
        fw.check_mode(self.req(), live)
        fw.check_mode(self.req(shadow=True), shadow)

    def test_a_live_run_is_not_marked_shadow(self):
        env = fw.prepare_env(self.req(), {"HOME": "/h", "PATH": "/usr/bin"})
        self.assertNotIn("FLOTILLA_SHADOW", env)

    def test_the_run_job_refuses_before_it_starts_anything(self):
        before = set(fw._running)
        with self.assertRaises(fw.Refused):
            fw.run_job(self.req(shadow=True), {"HOME": tempfile.mkdtemp(), "PATH": os.environ["PATH"]})
        self.assertEqual(set(fw._running), before)


class TestShadowRun(unittest.TestCase):
    def setUp(self):
        self.code = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.code, True)
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home, True)
        self.snaps = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.snaps, True)
        fw.CODE_ROOT = self.code
        self.saved = (fw.SNAPSHOT_DIR, fw.SNAPSHOT_MAX_BYTES, fw.SNAPSHOT_MAX_FILES)
        fw.SNAPSHOT_DIR = self.snaps
        self.environ = {"PATH": os.environ["PATH"], "HOME": self.home, "FLOTILLA_WORKER_MODE": "shadow"}

    def tearDown(self):
        fw.SNAPSHOT_DIR, fw.SNAPSHOT_MAX_BYTES, fw.SNAPSHOT_MAX_FILES = self.saved

    def req(self, command, **kw):
        r = {"run_id": "s1", "routine": "t/j", "command": command, "workdir": ".", "timeout_s": 10, "shadow": True,
             "env": {"FLEETD_STAGE": "live"}}
        r.update(kw)
        return r

    def snapshot(self, run_id, files):
        for rel, text in files.items():
            write_file(os.path.join(self.snaps, run_id, rel), text)

    def test_it_runs_as_shadow_with_the_marks_and_the_stage(self):
        res = fw.run_job(self.req('echo shadow=$FLOTILLA_SHADOW stage=$FLEETD_STAGE'), self.environ)
        self.assertEqual(res["status"], "ok", res)
        self.assertIn("shadow=1 stage=shadow", res["output_tail"])

    def test_a_shadow_is_started_by_nice_not_by_a_preexec_callback(self):
        import inspect
        src = inspect.getsource(fw._run_job)
        self.assertNotIn("preexec_fn=", src, "a Python callback between fork and exec in a threaded server can deadlock the child")
        self.assertIn('"-n", str(SHADOW_NICE)', src)

    def test_a_worker_without_nice_refuses_a_shadow_run_before_it_starts(self):
        env = dict(self.environ, PATH=tempfile.mkdtemp())   # an empty directory: no nice
        with self.assertRaises(fw.Refused) as cm:
            fw.prepare_env(self.req("true"), env)
        self.assertIn("nice", str(cm.exception))

    def test_it_runs_at_a_lower_priority_than_a_live_run(self):
        cmd = sys.executable + ' -c "import os; print(\'nice=%d\' % os.nice(0))"'
        shadow = fw.run_job(self.req(cmd), self.environ)
        live = fw.run_job({k: v for k, v in self.req(cmd).items() if k != "shadow"}, dict(self.environ, FLOTILLA_WORKER_MODE="live"))
        self.assertIn("nice=%d" % fw.SHADOW_NICE, shadow["output_tail"])
        self.assertNotIn("nice=%d" % fw.SHADOW_NICE, live["output_tail"])

    def test_the_state_is_the_snapshot_and_what_was_there_is_gone(self):
        self.snapshot("live-1", {"state/ledger.tsv": "from the live run\n", "state/sub/deep.txt": "deep\n"})
        write_file(os.path.join(self.home, "state", "ledger.tsv"), "the shadow worker's stale copy\n")
        write_file(os.path.join(self.home, "state", "leftover.txt"), "an earlier shadow's file\n")
        write_file(os.path.join(self.home, "other", "keep.txt"), "not a state path\n")
        res = fw.run_job(self.req('cd "$HOME"; cat state/ledger.tsv state/sub/deep.txt; ls state; cat other/keep.txt',
                                  snapshot_of="live-1", state=["state"]), self.environ)
        out = res["output_tail"]
        self.assertIn("from the live run", out)
        self.assertIn("deep", out)
        self.assertNotIn("stale copy", out)
        self.assertNotIn("leftover.txt", out, "the stale state is removed first")
        self.assertIn("not a state path", out, "HOME outside the state paths is left alone")
        self.assertEqual(oct(os.stat(os.path.join(self.home, "state", "ledger.tsv")).st_mode & 0o777), "0o600")

    def test_a_path_the_live_run_never_had_starts_empty(self):
        self.snapshot("live-1", {"a/x": "1"})
        write_file(os.path.join(self.home, "b", "stale"), "x")
        fw.run_job(self.req("true", snapshot_of="live-1", state=["a", "b"]), self.environ)
        self.assertTrue(os.path.exists(os.path.join(self.home, "a", "x")))
        self.assertFalse(os.path.exists(os.path.join(self.home, "b")))

    def test_a_missing_snapshot_is_a_refusal_before_the_run_starts(self):
        with self.assertRaises(fw.Refused) as cm:
            fw.prepare_env(self.req("true", snapshot_of="nope", state=["state"]), self.environ)
        self.assertIn("no snapshot of nope", str(cm.exception))
        with self.assertRaises(fw.Refused) as cm:
            fw.prepare_env(self.req("true", state=["state"]), self.environ)
        self.assertIn("snapshot_of", str(cm.exception))
        fw.prepare_env(self.req("true", snapshot_of="nope"), self.environ)   # no state paths: nothing to copy, no snapshot needed

    def test_a_symlink_in_the_snapshot_is_never_followed(self):
        victim = tempfile.mkdtemp()
        write_file(os.path.join(victim, "secret.txt"), "outside\n")
        self.snapshot("live-1", {"state/real.txt": "real\n"})
        os.symlink(victim, os.path.join(self.snaps, "live-1", "state", "link"))
        os.symlink(os.path.join(victim, "secret.txt"), os.path.join(self.snaps, "live-1", "state", "file-link"))
        fw.run_job(self.req("true", snapshot_of="live-1", state=["state"]), self.environ)
        self.assertTrue(os.path.exists(os.path.join(self.home, "state", "real.txt")))
        self.assertFalse(os.path.lexists(os.path.join(self.home, "state", "link")), "a linked directory is not copied")
        self.assertFalse(os.path.lexists(os.path.join(self.home, "state", "file-link")), "a linked file is not copied")

    def test_a_state_path_cannot_leave_home_or_be_all_of_it(self):
        self.snapshot("live-1", {"x": "1"})
        for bad in ("../x", "/etc", "a/../../x", "", ".", "./"):
            with self.subTest(bad):
                with self.assertRaises(ValueError):
                    fw.clean_state_path(bad)
        # a symlink in HOME that points out is caught by resolve
        outside = tempfile.mkdtemp()
        os.symlink(outside, os.path.join(self.home, "escape"))
        write_file(os.path.join(self.snaps, "live-1", "escape", "f"), "1")
        with self.assertRaises(ValueError):
            fw.run_job(self.req("true", snapshot_of="live-1", state=["escape"]), self.environ)
        self.assertEqual(os.listdir(outside), [], "nothing was written outside HOME")

    def test_a_snapshot_over_the_caps_is_refused(self):
        self.snapshot("live-1", {"state/big": "x" * 5000})
        fw.SNAPSHOT_MAX_BYTES = 1000
        with self.assertRaises(fw.TooBig):
            fw.run_job(self.req("true", snapshot_of="live-1", state=["state"]), self.environ)
        fw.SNAPSHOT_MAX_BYTES = 1 << 20
        fw.SNAPSHOT_MAX_FILES = 1
        self.snapshot("live-2", {"state/a": "1", "state/b": "2"})
        with self.assertRaises(fw.TooBig):
            fw.run_job(self.req("true", run_id="s2", snapshot_of="live-2", state=["state"]), self.environ)

    def test_a_live_run_never_restores_anything(self):
        self.snapshot("live-1", {"state/x": "1"})
        live_env = dict(self.environ, FLOTILLA_WORKER_MODE="live")
        req = {k: v for k, v in self.req("true", snapshot_of="live-1", state=["state"]).items() if k != "shadow"}
        fw.run_job(req, live_env)
        self.assertFalse(os.path.exists(os.path.join(self.home, "state")))


class TestSnapshots(unittest.TestCase):
    """The live worker's snapshots of a routine's state: whole or not at all, bounded, never through a symlink."""

    def setUp(self):
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home, True)
        self.snaps = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.snaps, True)
        self.environ = {"PATH": os.environ["PATH"], "HOME": self.home}
        self.saved = (fw.SNAPSHOT_DIR, fw.SNAPSHOT_MAX_BYTES, fw.SNAPSHOT_MAX_FILES)
        fw.SNAPSHOT_DIR = self.snaps
        write_file(os.path.join(self.home, "signal-fleet", "results", "filed.tsv"), "a\tb\n")
        write_file(os.path.join(self.home, "signal-fleet", "results", "dispositions.tsv"), "c\td\n")
        write_file(os.path.join(self.home, "other.txt"), "x")

    def tearDown(self):
        fw.SNAPSHOT_DIR, fw.SNAPSHOT_MAX_BYTES, fw.SNAPSHOT_MAX_FILES = self.saved

    def take(self, run_id="live-1", paths=("signal-fleet",), environ=None):
        return fw.make_snapshot(run_id, list(paths), environ or self.environ)

    def test_the_declared_paths_are_copied_with_private_modes(self):
        res = self.take(paths=["signal-fleet", "other.txt", "not-there"])
        self.assertEqual((res["files"], res["bytes"], res["skipped"], res["missing"]), (3, 4 + 4 + 1, 0, ["not-there"]))
        d = os.path.join(self.snaps, "live-1")
        self.assertTrue(os.path.exists(os.path.join(d, "signal-fleet", "results", "filed.tsv")))
        self.assertTrue(os.path.exists(os.path.join(d, "other.txt")))
        self.assertEqual(oct(os.stat(d).st_mode & 0o777), "0o700")
        self.assertEqual(oct(os.stat(os.path.join(d, "signal-fleet")).st_mode & 0o777), "0o700")
        self.assertEqual(oct(os.stat(os.path.join(d, "other.txt")).st_mode & 0o777), "0o600")
        self.assertEqual(sorted(os.listdir(self.snaps)), ["live-1"], "no .partial is left")

    def test_it_is_a_copy_not_a_view(self):
        self.take()
        write_file(os.path.join(self.home, "signal-fleet", "results", "filed.tsv"), "changed after\n")
        with open(os.path.join(self.snaps, "live-1", "signal-fleet", "results", "filed.tsv")) as f:
            self.assertEqual(f.read(), "a\tb\n")

    def test_a_second_snapshot_of_a_run_is_refused(self):
        self.take()
        with self.assertRaises(fw.Refused):
            self.take()

    def test_a_shadow_worker_takes_none(self):
        with self.assertRaises(fw.Refused):
            self.take(environ=dict(self.environ, FLOTILLA_WORKER_MODE="shadow"))

    def test_symlinks_are_skipped_and_counted(self):
        victim = tempfile.mkdtemp()
        write_file(os.path.join(victim, "secret"), "outside")
        os.symlink(victim, os.path.join(self.home, "signal-fleet", "dirlink"))
        os.symlink(os.path.join(victim, "secret"), os.path.join(self.home, "signal-fleet", "filelink"))
        res = self.take()
        self.assertEqual(res["skipped"], 2)
        d = os.path.join(self.snaps, "live-1", "signal-fleet")
        self.assertFalse(os.path.lexists(os.path.join(d, "dirlink")))
        self.assertFalse(os.path.lexists(os.path.join(d, "filelink")))
        for root, _dirs, files in os.walk(os.path.join(self.snaps, "live-1")):
            self.assertNotIn("secret", files)

    def test_a_declared_path_that_is_a_symlink_out_of_home_is_refused(self):
        outside = tempfile.mkdtemp()
        write_file(os.path.join(outside, "f"), "x")
        os.symlink(outside, os.path.join(self.home, "escape"))
        with self.assertRaises(ValueError):
            self.take(paths=["escape"])
        self.assertEqual(os.listdir(self.snaps), [], "nothing left behind")

    def test_bad_paths_are_refused(self):
        for bad in (["../x"], ["/etc"], [""], ["."], [], None, "state", [1]):
            with self.subTest(str(bad)):
                with self.assertRaises(ValueError):
                    fw.make_snapshot("live-1", bad, self.environ)
        self.assertEqual(os.listdir(self.snaps), [])

    def test_over_the_cap_nothing_is_kept(self):
        write_file(os.path.join(self.home, "big", "blob"), "x" * 5000)
        fw.SNAPSHOT_MAX_BYTES = 1000
        with self.assertRaises(fw.TooBig):
            self.take(paths=["big"])
        self.assertEqual(os.listdir(self.snaps), [], "no partial snapshot is visible or left")
        fw.SNAPSHOT_MAX_BYTES, fw.SNAPSHOT_MAX_FILES = 1 << 20, 1
        with self.assertRaises(fw.TooBig):
            self.take(paths=["signal-fleet"])
        self.assertEqual(os.listdir(self.snaps), [])

    def test_delete_is_idempotent(self):
        self.take()
        self.assertTrue(fw.delete_snapshot("live-1"))
        self.assertFalse(fw.delete_snapshot("live-1"))
        self.assertFalse(os.path.exists(os.path.join(self.snaps, "live-1")))
        self.take()   # and the run id is free again

    def test_the_run_id_cannot_steer_the_path(self):
        self.take(run_id="../../escape")
        self.assertEqual(os.listdir(self.snaps), [".._.._escape"], "the name is made safe, inside the snapshot directory")
        with self.assertRaises(ValueError):
            fw.snapshot_path("..")
        with self.assertRaises(ValueError):
            fw.snapshot_path("")

    def test_the_tar_holds_the_snapshot(self):
        self.take()
        f = fw.tar_snapshot("live-1")
        import tarfile
        with tarfile.open(fileobj=f, mode="r:gz") as tf:
            names = sorted(m.name for m in tf.getmembers() if m.isfile())
        self.assertEqual(names, ["./signal-fleet/results/dispositions.tsv", "./signal-fleet/results/filed.tsv"])
        self.assertIsNone(fw.tar_snapshot("nope"))


class TestSnapshotHTTP(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        os.environ["FLOTILLA_WORKER_TOKEN"] = "t" * 40
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        cls.port = s.getsockname()[1]
        s.close()
        cls.srv = fw.ThreadingHTTPServer(("127.0.0.1", cls.port), fw.Handler)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def setUp(self):
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home, True)
        self.snaps = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.snaps, True)
        self.saved = (fw.SNAPSHOT_DIR, fw.SNAPSHOT_MAX_BYTES, os.environ.get("HOME"))
        fw.SNAPSHOT_DIR = self.snaps
        os.environ["HOME"] = self.home
        write_file(os.path.join(self.home, "state", "f.txt"), "hello\n")

    def tearDown(self):
        fw.SNAPSHOT_DIR, fw.SNAPSHOT_MAX_BYTES = self.saved[0], self.saved[1]
        os.environ["HOME"] = self.saved[2]

    def call(self, method, path, body=None, token="t" * 40, raw=False):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}{path}", data=data, method=method,
                                     headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=20) as r:
                b = r.read()
                return r.status, (b if raw else json.loads(b)), r.headers
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read()), e.headers

    def test_take_export_and_delete(self):
        code, body, _ = self.call("POST", "/v1/snapshots", {"run_id": "live-1", "paths": ["state"]})
        self.assertEqual((code, body["files"], body["bytes"], body["missing"]), (200, 1, 6, []))
        code, body, _ = self.call("POST", "/v1/snapshots", {"run_id": "live-1", "paths": ["state"]})
        self.assertEqual(code, 409, body)
        code, raw, headers = self.call("GET", "/v1/snapshots/live-1", raw=True)
        self.assertEqual((code, headers["Content-Type"]), (200, "application/gzip"))
        import io
        import tarfile
        with tarfile.open(fileobj=io.BytesIO(raw), mode="r:gz") as tf:
            self.assertIn("./state/f.txt", [m.name for m in tf.getmembers()])
        code, body, _ = self.call("POST", "/v1/snapshots/live-1/delete", {})
        self.assertEqual((code, body["existed"]), (200, True))
        code, body, _ = self.call("POST", "/v1/snapshots/live-1/delete", {})
        self.assertEqual((code, body["existed"]), (200, False))
        self.assertEqual(self.call("GET", "/v1/snapshots/live-1")[0], 404)

    def test_refusals(self):
        self.assertEqual(self.call("POST", "/v1/snapshots", {"run_id": "x", "paths": ["../etc"]})[0], 400)
        self.assertEqual(self.call("POST", "/v1/snapshots", {"paths": ["state"]})[0], 400)
        self.assertEqual(self.call("POST", "/v1/snapshots", {"run_id": "x"})[0], 400)
        self.assertEqual(self.call("POST", "/v1/snapshots", {"run_id": "x", "paths": ["state"]}, token="wrong")[0], 401)
        self.assertEqual(self.call("GET", "/v1/snapshots/x", token="wrong")[0], 401)
        self.assertEqual(self.call("POST", "/v1/snapshots/a/b/delete", {})[0], 400)
        fw.SNAPSHOT_MAX_BYTES = 3
        code, body, _ = self.call("POST", "/v1/snapshots", {"run_id": "big", "paths": ["state"]})
        self.assertEqual(code, 413, body)
        self.assertEqual(os.listdir(self.snaps), [])

    def test_a_shadow_worker_takes_and_deletes_none_and_says_its_mode(self):
        with mode("shadow"):
            self.assertEqual(self.call("POST", "/v1/snapshots", {"run_id": "x", "paths": ["state"]})[0], 409)
            self.assertEqual(self.call("POST", "/v1/snapshots/x/delete", {})[0], 409)
            self.assertEqual(self.call("GET", "/v1/snapshots/x")[0], 409, "a shadow worker exports none either")
            code, body, _ = self.call("GET", "/healthz")
            self.assertEqual((code, body["mode"]), (200, "shadow"))
        code, body, _ = self.call("GET", "/healthz")
        self.assertEqual((code, body["mode"]), (200, "live"))

    def test_the_two_modes_refuse_each_others_runs_with_a_409(self):
        base = {"run_id": "m1", "routine": "t/mode", "command": "true", "timeout_s": 5}
        code, body, _ = self.call("POST", "/v1/runs", dict(base, shadow=True))
        self.assertEqual(code, 409, body)
        self.assertIn("not a shadow worker", body["error"])
        with mode("shadow"):
            code, body, _ = self.call("POST", "/v1/runs", dict(base))
            self.assertEqual(code, 409, body)
            self.assertIn("shadow runs only", body["error"])
            code, body, _ = self.call("POST", "/v1/runs", dict(base, shadow=True, state=["state"], snapshot_of="nope"))
            self.assertEqual(code, 409, body)
            self.assertIn("no snapshot", body["error"])
            self.assertEqual(self.call("POST", "/v1/runs", dict(base, shadow=True, state=["../x"], snapshot_of="x"))[0], 400)
            self.assertEqual(self.call("POST", "/v1/runs", dict(base, shadow=True, state="state"))[0], 400)
            self.assertEqual(self.call("POST", "/v1/runs", dict(base, shadow=True, snapshot_of=""))[0], 400)
            self.assertEqual(self.call("POST", "/v1/runs", dict(base, shadow=True, input_key=5))[0], 400)
        self.assertNotIn("m1", fw._cancels)
        self.assertNotIn("t/mode", fw._running, "a refused run never registered")


class TestResolveCommit(unittest.TestCase):
    SHA = "0123456789abcdef0123456789abcdef01234567"

    def opener(self, body, seen=None):
        import io

        class R(io.BytesIO):
            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

        def open_(req, timeout=None):
            if seen is not None:
                seen.append((req.full_url, dict(req.header_items()), timeout))
            return R(json.dumps(body).encode())
        return open_

    def test_the_head_of_a_branch(self):
        seen = []
        sha = fw.resolve_commit("example/shop", "release/1.0", {"ROUTINE_GITHUB_API": "http://api.local/", "PATH": "/usr/bin"}, self.opener({"sha": self.SHA}, seen))
        self.assertEqual(sha, self.SHA)
        self.assertEqual(seen[0][0], "http://api.local/repos/example/shop/commits/release/1.0")
        self.assertEqual(seen[0][2], 15)

    def test_the_default_api(self):
        seen = []
        fw.resolve_commit("o/r", "main", {"PATH": "/usr/bin"}, self.opener({"sha": self.SHA}, seen))
        self.assertTrue(seen[0][0].startswith("https://api.github.com/repos/o/r/commits/main"))

    def test_the_api_url_is_read_from_the_workers_env_files(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        f = os.path.join(d, "fleet.env")
        write_file(f, "GITHUB_API=http://from-env-file.local\n")
        seen = []
        fw.resolve_commit("o/r", "main", {"PATH": "/usr/bin", "FLOTILLA_ENV_FILES": f}, self.opener({"sha": self.SHA}, seen))
        self.assertTrue(seen[0][0].startswith("http://from-env-file.local/"))

    def test_bad_names_are_refused_before_any_request(self):
        for repo, branch in [("shop", "main"), ("a/b/c", "main"), ("o/r", ""), ("o/r", "../x"), ("o/r", "a b"), (5, "main"), ("o/r", None), ("o/r;x", "main")]:
            with self.subTest((repo, branch)):
                with self.assertRaises(ValueError):
                    fw.resolve_commit(repo, branch, {"PATH": "/usr/bin"}, self.opener({"sha": self.SHA}))

    def test_an_answer_that_is_not_a_commit_id_is_an_error(self):
        for body in ({}, {"sha": "main"}, {"sha": "XYZ" * 14}, {"sha": 5}, {"sha": self.SHA.upper()}):
            with self.subTest(str(body)):
                with self.assertRaises(OSError):
                    fw.resolve_commit("o/r", "main", {"PATH": "/usr/bin"}, self.opener(body))

    def test_a_shadow_worker_resolves_none(self):
        with self.assertRaises(fw.Refused):
            fw.resolve_commit("o/r", "main", {"PATH": "/usr/bin", "FLOTILLA_WORKER_MODE": "shadow"}, self.opener({"sha": self.SHA}))


class TestCommitHTTP(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        os.environ["FLOTILLA_WORKER_TOKEN"] = "t" * 40

        class API(fw.BaseHTTPRequestHandler):
            def do_GET(self):
                code, body = 200, {"sha": "0123456789abcdef0123456789abcdef01234567"}
                if self.path.endswith("/commits/gone"):
                    code, body = 404, {"message": "Not Found"}
                raw = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *a):
                pass
        cls.api = fw.ThreadingHTTPServer(("127.0.0.1", 0), API)
        threading.Thread(target=cls.api.serve_forever, daemon=True).start()
        cls.srv = fw.ThreadingHTTPServer(("127.0.0.1", 0), fw.Handler)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.port = cls.srv.server_address[1]

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.api.shutdown()

    def setUp(self):
        self.old = os.environ.get("GITHUB_API"), os.environ.get("ROUTINE_GITHUB_API")
        os.environ["ROUTINE_GITHUB_API"] = f"http://127.0.0.1:{self.api.server_address[1]}"

    def tearDown(self):
        for k, v in zip(("GITHUB_API", "ROUTINE_GITHUB_API"), self.old):
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def call(self, body, token="t" * 40):
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}/v1/commit", data=json.dumps(body).encode(), method="POST",
                                     headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=20) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_the_endpoint_answers_the_head_the_api_gives(self):
        self.assertEqual(self.call({"repo": "example/shop", "branch": "main"}), (200, {"sha": "0123456789abcdef0123456789abcdef01234567"}))

    def test_refusals(self):
        self.assertEqual(self.call({"repo": "example/shop", "branch": "main"}, token="wrong")[0], 401)
        self.assertEqual(self.call({"repo": "shop", "branch": "main"})[0], 400)
        self.assertEqual(self.call({"repo": "example/shop"})[0], 400)
        code, body = self.call({"repo": "example/shop", "branch": "gone"})
        self.assertEqual(code, 502, body)
        with mode("shadow"):
            self.assertEqual(self.call({"repo": "example/shop", "branch": "main"})[0], 409)


if __name__ == "__main__":
    unittest.main(verbosity=1)
