"""python3 runtime/test_loaded.py: what a model run read (G7), and that a skill load records itself."""
import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import loaded  # noqa: E402
import skills  # noqa: E402


class Loaded(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d)
        self.path = os.path.join(self.d, "loaded.jsonl")
        self.env = {"FLOTILLA_LOADED_FILE": self.path}
        loaded._seen.clear()
        loaded._count[0] = 0

    def lines(self):
        with open(self.path) as f:
            return [json.loads(x) for x in f]

    def test_records_once_each(self):
        loaded.record("playbook", "source", "triage playbook", "fleets/triage/playbooks/triage.md @ x", "ab", environ=self.env)
        loaded.record("playbook", "source", "triage playbook", "fleets/triage/playbooks/triage.md @ x", "ab", environ=self.env)
        loaded.record("input", "untrusted", "glitchtip: X", "summary " * 100, environ=self.env)
        got = self.lines()
        self.assertEqual([(x["kind"], x["trust"]) for x in got], [("playbook", "source"), ("input", "untrusted")])
        self.assertLessEqual(len(got[1]["source"]), loaded.TEXT_MAX)

    def test_without_the_variable_nothing_is_written(self):
        loaded.record("skill", "source", "s", "x", environ={})
        self.assertFalse(os.path.exists(self.path))

    def test_refuses_an_unknown_trust(self):
        with self.assertRaises(ValueError):
            loaded.record("skill", "trusted", "s", "x", environ=self.env)

    def test_caps_a_run(self):
        for i in range(loaded.MAX + 5):
            loaded.record("probe", "untrusted", f"probe {i}", "{}", environ=self.env)
        self.assertEqual(len(self.lines()), loaded.MAX)

    def test_a_skill_load_records_itself(self):
        root = os.path.join(self.d, "root")
        os.makedirs(os.path.join(root, "skills", "hint-x"))
        os.makedirs(os.path.join(root, "fleets", "f", "skills"))
        with open(os.path.join(root, "skills", "hint-x", "SKILL.md"), "w") as f:
            f.write("---\nname: hint-x\ndescription: a hint\n---\nDo the thing.\n")
        old = os.environ.get("FLOTILLA_LOADED_FILE")
        os.environ["FLOTILLA_LOADED_FILE"] = self.path
        try:
            s = skills.load("hint-x", "f", root, version="sha-1")
        finally:
            if old is None:
                del os.environ["FLOTILLA_LOADED_FILE"]
            else:
                os.environ["FLOTILLA_LOADED_FILE"] = old
        got = self.lines()
        self.assertEqual(len(got), 1)
        self.assertEqual((got[0]["kind"], got[0]["trust"], got[0]["title"], got[0]["sha256"]), ("skill", "source", "hint-x", s["sha256"]))
        self.assertIn("@ sha-1", got[0]["source"])


if __name__ == "__main__":
    unittest.main()
