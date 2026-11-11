"""python3 runtime/test_skills.py — the skills loader (ADR-0012), the same rules as internal/skills."""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import skills  # noqa: E402

GOOD = "---\nname: {n}\ndescription: Use when testing: a colon is fine.\n---\n\n  Do the thing.  \n\n"


class Loader(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()

    def skill(self, layer, name, body=GOOD):
        d = os.path.join(self.root, "skills" if layer == "flotilla" else os.path.join("fleets", "f", "skills"), name)
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "SKILL.md"), "w") as f:
            f.write(body.format(n=name))

    def test_load(self):
        self.skill("fleet", "in-fleet")
        self.skill("flotilla", "in-default")
        s = skills.load("in-fleet", "f", self.root, version="sha-test")
        self.assertEqual((s["layer"], s["text"], s["version"], len(s["sha256"])), ("fleet", "Do the thing.", "sha-test", 64))
        self.assertEqual(skills.load("in-default", "f", self.root)["layer"], "flotilla")

    def test_refusals(self):
        self.skill("fleet", "in-both")
        self.skill("flotilla", "in-both")
        self.skill("fleet", "wrong-name", "---\nname: other\ndescription: d\n---\nx\n")
        self.skill("fleet", "no-desc", "---\nname: {n}\n---\nx\n")
        self.skill("fleet", "no-body", "---\nname: {n}\ndescription: d\n---\n \n")
        self.skill("fleet", "no-front", "x\n")
        self.skill("fleet", "too-big", GOOD + "x" * skills.MAX_BYTES)
        os.makedirs(os.path.join(self.root, "elsewhere"))
        with open(os.path.join(self.root, "elsewhere", "SKILL.md"), "w") as f:
            f.write(GOOD.format(n="linked"))
        os.symlink(os.path.join(self.root, "elsewhere"), os.path.join(self.root, "fleets", "f", "skills", "linked"))
        for name, want in (("in-both", "more than one layer"), ("missing", "in no layer"), ("wrong-name", "folder's name"),
                           ("no-desc", "description"), ("no-body", "no instructions"), ("no-front", "frontmatter"),
                           ("too-big", "never trimmed"), ("linked", "symlink"), ("../f", "does not match"),
                           ("Bad", "does not match")):
            with self.subTest(name), self.assertRaisesRegex(skills.SkillError, want):
                skills.load(name, "f", self.root)

    def test_rules_and_match(self):
        rules = [{"when": {"class": "a"}, "load": ["s1", "s2"]}, {"when": {"class": "a", "src": "x"}, "load": ["s2", "s3"]},
                 {"when": {"class": "b"}, "load": ["s4"]}]
        self.assertEqual(skills.match(rules, {"class": "a", "src": "x"}), ["s1", "s2", "s3"], "spec order, each once")
        self.assertEqual(skills.match(rules, {"class": "a"}), ["s1", "s2"])
        self.assertEqual(skills.match(rules, {"class": "c"}), [])
        p = os.path.join(self.root, "rules.json")
        with open(p, "w") as f:
            json.dump(rules[:1], f)
        self.assertEqual(skills.rules_from_env(p, environ={}), rules[:1], "the default file without FLOTILLA_SKILLS")
        self.assertEqual(skills.rules_from_env(p, environ={"FLOTILLA_SKILLS": json.dumps({"rule": rules})}), rules,
                         "FLOTILLA_SKILLS wins")
        with self.assertRaises(skills.SkillError):
            skills.rules_from_env(None, environ={"FLOTILLA_SKILLS": json.dumps({"rule": [{"when": {}, "load": ["x"]}]})})


if __name__ == "__main__":
    unittest.main(verbosity=1)
