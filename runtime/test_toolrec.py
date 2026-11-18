"""python3 runtime/test_toolrec.py: a tool call as the calls file records it (runtime/toolrec.py)."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import toolrec  # noqa: E402

KEY = "sk-" + "a" * 24
GH = "ghp_" + "b" * 30
JWT = ".".join(["eyJhbGciOiJIUzI1NiJ9", "eyJzdWIiOiIxMjM0NSJ9", "c2lnbmF0dXJl"])


class Redact(unittest.TestCase):
    def test_keys_tokens_and_credentials_are_masked(self):
        for text, gone in [(f"fetch -H 'Authorization: Bearer abcdefghij1234567890' x", "abcdefghij1234567890"),
                           (f"echo {KEY}", KEY), (f"git push https://{GH}@github.com/o/r", GH), (f"jwt {JWT}", JWT),
                           ("psql postgres://user:hunter2@db/x", "hunter2"), ("API_KEY=topsecret ./run", "topsecret"),
                           ("DB_PASSWORD='p w' ./run", "p w"), ("run --token abc123def", "abc123def"),
                           ('{"api_key": "abc123def", "n": 1}', "abc123def"), ("mail ada@example.com", "ada@example.com"),
                           ("ssh root@10.1.2.3", "10.1.2.3"), ("fetch -u user:pw -H 'authorization: Basic dXNlcjpwdw=='", "dXNlcjpwdw==")]:
            out = toolrec.redact(text)
            self.assertNotIn(gone, out, text)
            self.assertIn("<", out, text)

    def test_what_is_not_secret_stays(self):
        for text in ["grep -n foo src/x.ts", "npm test -- --run src/a.test.ts", "git log -1 --format=%H", "ls -la /tmp",
                     "node --version 22.1.0", "curl -A Chrome/141.0.0.0 https://example.com/a?b=c"]:
            self.assertEqual(toolrec.redact(text), text, text)

    def test_idempotent(self):
        once = toolrec.redact(f"API_KEY=x {KEY} 10.0.0.1 a@b.example")
        self.assertEqual(toolrec.redact(once), once)


class RedactCredentials(unittest.TestCase):
    def test_credentials_are_masked_but_not_addresses(self):
        for text, gone in [(f"echo {KEY}", KEY), (f"git push https://{GH}@github.com/o/r", GH), (f"jwt {JWT}", JWT),
                           ("psql postgres://user:hunter2@db/x", "hunter2"), ("API_KEY=topsecret ./run", "topsecret"),
                           ('{"api_key": "abc123def", "n": 1}', "abc123def"), ("Authorization: Bearer abcdefghij1234567890", "abcdefghij1234567890")]:
            self.assertNotIn(gone, toolrec.redact_credentials(text), text)
        for text in ["mail ada@example.com from 10.1.2.3", "service logs: 5xx rate 0.2, host 192.168.1.4", "plain"]:
            self.assertEqual(toolrec.redact_credentials(text), text, text)
        self.assertEqual(toolrec.redact_credentials(f"a {KEY} b"), "a <redacted> b")

    def test_a_long_dashed_string_is_not_quadratic(self):
        import time
        for text in ("a-" * 25000, "a" * 50000, "key" * 17000):
            t = time.time()
            toolrec.redact_credentials(text)
            self.assertLess(time.time() - t, 0.1, text[:6])

    def test_it_is_redact_without_the_last_two_rules(self):
        self.assertEqual(toolrec.redact(f"{KEY} a@b.example 10.0.0.1"), "<redacted> <email> <ip>")


class Clean(unittest.TestCase):
    def test_one_line_cut_at_160_after_redaction(self):
        self.assertEqual(toolrec.clean_arg("ls\n  -la\t/tmp"), "ls -la /tmp")
        out = toolrec.clean_arg("x" * 150 + " " + KEY)
        self.assertLessEqual(len(out), toolrec.ARG_MAX)
        self.assertNotIn("sk-", out)
        long = toolrec.clean_arg("y" * 500)
        self.assertEqual((len(long), long[-1]), (160, "…"))
        self.assertEqual(toolrec.clean_arg("z" * 160), "z" * 160)

    def test_primary_arg(self):
        self.assertEqual(toolrec.primary_arg({"path": "p", "command": "c"}), "c")
        self.assertEqual(toolrec.primary_arg({"path": "src", "pattern": "needle"}), "needle")
        self.assertEqual(toolrec.primary_arg({"x": "", "y": "first", "z": "second"}), "first")
        self.assertEqual(toolrec.primary_arg({"n": 5}), "5")
        self.assertEqual([toolrec.primary_arg(a) for a in ({}, None, "s", {"b": True}, {"l": [1]})], [""] * 5)


class Entry(unittest.TestCase):
    def test_entry(self):
        self.assertEqual(toolrec.entry("bash", "ls", True), {"name": "bash", "arg": "ls", "ok": True})
        self.assertEqual(toolrec.entry("bash", "ls", False)["ok"], False)
        self.assertEqual(toolrec.entry("bash"), {"name": "bash", "arg": ""})
        self.assertNotIn("ok", toolrec.entry("bash", "ls", "yes"))
        self.assertEqual(toolrec.entry("bash", f"echo {KEY}")["arg"], "echo <redacted>")


if __name__ == "__main__":
    unittest.main()
