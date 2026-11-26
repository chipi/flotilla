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

# The stub prints the rows `gh api --jq` would print and records its arguments: the commit's own check runs are $STUB_RUNS, an
# ancestor's are $STUB_RUNS_<sha>, and the commit list (commits?sha=) is $STUB_ANCESTORS, newest first, the commit itself first.
STUB = textwrap.dedent("""\
    #!/usr/bin/env bash
    echo "$@" >> "$STUB_ARGS"
    [ -z "${STUB_FAIL:-}" ] || { echo "HTTP 404: Not Found"; exit 1; }
    case "$*" in
      *"commits?sha="*) [ -z "${STUB_LIST_FAIL:-}" ] || { echo "HTTP 500"; exit 1; }; printf '%b' "${STUB_ANCESTORS:-}" ;;
      *commits/6230cda/check-runs*) printf '%b' "${STUB_RUNS:-}" ;;
      *) a="$*"; sha="${a#*commits/}"; sha="${sha%%/*}"; v="STUB_RUNS_$sha"; printf '%b' "${!v:-}" ;;
    esac
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

    def run_script(self, args, runs="", fail=False, ancestors=None, list_fail=False):
        env = dict(os.environ, PATH=self.tmp.name + ":" + os.environ["PATH"], STUB_ARGS=self.args, STUB_RUNS=runs)
        if fail:
            env["STUB_FAIL"] = "1"
        if list_fail:
            env["STUB_LIST_FAIL"] = "1"
        for sha, rows in (ancestors or []):
            env["STUB_ANCESTORS"] = env.get("STUB_ANCESTORS", "6230cda\\n") + sha + "\\n"
            if rows:
                env["STUB_RUNS_" + sha] = rows
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

    def test_an_absent_optional_check_is_judged_by_the_nearest_ancestor_that_ran_it(self):
        """A path-filtered workflow (no-effect runs only for the code it guards) skips most commits: a later commit it skipped must not
        hide an earlier one it failed, so a commit with no run takes the nearest ancestor's run that has one."""
        green = row(1, "ci-fast", "completed", "success")
        red_before = [("aaaaaaa", ""), ("bbbbbbb", row(5, "no-effect", "completed", "failure")), ("ccccccc", row(4, "no-effect", "completed", "success"))]
        r = self.run_script(["6230cda", "ci-fast", "no-effect?"], green, ancestors=red_before)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("required no-effect: no run for this commit; bbbbbbb (the nearest ancestor with one): completed/failure", r.stdout)
        green_before = [("aaaaaaa", ""), ("bbbbbbb", row(5, "no-effect", "completed", "success")), ("ccccccc", row(4, "no-effect", "completed", "failure"))]
        r = self.run_script(["6230cda", "ci-fast", "no-effect?"], green, ancestors=green_before)
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("bbbbbbb (the nearest ancestor with one): ok", r.stdout)
        running_before = [("bbbbbbb", row(5, "no-effect", "in_progress", "-"))]
        self.assertEqual(self.run_script(["6230cda", "ci-fast", "no-effect?"], green, ancestors=running_before).returncode, 1)

    def test_no_ancestor_ran_it_is_ok_and_an_unreadable_history_is_red(self):
        green = row(1, "ci-fast", "completed", "success")
        r = self.run_script(["6230cda", "ci-fast", "no-effect?"], green, ancestors=[("aaaaaaa", ""), ("bbbbbbb", "")])
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("no run for this commit or its last", r.stdout)
        r = self.run_script(["6230cda", "ci-fast", "no-effect?"], green, list_fail=True)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("could not read the commits before", r.stdout)

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


class NoEffectScope(unittest.TestCase):
    """no-effect runs only for the code it guards (ADR-0022): push and PR guard the same paths, and every file a no-effect mutant alters
    runs the mutants when it changes, or a change to it would skip the very checks that test it."""

    WF = os.path.join(ROOT, ".github", "workflows", "no-effect.yml")

    def paths(self, event):
        src = open(self.WF).read()
        block = src.split("\n  pull_request:\n", 1)[1].split("\n  workflow_dispatch:", 1)[0] if event == "pull_request" else src.split("\n  push:\n", 1)[1].split("\n  pull_request:", 1)[0]
        return re.findall(r"^      - '([^']+)'$", block, re.M)

    def test_push_and_pull_request_guard_the_same_paths(self):
        push = self.paths("push")
        self.assertIn("internal/sched/**", push)
        self.assertEqual(push, self.paths("pull_request"))

    def test_every_file_a_mutant_alters_runs_the_mutants(self):
        rx = re.compile("|".join(x for x in open(os.path.join(ROOT, "deploy", "e2e", "mutant-paths.txt")).read().split("\n") if x and not x.startswith("#")))
        run = open(os.path.join(ROOT, "deploy", "e2e", "run.sh")).read()
        altered = set(re.findall(r'"\$BUILD_CTX/([^"]+)"', run))
        self.assertTrue({"internal/sched/shadow.go", "runtime/harness.py"} <= altered, altered)
        mutants = re.findall(r"mutant\.sh (\S+) ", open(self.WF).read())
        self.assertEqual(len(mutants), 5, mutants)
        for name, path in (("shadow-live-handler", "internal/sched/shadow.go"), ("shadow-reads-live", "runtime/harness.py")):
            self.assertIn(name, mutants)
            self.assertTrue(rx.search(path), f"{path} ({name} alters it) does not run the mutants")
        for path in ("deploy/compose/docker-compose.yml", "deploy/e2e/matrix.py", "deploy/e2e/docker-compose.mutant-live-write.yml", ".github/workflows/no-effect.yml"):
            self.assertTrue(rx.search(path), path)
        for path in ("internal/ui/decisions.go", "deploy/e2e/run.sh", "knowledge/answer.py"):
            self.assertFalse(rx.search(path), path)


if __name__ == "__main__":
    unittest.main()
