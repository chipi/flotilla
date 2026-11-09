#!/usr/bin/env python3
"""Tests for triage_hooks.sh and its wiring in deploy.sh: the filing gate and the fleet's deploy log.

The deploy tests run deploy.sh for real against a throwaway checkout, with a stub `docker` on PATH (compose
pull/up/run and the health checks succeed) and a fake filing gate, so the gate's PASS, FAIL and accept paths,
the deploy-log rows and the checkout after a refused deploy are all exercised without a mini.
"""
import csv
import os
import shutil
import subprocess
import tempfile
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
HOOKS = os.path.join(HERE, "triage_hooks.sh")
DEPLOY = os.path.join(HERE, "deploy.sh")


def sh(cmd, cwd=None, env=None, check=True):
    r = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise AssertionError(f"{cmd}: {r.returncode}\n{r.stdout}\n{r.stderr}")
    return r


def git(repo, *args):
    return sh(["git", "-C", repo, *args]).stdout.strip()


FAKE_GATE = textwrap.dedent("""\
    #!/usr/bin/env python3
    import json, os, sys
    with open(os.environ["GATE_CALLS"], "a") as f:
        f.write(json.dumps({"argv": sys.argv[1:], "checkout": os.environ.get("SF_CHECKOUT"),
                            "accept": os.environ.get("SF_FILING_GATE_ACCEPT")}) + "\\n")
    if os.environ.get("GATE_MODE") == "fail" and not os.environ.get("SF_FILING_GATE_ACCEPT"):
        print("filing changed on 3 decisions"); print("FILING GATE: FAIL"); sys.exit(1)
    print("FILING GATE: PASS (0 changed)")
""")

FAKE_DOCKER = textwrap.dedent("""\
    #!/usr/bin/env bash
    echo "docker $* | hash_flotillad=${FLOTILLA_CONFIG_HASH_FLOTILLAD:-} hash_egress=${FLOTILLA_CONFIG_HASH_EGRESS:-}" >> "$DOCKER_CALLS"
    case "$*" in
      *" run "*) echo "CHECK PASS";;
      *"flotilla status"*) echo '{"ok":true}';;
    esac
    exit 0
""")


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.repo = os.path.join(self.tmp, "home", "repo")
        os.makedirs(os.path.join(self.repo, "signal-fleet", "mvp"))
        git(self.repo, "init", "-q")
        git(self.repo, "config", "user.email", "t@t"); git(self.repo, "config", "user.name", "t")
        os.makedirs(os.path.join(self.repo, "deploy", "mini"))
        os.makedirs(os.path.join(self.repo, "deploy", "compose"))
        shutil.copy(HOOKS, os.path.join(self.repo, "deploy", "mini", "triage_hooks.sh"))
        with open(os.path.join(self.repo, "deploy", "mini", "secrets.sh"), "w") as f:
            f.write("#!/usr/bin/env bash\necho secrets ok\n")
        open(os.path.join(self.repo, "deploy", "compose", "docker-compose.yml"), "w").close()
        self.commits = []
        for i in range(3):
            with open(os.path.join(self.repo, "signal-fleet", "mvp", "triage.py"), "w") as f:
                f.write(f"VERSION = {i}\n")
            git(self.repo, "add", "-A"); git(self.repo, "commit", "-qm", f"c{i}")
            self.commits.append(git(self.repo, "rev-parse", "HEAD"))
        git(self.repo, "remote", "add", "origin", self.repo)
        self.home = os.path.join(self.tmp, "home")
        self.log = os.path.join(self.tmp, "sf", "results", "deploys.tsv")
        self.calls = os.path.join(self.tmp, "gate_calls.jsonl")
        self.gate = os.path.join(self.tmp, "filing_gate.py")
        with open(self.gate, "w") as f:
            f.write(FAKE_GATE)
        bindir = os.path.join(self.tmp, "bin"); os.makedirs(bindir)
        with open(os.path.join(bindir, "docker"), "w") as f:
            f.write(FAKE_DOCKER)
        os.chmod(os.path.join(bindir, "docker"), 0o755)
        self.env = dict(os.environ, PATH=bindir + ":" + os.environ["PATH"], HOME=self.tmp, USER="tester",
                        GATE_CALLS=self.calls, DOCKER_CALLS=os.path.join(self.tmp, "docker_calls"),
                        FLOTILLA_HOME=self.home, VLOGS="http://127.0.0.1:9")
        self.env.pop("SF_FILING_GATE_ACCEPT", None)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def hook(self, call, env=None):
        script = f'H="{self.home}"; REPO="{self.repo}"; . "{HOOKS}"; {call}'
        return sh(["bash", "-c", script], env=env or self.env, check=False)

    def rows(self):
        with open(self.log) as f:
            return list(csv.reader(f, delimiter="\t"))

    def gate_calls(self):
        import json
        if not os.path.exists(self.calls):
            return []
        with open(self.calls) as f:
            return [json.loads(line) for line in f]


class DeployLogTest(Base):
    def test_unset_writes_nothing(self):
        r = self.hook(f"triage_deploy_log deploy {self.commits[0]} {self.commits[1]}")
        self.assertEqual(r.returncode, 0)
        self.assertIn("TRIAGE_DEPLOY_LOG unset", r.stdout)
        self.assertFalse(os.path.exists(self.log))

    def test_row_has_flotilla_in_7th_field_and_six_column_header(self):
        env = dict(self.env, TRIAGE_DEPLOY_LOG=self.log)
        self.hook(f"triage_deploy_log deploy {self.commits[0]} {self.commits[1]}", env)
        self.hook(f"triage_deploy_log rollback {self.commits[1]} {self.commits[0]}", env)
        rows = self.rows()
        self.assertEqual(rows[0], ["utc", "event", "old", "new", "mvp_tree", "by"])
        self.assertEqual(len(rows), 3)
        d, rb = rows[1], rows[2]
        self.assertEqual(len(d), 7)
        self.assertEqual(d[1:4], ["deploy", self.commits[0][:7], self.commits[1][:7]])
        self.assertEqual(d[4], git(self.repo, "rev-parse", f"{self.commits[1]}:signal-fleet/mvp"))
        self.assertEqual(d[5:], ["tester", "flotilla"])
        self.assertEqual(rb[1:4], ["rollback", self.commits[1][:7], self.commits[0][:7]])
        self.assertEqual(rb[6], "flotilla")

    def test_reader_takes_repo_by_position(self):
        # how fleet-eval-data's hostpaths.deploy_rows reads it (2026-11-08)
        env = dict(self.env, TRIAGE_DEPLOY_LOG=self.log)
        self.hook(f"triage_deploy_log deploy {self.commits[0]} {self.commits[1]}", env)
        with open(self.log) as f:
            r = list(csv.DictReader(f, delimiter="\t", restkey="_more"))[0]
        self.assertEqual((r.get("repo") or (r.get("_more") or [None])[0]), "flotilla")
        self.assertEqual(r["new"], self.commits[1][:7])

    def test_existing_header_kept(self):
        os.makedirs(os.path.dirname(self.log))
        with open(self.log, "w") as f:
            f.write("utc\tevent\told\tnew\tmvp_tree\tby\n2026-07-20T13:31:34Z\tpull\taaa\tbbb\t-\tx\n")
        env = dict(self.env, TRIAGE_DEPLOY_LOG=self.log)
        self.hook(f"triage_deploy_log deploy {self.commits[0]} {self.commits[1]}", env)
        rows = self.rows()
        self.assertEqual(len(rows), 3)
        self.assertEqual(rows[1][1], "pull")

    def test_unwritable_log_never_fails(self):
        env = dict(self.env, TRIAGE_DEPLOY_LOG="/proc/nope/deploys.tsv")
        r = self.hook(f"triage_deploy_log deploy {self.commits[0]} {self.commits[1]}", env)
        self.assertEqual(r.returncode, 0)
        self.assertIn("row not written", r.stdout)


class FilingGateTest(Base):
    def test_pass(self):
        env = dict(self.env, TRIAGE_FILING_GATE=self.gate)
        r = self.hook(f"triage_filing_gate {self.commits[0]} {self.commits[1]}", env)
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("FILING GATE: PASS", r.stdout)
        self.assertEqual(self.gate_calls(), [{"argv": [self.commits[0], self.commits[1]],
                                              "checkout": self.repo, "accept": None}])

    def test_fail(self):
        env = dict(self.env, TRIAGE_FILING_GATE=self.gate, GATE_MODE="fail")
        r = self.hook(f"triage_filing_gate {self.commits[0]} {self.commits[1]}", env)
        self.assertEqual(r.returncode, 1)
        self.assertIn("filing gate: FAIL", r.stdout)
        self.assertIn("filing changed on 3 decisions", r.stdout)

    def test_accept_reason_reaches_the_gate(self):
        env = dict(self.env, TRIAGE_FILING_GATE=self.gate, GATE_MODE="fail", SF_FILING_GATE_ACCEPT="new rollup rule")
        r = self.hook(f"triage_filing_gate {self.commits[0]} {self.commits[1]}", env)
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertEqual(self.gate_calls()[0]["accept"], "new rollup rule")

    def test_not_run_without_gate_or_previous(self):
        env = dict(self.env, TRIAGE_FILING_GATE=os.path.join(self.tmp, "absent.py"))
        r = self.hook(f"triage_filing_gate {self.commits[0]} {self.commits[1]}", env)
        self.assertEqual(r.returncode, 0); self.assertIn("NOT RUN (no ", r.stdout)
        r = self.hook(f'triage_filing_gate "" {self.commits[1]}', dict(self.env, TRIAGE_FILING_GATE=self.gate))
        self.assertEqual(r.returncode, 0); self.assertIn("NOT RUN (no earlier deploy", r.stdout)
        self.assertEqual(self.gate_calls(), [])


class DeployWiringTest(Base):
    """deploy.sh end to end with the stub docker: gate before pull, log row after up, checkout restored on refusal."""

    def setUp(self):
        super().setUp()
        shutil.copy(DEPLOY, os.path.join(self.repo, "deploy", "mini", "deploy.sh"))
        self.cfg = os.path.join(self.tmp, "config")
        os.makedirs(self.cfg)
        for name in ("flotillad.yaml", "callers.yaml", "cli.token", "worker-triage.token", "smokescreen.yaml", "egress-acl.yaml"):
            with open(os.path.join(self.cfg, name), "w") as f:
                f.write(name + " v1\n")
        with open(os.path.join(self.home, ".env"), "w") as f:
            f.write(f"TRIAGE_DEPLOY_LOG={self.log}\nTRIAGE_FILING_GATE={self.gate}\nFLOTILLA_CONFIG={self.cfg}\n")
        # one earlier passing deploy of commit 0
        with open(os.path.join(self.home, "deployed.tsv"), "w") as f:
            f.write(f"2026-11-08T18:45:35Z\tsha-{self.commits[0][:7]}\t{self.commits[0]}\tpass\n")
        git(self.repo, "checkout", "-q", "--detach", self.commits[0])

    def deploy(self, *args, env=None):
        return sh(["bash", os.path.join(self.repo, "deploy", "mini", "deploy.sh"), *args],
                  env=env or self.env, check=False)

    def test_deploy_runs_gate_then_logs(self):
        r = self.deploy(f"sha-{self.commits[1][:7]}")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("DEPLOY PASS", r.stdout)
        self.assertEqual(self.gate_calls()[0]["argv"], [self.commits[0], self.commits[1]])
        rows = self.rows()
        self.assertEqual(rows[-1][1:4], ["deploy", self.commits[0][:7], self.commits[1][:7]])
        self.assertEqual(rows[-1][6], "flotilla")

    def test_gate_failure_refuses_and_restores_checkout(self):
        r = self.deploy(f"sha-{self.commits[1][:7]}", env=dict(self.env, GATE_MODE="fail"))
        self.assertEqual(r.returncode, 1)
        self.assertIn("DEPLOY FAIL: filing gate failed", r.stdout)
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), self.commits[0])
        self.assertFalse(os.path.exists(self.log), "no deploy-log row for a refused deploy")
        p = os.path.join(self.tmp, "docker_calls")
        calls = open(p).read() if os.path.exists(p) else ""
        self.assertNotIn(" pull ", calls)
        self.assertNotIn(" up ", calls)

    def test_gate_failure_accepted_deploys(self):
        r = self.deploy(f"sha-{self.commits[1][:7]}",
                        env=dict(self.env, GATE_MODE="fail", SF_FILING_GATE_ACCEPT="intended"))
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertEqual(self.rows()[-1][1], "deploy")

    def hashes(self):
        p = os.path.join(self.tmp, "docker_calls")
        ups = [l for l in open(p).read().splitlines() if " up -d " in l]
        return ups[-1].split("| ")[1]

    def test_a_changed_config_changes_the_service_hash(self):
        """compose recreates a container only when its definition changes: the config's hash is in it."""
        self.assertEqual(self.deploy(f"sha-{self.commits[1][:7]}").returncode, 0)
        first = self.hashes()
        self.assertRegex(first, r"hash_flotillad=[0-9a-f]{16} hash_egress=[0-9a-f]{16}")
        self.assertEqual(self.deploy(f"sha-{self.commits[1][:7]}").returncode, 0)
        self.assertEqual(self.hashes(), first, "same config, same hashes: nothing is recreated")
        with open(os.path.join(self.cfg, "flotillad.yaml"), "a") as f:
            f.write("models: {}\n")
        self.assertEqual(self.deploy(f"sha-{self.commits[1][:7]}").returncode, 0)
        second = self.hashes()
        self.assertNotEqual(second.split()[0], first.split()[0], "flotillad.yaml changed: flotillad's hash too")
        self.assertEqual(second.split()[1], first.split()[1], "the proxy's config did not change")
        with open(os.path.join(self.cfg, "egress-acl.yaml"), "a") as f:
            f.write("# allow one more\n")
        self.assertEqual(self.deploy(f"sha-{self.commits[1][:7]}").returncode, 0)
        self.assertNotEqual(self.hashes().split()[1], second.split()[1], "the ACL changed: the proxy's hash too")

    def test_rollback_skips_gate_and_logs_rollback(self):
        self.assertEqual(self.deploy(f"sha-{self.commits[1][:7]}").returncode, 0)
        n = len(self.gate_calls())
        r = self.deploy("--rollback", env=dict(self.env, GATE_MODE="fail"))
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertEqual(len(self.gate_calls()), n, "a rollback never runs the gate")
        self.assertEqual(self.rows()[-1][1:4], ["rollback", self.commits[1][:7], self.commits[0][:7]])


if __name__ == "__main__":
    unittest.main(verbosity=2)
