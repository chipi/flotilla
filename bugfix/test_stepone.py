"""python3 bugfix/test_stepone.py: RFC-0013 §1's step one (bugfix/stepone.py), advisory (operator, 2026-11-17): it only
ever notes facts, never closes. The four orrery issues phase A's plans were judged on, as filed
(testdata/orrery-*.txt), against a repo with the parts of orrery's main of 2026-11-16 (28548a3) they touch, and the
cases homelab's reviews of the two close-rule versions found closing real bugs."""
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import stepone  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))

# what orrery's main had on 2026-11-16 (28548a3), only the parts the four issues touch, plus homelab's shapes
FILES = {
    "src/lib/components/StarPanel.svelte": "<script>\n  import * as m from '$lib/paraglide/messages';\n  const tab = m.panel_tab_info();\n</script>\n",
    "messages/en-US.json": '{\n  "surface_true_scale_short": "True-scale ground view · orbital imagery",\n  "panel_tab_info": "Info"\n}\n',
    "src/lib/surface-scene/SurfaceScene.svelte": "<script>\n  import { WebGLUnavailableError } from './renderer';\n"
                                                 "  const label = m.surface_true_scale_short();\n</script>\n",
    "src/lib/observability/sentry.ts": "// WebGLUnavailableError: the device cannot start a 3D context\n",
    "tests/unit/only-a-test.test.ts": "it('calls ghost_in_tests', () => ghost_in_tests());\n",
    "src/a.py": "def run():\n    return build_index_x(1)\n",
    "src/b.py": "def build_index_x(n):\n    return n\n",
    "src/events.js": "export function on(evt) {\n  const n = 'handle_' + evt;\n  this[n]();\n}\n",
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

    def note(self, text):
        decision, facts = stepone.check(text, self.d, self.base)
        self.assertEqual(decision, "note", "step one is advisory: it never decides")
        self.assertTrue(facts.startswith("step one (advisory): "), facts)
        return facts

    def test_659_notes_the_name_appears_nowhere(self):
        facts = self.note(issue(659))
        self.assertIn("panel_tab_sky appears nowhere", facts)
        self.assertIn("named files on main: src/lib/components/StarPanel.svelte", facts)

    def test_651_notes_where_the_name_is(self):
        facts = self.note(issue(651))
        self.assertIn("surface_true_scale_short in a named file; at src/lib/surface-scene/SurfaceScene.svelte:3", facts)
        self.assertIn("named files not on main: src/lib/paraglide/messages/_index.js", facts)

    def test_648_and_656_are_not_checked(self):
        for n in (648, 656):
            self.assertEqual(self.note(issue(n)),
                             "step one (advisory): not checked (no project-specific name the error calls missing)", n)

    def test_homelabs_cases_never_close_and_note_the_truth(self):
        # each of these closed a real bug under 444a153 or 0731108
        for text in ("AttributeError: 'NoneType' object has no attribute 'get' in src/a.py",
                     "TypeError: undefined is not a function at src/a.py:2",
                     "TypeError: items.map is not a function at src/a.py:2"):
            self.assertIn("not checked", self.note(text), text)
        # a missing import: called in a.py, defined in b.py; both noted, nothing decided
        facts = self.note("NameError: name 'build_index_x' is not defined\n  File \"src/a.py\", line 2")
        self.assertIn("build_index_x in a named file; at src/a.py:2, src/b.py:1", facts)

    def test_a_name_only_in_a_test_is_not_nowhere(self):
        facts = self.note("TypeError: x.ghost_in_tests is not a function at src/a.py:1")
        self.assertIn("ghost_in_tests not in a named file; at tests/unit/only-a-test.test.ts:1", facts)
        self.assertNotIn("nowhere", facts)

    def test_a_computed_name_is_noted(self):
        facts = self.note("TypeError: this.handle_click is not a function at src/events.js:3")
        self.assertIn("handle_click appears nowhere", facts)
        self.assertIn("a named file computes names", facts)

    def test_a_git_failure_is_not_checked(self):
        decision, facts = stepone.check(issue(659), self.d, "0" * 40)   # a base the repo does not have
        self.assertEqual(decision, "note")
        self.assertIn("step one (advisory): not checked (git ls-tree: exit", facts)

    def test_python_shapes(self):
        self.assertEqual(stepone.names_of("NameError: name 'load_cfg' is not defined"), ["load_cfg"])
        self.assertEqual(stepone.names_of("AttributeError: 'Feed' object has no attribute 'episodes_v2'"), [])

    def test_bundled_paths_are_never_named(self):
        files, absent = stepone.files_of("at /orrery/_app/immutable/nodes/33.FFOEfYOl.js:275 and chunks/BRzXeRZN2.js",
                                         self.d, self.base)
        self.assertEqual((files, absent), ([], []))
        files, absent = stepone.files_of("from src/build/app.DkT9x2Qa.js via src/node_modules/three/build/three.module.js",
                                         self.d, self.base)
        self.assertEqual((files, absent), ([], []))


if __name__ == "__main__":
    unittest.main()
