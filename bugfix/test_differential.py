"""python3 bugfix/test_differential.py: the differential harness (bugfix/differential.py) finds what it should and nothing else.

The old side is the chain at differential.OLD_SHA in a detached worktree; a copy of the working tree stands in for the new side
where a test needs to edit it."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import differential as d  # noqa: E402


class Harness(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tree = d.OldTree()
        cls.old = cls.tree.__enter__()

    @classmethod
    def tearDownClass(cls):
        cls.tree.__exit__(None, None, None)

    def test_the_old_side_is_the_commit_named_not_the_working_tree(self):
        head = subprocess.run(["git", "-C", self.old, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
        self.assertEqual(head, d.OLD_SHA)

    def test_identical_code_has_an_empty_delta_in_every_scenario(self):
        """The chain against itself, every golden scenario: no field differs, so none needs an allowance."""
        rows = d.differential(self.old, d.ROOT, "fix.py")
        self.assertEqual(rows, [], d.table(rows)[0])

    # what a station's call may differ in between the chain's own and the generic pi adapter's (the allow-list's rules, named)
    DELTAS = {"prompt_channel", "models_json", "env.PI_OFFLINE", "env.PI_SKIP_VERSION_CHECK", "positional"}

    def test_the_stations_on_the_pi_adapter_differ_by_the_allow_list_and_nothing_else(self):
        """bugfix/fix2.py on stations: verdict, fix, repro, advisor and review are harness_pi.Pi's own calls; what each sends the model, its system
        file, tools, --session, cwd and NODE_OPTIONS are the chain's (none of them is in the table), and every other delta is a rule."""
        rows = d.differential(self.old, d.ROOT, "fix2.py")
        self.assertEqual([r for r in rows if r[6] is None], [], d.table(rows)[0])
        self.assertEqual({r[2] for r in rows}, {"verdict", "fix", "repro", "advisor", "review"})
        self.assertEqual({r[3] for r in rows}, self.DELTAS)
        self.assertEqual([r for r in rows if r[3] == "no_context_files"], [],
                         "v1 passed --no-context-files only with a rules file declared, and so does fix2")

    def test_one_prompt_byte_changed_in_the_new_path_is_found(self):
        new = d.copy_tree(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, new, True)
        sh = os.path.join(new, "bugfix-fleet", "bakeoff", "harnesses", "pi.sh")
        with open(sh) as f:
            text = f.read()
        self.assertIn("Diagnose the cause", text)
        with open(sh, "w") as f:
            f.write(text.replace("Diagnose the cause", "Diagnoze the cause"))
        rows = d.differential(self.old, new, "fix.py", ["fresh_clean_fix"])
        self.assertEqual([(r[3], r[6]) for r in rows], [("prompt", None), ("positional", None)], d.table(rows)[0])
        self.assertEqual(d.table(rows)[1], 2, "the text is in argv on both sides, so both fields show it")


class OldTreeLifecycle(unittest.TestCase):
    """The old side's worktree is made under TMPDIR, pruned at the start and gone after a SIGTERM."""

    def registered(self):
        out = subprocess.run(["git", "-C", d.ROOT, "worktree", "list", "--porcelain"], capture_output=True, text=True, check=True).stdout
        return [x[len("worktree "):] for x in out.splitlines() if x.startswith("worktree ")]

    def test_it_lives_under_tmpdir(self):
        with d.OldTree() as old:
            self.assertTrue(os.path.realpath(old).startswith(os.path.realpath(tempfile.gettempdir()) + os.sep), old)
            self.assertIn(old, self.registered())
        self.assertFalse(os.path.exists(old))
        self.assertNotIn(old, self.registered())

    def test_a_worktree_a_killed_run_left_registered_is_pruned_at_the_start(self):
        stale = os.path.join(tempfile.mkdtemp(), "old")
        subprocess.run(["git", "-C", d.ROOT, "worktree", "add", "--detach", stale, d.OLD_SHA], check=True, capture_output=True)
        shutil.rmtree(os.path.dirname(stale))
        self.assertIn(stale, self.registered())
        with d.OldTree():
            self.assertNotIn(stale, self.registered())

    def test_sigterm_removes_the_folder_and_the_registration(self):
        code = ("import os, sys, time\nsys.path.insert(0, %r)\nimport differential as d\n"
                "with d.OldTree() as old:\n    print(old, flush=True)\n    time.sleep(60)\n") % os.path.dirname(os.path.abspath(d.__file__))
        p = subprocess.Popen([sys.executable, "-c", code], stdout=subprocess.PIPE, text=True)
        self.addCleanup(p.stdout.close)
        old = p.stdout.readline().strip()
        self.assertTrue(os.path.isdir(old))
        self.assertIn(old, self.registered())
        p.terminate()
        self.assertEqual(p.wait(timeout=30), 128 + 15)
        self.assertFalse(os.path.exists(old))
        self.assertFalse(os.path.exists(os.path.dirname(old)))
        self.assertNotIn(old, self.registered())


class FakePi(unittest.TestCase):
    """The fake pi records the bytes it is sent, as sent."""

    def test_stdin_is_read_as_bytes(self):
        tmp = tempfile.mkdtemp(dir=os.environ.get("TMPDIR"))
        self.addCleanup(shutil.rmtree, tmp, True)
        leaves = os.path.join(tmp, "leaves")
        os.makedirs(leaves)
        with open(os.path.join(leaves, "repro.sh"), "w") as f:   # a call with no tools switch and no resume is the reproducer's
            f.write("exit 0\n")
        pi = os.path.join(tmp, "pi")
        with open(pi, "w") as f:
            f.write(d.FAKE_PI)
        sent = b"line one\r\nline two \xff\xfe\r\n\n"
        subprocess.run([sys.executable, pi, "-p"], input=sent, cwd=tmp, check=True, capture_output=True,
                       env=dict(os.environ, FAKE_STATE=tmp, DIFF_LEAVES=leaves, PI_CODING_AGENT_DIR=tmp))
        with open(os.path.join(tmp, "differential.jsonl")) as fh:
            got = json.loads(fh.readline())
        self.assertEqual(got["prompt"].encode("utf-8", "surrogateescape"), sent)
        self.assertEqual(got["prompt_channel"], "stdin")


class Allowances(unittest.TestCase):
    """What the allow-list says, call by call, with no chain run."""

    def call(self, **kw):
        base = {"role": "fix", "argv": ["-p", "--mode", "json", "--provider", "p1", "--model", "m1", "--no-context-files",
                                        "--append-system-prompt", "<ROOT>/r.md", "TEXT"],
                "prompt": "TEXT", "prompt_channel": "argv", "cwd": "<D>/wt", "env": {}, "system_sha": "aa",
                "models_json": {"providers": {"p1": {"baseUrl": "u", "models": [{"id": "m1"}, {"id": "m2"}]}}}}
        base.update(kw)
        return base

    def rows(self, old, new):
        return d.compare("s", {"plan": {}, "calls": [old]}, {"plan": {}, "calls": [new]})

    def test_the_listed_deltas_are_allowed(self):
        new = self.call(argv=["-p", "--mode", "json", "--provider", "flotilla-g", "--model", "m1", "--no-context-files",
                              "--append-system-prompt", "<ROOT>/r.md"], prompt_channel="stdin",
                        env={"PI_OFFLINE": "1", "PI_SKIP_VERSION_CHECK": "1"},
                        models_json={"providers": {"flotilla-g": {"baseUrl": "u", "models": [{"id": "m1"}]}}})
        got = self.rows(self.call(), new)
        self.assertEqual(sorted(r[3] for r in got), ["env.PI_OFFLINE", "env.PI_SKIP_VERSION_CHECK", "models_json", "positional",
                                                     "prompt_channel", "provider"])
        self.assertTrue(all(r[6] for r in got), got)

    def test_what_must_match_is_never_allowed(self):
        argv = self.call()["argv"]
        for field, new in (("prompt", self.call(prompt="TEXT!")),
                           ("tools", self.call(argv=argv[:-1] + ["--no-tools", "TEXT"])),
                           ("system_file", self.call(argv=[a for a in argv if a != "--append-system-prompt" and a != "<ROOT>/r.md"])),
                           ("system_sha", self.call(system_sha="bb")),
                           ("session", self.call(argv=argv + ["--session", "s"])),
                           ("cwd", self.call(cwd="<D>/other")),
                           ("env.NODE_OPTIONS", self.call(env={"NODE_OPTIONS": "--max-old-space-size=4096"})),
                           ("model", self.call(argv=[("m2" if a == "m1" else a) for a in argv])),
                           # the text in stdin and still in argv: the model is sent it twice
                           ("prompt_channel", self.call(prompt_channel="stdin")),
                           ("positional", self.call(argv=argv + ["EXTRA"]))):
            with self.subTest(field):
                got = [r for r in self.rows(self.call(), new) if r[3] == field]
                self.assertEqual([r[6] for r in got], [None], got)

    def test_a_models_json_that_changes_the_called_entry_is_unexplained(self):
        new = self.call(models_json={"providers": {"p1": {"baseUrl": "u", "models": [{"id": "m1", "maxTokens": 1}]}}})
        self.assertEqual([r[6] for r in self.rows(self.call(), new) if r[3] == "models_json"], [None])


if __name__ == "__main__":
    unittest.main()
