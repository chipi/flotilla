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


if __name__ == "__main__":
    unittest.main()
