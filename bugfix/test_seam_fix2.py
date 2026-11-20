"""python3 bugfix/test_seam_fix2.py: the seam end-to-end of bugfix/fix2.py (runtime/HARNESS_SEAMS.md, S1 to S11), the real pi run by the
chain of a version that declares stations, against the scripted gateway, for the roles fix, repro and review: the checks of
test_seam_pi.py (which runs bugfix/fix.py) over fix2.py. The chain's models, texts and limits come from FLOTILLA_STATIONS here, not
from flags (bugfix/differential.py builds the document from the flags the seam runs name). Skipped, with the reason, when pi is not on PATH."""
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import differential  # noqa: E402
import fix  # noqa: E402
import fix2  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))


def _load(path):
    """bugfix/test_seam_pi.py by its path: runtime/test_seam_pi.py is another file of the same name, and runtime/ is ahead of bugfix/ on
    the path once the harness layer is imported."""
    import importlib.util
    spec = importlib.util.spec_from_file_location("bugfix_test_seam_pi", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


base = _load(os.path.join(HERE, "test_seam_pi.py"))
sys.path.insert(0, HERE)   # the suite imports test_harness by name when it runs: bugfix/'s, not runtime/'s


class Fix2Seams(base.PiSeams):
    @classmethod
    def declared_rules(cls, marker, honest):
        """A spec that declares its `rules` text on its stations: the code root carries the version's files, and the stations name
        playbooks/rules.md with the hash flotillad loaded (or one it did not)."""
        env = super().declared_rules(marker, honest)
        differential.code_root(env["FLOTILLA_CODE_ROOT"])
        return env

    @classmethod
    def stations(cls, f, args):
        """(the chain's flags without the ones the stations say, FLOTILLA_STATIONS, code root) for a run that v1 would take `args` for."""
        declared = f.extra_env.get("FLOTILLA_PROMPTS")
        code = f.extra_env.get("FLOTILLA_CODE_ROOT") or differential.code_root(os.path.join(f.d, "code"))
        flags, _, settings = differential.translate(args, "")
        doc = differential.stations_doc(settings, code)
        if declared:
            for st in doc["stations"].values():
                if "rules" in st["texts"]:
                    st["texts"]["rules"] = json.loads(declared)["rules"]
        return flags, doc, code

    @classmethod
    def environment(cls, f, doc, code):
        env = dict(f.extra_env, FLOTILLA_STATIONS=json.dumps(doc), FLOTILLA_CODE_ROOT=code, FLOTILLA_ROUTINE="bugfix/fix")
        env.pop("FLOTILLA_PROMPTS", None)
        return env

    @classmethod
    def common(cls, f):
        return ["--repo", f.repo, "--setup", "true", "--test-cmd", f"python3 {f.suite} run %s",
                "--related-cmd", f"python3 {f.suite} related %s", "--scratch", f.scratch]

    @classmethod
    def run_main(cls, f, args):
        flags, doc, code = cls.stations(f, args)
        plan_path = os.path.join(f.d, "plan.json")
        env = dict(os.environ, FLOTILLA_PLAN_FILE=plan_path, FAKE_STATE=f.state, GIT_ALLOW_PROTOCOL="file", GIT_REMOTE_BASE=f.remotes,
                   PATH=os.path.join(f.d, "bin") + os.pathsep + os.environ["PATH"], GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t",
                   GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_CASE=json.dumps({"id": "case-20261115-abc", "kind": "code_bug", "target": f"repo:{f.repo}/issues/12",
                                             "data": {"issue": {"repo": f.repo, "number": 12, "title": "value is 1",
                                                                "body": "value() returns 1, want 2", "url": "u"}}}),
                   **cls.environment(f, doc, code))
        old, fix.BAKEOFF = fix.BAKEOFF, f.bakeoff
        try:
            out = fix2.main([*cls.common(f), *flags], environ=env)
        finally:
            fix.BAKEOFF = old
        with open(plan_path) as fh:
            return json.load(fh), out

    @classmethod
    def killed_setup(cls, f):
        _, doc, code = cls.stations(f, [])
        f.extra_env.update(cls.environment(f, doc, code))

    @classmethod
    def killed_command(cls, f):
        return [sys.executable, os.path.join(HERE, "fix2.py"), *cls.common(f), *cls.flags()]

    def test_the_chain_ran_on_fix2_and_not_on_the_leaves(self):
        """No bash leaf was started for the fix or the reproducer: every pi call of the run was the adapter's (the prompt on stdin)."""
        main = self.runs["main"]
        self.assertEqual(len(main.invocations), 5, "repro, fix, review, fix (resumed), review")
        for inv in main.invocations:
            self.assertNotIn(main.review_prompts[0], inv["argv"], "no prompt in argv")
            self.assertFalse(any(a.startswith(("You are fixing a bug", "Review this automated fix")) for a in inv["argv"]))


if __name__ == "__main__":
    unittest.main()
