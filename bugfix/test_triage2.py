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


class Bytes(unittest.TestCase):
    """What the scripts keep and the port must keep too (the second review, 2026-11-21)."""

    def run_(self):
        import tempfile
        self.d = tempfile.mkdtemp()
        self.addCleanup(__import__("shutil").rmtree, self.d, True)
        return triage2.Run(dict(os.environ), self.d)

    def write(self, name, text):
        p = os.path.join(self.d, name)
        with open(p, "w", newline="") as f:
            f.write(text)
        return p

    def test_a_cr_from_jq_is_kept_as_bash_keeps_it(self):
        r = self.run_()
        ticket = self.write("t.json", '{"description": "line1\\r\\nline2\\rline3 café"}')
        self.assertEqual(triage2._val(r, "jq -r .description \"$1\"", ticket), "line1\r\nline2\rline3 café")

    def test_the_pin_line_of_an_answer_that_is_json_but_not_an_object_is_empty_and_the_run_goes_on(self):
        r = self.run_()
        self.assertEqual(triage2.pin_shown(r, self.write("a.json", '["a"]')), "")
        self.assertEqual(triage2.pin_shown(r, self.write("b.json", '{"file": "src/a.js", "function": "value"}')), "src/a.js :: value")

    def test_the_stream_splits_at_newlines_only_and_a_stderr_brace_is_stderr(self):
        ev1 = '{"type": "message_end", "text": "x y\u0085z"}'
        out, err = triage2.split_stream(ev1 + "\n" + "{ inspected error object\n" + '{"type": "agent_end"}\n')
        self.assertEqual(out, ev1 + "\n" + '{"type": "agent_end"}\n')
        self.assertEqual(err, "{ inspected error object\n")


if __name__ == "__main__":
    unittest.main()
