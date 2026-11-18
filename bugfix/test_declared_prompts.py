"""python3 bugfix/test_declared_prompts.py: bugfix/declared_prompts.py (the bugfix image has no runtime/) accepts and
refuses what runtime/prompts.py does, and verifies the same hash."""
import hashlib
import importlib.util
import json
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import declared_prompts  # noqa: E402


def runtime_prompts():
    spec = importlib.util.spec_from_file_location("runtime_prompts", os.path.join(HERE, "..", "runtime", "prompts.py"))
    mod = importlib.util.module_from_spec(spec)
    sys.path.insert(0, os.path.join(HERE, "..", "runtime"))
    spec.loader.exec_module(mod)
    return mod


class Agreement(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.dir = os.path.join(self.root, "fleets", "bugfix")
        os.makedirs(os.path.join(self.dir, "playbooks"))
        os.makedirs(os.path.join(self.root, "elsewhere"))
        self.rt = runtime_prompts()

    def put(self, rel, data):
        with open(os.path.join(self.dir, rel), "wb") as f:
            f.write(data if isinstance(data, bytes) else data.encode())

    def both(self, rel, sha=None, data=b"x\n"):
        env = {"FLOTILLA_PROMPTS": json.dumps({"rules": {"path": rel, "sha256": sha or hashlib.sha256(data).hexdigest()}})}
        out = []
        for fn in (lambda: declared_prompts.resolve("rules", env, self.root, "bugfix"),
                   lambda: self.rt.load("rules", "bugfix", self.root, env)["path"]):
            try:
                fn()
                out.append("ok")
            except (declared_prompts.PromptError, self.rt.PromptError) as e:
                out.append(str(e))
        return out

    def test_the_same_verdict_on_the_same_files(self):
        self.put("playbooks/ok.md", "x\n")
        self.put("playbooks/empty.md", " \n")
        self.put("playbooks/latin1.md", b"caf\xe9\n")
        self.put("playbooks/big.md", "x" * (declared_prompts.MAX_BYTES + 1))
        self.put("playbooks/exact.md", "x" * declared_prompts.MAX_BYTES)
        with open(os.path.join(self.root, "elsewhere", "s.md"), "w") as f:
            f.write("outside\n")
        os.symlink(os.path.join(self.root, "elsewhere", "s.md"), os.path.join(self.dir, "playbooks", "link.md"))
        os.symlink(os.path.join(self.root, "elsewhere"), os.path.join(self.dir, "linked"))
        os.makedirs(os.path.join(self.dir, "playbooks", "dir.md"))
        cases = [("playbooks/ok.md", None, b"x\n"), ("playbooks/ok.md", "0" * 64, b"x\n"),
                 ("playbooks/exact.md", None, b"x" * declared_prompts.MAX_BYTES), ("playbooks/none.md", "a" * 64, b"x\n"),
                 ("playbooks/empty.md", "a" * 64, b" \n"), ("playbooks/latin1.md", "a" * 64, b""),
                 ("playbooks/big.md", "a" * 64, b""), ("playbooks/link.md", "a" * 64, b""), ("linked/s.md", "a" * 64, b""),
                 ("playbooks/dir.md", "a" * 64, b""), ("../x.md", "a" * 64, b""), ("/etc/x.md", "a" * 64, b""),
                 ("a/../b.md", "a" * 64, b""), ("playbooks/ok.py", "a" * 64, b"")]
        for rel, sha, data in cases:
            with self.subTest(rel, sha=sha):
                a, b = self.both(rel, sha, data)
                self.assertEqual(a, b)
                self.assertEqual(a == "ok", rel in ("playbooks/ok.md", "playbooks/exact.md") and sha is None, a)

    def test_the_path_it_returns_is_the_file(self):
        self.put("playbooks/ok.md", "x\n")
        env = {"FLOTILLA_PROMPTS": json.dumps({"rules": {"path": "playbooks/ok.md", "sha256": sha256(b"x\n")}})}
        self.assertEqual(declared_prompts.resolve("rules", env, self.root, "bugfix"), os.path.join(self.dir, "playbooks", "ok.md"))

    def test_nothing_declared_is_none(self):
        self.assertIsNone(declared_prompts.resolve("rules", {}, self.root, "bugfix"))
        self.assertIsNone(declared_prompts.resolve("rules", {"FLOTILLA_PROMPTS": json.dumps({"system": {"path": "a.md", "sha256": "x"}})},
                                                    self.root, "bugfix"))


def sha256(b):
    return hashlib.sha256(b).hexdigest()


if __name__ == "__main__":
    unittest.main(verbosity=1)
