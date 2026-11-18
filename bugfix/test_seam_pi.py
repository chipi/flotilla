"""python3 bugfix/test_seam_pi.py: pi's seam end-to-end (bugfix/HARNESS_SEAMS.md): the real pi, run by the chain against a
scripted model endpoint, for the roles fix, repro and review. The reference every next harness's test_seam_<name>.py follows.
Skipped, with the reason, when pi is not on PATH."""
import json
import os
import subprocess
import sys
import tempfile
import shutil
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pi_config  # noqa: E402
import seam_e2e as se  # noqa: E402


class PiSeams(se.SeamSuite):
    NAME = "pi"
    BINARY = "pi"
    READ_TOOLS = ("find", "grep", "ls", "read")
    WRITE_TOOLS = ("bash", "edit", "write")
    SESSIONS = "pi/sessions/*/*.jsonl"

    @classmethod
    def control_sees_other_hosts(cls):
        """pi pointed at a gateway that is not on NO_PROXY: its request must reach the recording proxy."""
        proxy = se.RecordingProxy()
        d = tempfile.mkdtemp()
        try:
            wt = os.path.join(d, "wt")
            os.makedirs(wt)
            subprocess.run(["git", "init", "-q"], cwd=wt, check=True)
            kf = os.path.join(d, "key")
            with open(kf, "w") as f:
                f.write(se.KEY + "\n")
            models = {"endpoints": {"gw": {"base_url": "http://gateway.invalid:4001/v1", "key_file": kf}},
                      "aliases": {"a": {"endpoint": "gw", "model": "fleet-a"}}}
            agent = os.path.join(d, "pi")
            env = dict(os.environ, FLOTILLA_MODELS=json.dumps(models), PI_CODING_AGENT_DIR=agent, HOME=d, PI_OFFLINE="1",
                       PI_SKIP_VERSION_CHECK="1", **cls.env(None, proxy, kf))
            pi_config.write(agent, ["a"], env, {}, meta={})
            subprocess.run([cls.real, "-p", "--mode", "json", "--provider", "flotilla-gw", "--model", "fleet-a", "hi"], cwd=wt,
                           env=env, capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=120)
            return any("gateway.invalid" in x for x in proxy.seen)
        finally:
            proxy.close()
            shutil.rmtree(d, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
