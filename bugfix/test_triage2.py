"""python3 bugfix/test_triage2.py: bugfix/triage2.py against the bash it ports. The behaviour is proved by bugfix/differential.py (the real
scripts and the port, scenario by scenario); this keeps the prompt texts from drifting from the scripts' own."""
import os
import re
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import triage2  # noqa: E402

BAKE = os.path.join(os.path.dirname(HERE), "bugfix-fleet", "bakeoff")


def lines(text):
    """The prompt's whole lines, those with no placeholder (the doubled braces of a format string undone)."""
    text = text.replace("{{", "{").replace("}}", "}")
    return [ln for ln in text.split("\n") if len(ln.strip()) > 20 and not re.search(r"\{[a-z_]+\}", ln)]


class Drift(unittest.TestCase):
    def script(self, name):
        with open(os.path.join(BAKE, name)) as f:
            return f.read()

    def test_every_line_of_the_verdict_prompts_is_in_triage_run_sh(self):
        sh = self.script("triage_run.sh").replace("\\`", "`")
        for text in (triaged for triaged in (triage2.FIRST_PASS, triage2.KICK_BACK, triage2.REFUTED_PIN, triage2.UNFOLLOWED_PIN,
                                             triage2.ADVISOR_FINDINGS, triage2.ACCEPTANCE_GAP)):
            for ln in lines(text):
                self.assertIn(ln, sh)

    def test_every_line_of_the_advisor_prompt_is_in_advisor_run_sh(self):
        sh = self.script("advisor_run.sh").replace("\\`", "`")
        for ln in lines(triage2.ADVISOR_PROMPT.replace("{{", "{").replace("}}", "}")):
            self.assertIn(ln, sh)

    def test_the_prints_are_the_scripts(self):
        sh = self.script("triage_run.sh")
        self.assertIn(f'echo "{triage2.RULE}"', sh)
        for word in ("══ run triager ══", "══ extract + shape-check verdict ══", "EXTRACT: no JSON found", "ABORT: kick-back needs the prior triage.json"):
            self.assertIn(word, sh)
        self.assertEqual(len(re.findall("═", self.script("advisor_run.sh"))) % 2, 0)


if __name__ == "__main__":
    unittest.main()
