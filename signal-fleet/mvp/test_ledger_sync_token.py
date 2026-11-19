"""python3 test_ledger_sync_token.py: ledger_sync needs an operator's token under run_tokens: required."""
import io
import os
import sys
import unittest
from contextlib import redirect_stdout
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
os.environ.setdefault("SF_OBSERV_DISABLED", "1")
import ledger_sync  # noqa: E402


class Token(unittest.TestCase):
    def run_main(self, argv, env):
        out = io.StringIO()
        with mock.patch.dict(os.environ, env, clear=False), redirect_stdout(out):
            os.environ.pop("FLOTILLA_API_TOKEN_FILE", None) if "FLOTILLA_API_TOKEN_FILE" not in env else None
            rc = ledger_sync.main(argv)
        return rc, out.getvalue()

    def test_import_and_check_without_a_token_file_say_what_is_missing(self):
        for verb in ("import", "check"):
            rc, out = self.run_main([verb], {})
            self.assertEqual(rc, 2, verb)
            self.assertIn("operator token required (run_tokens: required)", out)
            self.assertIn("--token-file", out)

    def test_token_file_option_is_taken_before_the_verb_runs(self):
        argv, err = ledger_sync.take_token_file(["import", "--token-file", "/x/op.token"], {})
        self.assertEqual((argv, err), (["import"], ""))
        env = {}
        ledger_sync.take_token_file(["--token-file", "/x/op.token", "check"], env)
        self.assertEqual(env["FLOTILLA_API_TOKEN_FILE"], "/x/op.token")
        self.assertEqual(ledger_sync.take_token_file(["check", "--token-file"], {})[0], None)

    def test_pad_needs_no_token(self):
        rc, out = self.run_main(["pad"], {})
        self.assertNotIn("operator token required", out)


if __name__ == "__main__":
    unittest.main()
