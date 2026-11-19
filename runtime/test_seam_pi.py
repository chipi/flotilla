"""python3 runtime/test_seam_pi.py: pi's seam end-to-end (runtime/HARNESS_SEAMS.md): the real pi, run through the station driver for
stations of every `tools` value against a scripted model endpoint. The reference every next harness's test_seam_<name>.py follows.
Skipped, with the reason, when pi is not on PATH (CI's image has none: runtime/test_seam_fakeh.py runs the same suite there)."""
import glob
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import seam_e2e as se  # noqa: E402


class PiSeams(se.SeamSuite):
    NAME = "pi"
    READ_TOOLS = ("find", "grep", "ls", "read")
    WRITE_TOOLS = ("bash", "edit", "write")
    SESSIONS = "pi/sessions/*/*.jsonl"

    @classmethod
    def session_ref_after_kill(cls, run_dir):
        files = sorted(glob.glob(os.path.join(run_dir, cls.SESSIONS)))
        with open(files[-1]) as f:
            return json.loads(f.readline())["id"]

    # KNOWN HOLE (HARNESS_SEAMS.md, S5): a process pi's bash tool starts and detaches (found 2026-11-19: `nohup sleep 300 &` after the turn
    # cap's kill is alone, ppid 1, in a session of its own, outside the group the runner kills) outlives the episode. Everything else
    # of S5 holds. This stays an expected failure until the runner stops such a process too; a pass then fails the suite, so the
    # marker cannot be forgotten.
    @unittest.expectedFailure
    def test_s5_a_process_a_tool_detaches_is_not_left_either(self):
        super().test_s5_a_process_a_tool_detaches_is_not_left_either()


if __name__ == "__main__":
    unittest.main()
