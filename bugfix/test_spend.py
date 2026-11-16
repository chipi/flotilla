"""python3 bugfix/test_spend.py: what a bugfix run reports as its cost (bugfix/spend.py, fix.py --spend-file).
The gateway is a local stand-in for LiteLLM's /key/info; pi's sessions are written as pi writes them."""
import http.server
import json
import os
import sys
import tempfile
import threading
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fix  # noqa: E402
import spend  # noqa: E402


class Gateway:
    """/key/info answering the next spend of a script per read (the last one repeats); records what it saw."""

    def __init__(self, spends, status=200, delay=0.0, step=0.0):
        self.spends, self.status, self.seen, self.delay, self.step = list(spends), status, [], delay, step
        g = self

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(h):
                g.seen.append((h.path, h.headers.get("Authorization")))
                if g.delay and len(g.seen) > 1:
                    import time
                    time.sleep(g.delay)
                s = g.spends.pop(0) if len(g.spends) > 1 else g.spends[0]
                if g.step:   # never settles: every read is higher
                    s = s + g.step * len(g.seen)
                body = json.dumps({"key": "x", "info": {"spend": s, "max_budget": 12.0}}).encode()
                h.send_response(g.status)
                h.end_headers()
                try:
                    h.wfile.write(body)
                except BrokenPipeError:   # the slow-gateway test's client gives up first, as meant
                    pass

            def log_message(h, *a):
                pass

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.srv.server_address[1]}/v1"

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


def session(agent_dir, costs):
    d = os.path.join(agent_dir, "sessions", "--wt--")
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "s.jsonl"), "w") as f:
        f.write(json.dumps({"type": "session", "version": 3}) + "\n")
        for c in costs:
            f.write(json.dumps({"type": "message", "message": {"role": "assistant", "usage": {"cost": {"total": c}}}}) + "\n")
            f.write(json.dumps({"type": "message", "message": {"role": "toolResult", "content": "x"}}) + "\n")


class SpendTest(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()

    def gateway(self, spends, status=200, **kw):
        g = Gateway(spends, status, **kw)
        self.addCleanup(g.close)
        return g

    def test_the_cost_is_the_gateways_bill_once_it_settles(self):
        g = self.gateway([1.0, 1.2, 1.35, 1.35])
        m = spend.Meter([(g.base, "sk-test")], poll_s=0.01, settle_s=5)
        session(os.path.join(self.d, "pi"), [0.5, 0.5])   # pi's own figure is not the bill
        usd, source, why = m.cost(os.path.join(self.d, "pi"))
        self.assertEqual((round(usd, 6), source, why), (0.35, "gateway", ""))

    def test_the_key_is_in_the_header_never_in_the_url(self):
        g = self.gateway([2.0])
        spend.read_spend("sk-secret", g.base)
        self.assertEqual(g.seen, [("/key/info", "Bearer sk-secret")])

    def test_an_unreadable_gateway_reports_pis_figure(self):
        g = self.gateway([1.0], status=500)
        m = spend.Meter([(g.base, "sk-test")], poll_s=0.01, settle_s=1)
        session(os.path.join(self.d, "pi"), [0.25, 0.5])
        usd, source, why = m.cost(os.path.join(self.d, "pi"))
        self.assertEqual((usd, source), (0.75, "pi"))
        self.assertIn("unreadable", why)

    def test_a_spend_that_went_down_reports_pis_figure(self):
        g = self.gateway([3.0, 0.1, 0.1])
        m = spend.Meter([(g.base, "sk-test")], poll_s=0.01, settle_s=1)
        session(os.path.join(self.d, "pi"), [0.4])
        usd, source, why = m.cost(os.path.join(self.d, "pi"))
        self.assertEqual((usd, source), (0.4, "pi"))
        self.assertIn("went down", why)

    def test_two_keys_add_up(self):
        a, b = self.gateway([1.0, 1.5, 1.5]), self.gateway([10.0, 10.25, 10.25])
        m = spend.Meter([(a.base, "sk-a"), (b.base, "sk-b")], poll_s=0.01, settle_s=5)
        usd, source, _ = m.cost(os.path.join(self.d, "pi"))
        self.assertEqual((round(usd, 6), source), (0.75, "gateway"))

    def test_keys_of_reads_key_env_and_key_file_and_skips_keyless_endpoints(self):
        kf = os.path.join(self.d, "k")
        with open(kf, "w") as f:
            f.write("sk-file\n")
        cfg = {"endpoints": {"a": {"base_url": "http://a/v1", "key_env": "K"}, "b": {"base_url": "http://b/v1", "key_file": kf},
                             "c": {"base_url": "http://c/v1"}},
               "aliases": {"x": {"endpoint": "a"}, "y": {"endpoint": "b"}, "z": {"endpoint": "c"}, "w": {"endpoint": "a"}}}
        self.assertEqual(spend.keys_of(cfg, ["x", "y", "z", "w", "missing"], {"K": "sk-env"}),
                         [("http://a/v1", "sk-env"), ("http://b/v1", "sk-file")])

    def test_fix_writes_the_runs_cost_to_the_spend_file(self):
        # a run that stops early (the case has no issue) still reports what it spent
        g = self.gateway([4.0, 4.125, 4.125])
        kf = os.path.join(self.d, "key")
        with open(kf, "w") as f:
            f.write("sk-test")
        models = {"endpoints": {"gw": {"base_url": g.base, "kind": "litellm", "key_file": kf}},
                  "aliases": {"bugfix-flash": {"endpoint": "gw", "model": "fleet-bugfix-flash"},
                              "bugfix-reviewer": {"endpoint": "gw", "model": "fleet-bugfix-reviewer"}}}
        home = os.path.join(self.d, "home")
        os.makedirs(home)
        out = fix.main(["--repo", "chipi/orrery", "--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer",
                        "--reviewer-model", "bugfix-reviewer", "--scratch", os.path.join(self.d, "scratch"),
                        "--spend-file", ".bugfix-fix-spend", "--spend-poll", "0.01"],
                       environ={"FLOTILLA_CASE": json.dumps({"id": "c", "data": {}}), "HOME": home,
                                "FLOTILLA_PLAN_FILE": os.path.join(self.d, "p.json"), "FLOTILLA_MODELS": json.dumps(models),
                                "PATH": os.environ["PATH"]})
        self.assertEqual(out["outcome"], "escalate")
        with open(os.path.join(home, ".bugfix-fix-spend")) as f:
            self.assertEqual(float(f.read()), 0.125)

    def test_two_keys_on_one_gateway_are_both_read(self):
        cfg = {"endpoints": {"a": {"base_url": "http://g/v1", "key_env": "A"}, "b": {"base_url": "http://g/v1", "key_env": "B"}},
               "aliases": {"x": {"endpoint": "a"}, "y": {"endpoint": "b"}, "z": {"endpoint": "a"}}}
        self.assertEqual(spend.keys_of(cfg, ["x", "y", "z"], {"A": "sk-a", "B": "sk-b"}), [("http://g/v1", "sk-a"), ("http://g/v1", "sk-b")])

    def test_a_hurried_read_of_a_slow_gateway_falls_back_in_time(self):
        # the worker SIGKILLs 10 s after its SIGTERM: a 20 s read must not hold the report
        import time
        g = self.gateway([1.0, 1.5], delay=20)
        m = spend.Meter([(g.base, "sk-test")], poll_s=0.01, settle_s=60)
        session(os.path.join(self.d, "pi"), [0.3])
        m.hurry()
        t0 = time.monotonic()
        usd, source, _ = m.cost(os.path.join(self.d, "pi"))
        self.assertLess(time.monotonic() - t0, 7)
        self.assertEqual((usd, source), (0.3, "pi"))

    def test_a_sigterm_during_the_settle_still_writes_the_spend(self):
        # the run has ended and the spend is being read (a gateway that never settles, settle 60 s) when the worker's
        # SIGTERM comes: the report must still land before its SIGKILL 10 s later
        import signal
        import subprocess
        import time
        g = self.gateway([4.0], step=0.01)
        kf = os.path.join(self.d, "key")
        with open(kf, "w") as f:
            f.write("sk-test")
        models = {"endpoints": {"gw": {"base_url": g.base, "kind": "litellm", "key_file": kf}},
                  "aliases": {"bugfix-flash": {"endpoint": "gw", "model": "m"}, "bugfix-reviewer": {"endpoint": "gw", "model": "m"}}}
        home = os.path.join(self.d, "home")
        os.makedirs(home)
        env = dict(os.environ, FLOTILLA_CASE=json.dumps({"id": "c", "data": {}}), HOME=home,
                   FLOTILLA_PLAN_FILE=os.path.join(self.d, "p.json"), FLOTILLA_MODELS=json.dumps(models))
        p = subprocess.Popen([sys.executable, os.path.abspath(fix.__file__), "--repo", "chipi/orrery", "--worker-model", "bugfix-flash",
                              "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer",
                              "--scratch", os.path.join(self.d, "scratch"), "--spend-file", ".spend", "--spend-poll", "0.5"],
                             env=env, start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: p.poll() is None and os.killpg(p.pid, signal.SIGKILL))
        time.sleep(3)   # the case has no issue: the chain stops at once and the settle is under way
        os.killpg(p.pid, signal.SIGTERM)
        p.wait(timeout=10)
        with open(os.path.join(home, ".spend")) as f:
            self.assertGreaterEqual(float(f.read()), 0.0)

    def test_a_sigkill_during_the_gateway_read_still_leaves_a_number(self):
        # pi's figure is written before the gateway is read: a SIGKILL mid-read (past the worker's grace) leaves it
        import signal
        import subprocess
        import time
        g = self.gateway([4.0], step=0.01)   # never settles: the read goes on until the settle limit
        kf = os.path.join(self.d, "key")
        with open(kf, "w") as f:
            f.write("sk-test")
        models = {"endpoints": {"gw": {"base_url": g.base, "kind": "litellm", "key_file": kf}},
                  "aliases": {"bugfix-flash": {"endpoint": "gw", "model": "m"}, "bugfix-reviewer": {"endpoint": "gw", "model": "m"}}}
        home = os.path.join(self.d, "home")
        os.makedirs(home)
        env = dict(os.environ, FLOTILLA_CASE=json.dumps({"id": "c", "data": {}}), HOME=home,
                   FLOTILLA_PLAN_FILE=os.path.join(self.d, "p.json"), FLOTILLA_MODELS=json.dumps(models))
        p = subprocess.Popen([sys.executable, os.path.abspath(fix.__file__), "--repo", "chipi/orrery", "--worker-model", "bugfix-flash",
                              "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer",
                              "--scratch", os.path.join(self.d, "scratch"), "--spend-file", ".spend", "--spend-poll", "0.5"],
                             env=env, start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: p.poll() is None and os.killpg(p.pid, signal.SIGKILL))
        time.sleep(3)
        os.killpg(p.pid, signal.SIGKILL)
        p.wait(timeout=10)
        with open(os.path.join(home, ".spend")) as f:
            self.assertEqual(float(f.read()), 0.0)   # no pi session ran: pi's figure is 0, and it is there

    def test_without_spend_file_nothing_is_read_or_written(self):
        g = self.gateway([1.0])
        home = os.path.join(self.d, "home")
        os.makedirs(home)
        models = {"endpoints": {"gw": {"base_url": g.base, "key_env": "K"}},
                  "aliases": {"bugfix-flash": {"endpoint": "gw", "model": "m"}, "bugfix-reviewer": {"endpoint": "gw", "model": "m"}}}
        fix.main(["--repo", "chipi/orrery", "--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer",
                  "--reviewer-model", "bugfix-reviewer", "--scratch", os.path.join(self.d, "scratch")],
                 environ={"FLOTILLA_CASE": json.dumps({"id": "c", "data": {}}), "HOME": home, "K": "sk",
                          "FLOTILLA_PLAN_FILE": os.path.join(self.d, "p.json"), "FLOTILLA_MODELS": json.dumps(models),
                          "PATH": os.environ["PATH"]})
        self.assertEqual((g.seen, os.listdir(home)), ([], []))


if __name__ == "__main__":
    unittest.main()
