"""python3 bugfix/test_stepone.py: RFC-0013 §1's step one (bugfix/stepone.py). Its proof is a test, not a reading: the
four orrery issues phase A's plans were judged on, as filed (testdata/orrery-*.txt), against a repo with the parts of
orrery's main of 2026-11-16 (28548a3) they touch. #651 and #659 must close, #648 and #656 must not."""
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import stepone  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))

# what orrery's main had on 2026-11-16 (28548a3), only the parts the four issues touch
FILES = {
    "src/lib/components/StarPanel.svelte": "<script>\n  import * as m from '$lib/paraglide/messages';\n  const tab = m.panel_tab_info();\n</script>\n",
    "messages/en-US.json": '{\n  "surface_true_scale_short": "True-scale ground view · orbital imagery",\n  "panel_tab_info": "Info"\n}\n',
    "src/lib/surface-scene/SurfaceScene.svelte": "<script>\n  import { WebGLUnavailableError } from './renderer';\n"
                                                 "  const label = m.surface_true_scale_short();\n</script>\n",
    "src/lib/observability/sentry.ts": "// WebGLUnavailableError: the device cannot start a 3D context\n",
    "tests/unit/only-a-test.test.ts": "it('calls ghost_in_tests', () => ghost_in_tests());\n",
}


def issue(n):
    with open(os.path.join(HERE, "testdata", f"orrery-{n}.txt")) as f:
        return f.read()


class StepOne(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.d = tempfile.mkdtemp()
        for path, text in FILES.items():
            os.makedirs(os.path.join(cls.d, os.path.dirname(path)), exist_ok=True)
            with open(os.path.join(cls.d, path), "w") as f:
                f.write(text)
        for args in (["init", "-q"], ["add", "-A"], ["-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base"]):
            subprocess.run(["git", "-C", cls.d, *args], check=True, capture_output=True)
        cls.base = subprocess.run(["git", "-C", cls.d, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.d, ignore_errors=True)

    def check(self, text):
        return stepone.check(text, self.d, self.base)

    def test_659_closes_the_failing_call_is_not_on_main(self):
        decision, ev = self.check(issue(659))
        self.assertEqual(decision, "close", ev)
        self.assertIn("the failing code is not on main: panel_tab_sky appears nowhere", ev)
        self.assertIn("searched src/lib/components/StarPanel.svelte and the tree", ev)

    def test_651_closes_what_is_missing_is_on_main(self):
        decision, ev = self.check(issue(651))
        self.assertEqual(decision, "close", ev)
        self.assertIn("what the error says is missing is on main", ev)
        self.assertIn("surface_true_scale_short at", ev)
        self.assertNotIn("_index.js", ev.split("searched")[0], "a bundled path is never searched")

    def test_648_and_656_are_not_checked(self):
        # a library class, an app error class and bundle paths: nothing the error calls missing
        for n in (648, 656):
            decision, ev = self.check(issue(n))
            self.assertEqual((decision, ev), ("go", "step one: not checked (no name the error calls missing)"), n)

    def test_a_name_found_only_in_a_test_counts_as_absent(self):
        decision, ev = self.check("TypeError: x.ghost_in_tests is not a function")
        self.assertEqual(decision, "close", ev)
        self.assertIn("the failing code is not on main: ghost_in_tests appears nowhere", ev)

    def test_some_names_present_some_absent_goes_on(self):
        decision, ev = self.check("TypeError: m.panel_tab_info is not a function; ReferenceError: nowhere_fn is not defined")
        self.assertEqual(decision, "go", ev)
        self.assertIn("panel_tab_info found, nowhere_fn absent", ev)

    def test_python_shapes(self):
        self.assertEqual(stepone.names_of("NameError: name 'load_cfg' is not defined"), ["load_cfg"])
        self.assertEqual(stepone.names_of("AttributeError: 'Feed' object has no attribute 'episodes_v2'"), ["episodes_v2"])

    def test_bundled_paths_are_never_named(self):
        files, absent = stepone.files_of("at /orrery/_app/immutable/nodes/33.FFOEfYOl.js:275 and chunks/BRzXeRZN2.js",
                                         self.d, self.base)
        self.assertEqual((files, absent), ([], []))
        # a src/ path the path pattern takes, but bundled or vendored: dropped by name
        files, absent = stepone.files_of("from src/build/app.DkT9x2Qa.js via src/node_modules/three/build/three.module.js",
                                         self.d, self.base)
        self.assertEqual((files, absent), ([], []))


if __name__ == "__main__":
    unittest.main()
