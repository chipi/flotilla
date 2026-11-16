#!/usr/bin/env python3
"""Tests for checks_green.sh against a stub `gh` on PATH: green, red, missing, pending, cancelled,
a re-run that replaced a failure, several required names, an API error and a bad argument."""
import os
import subprocess
import tempfile
import textwrap
import unittest

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "checks_green.sh")

# The stub prints $STUB_RUNS (the rows `gh api --jq` would print) and records its arguments.
STUB = textwrap.dedent("""\
    #!/usr/bin/env bash
    echo "$@" > "$STUB_ARGS"
    [ -z "${STUB_FAIL:-}" ] || { echo "HTTP 404: Not Found"; exit 1; }
    printf '%b' "${STUB_RUNS:-}"
    """)


def row(id_, name, status, conclusion):
    return f"{id_}\t{name}\t{status}\t{conclusion}\\n"


class ChecksGreen(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        path = os.path.join(self.tmp.name, "gh")
        with open(path, "w") as f:
            f.write(STUB)
        os.chmod(path, 0o755)
        self.args = os.path.join(self.tmp.name, "args")

    def run_script(self, args, runs="", fail=False):
        env = dict(os.environ, PATH=self.tmp.name + ":" + os.environ["PATH"], STUB_ARGS=self.args, STUB_RUNS=runs)
        if fail:
            env["STUB_FAIL"] = "1"
        return subprocess.run(["bash", SCRIPT, *args], capture_output=True, text=True, env=env)

    def test_green(self):
        r = self.run_script(["6230cda"], row(1, "ci-fast", "completed", "success") + row(2, "images / images", "completed", "success"))
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("checks_green: GREEN", r.stdout)
        self.assertIn("ci-fast", r.stdout)
        with open(self.args) as f:
            self.assertIn("repos/chipi/flotilla/commits/6230cda/check-runs", f.read())

    def test_tag_form_is_accepted(self):
        r = self.run_script(["sha-6230cda"], row(1, "ci-fast", "completed", "success"))
        self.assertEqual(r.returncode, 0, r.stdout)
        with open(self.args) as f:
            self.assertIn("commits/6230cda/check-runs", f.read())

    def test_failed_is_red(self):
        r = self.run_script(["6230cda"], row(1, "ci-fast", "completed", "failure"))
        self.assertEqual(r.returncode, 1)
        self.assertIn("required ci-fast: completed/failure", r.stdout)
        self.assertIn("checks_green: RED", r.stdout)

    def test_cancelled_is_red(self):
        r = self.run_script(["6230cda"], row(1, "ci-fast", "completed", "cancelled"))
        self.assertEqual(r.returncode, 1)

    def test_pending_is_red(self):
        r = self.run_script(["6230cda"], row(1, "ci-fast", "in_progress", "-"))
        self.assertEqual(r.returncode, 1)
        self.assertIn("in_progress/-", r.stdout)

    def test_missing_check_is_red(self):
        r = self.run_script(["6230cda"], row(1, "docs / Build (strict)", "completed", "success"))
        self.assertEqual(r.returncode, 1)
        self.assertIn("required ci-fast: missing", r.stdout)

    def test_no_check_runs_is_red(self):
        r = self.run_script(["6230cda"], "")
        self.assertEqual(r.returncode, 1)
        self.assertIn("(none)", r.stdout)

    def test_a_rerun_replaces_the_failure_before_it(self):
        r = self.run_script(["6230cda"], row(1, "ci-fast", "completed", "failure") + row(9, "ci-fast", "completed", "success"))
        self.assertEqual(r.returncode, 0, r.stdout)

    def test_a_failed_rerun_replaces_the_success_before_it(self):
        r = self.run_script(["6230cda"], row(1, "ci-fast", "completed", "success") + row(9, "ci-fast", "completed", "failure"))
        self.assertEqual(r.returncode, 1)

    def test_every_named_check_must_pass(self):
        runs = row(1, "ci-fast", "completed", "success") + row(2, "e2e", "completed", "failure")
        self.assertEqual(self.run_script(["6230cda", "ci-fast"], runs).returncode, 0)
        self.assertEqual(self.run_script(["6230cda", "ci-fast", "e2e"], runs).returncode, 1)

    def test_api_error_is_red(self):
        r = self.run_script(["6230cda"], fail=True)
        self.assertEqual(r.returncode, 1)
        self.assertIn("could not read the check runs", r.stdout)

    def test_bad_argument(self):
        for a in ([], ["main"], ["xyz1234"], ["abc"]):
            self.assertEqual(self.run_script(a).returncode, 2, a)


if __name__ == "__main__":
    unittest.main()
