"""python3 bugfix/test_harness.py: the bugfix chain's use of the platform's harness layer (runtime/adapter.py, runtime/harness_pi.py):
the review's call as the chain has always sent it (pi_review.py), the chain's reading of pi's streams, fix.sh as the shared
runner, and the chain's episode file. The layer's own tests are runtime/test_adapter.py; the fake harness and the contract
check every adapter's samples pass are runtime/fakeh.py (re-exported here: test_episodes.py uses them).
"""
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
sys.path.append(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "runtime"))
import adapter  # noqa: E402
import fix  # noqa: E402
import harness_pi  # noqa: E402
import pi_review  # noqa: E402
from fakeh import FAKE_CLI, FakeH, check_contract, install_fake, request  # noqa: E402,F401

HERE = os.path.dirname(os.path.abspath(__file__))
PI_STREAMS = os.path.join(HERE, "testdata", "pi")


class Runner(unittest.TestCase):
    def test_fix_sh_is_this_runner_with_pi_s_turn_test_by_default(self):
        import tempfile
        turn = json.dumps({"type": "message_end", "message": {"role": "assistant", "usage": {"output": 1}}})
        code, out = fix.sh(["sh", "-c", f"for i in 1 2 3 4 5; do echo '{turn}'; done; sleep 30"], ".", dict(os.environ), 60, max_turns=2)
        self.assertEqual(code, fix.TURN_CAPPED)
        with self.assertRaises(fix.Stop):
            fix.sh(["sh", "-c", "exit 3"], ".", dict(os.environ), 10, check=True)
        d = tempfile.mkdtemp()
        tee = os.path.join(d, "t")
        self.assertEqual(fix.sh(["echo", "x"], ".", dict(os.environ), 10, tee_path=tee), (0, "x\n"))
        with open(tee) as f:
            self.assertEqual(f.read(), "x\n")


class ChainReview(unittest.TestCase):
    """The review as Chain.review has always called pi: the platform's generic adapter is beside this, not under it."""

    def test_the_reviews_command_is_what_the_chain_always_sent(self):
        route = adapter.ModelRoute(alias="r", model="fleet-r", base_url="u", endpoint="gw", meta={"pi_provider": "flotilla-gw"})
        head = ["pi", "-p", "--mode", "json", "--provider", "flotilla-gw", "--model", "fleet-r"]
        for tools, rules, want in (
                ("none", None, ["--no-tools"]),
                ("none", "/r.md", ["--no-tools", "--no-context-files"]),
                ("read", None, ["--tools", "read,grep,find,ls"]),
                ("read", "/r.md", ["--tools", "read,grep,find,ls", "--no-context-files", "--append-system-prompt", "/r.md"])):
            req = request("review", model=route, tools=tools, system_path=rules, workdir="/wt", prompt="PROMPT")
            inv = pi_review.ChainReview().build(req)
            self.assertEqual((inv.argv, inv.env, inv.stdin, inv.cwd, inv.files), (head + want + ["PROMPT"], {}, None, "/wt", {}), (tools, rules))
            self.assertEqual(pi_review.ChainReview().phone_home_env(), {}, "the worker image sets them, as it always did")

    def test_the_provider_is_the_endpoints_unless_the_chain_names_one(self):
        route = adapter.ModelRoute(alias="r", model="m", base_url="u", endpoint="gw")
        self.assertEqual(pi_review.ChainReview().build(request("review", model=route, tools="none")).argv[5], "flotilla-gw")

    def test_the_review_is_pi_to_the_layer_and_to_the_checks(self):
        r = pi_review.ChainReview()
        self.assertEqual((r.name, r.tools_served), ("pi", harness_pi.PI.tools_served))
        self.assertEqual(adapter.capabilities(r), dict(adapter.capabilities(harness_pi.PI), prompt_via_stdin=False))


class PiStreams(unittest.TestCase):
    def stream(self, name):
        with open(os.path.join(PI_STREAMS, name)) as f:
            return f.read()

    def test_what_the_chain_reads_of_a_stream_is_the_adapters_answer_error_and_turns(self):
        for name in ("stream_ok.jsonl", "stream_error.jsonl", "stream_error_bare.jsonl", "stream_capped.jsonl"):
            s = self.stream(name)
            ep = harness_pi.PI.parse(s, {})
            with self.subTest(name):
                check_contract(self, ep, harness_pi.PI)
                self.assertEqual(ep.answer, fix.assistant_text(s))
                self.assertEqual(ep.error, fix.stream_error(s))
                turns, out = fix.usage(s)
                self.assertEqual((len(ep.turns), (ep.usage_total or {}).get("output", 0)), (turns, out))
                self.assertEqual(ep.exit_reason, "error" if ep.error else "completed")

    def test_a_turn_carries_its_tools_usage_and_cost(self):
        ep = harness_pi.PI.parse(self.stream("stream_ok.jsonl"), {})
        t = ep.turns[0]
        self.assertEqual((t.n, t.text, t.stop, t.cost_est), (1, "Looking at the code.", "tool_calls", 0.001))
        self.assertEqual(t.tools, [{"name": "read", "arg": "src/a.js", "args": {"path": "src/a.js"}, "result": "module.exports = () => 1;", "ok": True}])
        self.assertEqual([{k: x[k] for k in ("name", "arg", "ok")} for x in t.tools], [{"name": "read", "arg": "src/a.js", "ok": True}],
                         "what the chain's calls file reads of a tool is unchanged")
        self.assertEqual(t.usage, adapter.usage_dict(100, 12))
        self.assertEqual(ep.session_id, "s-ok")
        self.assertEqual(ep.answer, "Fixed: value() now returns 2.")

    def test_a_failed_turn_says_why(self):
        ep = harness_pi.PI.parse(self.stream("stream_error.jsonl"), {})
        self.assertEqual((ep.turns[-1].stop, ep.turns[-1].error), ("error", "400: UnsupportedParamsError reasoning_effort"))
        self.assertEqual(ep.exit_reason, "error")

    def test_garbage_and_nothing_parse_to_an_empty_episode(self):
        for s in ("", None, "not json\n[1,2]\n{\"type\": 3}\n"):
            ep = harness_pi.PI.parse(s, {})
            check_contract(self, ep, harness_pi.PI)
            self.assertEqual((ep.answer, ep.turns), ("", []))

    def test_the_turn_test_is_the_one_the_cap_always_used(self):
        end = json.dumps({"type": "message_end", "message": {"role": "assistant"}})
        self.assertTrue(harness_pi.turn_ended(end))
        for line in (json.dumps({"type": "message_end", "message": {"role": "user"}}), json.dumps({"type": "message_update"}),
                     "message_end", '["message_end"]', "", '{"type": "message_end"'):
            self.assertFalse(harness_pi.turn_ended(line), line)


class EpisodeFile(unittest.TestCase):
    def test_the_chain_s_episode_file_is_version_1_and_says_role_as_it_always_did(self):
        ep = adapter.Episode(harness="fakeh", station="fix", session_id="s")
        doc = fix.episode_file(ep, "bugfix-flash", 2, 0, "ref")
        self.assertEqual((doc["version"], doc["role"], "station" in doc), (1, "fix", False))
        self.assertEqual(list(doc), ["harness", "role", "session_id", "turns", "answer", "error", "exit_reason", "usage_total", "cost_est",
                                     "raw_path", "notes", "version", "alias", "seq", "code", "resume_ref"], "the keys keep their order")
        self.assertEqual(adapter.Episode.from_dict(doc).station, "fix", "and the platform reads it back")


if __name__ == "__main__":
    unittest.main()
