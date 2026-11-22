"""plumbing/call.py, the shared sender of the plumbing's callbacks: the signature (the vector the Go hook
verifier asserts too), the plan's hash, the payload's shape and the chat text's checks before anything is
sent, the installation's setting and its secret file, and what each answer of the plumbing reports. A local
HTTP receiver stands in for the plumbing and verifies every call it gets.

  python3 plumbing/test_call.py
"""
import hashlib
import hmac
import http.server
import json
import os
import sys
import tempfile
import threading
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(HERE, "..", "business"))
import apply  # noqa: E402
import call  # noqa: E402

SECRET = "".join(["0123456789abcdef"] * 2)  # the Go tests' testSecret (internal/hook)


class Plumbing(http.server.BaseHTTPRequestHandler):
    """The stand-in plumbing: refuses a bad signature with 401, records what it got, answers what the test set."""
    calls, answer = [], (200, {"ref": "task-1"})

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        ts, dl, sig = (self.headers.get(h, "") for h in ("X-Flotilla-Timestamp", "X-Flotilla-Delivery", "X-Flotilla-Signature"))
        want = "sha256=" + hmac.new(SECRET.encode(), f"{ts}.{dl}.".encode() + body, hashlib.sha256).hexdigest()
        if not hmac.compare_digest(sig, want):
            self.send_response(401)
            self.end_headers()
            return
        Plumbing.calls.append({"delivery": dl, "doc": json.loads(body), "type": self.headers.get("Content-Type")})
        code, doc = Plumbing.answer
        out = json.dumps(doc).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *a):
        pass


class CallTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = http.server.HTTPServer(("127.0.0.1", 0), Plumbing)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.dir = tempfile.mkdtemp()
        cls.secret = os.path.join(cls.dir, "s")
        with open(cls.secret, "w") as f:
            f.write(SECRET + "\n")
        os.chmod(cls.secret, 0o600)

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()

    def setUp(self):
        Plumbing.calls, Plumbing.answer = [], (200, {"ref": "task-1"})
        url = f"http://127.0.0.1:{self.srv.server_port}/webhook/x"
        self.env = {}
        for n in call.ACTIONS:
            self.env[f"PLUMBING_{n.upper()}_URL"] = url
            self.env[f"PLUMBING_{n.upper()}_SECRET_FILE"] = self.secret

    def plan(self, action, params, case_id="case-1"):
        return {"id": 7, "action": action, "params": params, "plan_hash": apply.plan_hash(case_id, action, params)}

    def tasks(self, items=None):
        return self.plan("plumbing.create_tasks", {"items": json.dumps(items or [{"who": "ana@example.com", "what": "send the notes", "by": "2026-11-30"}])})

    def test_the_shared_signing_vector(self):
        self.assertEqual(call.sign(SECRET, "1795356651", "plan-0123456789abcdef", b'{"probe":"x"}'),
                         "123685c603afba91ed5362b46c17afd047c90bba06b58b45a6731572cd7da414", "internal/hook TestTheSharedSigningVector")

    def test_the_plan_hash_is_policys(self):
        prm = {"text": "a < b & c", "channel": "general"}
        self.assertEqual(call.plan_hash("case-1", "plumbing.post", prm), apply.plan_hash("case-1", "plumbing.post", prm))

    def test_an_approved_plan_is_sent_signed_and_its_ref_reported(self):
        p = self.tasks()
        out = call.execute({"id": "case-1"}, p, self.env)
        self.assertEqual(out["outcome"], "executed", out)
        self.assertEqual(out["ref"], "task-1")
        self.assertEqual(len(Plumbing.calls), 1)
        got = Plumbing.calls[0]
        self.assertEqual(got["delivery"], "plan-" + p["plan_hash"][:16])
        self.assertEqual(got["type"], "application/json")
        self.assertEqual(got["doc"]["mode"], "send")
        self.assertEqual(got["doc"]["case_id"], "case-1")
        self.assertEqual(got["doc"]["params"]["items"][0]["who"], "ana@example.com", "the list is sent as a list")

    def test_nothing_is_sent_for_a_plan_it_refuses(self):
        tampered = self.tasks()
        tampered["params"] = {"items": json.dumps([{"who": "x", "what": "y", "by": "2026-12-01"}])}
        bad = {
            "a tampered plan": tampered,
            "not a plumbing action": self.plan("crm.merge", {"keep": "a"}),
            "items not JSON": self.plan("plumbing.create_tasks", {"items": "send notes"}),
            "too many items": self.tasks([{"who": "a", "what": "b", "by": "2026-12-01"}] * (call.MAX_ITEMS + 1)),
            "an item with more": self.tasks([{"who": "a", "what": "b", "by": "2026-12-01", "cc": "c"}]),
            "an item's date not a date": self.tasks([{"who": "a", "what": "b", "by": "Friday"}]),
            "a reply without the label": self.plan("plumbing.reply", {"channel": "general", "thread": "1", "text": "hello"}),
            "a post that calls the channel": self.plan("plumbing.post", {"channel": "general", "text": call.LABEL + " hi @channel"}),
            "a post that calls everyone, Slack's way": self.plan("plumbing.post", {"channel": "general", "text": call.LABEL + " hi <!here>"}),
        }
        for name, p in bad.items():
            with self.subTest(name):
                out = call.execute({"id": "case-1"}, p, self.env)
                self.assertEqual(out["outcome"], "escalate", out)
                self.assertIn("nothing sent", out["detail"])
        self.assertEqual(Plumbing.calls, [], "the plumbing got nothing")

    def test_the_setting_and_its_secret(self):
        weak = os.path.join(self.dir, "weak")
        with open(weak, "w") as f:
            f.write(SECRET)
        os.chmod(weak, 0o644)
        short = os.path.join(self.dir, "short")
        with open(short, "w") as f:
            f.write("x" * 31)
        os.chmod(short, 0o600)
        for name, (k, v) in {"no URL": ("PLUMBING_CREATE_TASKS_URL", ""), "not http": ("PLUMBING_CREATE_TASKS_URL", "file:///etc/passwd"),
                             "no secret file": ("PLUMBING_CREATE_TASKS_SECRET_FILE", ""),
                             "a missing file": ("PLUMBING_CREATE_TASKS_SECRET_FILE", os.path.join(self.dir, "none")),
                             "readable by others": ("PLUMBING_CREATE_TASKS_SECRET_FILE", weak),
                             "too short": ("PLUMBING_CREATE_TASKS_SECRET_FILE", short)}.items():
            with self.subTest(name):
                env = dict(self.env, **{k: v})
                out = call.execute({"id": "case-1"}, self.tasks(), env)
                self.assertEqual(out["outcome"], "escalate", out)
        self.assertEqual(Plumbing.calls, [])

    def test_what_each_answer_reports(self):
        for name, (answer, want) in {"no ref": ((200, {}), "named nothing"), "refused": ((400, {}), "answered 400"),
                                     "a server error": ((503, {}), "answered 503")}.items():
            with self.subTest(name):
                Plumbing.answer = answer
                out = call.execute({"id": "case-1"}, self.tasks(), self.env)
                self.assertEqual(out["outcome"], "escalate", out)
                self.assertIn(want, out["detail"])

    def test_an_unreachable_plumbing_escalates_and_is_not_retried(self):
        sent = []

        def send(*a):
            sent.append(a)
            raise OSError("connection refused")
        out = call.execute({"id": "case-1"}, self.tasks(), self.env, send=send)
        self.assertEqual(out["outcome"], "escalate")
        self.assertIn("may have been delivered", out["detail"])
        self.assertEqual(len(sent), 1)

    def test_the_check_asks_for_the_same_delivery(self):
        p = self.tasks()
        chk = {"verify": "plumbing_confirmed", "action": p["action"], "params": p["params"]}
        Plumbing.answer = (200, {"done": True, "ref": "task-1"})
        self.assertEqual(call.check({"id": "case-1"}, chk, self.env)["outcome"], "verified")
        got = Plumbing.calls[-1]
        self.assertEqual(got["delivery"], "plan-" + p["plan_hash"][:16], "the send's delivery")
        self.assertEqual(got["doc"]["mode"], "status")
        for answer in ({"done": False}, {"done": True}, {"done": "yes", "ref": "t"}):
            with self.subTest(answer):
                Plumbing.answer = (200, answer)
                self.assertEqual(call.check({"id": "case-1"}, chk, self.env)["outcome"], "not_yet")
        Plumbing.answer = (503, {})
        self.assertEqual(call.check({"id": "case-1"}, chk, self.env)["outcome"], "not_yet")

    def test_the_entry_point_reports_to_the_plan_file(self):
        out = os.path.join(self.dir, "plan.json")
        p = self.tasks()
        env = dict(self.env, FLOTILLA_CASE=json.dumps({"id": "case-1"}), FLOTILLA_PLAN=json.dumps(p), FLOTILLA_PLAN_FILE=out)
        old = os.environ.copy()
        os.environ.update(env)
        try:
            self.assertEqual(call.main(["execute"]), 0)
        finally:
            os.environ.clear()
            os.environ.update(old)
        with open(out) as f:
            self.assertEqual(json.load(f)["outcome"], "executed")
        self.assertEqual(call.main(["send"]), 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
