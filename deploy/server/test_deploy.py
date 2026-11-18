#!/usr/bin/env python3
"""Tests for deploy.sh: the commit's checks, an installation's deploy hooks (FLOTILLA_DEPLOY_HOOKS: its gate
before the pull, its log after the up), the config hashes and a rollback.

deploy.sh runs for real against a throwaway checkout, with a stub `docker` on PATH and a stand-in hooks file
(the gate passes or fails on GATE_MODE, and both hooks record their calls), so a refused deploy and the
checkout it leaves are exercised without a host. The stub is a small engine: compose commands, the health
checks, and `docker run` of deploy/server/store_snapshot.py (the real helper, on real SQLite files standing in
for the state volume); test_deploy_store.py builds on it.
"""
import csv
import os
import shutil
import subprocess
import tempfile
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
DEPLOY = os.path.join(HERE, "deploy.sh")
HELPER = os.path.join(HERE, "store_snapshot.py")


def sh(cmd, cwd=None, env=None, check=True):
    r = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise AssertionError(f"{cmd}: {r.returncode}\n{r.stdout}\n{r.stderr}")
    return r


def git(repo, *args):
    return sh(["git", "-C", repo, *args]).stdout.strip()


STANDIN_HOOKS = textwrap.dedent("""\
    # a stand-in for an installation's hooks: what deploy.sh gives them (REPO, H, envget) is used
    deploy_gate() {
      printf '%s|%s|%s\\n' "$1" "$2" "$(envget FLOTILLA_CONFIG)" >> "$GATE_CALLS"
      if [ "${GATE_MODE:-}" = fail ]; then echo "stand-in gate: FAIL"; return 1; fi
      echo "stand-in gate: PASS"
    }
    deploy_log() {
      printf '%s\\t%s\\t%s\\n' "$1" "$(git -C "$REPO" rev-parse --short "$2" 2>/dev/null || echo -)" \\
        "$(git -C "$REPO" rev-parse --short "$3")" >> "$HOOK_LOG"
    }
""")

FAKE_DOCKER = textwrap.dedent("""\
    #!/usr/bin/env bash
    # A stub engine. STUB_CFG holds its state: running (the tag compose last brought up), and per tag
    # <tag>.max (the newest store schema that binary accepts: an older binary on a newer store fails its
    # -check, unless <tag>.nocheck, and is unhealthy at up), <tag>.migrate (N: the binary moves the store to
    # schema N at start), <tag>.unhealthy. STUB_VOLS/<volume> is a volume's folder.
    live_schema() { STORE_DIR="$STUB_VOLS/flotilla-state" python3 "$HELPER_PATH" schema; }
    tagfile() { echo "$STUB_CFG/$1.$2"; }
    refuses() { local m; m="$(cat "$(tagfile "$1" max)" 2>/dev/null)"; [ -n "$m" ] && [ "$(live_schema)" -gt "$m" ]; }
    if [ "$1" = run ]; then   # deploy.sh's store_run: docker run ... -v VOL:/v --entrypoint python3 IMG -c SRC MODE
      args=("$@"); vol=; mode=; src=
      for ((i=0; i<${#args[@]}; i++)); do
        case "${args[i]}" in
          -v) vol="${args[i+1]%%:*}";;
          -c) src="${args[i+1]}"; mode="${args[i+2]}"; break;;
        esac
      done
      echo "helper $mode" >> "$DOCKER_CALLS"
      [ "$mode" = backup ] && [ -n "${STUB_FAIL_BACKUP:-}" ] && exit 1
      printf '%s\\n' "$src" > "$STUB_CFG/helper.py"
      STORE_DIR="$STUB_VOLS/$vol" python3 "$STUB_CFG/helper.py" "$mode"
      exit $?
    fi
    echo "docker $* | hash_flotillad=${FLOTILLA_CONFIG_HASH_FLOTILLAD:-} hash_egress=${FLOTILLA_CONFIG_HASH_EGRESS:-}" >> "$DOCKER_CALLS"
    case "$1" in
      compose)
        sub=; args=("$@")
        for ((i=0; i<${#args[@]}; i++)); do [ "${args[i]}" = -f ] && { sub="${args[i+2]}"; break; }; done
        tag="$FLOTILLA_TAG"
        case "$sub" in
          run)
            if [ ! -f "$(tagfile "$tag" nocheck)" ] && refuses "$tag"; then
              echo "store: database schema $(live_schema) is newer than this flotillad ($(cat "$(tagfile "$tag" max)")): refusing to run an older binary on it" >&2
              exit 1
            fi
            echo "CHECK PASS";;
          stop) : > "$STUB_CFG/running";;
          up)
            echo "$tag" > "$STUB_CFG/running"
            if [ -f "$(tagfile "$tag" migrate)" ] && ! refuses "$tag"; then
              python3 "$STUB_CFG/migrate.py" "$STUB_VOLS/flotilla-state/flotilla.db" "$(cat "$(tagfile "$tag" migrate)")"
            fi;;
        esac
        exit 0;;
      exec)
        t="$(cat "$STUB_CFG/running" 2>/dev/null)"
        healthy() { [ -n "$t" ] && [ ! -f "$(tagfile "$t" unhealthy)" ] && ! refuses "$t"; }
        case "$*" in
          *"flotilla status"*) if healthy; then echo '{"ok":true}'; else echo '{"ok":false}'; fi; exit 0;;
          *health*) healthy; exit $?;;
        esac;;
    esac
    exit 0
""")

MIGRATE_PY = textwrap.dedent("""\
    import sqlite3, sys
    db = sqlite3.connect(sys.argv[1])
    db.execute("INSERT INTO schema_version (n) VALUES (?)", (int(sys.argv[2]),))
    db.execute("INSERT INTO t (v) VALUES ('after-migration')")
    db.commit()
""")


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.repo = os.path.join(self.tmp, "home", "repo")
        os.makedirs(os.path.join(self.repo, "signal-fleet", "mvp"))
        git(self.repo, "init", "-q")
        git(self.repo, "config", "user.email", "t@t"); git(self.repo, "config", "user.name", "t")
        os.makedirs(os.path.join(self.repo, "deploy", "server"))
        os.makedirs(os.path.join(self.repo, "deploy", "compose"))
        with open(os.path.join(self.repo, "deploy", "server", "secrets.sh"), "w") as f:
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
        self.log = os.path.join(self.tmp, "hook_log.tsv")
        self.calls = os.path.join(self.tmp, "gate_calls")
        self.hooks = os.path.join(self.tmp, "hooks.sh")
        with open(self.hooks, "w") as f:
            f.write(STANDIN_HOOKS)
        bindir = os.path.join(self.tmp, "bin"); os.makedirs(bindir)
        with open(os.path.join(bindir, "docker"), "w") as f:
            f.write(FAKE_DOCKER)
        os.chmod(os.path.join(bindir, "docker"), 0o755)
        # a stub gh: the commit's check runs as checks_green.sh reads them (STUB_CHECKS, default ci-fast green)
        with open(os.path.join(bindir, "gh"), "w") as f:
            f.write('#!/bin/bash\nprintf "%b\\n" "${STUB_CHECKS:-1\\tci-fast\\tcompleted\\tsuccess}"\n')
        os.chmod(os.path.join(bindir, "gh"), 0o755)
        shutil.copy(HELPER, os.path.join(self.repo, "deploy", "server", "store_snapshot.py"))
        shutil.copy(os.path.join(HERE, "checks_green.sh"), os.path.join(self.repo, "deploy", "server", "checks_green.sh"))
        self.stub = os.path.join(self.tmp, "stub"); self.vols = os.path.join(self.stub, "vols")
        os.makedirs(self.vols)
        with open(os.path.join(self.stub, "migrate.py"), "w") as f:
            f.write(MIGRATE_PY)
        self.env = dict(os.environ, PATH=bindir + ":" + os.environ["PATH"], HOME=self.tmp, USER="tester",
                        GATE_CALLS=self.calls, HOOK_LOG=self.log, DOCKER_CALLS=os.path.join(self.tmp, "docker_calls"),
                        FLOTILLA_HOME=self.home, VLOGS="http://127.0.0.1:9", STUB_CFG=self.stub, STUB_VOLS=self.vols,
                        HELPER_PATH=HELPER, FLOTILLA_HEALTH_TRIES="2", FLOTILLA_HEALTH_SLEEP="0")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def rows(self):
        with open(self.log) as f:
            return list(csv.reader(f, delimiter="\t"))

    def gate_calls(self):
        """Each call of the stand-in gate: [PREV, NOW, the FLOTILLA_CONFIG it read with envget]."""
        if not os.path.exists(self.calls):
            return []
        with open(self.calls) as f:
            return [line.rstrip("\n").split("|") for line in f]


class DeployBase(Base):
    """A host with one earlier passing deploy (commit 0) and a config folder, for deploy.sh run with the stub docker."""

    def setUp(self):
        super().setUp()
        shutil.copy(DEPLOY, os.path.join(self.repo, "deploy", "server", "deploy.sh"))
        self.cfg = os.path.join(self.tmp, "config")
        os.makedirs(self.cfg)
        for name in ("flotillad.yaml", "callers.yaml", "cli.token", "worker-triage.token", "smokescreen.yaml", "egress-acl.yaml"):
            with open(os.path.join(self.cfg, name), "w") as f:
                f.write(name + " v1\n")
        with open(os.path.join(self.home, ".env"), "w") as f:
            f.write(f"FLOTILLA_DEPLOY_HOOKS={self.hooks}\nFLOTILLA_CONFIG={self.cfg}\n")
        # one earlier passing deploy of commit 0
        with open(os.path.join(self.home, "deployed.tsv"), "w") as f:
            f.write(f"2026-11-08T18:45:35Z\tsha-{self.commits[0][:7]}\t{self.commits[0]}\tpass\n")
        git(self.repo, "checkout", "-q", "--detach", self.commits[0])

    def deploy(self, *args, env=None):
        return sh(["bash", os.path.join(self.repo, "deploy", "server", "deploy.sh"), *args],
                  env=env or self.env, check=False)


class DeployWiringTest(DeployBase):
    """deploy.sh end to end with the stub docker: the installation's gate before the pull, its log after the up,
    the checkout restored on a refusal; and a host without hooks says it deploys with no gate."""

    def test_deploy_runs_gate_then_logs(self):
        r = self.deploy(f"sha-{self.commits[1][:7]}")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("DEPLOY PASS", r.stdout)
        self.assertIn("stand-in gate: PASS", r.stdout)
        self.assertEqual(self.gate_calls(), [[self.commits[0], self.commits[1], self.cfg]], "REPO, H and envget reach the hooks")
        self.assertEqual(self.rows()[-1], ["deploy", self.commits[0][:7], self.commits[1][:7]])

    def test_gate_failure_refuses_and_restores_checkout(self):
        r = self.deploy(f"sha-{self.commits[1][:7]}", env=dict(self.env, GATE_MODE="fail"))
        self.assertEqual(r.returncode, 1)
        self.assertIn("DEPLOY FAIL: the installation's deploy gate failed", r.stdout)
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), self.commits[0])
        self.assertFalse(os.path.exists(self.log), "no log row for a refused deploy")
        p = os.path.join(self.tmp, "docker_calls")
        calls = open(p).read() if os.path.exists(p) else ""
        self.assertNotIn(" pull ", calls)
        self.assertNotIn(" up ", calls)

    def test_without_hooks_it_deploys_and_says_there_is_no_gate(self):
        with open(os.path.join(self.home, ".env"), "w") as f:
            f.write(f"FLOTILLA_CONFIG={self.cfg}\n")
        r = self.deploy(f"sha-{self.commits[1][:7]}")
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("no installation deploy gate (FLOTILLA_DEPLOY_HOOKS: unset)", r.stdout)
        self.assertEqual(self.gate_calls(), [])
        self.assertFalse(os.path.exists(self.log))

    def test_hooks_without_a_gate_say_so(self):
        with open(self.hooks, "w") as f:
            f.write(STANDIN_HOOKS.split("deploy_log()")[0].split("deploy_gate()")[0] + "deploy_log() { :; }\n")
        r = self.deploy(f"sha-{self.commits[1][:7]}")
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn(f"no installation deploy gate (FLOTILLA_DEPLOY_HOOKS: {self.hooks} defines none)", r.stdout)

    def test_hooks_named_but_missing_refuse_the_deploy(self):
        with open(os.path.join(self.home, ".env"), "a") as f:
            f.write(f"FLOTILLA_DEPLOY_HOOKS={os.path.join(self.tmp, 'absent.sh')}\n")
        r = self.deploy(f"sha-{self.commits[1][:7]}")
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("FLOTILLA_DEPLOY_HOOKS names", r.stdout)
        p = os.path.join(self.tmp, "docker_calls")
        self.assertNotIn(" pull ", open(p).read() if os.path.exists(p) else "")

    def test_a_commit_whose_e2e_is_red_is_not_deployed(self):
        # 2026-11-16: sha-6b18417 deployed while its e2e was red; ci-fast alone was required
        red = dict(self.env, STUB_CHECKS="1\\tci-fast\\tcompleted\\tsuccess\\n2\\te2e\\tcompleted\\tfailure")
        r = self.deploy(f"sha-{self.commits[1][:7]}", env=red)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("required e2e: completed/failure", r.stdout)
        self.assertIn("DEPLOY FAIL: checks are not green", r.stdout)
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), self.commits[0])

    def test_a_commit_whose_ci_fast_is_not_green_is_not_deployed(self):
        # the Fable advisor's review (2026-11-16): an image existing was the deploy's only check
        red = dict(self.env, STUB_CHECKS="1\\tci-fast\\tcompleted\\tfailure")
        r = self.deploy(f"sha-{self.commits[1][:7]}", env=red)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("checks_green: RED", r.stdout)
        self.assertIn("DEPLOY FAIL: checks are not green", r.stdout)
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), self.commits[0])
        p = os.path.join(self.tmp, "docker_calls")
        calls = open(p).read() if os.path.exists(p) else ""
        self.assertNotIn(" pull ", calls)
        self.assertNotIn(" up ", calls)
        r = self.deploy(f"sha-{self.commits[1][:7]}", env=dict(red, FLOTILLA_SKIP_CHECKS="checks re-run by hand"))
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("checks: skipped (checks re-run by hand)", r.stdout)

    def test_a_first_install_gives_the_gate_no_previous_commit(self):
        os.remove(os.path.join(self.home, "deployed.tsv"))
        r = self.deploy(f"sha-{self.commits[1][:7]}")
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertEqual(self.gate_calls(), [["", self.commits[1], self.cfg]], "the hooks decide what a first install means")

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
        self.assertEqual(self.rows()[-1], ["rollback", self.commits[1][:7], self.commits[0][:7]])


FLEETS_DOCKER = textwrap.dedent("""\
    #!/usr/bin/env bash
    # A stub engine for deploy_fleets.sh: the running worker's fleets (a tar of STUB_RUNNING/fleets), flotillad's
    # -check (STUB_CHECK_FAIL: a frozen version), the flotilla CLI inside flotillad (status, versions, pause,
    # resume), and compose up (which flips the recorded versions from versions.before to versions.after).
    echo "docker $*" >> "$DOCKER_CALLS"
    case "$1" in
      exec)
        case "$*" in
          *"flotilla-worker-triage tar"*) tar -C "$STUB_RUNNING" -cf - fleets; exit 0;;
          *"flotilla status -json"*) cat "$STUB_CFG/status.json"; exit 0;;
          *"flotilla status"*) echo "triage/cycle  record  record  10m"; echo 'worker triage: {"ok":true}'; exit 0;;
          *"flotilla versions"*)
            if [ -f "$STUB_CFG/up" ]; then cat "$STUB_CFG/versions.after"
            elif [ -n "${STUB_NO_VERSIONS:-}" ]; then echo "usage: flotilla ..." >&2; exit 2
            else cat "$STUB_CFG/versions.before"; fi; exit 0;;
          *"flotilla health"*) exit 0;;
        esac; exit 0;;
      compose)
        case "$*" in
          *" run "*)
            if [ -n "${STUB_CHECK_FAIL:-}" ]; then
              echo "specs: none" >&2
              echo "versions: triage/cycle v1 is live and its folder changed (recorded aaaaaaaaaaaa, found bbbbbbbbbbbb): v1/spec.yaml (changed)"
              exit 1
            fi
            echo "CHECK PASS"; exit 0;;
          *" up "*) : > "$STUB_CFG/up"; exit 0;;
        esac; exit 0;;
      ps) printf 'flotillad\nflotilla-worker-triage\n'; exit 0;;
      inspect) echo /srv/flotilla/fleets-mounted; exit 0;;
    esac
    exit 0
""")

ROUTINE = "fleet: triage\nroutine: cycle\ndescription: one pass\nenabled: true\nworker: triage\ntrigger: {every: 10m}\nautonomy: record\n"
VERSION = 'run: {command: "echo %s", timeout: 20m}\n'


def versions_json(*rows):
    import json
    return json.dumps({"routine": "triage/cycle", "versions": [
        {"version": n, "state": st, "hash": h * 12} for n, st, h in rows]})


class DeployFleetsTest(unittest.TestCase):
    """deploy_fleets.sh with a stub engine: the proof prints the versions that changed (a new version of a
    routine is one new shadow line), and a changed live version is refused before anything is paused or switched."""

    SCRIPT = os.path.join(HERE, "deploy_fleets.sh")

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.home = os.path.join(self.tmp, "home")
        repo = os.path.join(self.home, "repo", "deploy", "compose")
        os.makedirs(repo)
        open(os.path.join(repo, "fleets.yml"), "w").close()
        self.fr = os.path.join(self.tmp, "fleets-repo")
        os.makedirs(os.path.join(self.fr, "fleets", "triage", "cycle", "v1"))
        git(self.fr, "init", "-q")
        git(self.fr, "config", "user.email", "t@t"); git(self.fr, "config", "user.name", "t")
        self.write("fleets/triage/cycle/routine.yaml", ROUTINE)
        self.write("fleets/triage/cycle/v1/spec.yaml", VERSION % "v1")
        git(self.fr, "add", "-A"); git(self.fr, "commit", "-qm", "one version")
        self.c0 = git(self.fr, "rev-parse", "HEAD")
        self.write("fleets/triage/cycle/v2/spec.yaml", VERSION % "v2")
        git(self.fr, "add", "-A"); git(self.fr, "commit", "-qm", "a second version")
        self.c1 = git(self.fr, "rev-parse", "HEAD")
        git(self.fr, "remote", "add", "origin", self.fr)
        admin = os.path.join(self.tmp, "admin.token")
        self.put(admin, "t" * 40)
        with open(os.path.join(self.home, ".env"), "w") as f:
            f.write(f"FLOTILLA_FLEETS_REPO={self.fr}\nFLOTILLA_FLEETS_DIR={self.home}/fleets\nFLOTILLA_ADMIN_TOKEN_FILE={admin}\n")
        with open(os.path.join(self.home, "deployed.tsv"), "w") as f:
            f.write("2026-11-18T10:00:00Z\tsha-1234567\tabc\tpass\n")
        # what the triage worker runs now: the first commit's fleets
        self.running = os.path.join(self.tmp, "running"); os.makedirs(self.running)
        subprocess.run(f"git -C {self.fr} archive {self.c0} fleets | tar -x -C {self.running}", shell=True, check=True)
        self.stub = os.path.join(self.tmp, "stub"); os.makedirs(self.stub)
        self.put(os.path.join(self.stub, "status.json"), '{"routines":[{"id":"triage/cycle","spec_hash":"h0"}]}')
        bindir = os.path.join(self.tmp, "bin"); os.makedirs(bindir)
        with open(os.path.join(bindir, "docker"), "w") as f:
            f.write(FLEETS_DOCKER)
        os.chmod(os.path.join(bindir, "docker"), 0o755)
        self.calls = os.path.join(self.tmp, "docker_calls")
        self.env = dict(os.environ, PATH=bindir + ":" + os.environ["PATH"], HOME=self.tmp, FLOTILLA_HOME=self.home,
                        STUB_CFG=self.stub, STUB_RUNNING=self.running, DOCKER_CALLS=self.calls)

    def tearDown(self):
        subprocess.run(["chmod", "-R", "u+w", self.tmp])
        shutil.rmtree(self.tmp, ignore_errors=True)

    @staticmethod
    def put(path, text):
        with open(path, "w") as f:
            f.write(text)

    def write(self, rel, text):
        p = os.path.join(self.fr, rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "w") as f:
            f.write(text)

    def versions(self, before, after):
        self.put(os.path.join(self.stub, "versions.before"), before)
        self.put(os.path.join(self.stub, "versions.after"), after)

    def deploy(self, commit, env=None):
        return sh(["bash", self.SCRIPT, commit], env=env or self.env, check=False)

    def docker_calls(self):
        if not os.path.exists(self.calls):
            return ""
        with open(self.calls) as f:
            return f.read()

    def test_a_new_version_is_one_new_shadow_line(self):
        self.versions(versions_json((1, "live", "a")), versions_json((1, "live", "a"), (2, "shadow", "b")))
        r = self.deploy(self.c1)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("FLEETS DEPLOY PASS", r.stdout)
        block = r.stdout.split("== versions:")[1].split("\n")
        changed = [l.strip() for l in block if l.strip().startswith(("<", ">"))]
        self.assertEqual(changed, ["> triage/cycle v2 shadow " + "b" * 12], r.stdout)

    def test_no_change_in_versions_says_unchanged(self):
        same = versions_json((1, "live", "a"), (2, "shadow", "b"))
        self.versions(same, same)
        r = self.deploy(self.c1)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("== versions:\n   unchanged (2 versions)", r.stdout)

    def test_a_flotillad_without_versions_is_said_and_does_not_fail_the_deploy(self):
        self.versions("", versions_json((1, "live", "a")))
        r = self.deploy(self.c1, env=dict(self.env, STUB_NO_VERSIONS="1"))
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("< (none: the running flotillad has no versions)", r.stdout)
        self.assertIn("> triage/cycle v1 live " + "a" * 12, r.stdout)

    def test_a_frozen_version_refuses_the_deploy_before_anything_moves(self):
        self.versions(versions_json((1, "live", "a")), versions_json((1, "live", "a")))
        r = self.deploy(self.c1, env=dict(self.env, STUB_CHECK_FAIL="1"))
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("versions: triage/cycle v1 is live and its folder changed", r.stdout)
        self.assertIn("FLEETS DEPLOY FAIL: versions: flotillad -check refused fleets", r.stdout)
        self.assertFalse(os.path.lexists(os.path.join(self.home, "fleets")), "the live folder was switched")
        calls = self.docker_calls()
        self.assertNotIn(" up ", calls)
        self.assertNotIn("pause", calls, "a routine was paused before the check passed")
        self.assertFalse(os.path.exists(os.path.join(self.home, "fleets-deployed.tsv")), "a refused deploy is not logged")


if __name__ == "__main__":
    unittest.main(verbosity=2)
