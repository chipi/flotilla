#!/usr/bin/env python3
"""Tests for checks_green.sh against a stub `gh` on PATH: green, red, missing, pending, cancelled,
a re-run that replaced a failure, several required names, an API error and a bad argument."""
import os
import re
import subprocess
import tempfile
import textwrap
import unittest

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "checks_green.sh")
ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..")

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

    def test_optional_name_absent_is_ok_present_must_pass(self):
        green = row(1, "ci-fast", "completed", "success")
        r = self.run_script(["6230cda", "ci-fast", "e2e?"], green)
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("required e2e: no run for this commit", r.stdout)
        for status, concl in (("completed", "failure"), ("in_progress", "-"), ("queued", "-"),
                              ("completed", "cancelled")):
            r = self.run_script(["6230cda", "ci-fast", "e2e?"], green + row(2, "e2e", status, concl))
            self.assertEqual(r.returncode, 1, (status, concl))
            self.assertIn(f"required e2e: {status}/{concl}", r.stdout)
        r = self.run_script(["6230cda", "ci-fast", "e2e?"], green + row(2, "e2e", "completed", "success"))
        self.assertEqual(r.returncode, 0)
        self.assertIn("required e2e: ok", r.stdout)
        self.assertEqual(self.run_script(["6230cda", "e2e?"], "").returncode, 0)
        self.assertEqual(self.run_script(["6230cda", "ci-fast?", "e2e"], green).returncode, 1)

    def test_api_error_is_red(self):
        r = self.run_script(["6230cda"], fail=True)
        self.assertEqual(r.returncode, 1)
        self.assertIn("could not read the check runs", r.stdout)

    def test_bad_argument(self):
        for a in ([], ["main"], ["xyz1234"], ["abc"]):
            self.assertEqual(self.run_script(a).returncode, 2, a)


def workflow_job_names():
    """The check-run names the workflows produce: each job's name: when it has one, else its id (stdlib only: no YAML parser)."""
    names = set()
    wf = os.path.join(ROOT, ".github", "workflows")
    for fn in sorted(os.listdir(wf)):
        if not fn.endswith((".yml", ".yaml")):
            continue
        in_jobs, job = False, None
        for line in open(os.path.join(wf, fn)).read().split("\n"):
            if re.match(r"^jobs:\s*$", line):
                in_jobs = True
            elif in_jobs and re.match(r"^\S", line):
                in_jobs = False
            elif in_jobs and (m := re.match(r"^  ([A-Za-z0-9_-]+):\s*$", line)):
                job = m.group(1)
                names.add(job)
            elif in_jobs and job and (m := re.match(r"^    name:\s*(.+?)\s*$", line)):
                names.discard(job)
                names.add(m.group(1).strip("\"'"))
                job = None
    return names


def required_checks():
    """The names deploy.sh passes to checks_green.sh for a deploy, with their ? removed."""
    src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "deploy.sh")).read()
    m = re.search(r'checks_green\.sh" "\$COMMIT"((?: [^ |]+)+) \|', src)
    assert m, "deploy.sh no longer calls checks_green.sh the way this test reads it"
    return [x.strip("'").rstrip("?") for x in m.group(1).split()]


class RequiredNamesAreJobs(unittest.TestCase):
    """The required list is code (GitHub has no branch protection): a name that is no workflow's job name would be required of a check that never
    comes, or, with its ? (a docs-only commit has none), would silently require nothing."""

    def test_a_deploy_requires_ci_fast_e2e_and_no_effect(self):
        self.assertEqual(required_checks(), ["ci-fast", "e2e", "no-effect"])

    def test_every_required_name_is_a_job_of_a_workflow(self):
        jobs = workflow_job_names()
        for name in required_checks():
            self.assertIn(name, jobs, f"{name} is required by deploy.sh but no workflow has a job of that name (jobs: {sorted(jobs)})")


if __name__ == "__main__":
    unittest.main()
