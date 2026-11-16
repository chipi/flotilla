#!/usr/bin/env python3
"""Tests for knowledge.sh: the deploy's copy of declared paths into the knowledge volume (RFC-0002 §7).

The script runs for real against a throwaway git checkout, with a stub `docker` on PATH that keeps each
named volume as a folder and runs the container's command inside it, so the copy, the VERSION file, the
whole-folder replacement and every refusal are exercised without an engine. The folder the script fills is
then read through runtime/knowledge.py, so the layout the deploy writes is the layout a run reads.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "knowledge.sh")
ROOT = os.path.dirname(os.path.dirname(HERE))
COMPOSE = os.path.join(ROOT, "deploy", "compose", "docker-compose.yml")
sys.path.insert(0, os.path.join(ROOT, "runtime"))
import knowledge  # noqa: E402

FAKE_DOCKER = textwrap.dedent("""\
    #!/usr/bin/env bash
    echo "docker $*" >> "$DOCKER_CALLS"
    [ "${1:-}" != --context ] || shift 2
    if [ "$1 $2" = "volume create" ]; then mkdir -p "$VOLROOT/$3"; echo "$3"; exit 0; fi
    [ "$1" = run ] || exit 0
    shift
    vol=""; script=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -v) vol="${2%%:*}"; shift 2;;
        -e) export "$2"; shift 2;;
        -c) script="$2"; shift 2;;
        --network|--user|--entrypoint|-w) shift 2;;
        *) shift;;
      esac
    done
    cd "$VOLROOT/$vol" && exec sh -c "$script"
""")


def sh(cmd, cwd=None, env=None):
    return subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)


def git(repo, *args):
    r = sh(["git", "-C", repo, *args])
    assert r.returncode == 0, f"git {args}: {r.stderr}"
    return r.stdout.strip()


class Knowledge(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp)
        self.checkout = os.path.join(self.tmp, "homelab")
        self.home = os.path.join(self.tmp, "home")
        self.volroot = os.path.join(self.tmp, "volumes")
        self.calls = os.path.join(self.tmp, "docker_calls")
        os.makedirs(self.home)
        os.makedirs(self.volroot)
        bindir = os.path.join(self.tmp, "bin")
        os.makedirs(bindir)
        with open(os.path.join(bindir, "docker"), "w") as f:
            f.write(FAKE_DOCKER)
        os.chmod(os.path.join(bindir, "docker"), 0o755)
        self.env = dict(os.environ, PATH=bindir + ":" + os.environ["PATH"], DOCKER_CALLS=self.calls, VOLROOT=self.volroot,
                        FLOTILLA_HOME=self.home)
        os.makedirs(os.path.join(self.checkout, "docs", "recipes", "sub"))
        os.makedirs(os.path.join(self.checkout, "docs", "other"))
        self.write("docs/recipes/a.md", "# A\nfirst\n")
        self.write("docs/recipes/sub/b.md", "# B\n")
        self.write("docs/other/c.md", "# C\nnot declared\n")
        git(self.checkout, "init", "-q")
        git(self.checkout, "config", "user.email", "t@t")
        git(self.checkout, "config", "user.name", "t")
        git(self.checkout, "add", "-A")
        git(self.checkout, "commit", "-qm", "c1")
        self.c1 = git(self.checkout, "rev-parse", "HEAD")
        self.env_file(KNOWLEDGE_HOMELAB_FROM=self.checkout, KNOWLEDGE_HOMELAB_PATHS="docs/recipes")

    def write(self, rel, text):
        with open(os.path.join(self.checkout, rel), "w") as f:
            f.write(text)

    def env_file(self, **kv):
        with open(os.path.join(self.home, ".env"), "w") as f:
            f.write("".join(f"{k}={v}\n" for k, v in kv.items()))

    def run_script(self, tag="sha-abc1234"):
        return sh(["bash", SCRIPT, tag], env=self.env)

    def vol(self, *parts):
        return os.path.join(self.volroot, "flotilla-knowledge", *parts)

    def test_copies_the_declared_paths_at_head_and_writes_version(self):
        self.write("docs/recipes/a.md", "# A\nedited, not committed\n")   # the working tree is not what ships
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("KNOWLEDGE PASS", r.stdout)
        self.assertIn("uncommitted changes", r.stdout)
        src = self.vol("homelab-handbook")
        with open(os.path.join(src, "docs", "recipes", "a.md")) as f:
            self.assertEqual(f.read(), "# A\nfirst\n")
        self.assertTrue(os.path.isfile(os.path.join(src, "docs", "recipes", "sub", "b.md")))
        self.assertFalse(os.path.exists(os.path.join(src, "docs", "other")), "a path that is not declared was copied")
        with open(os.path.join(src, "VERSION")) as f:
            self.assertEqual(f.read().strip(), self.c1)
        self.assertEqual(sorted(os.listdir(self.vol())), ["homelab-handbook"], "a .new or .old folder was left")

    def test_the_container_has_no_network_and_no_host_folder(self):
        self.run_script()
        with open(self.calls) as f:
            runs = [l for l in f if " run " in l]
        self.assertEqual(len(runs), 1)
        self.assertIn("--network none", runs[0])
        self.assertIn("-v flotilla-knowledge:/k", runs[0])
        self.assertIn("flotilla-worker-triage:sha-abc1234", runs[0])
        self.assertNotIn(self.checkout, runs[0], "the checkout was mounted or named to the container")

    def test_what_it_fills_is_what_a_run_reads(self):
        self.run_script()
        env = {"FLOTILLA_KNOWLEDGE": json.dumps({"sources": {"homelab-handbook": {"kind": "files", "maps": {
            "by_service": {"a": "docs/recipes/a.md"}}}}}), "FLOTILLA_KNOWLEDGE_ROOT": self.vol()}
        e = knowledge.read("homelab-handbook", "by_service", "a", env)
        self.assertEqual((e["version"], e["text"]), (self.c1, "# A\nfirst\n"))

    def test_a_new_commit_replaces_the_folder_whole(self):
        self.run_script()
        os.remove(os.path.join(self.checkout, "docs", "recipes", "sub", "b.md"))
        self.write("docs/recipes/d.md", "# D\n")
        git(self.checkout, "add", "-A")
        git(self.checkout, "commit", "-qm", "c2")
        c2 = git(self.checkout, "rev-parse", "HEAD")
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        src = self.vol("homelab-handbook")
        with open(os.path.join(src, "VERSION")) as f:
            self.assertEqual(f.read().strip(), c2)
        self.assertFalse(os.path.exists(os.path.join(src, "docs", "recipes", "sub", "b.md")), "a removed file stayed")
        self.assertTrue(os.path.isfile(os.path.join(src, "docs", "recipes", "d.md")))
        self.assertEqual(sorted(os.listdir(self.vol())), ["homelab-handbook"])

    def test_without_a_checkout_it_skips_and_still_creates_the_volume(self):
        self.env_file()
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("KNOWLEDGE SKIP", r.stdout)
        self.assertTrue(os.path.isdir(self.vol()), "compose mounts the volume: it must exist")
        self.assertEqual(os.listdir(self.vol()), [])

    def test_refusals_leave_the_volume_as_it_was(self):
        self.run_script()
        for paths in ("../homelab", "docs/*", "/etc", "docs/recipes/../other", "docs/missing", "docs//recipes", "docs/recipes/"):
            self.env_file(KNOWLEDGE_HOMELAB_FROM=self.checkout, KNOWLEDGE_HOMELAB_PATHS=paths)
            r = self.run_script()
            self.assertEqual(r.returncode, 1, f"{paths!r}: {r.stdout}{r.stderr}")
            self.assertIn("KNOWLEDGE FAIL", r.stdout, paths)
            with open(self.vol("homelab-handbook", "VERSION")) as f:
                self.assertEqual(f.read().strip(), self.c1)
        self.env_file(KNOWLEDGE_HOMELAB_FROM=self.tmp, KNOWLEDGE_HOMELAB_PATHS="docs/recipes")
        r = self.run_script()
        self.assertEqual((r.returncode, "is not a git checkout" in r.stdout), (1, True), r.stdout + r.stderr)
        self.env_file(KNOWLEDGE_HOMELAB_FROM=self.checkout, KNOWLEDGE_HOMELAB_SOURCE="Bad_Name")
        self.assertEqual(self.run_script().returncode, 1)


class Compose(unittest.TestCase):
    def test_the_workers_that_read_it_mount_it_read_only(self):
        with open(COMPOSE) as f:
            text = f.read()
        self.assertEqual(text.count("- knowledge:/knowledge:ro"), 3, "worker-triage, worker-triage-candidate and worker-ops-read")
        self.assertNotIn("knowledge:/knowledge\n", text)
        self.assertIn("  knowledge:\n    name: ${KNOWLEDGE_VOLUME:-${FLOTILLA_VOLUME_PREFIX:-flotilla}-knowledge}\n    external: true", text)


if __name__ == "__main__":
    unittest.main()
