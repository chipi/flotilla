"""python3 bugfix/test_harness_prompts.py: bugfix/harness_prompts.py renders what harnesses/pi.sh and repro.sh render.

The shell versions are run by bash with a fake `pi` on PATH that records the prompt it was given; the Python versions
must give the same text, for the real agent definition and for awkward input (shell metacharacters, backslashes,
whitespace at the ends, lines of dashes, unicode)."""
import json
import os
import random
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import harness_prompts as hp  # noqa: E402

BAKEOFF = os.path.join(os.path.dirname(HERE), "bugfix-fleet", "bakeoff")
FAKE_PI = ("#!/usr/bin/env bash\n"
           "python3 - \"$@\" <<'PY'\nimport json, os, sys\njson.dump(sys.argv[1:], open(os.environ['OUT'], 'w'))\nPY\n")

DESCS = [
    "value() returns 1, want 2",
    "",
    "\n\n  leading blank lines and spaces\n\n",
    "trailing whitespace   \n\t \n\n",
    "shell: $HOME ${PATH} $(rm -rf /) `id` \"q\" 'q' \\n \\\\ \\$X \\` & | ; < > * ? [a] !",
    "heredoc end:\nEOF\nEOF \n  EOF\nmore",
    "dashes:\n---\nbody\n---\n--- \n----",
    "unicode: \u2014 \u00e9 \u4e2d\u6587 \U0001f600",
    "a line that is only a backslash:\n\\\nand a continuation \\\nnext",
    "tabs\tand\r\nCRLF lines\r\n",
    "x" * 100_000,
    "THE ISSUE AS FILED:\nvalue is 1\n\nvalue() returns 1, want 2\n\nTHE TASK THE VERDICT STEP WROTE FROM IT (it can be wrong):\nmake value() return 2"
    "\n\nIMPORTANT: the failing test src/a.test.js reproduces the bug and is frozen. Fix the source so that it passes.",
]


def rendered(script, *args, env_extra=None):
    """What `pi` was called with by script (bash), as its argv."""
    d = tempfile.mkdtemp()
    try:
        os.makedirs(os.path.join(d, "bin"))
        with open(os.path.join(d, "bin", "pi"), "w") as f:
            f.write(FAKE_PI)
        os.chmod(os.path.join(d, "bin", "pi"), 0o755)
        out = os.path.join(d, "out.json")
        env = {k: v for k, v in os.environ.items() if not k.startswith(("PI_", "BUGFIX_"))}
        env.update(PATH=os.path.join(d, "bin") + os.pathsep + env["PATH"], OUT=out, **(env_extra or {}))
        wt = os.path.join(d, "wt")
        os.makedirs(wt)
        subprocess.run(["bash", script, wt, *args], env=env, check=True, capture_output=True)
        with open(out) as f:
            return json.load(f)
    finally:
        shutil.rmtree(d, ignore_errors=True)


class FixPrompt(unittest.TestCase):
    PI_SH = os.path.join(BAKEOFF, "harnesses", "pi.sh")

    def test_it_is_what_pi_sh_sends(self):
        for desc in DESCS:
            with self.subTest(desc[:40]):
                self.assertEqual(hp.fix_prompt(desc), rendered(self.PI_SH, desc)[-1])

    def test_it_is_what_pi_sh_sends_for_random_text(self):
        rng = random.Random(20261118)
        alphabet = list(" \t\n\\$`\"'{}()-=EOFx\u2014\u00e9") + ["---\n", "\n\n", "EOF\n", "$HOME", "`x`", "\\\n"]
        for i in range(40):
            desc = "".join(rng.choice(alphabet) for _ in range(rng.randint(0, 60)))
            with self.subTest(i=i, desc=desc):
                self.assertEqual(hp.fix_prompt(desc), rendered(self.PI_SH, desc)[-1])

    def test_a_resumed_session_is_sent_the_task_itself(self):
        got = rendered(self.PI_SH, "THE REVIEW:\nfix it  \n", env_extra={"PI_RESUME_SESSION": "/s/one.jsonl"})
        self.assertEqual(got[-1], "THE REVIEW:\nfix it  \n", "no template, no trimming: the chain's text is the prompt")


class ReproPrompt(unittest.TestCase):
    REPRO_SH = os.path.join(BAKEOFF, "harnesses", "repro.sh")

    def tree(self, agent_text):
        """A copy of repro.sh in a tree whose agents/repro.md is agent_text (repro.sh finds it from its own place)."""
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        os.makedirs(os.path.join(d, "bakeoff", "harnesses"))
        os.makedirs(os.path.join(d, "agents"))
        shutil.copy(self.REPRO_SH, os.path.join(d, "bakeoff", "harnesses", "repro.sh"))
        with open(os.path.join(d, "agents", "repro.md"), "w", newline="") as f:
            f.write(agent_text)
        return os.path.join(d, "bakeoff", "harnesses", "repro.sh")

    def test_it_is_what_repro_sh_sends_with_the_real_agent_definition(self):
        with open(os.path.join(os.path.dirname(BAKEOFF), "agents", "repro.md")) as f:
            body = hp.agent_def_body(f.read())
        self.assertTrue(body.startswith("\n# reproducer"), "the body is what follows the front matter")
        for desc in DESCS:
            with self.subTest(desc[:40]):
                self.assertEqual(hp.repro_prompt(body, desc), rendered(self.REPRO_SH, desc)[-1])

    def test_it_is_what_repro_sh_sends_for_awkward_agent_definitions(self):
        for text in ("---\nname: r\n---\n\n# body\n\ntext\n",
                     "---\nname: r\n---\n# body without a blank line\n",
                     "---\nname: r\n---\nno trailing newline",
                     "---\nname: r\n---\n\n\n  indented first line\n\n\n\n",
                     "---\nname: r\n---\npart one\n---\npart two after a horizontal rule\n",
                     "---\nname: r\n---\n",
                     "---\nname: r\n",
                     "no front matter\nat all\n",
                     "---\r\nname: r\r\n---\r\nCRLF file\r\n",
                     "\n---\nname: r\n---\n  \t \n$HOME `x` \\n\n",
                     "---\n---\nempty front matter\n"):
            script = self.tree(text)
            for desc in ("the issue", "\n  padded  \n", ""):
                with self.subTest(text=text[:30], desc=desc):
                    self.assertEqual(hp.repro_prompt(hp.agent_def_body(text), desc), rendered(script, desc)[-1])

    def test_the_agent_definition_body_is_what_follows_the_second_fence(self):
        self.assertEqual(hp.agent_def_body("---\na: b\n---\n\n# t\n\nx\n"), "\n# t\n\nx")
        self.assertEqual(hp.agent_def_body("no fences\n"), "")
        self.assertEqual(hp.agent_def_body("---\na\n---\nx\n---\ny\n"), "x\ny")


if __name__ == "__main__":
    unittest.main()
