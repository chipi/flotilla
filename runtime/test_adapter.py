"""python3 runtime/test_adapter.py: the harness layer (runtime/adapter.py and the adapters, runtime/harness_<name>.py) as a
platform part: it stands on runtime/ alone, whichever fleet's image carries it (v2-design.md section 3). The bugfix
chain's use of it is tested in bugfix/test_harness.py and bugfix/test_episodes.py.
"""
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import adapter  # noqa: E402


def run_isolated(code):
    """`code` in a fresh interpreter whose only import path is runtime/ (and the standard library), from an empty cwd:
    what a worker image that carries runtime/ and no fleet code has."""
    with tempfile.TemporaryDirectory() as cwd:
        env = {"PATH": os.environ.get("PATH", ""), "PYTHONPATH": HERE, "HOME": cwd, "PYTHONDONTWRITEBYTECODE": "1"}
        p = subprocess.run([sys.executable, "-c", code], cwd=cwd, env=env, capture_output=True, text=True, timeout=60)
    return p.returncode, p.stdout, p.stderr


class Standalone(unittest.TestCase):
    def test_the_layer_loads_with_runtime_alone_and_pulls_in_no_fleet_code(self):
        code = ("import sys, json, adapter\n"
                "pi = adapter.get('pi')\n"
                "print(json.dumps({'name': pi.name, 'bugfix': sorted(m for m in sys.modules if m in "
                "('fix', 'callrec_pi', 'harness_prompts', 'declared_prompts', 'pi_config', 'spend', 'stepone'))}))\n")
        rc, out, err = run_isolated(code)
        self.assertEqual(rc, 0, err)
        self.assertEqual(json.loads(out), {"name": "pi", "bugfix": []})

    def test_the_adapters_are_the_runtime_files_named_harness_name(self):
        names = adapter.names()
        self.assertIn("pi", names)
        for n in names:
            self.assertTrue(os.path.exists(os.path.join(HERE, f"harness_{n}.py")), n)

    def test_the_shared_module_is_not_the_in_process_loop(self):
        """runtime/harness.py (triage's loop, `import harness` in signal-fleet and test_hooks) keeps its name; the
        adapter layer is `adapter`, so neither shadows the other."""
        import harness
        self.assertTrue(hasattr(harness, "run_json_step"))
        self.assertFalse(hasattr(adapter, "run_json_step"))
        self.assertTrue(hasattr(adapter, "execute"))
        self.assertFalse(hasattr(harness, "execute"))


class Images(unittest.TestCase):
    def test_the_bugfix_worker_image_carries_runtime(self):
        """bugfix/fix.py imports the layer from ../runtime: /opt/flotilla/runtime beside /opt/flotilla/bugfix in the image."""
        with open(os.path.join(HERE, "..", "deploy", "docker", "worker-bugfix.Dockerfile")) as f:
            lines = [ln.strip() for ln in f]
        self.assertIn("COPY runtime /opt/flotilla/runtime", lines)
        self.assertIn("COPY bugfix /opt/flotilla/bugfix", lines)


if __name__ == "__main__":
    unittest.main()
