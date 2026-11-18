"""python3 runtime/test_prompts.py — the prompts loader (ADR-0018), the same rules as internal/prompts."""
import hashlib
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import prompts  # noqa: E402


def sha(b):
    return hashlib.sha256(b).hexdigest()


class Loader(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.dir = os.path.join(self.root, "fleets", "f")
        os.makedirs(os.path.join(self.dir, "playbooks"))

    def put(self, rel, data):
        with open(os.path.join(self.dir, rel), "wb") as f:
            f.write(data if isinstance(data, bytes) else data.encode())

    def env(self, role="system", rel="playbooks/a.md", digest=None, data=b"the prompt\n"):
        return {"FLOTILLA_PROMPTS": json.dumps({role: {"path": rel, "sha256": digest or sha(data)}})}

    def test_load_declared(self):
        self.put("playbooks/a.md", "the prompt\n")
        p = prompts.load("system", "f", self.root, self.env())
        self.assertEqual((p["role"], p["path"], p["text"], p["sha256"]),
                         ("system", "playbooks/a.md", "the prompt\n", sha(b"the prompt\n")))

    def test_a_changed_file_is_refused(self):
        self.put("playbooks/a.md", "the prompt, edited\n")
        with self.assertRaisesRegex(prompts.PromptError, "refused"):
            prompts.load("system", "f", self.root, self.env())

    def test_without_flotillad_the_default_loads_unverified(self):
        self.put("playbooks/a.md", "anything\n")
        self.assertEqual(prompts.load("system", "f", self.root, {}, default="playbooks/a.md")["text"], "anything\n")
        with self.assertRaisesRegex(prompts.PromptError, "no default"):
            prompts.load("system", "f", self.root, {})

    def test_the_spec_wins_over_the_default(self):
        self.put("playbooks/a.md", "the prompt\n")
        self.put("playbooks/b.md", "other\n")
        p = prompts.load("system", "f", self.root, self.env(), default="playbooks/b.md")
        self.assertEqual(p["path"], "playbooks/a.md")

    def test_a_role_the_spec_does_not_declare(self):
        self.put("playbooks/a.md", "x\n")
        with self.assertRaisesRegex(prompts.PromptError, "declares none"):
            prompts.load("rules", "f", self.root, self.env(), default="playbooks/a.md")

    def test_refusals(self):
        self.put("playbooks/empty.md", " \n")
        self.put("playbooks/latin1.md", b"caf\xe9\n")
        self.put("playbooks/big.md", "x" * (prompts.MAX_BYTES + 1))
        os.makedirs(os.path.join(self.root, "elsewhere"))
        with open(os.path.join(self.root, "elsewhere", "s.md"), "w") as f:
            f.write("outside\n")
        os.symlink(os.path.join(self.root, "elsewhere", "s.md"), os.path.join(self.dir, "playbooks", "link.md"))
        os.symlink(os.path.join(self.root, "elsewhere"), os.path.join(self.dir, "linked"))
        os.makedirs(os.path.join(self.dir, "playbooks", "dir.md"))
        for rel, want in (("playbooks/none.md", "not a file"), ("playbooks/empty.md", "empty"),
                          ("playbooks/latin1.md", "not UTF-8"), ("playbooks/big.md", "never trimmed"),
                          ("playbooks/link.md", "symlink"), ("linked/s.md", "symlink"),
                          ("playbooks/dir.md", "not a file"), ("../g/x.md", "inside the fleet"),
                          ("/etc/passwd.md", "inside the fleet"), ("a/../b.md", "inside the fleet"),
                          ("playbooks/a.py", "inside the fleet")):
            with self.subTest(rel), self.assertRaisesRegex(prompts.PromptError, want):
                prompts.load("system", "f", self.root, {}, default=rel)
        with self.assertRaisesRegex(prompts.PromptError, "role"):
            prompts.load("Bad", "f", self.root, {}, default="playbooks/a.md")


if __name__ == "__main__":
    unittest.main(verbosity=1)
