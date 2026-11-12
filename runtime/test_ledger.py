"""python3 runtime/test_ledger.py — the ledger client against a stand-in transport; the client against a real
flotillad is signal-fleet/mvp/gate_ledger_backend_replay.py (the e2e runs it)."""
import io
import json
import os
import sys
import tempfile
import unittest
import urllib.error
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ledger  # noqa: E402


class Resp(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class Opener:
    def __init__(self, *answers):
        self.answers, self.seen = list(answers), []

    def __call__(self, req, timeout=None):
        self.seen.append((req.get_method(), req.full_url, req.get_header("Authorization"), req.data))
        a = self.answers.pop(0)
        if isinstance(a, Exception):
            raise a
        return Resp(json.dumps(a).encode())


class Client(unittest.TestCase):
    def client(self, *answers, record=None):
        op = Opener(*answers)
        return ledger.Client("http://flotillad:8485/", "tok", "triage/cycle", "run-1", record=record, opener=op), op

    def test_calls(self):
        c, op = self.client({"filings": [{"seq": 1, "row": {"fingerprint": "a"}}]}, {"seq": 7}, {"seq": 8})
        self.assertEqual(c.filings({"fingerprint": "a", "norm_key": ""}), [{"fingerprint": "a"}])
        self.assertEqual(c.write_filing({"fingerprint": "a"}), 7)
        self.assertEqual(c.append_entry("decision", {"occurrence_id": "o"}), 8)
        method, url, auth, _ = op.seen[0]
        q = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
        self.assertEqual((method, auth, q), ("GET", "Bearer tok", {"fingerprint": ["a"], "ns": ["triage/cycle"]}),
                         "empty keys are not sent; the namespace always is")
        self.assertEqual(json.loads(op.seen[1][3]), {"row": {"fingerprint": "a"}, "run_id": "run-1"})

    def test_no_keys_means_none_not_all(self):
        c, op = self.client()
        self.assertEqual(c.filings({"group_key": ""}), [], "keys that are all empty match nothing, with no call")
        self.assertEqual(op.seen, [])

    def test_failures_are_ledger_unavailable_and_recorded(self):
        rec = []
        c, _ = self.client(urllib.error.HTTPError("u", 409, "x", {}, io.BytesIO(b'{"error":"once"}')),
                           ConnectionRefusedError("refused"), record=lambda k, v: rec.append((k, v)))
        with self.assertRaisesRegex(ledger.LedgerUnavailable, "HTTP 409"):
            c.import_ledger([], [])
        with self.assertRaisesRegex(ledger.LedgerUnavailable, "ConnectionRefusedError"):
            c.counts()
        self.assertEqual([k for k, _ in rec], ["ledger", "ledger"], "ledger calls go to their own list")
        self.assertIn("error", rec[0][1])

    def test_from_env(self):
        with tempfile.NamedTemporaryFile("w", delete=False) as f:
            f.write("tok\n")
        try:
            c = ledger.from_env({"FLOTILLA_API_URL": "http://x", "FLOTILLA_API_TOKEN_FILE": f.name, "FLOTILLA_ROUTINE": "t/c"})
            self.assertEqual((c.token, c.ns), ("tok", "t/c"))
            with self.assertRaises(ledger.LedgerUnavailable):
                ledger.from_env({"FLOTILLA_API_URL": "http://x", "FLOTILLA_API_TOKEN_FILE": f.name})
            with self.assertRaises(ledger.LedgerUnavailable):
                ledger.from_env({"FLOTILLA_API_URL": "http://x", "FLOTILLA_API_TOKEN_FILE": "/nope", "FLOTILLA_ROUTINE": "t"})
        finally:
            os.unlink(f.name)


if __name__ == "__main__":
    unittest.main(verbosity=1)
