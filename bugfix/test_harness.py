"""python3 bugfix/test_harness.py: the harness interface (bugfix/harness.py), pi's adapter (harness_pi.py) and the chain's
handling of an episode, with a fake harness ("fakeh") that exists only here.

An adapter's own tests live in test_harness_<name>.py and use check_contract() below on every sample stream.
"""
import json
import os
import shutil
import sys
import tempfile
import textwrap
import time
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fix  # noqa: E402
import harness  # noqa: E402
import harness_pi  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
PI_STREAMS = os.path.join(HERE, "testdata", "pi")


def check_contract(tc, ep, h=None):
    """What every adapter's parse() promises (the top of harness.py, point 5): used on each sample stream by every
    adapter's tests."""
    tc.assertIsInstance(ep, harness.Episode)
    if h is not None:
        tc.assertEqual(ep.harness, h.name)
    tc.assertIn(ep.exit_reason, harness.EXIT_REASONS)
    tc.assertIsInstance(ep.answer, str)
    tc.assertEqual(ep.answer, ep.answer.strip(), "the answer is stripped")
    tc.assertIsInstance(ep.error, str)
    tc.assertEqual([t.n for t in ep.turns], list(range(1, len(ep.turns) + 1)), "turns are numbered from 1")
    for t in ep.turns:
        tc.assertIsInstance(t.text, str)
        tc.assertIsInstance(t.stop, str)
        tc.assertTrue(t.cost_est is None or t.cost_est >= 0)
        if t.usage is not None:
            tc.assertEqual(sorted(t.usage), sorted(harness.USAGE_KEYS))
            tc.assertTrue(all(isinstance(v, int) and v >= 0 for v in t.usage.values()), t.usage)
        for tool in t.tools:
            tc.assertIsInstance(tool["name"], str)
            tc.assertIsInstance(tool["arg"], str)
            tc.assertTrue("ok" not in tool or isinstance(tool["ok"], bool))
    tc.assertEqual(ep.usage_total, harness.sum_usage(ep.turns))
    tc.assertEqual(harness.Episode.from_dict(json.loads(json.dumps(ep.to_dict()))), ep, "an episode survives its JSON")


# the fake harness: the native format is the normalized events as JSON lines, one {"event": "turn", ...} per model call
# and a closing {"event": "final", ...}. Its CLI (a script) takes the prompt on stdin, takes `--resume <session>`, and
# does what FAKE_STATE/fakeh_script says for the call: a JSON line {"turns": [...], "files": {path: text}, "answer",
# "error", "sleep", "exit"}; without a line, one turn that says ok.
FAKE_CLI = textwrap.dedent('''\
    import json, os, sys, time
    state = os.environ["FAKE_STATE"]
    args = sys.argv[1:]
    prompt = sys.stdin.read()
    resume = args[args.index("--resume") + 1] if "--resume" in args else ""
    path = os.path.join(state, "fakeh_script")
    step = {}
    if os.path.exists(path):
        lines = open(path).read().splitlines()
        if lines:
            step = json.loads(lines[0])
            open(path, "w").write("".join(x + "\\n" for x in lines[1:]))
    with open(os.path.join(state, "fakeh_calls"), "a") as f:
        f.write(json.dumps({"argv": args, "stdin": prompt, "cwd": os.getcwd(), "key": os.environ.get("FAKEH_KEY", ""),
                            "home": os.environ.get("HOME", "")}) + "\\n")
    n = len([x for x in os.listdir(state) if x.startswith("fakeh_sess_")])
    sid = resume or f"fakeh-sess-{n + 1}"
    open(os.path.join(state, "fakeh_sess_" + sid), "a").write("episode\\n")
    for p, text in (step.get("files") or {}).items():
        os.makedirs(os.path.dirname(os.path.join(os.getcwd(), p)) or ".", exist_ok=True)
        open(os.path.join(os.getcwd(), p), "w").write(text)
    turns = step.get("turns")
    turns = [{"text": "ok"}] if turns is None else turns
    for i, t in enumerate(turns, 1):
        print(json.dumps(dict({"event": "turn", "stop": "tool_calls", "model": "fakeh-m"}, **t)), flush=True)
    time.sleep(step.get("sleep", 0))
    if step.get("exit", 0) != 0:
        sys.exit(step["exit"])
    print(json.dumps({"event": "final", "session": sid, "answer": step.get("answer", "done"), "error": step.get("error", "")}), flush=True)
    ''')


class FakeH(harness.Harness):
    name = "fakeh"
    roles = frozenset(harness.ROLES)
    can_resume = True
    enforces_readonly = False

    def __init__(self, cli):
        self.cli = cli

    def build(self, req):
        argv = [sys.executable, self.cli, "--model", req.model.model, "--tools", req.tools]
        if req.resume:
            argv += ["--resume", req.resume]
        return harness.Invocation(argv=argv, env={"FAKEH_KEY": harness.read_key(req.model, os.environ)}, stdin=req.prompt,
                                  files={"fakeh/config.json": json.dumps({"alias": req.model.alias})},
                                  notes=["fakeh: rules not delivered"] if req.rules_path else [])

    def turn_ended(self, line):
        return '"event": "turn"' in line

    def parse(self, stream_text, files):
        turns, final = [], {}
        for line in (stream_text or "").splitlines():
            try:
                e = json.loads(line)
            except ValueError:
                continue
            if not isinstance(e, dict):
                continue
            if e.get("event") == "turn":
                u = e.get("usage")
                turns.append(harness.Turn(n=len(turns) + 1, model=e.get("model"), text=e.get("text", ""), tools=e.get("tools") or [],
                                          usage=harness.usage_dict(**u) if u else None, cost_est=e.get("cost"), stop=e.get("stop", "")))
            elif e.get("event") == "final":
                final = e
        answer = (final.get("answer") or next((t.text for t in reversed(turns) if t.text.strip()), "")).strip()
        err = final.get("error") or ("" if final else "no final event")
        return harness.Episode(harness="fakeh", role="", session_id=final.get("session"), turns=turns, answer=answer, error=err,
                               exit_reason="error" if err else "completed", usage_total=harness.sum_usage(turns),
                               cost_est=harness.sum_cost(turns))


def install_fake(tc):
    """Register the fake harness for this test (and take it away again); returns (harness, its CLI's path)."""
    d = tempfile.mkdtemp()
    tc.addCleanup(shutil.rmtree, d, True)
    cli = os.path.join(d, "fakeh_cli.py")
    with open(cli, "w") as f:
        f.write(FAKE_CLI)
    h = FakeH(cli)
    harness.register(h)
    tc.addCleanup(harness.unregister, "fakeh")
    return h, cli


def request(role="fix", **kw):
    route = harness.ModelRoute(alias="bugfix-flash", model="fleet-bugfix-flash", base_url="http://127.0.0.1:9/v1",
                               endpoint="gateway-bugfix", key_file="/dev/null", meta=kw.pop("meta", {}))
    base = dict(role=role, prompt="fix the bug", rules_path=None, model=route, tools="all", readonly=False, resume=None,
                max_turns=0, workdir=".", run_dir=".", timeout=60)
    base.update(kw)
    return harness.RoleRequest(**base)


class Types(unittest.TestCase):
    def test_an_episode_survives_its_json(self):
        ep = harness.Episode(harness="x", role="fix", session_id="s", answer="done", cost_est=0.5, notes=["n"],
                             turns=[harness.Turn(n=1, model="m", text="t", tools=[{"name": "read", "arg": "a", "ok": True}],
                                                 usage=harness.usage_dict(1, 2), cost_est=0.5, stop="stop")],
                             usage_total=harness.usage_dict(1, 2))
        self.assertEqual(harness.Episode.from_dict(json.loads(json.dumps(ep.to_dict()))), ep)

    def test_usage_is_summed_over_the_turns_that_report_it(self):
        t = [harness.Turn(n=1, usage=harness.usage_dict(1, 2, 3, 4, 5)), harness.Turn(n=2), harness.Turn(n=3, usage=harness.usage_dict(10, 20))]
        self.assertEqual(harness.sum_usage(t), {"input": 11, "output": 22, "cache_read": 3, "cache_write": 4, "reasoning": 5})
        self.assertIsNone(harness.sum_usage([harness.Turn(n=1)]))
        self.assertIsNone(harness.sum_cost([harness.Turn(n=1)]))
        self.assertEqual(harness.sum_cost([harness.Turn(n=1, cost_est=0.25), harness.Turn(n=2, cost_est=0.5)]), 0.75)

    def test_a_route_is_read_from_the_models_section(self):
        cfg = {"endpoints": {"gw": {"base_url": "http://h/v1", "key_file": "/k"}},
               "aliases": {"a": {"endpoint": "gw", "model": "m1"}}}
        r = harness.route(cfg, "a", {"x": 1})
        self.assertEqual((r.alias, r.model, r.base_url, r.endpoint, r.key_file, r.key_env, r.meta),
                         ("a", "m1", "http://h/v1", "gw", "/k", "", {"x": 1}))
        with self.assertRaises(harness.HarnessError):
            harness.route(cfg, "nope")
        with self.assertRaises(harness.HarnessError):
            harness.route({"aliases": {"a": {"endpoint": "gone", "model": "m"}}}, "a")

    def test_the_key_comes_from_the_environment_or_the_file_never_elsewhere(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        kf = os.path.join(d, "key")
        with open(kf, "w") as f:
            f.write("sk-file\n")
        self.assertEqual(harness.read_key(harness.ModelRoute("a", "m", "u", key_file=kf), {}), "sk-file")
        self.assertEqual(harness.read_key(harness.ModelRoute("a", "m", "u", key_file=kf, key_env="K"), {"K": "sk-env"}), "sk-env")
        self.assertEqual(harness.read_key(harness.ModelRoute("a", "m", "u", key_file=kf, key_env="K"), {}), "sk-file")
        self.assertEqual(harness.read_key(harness.ModelRoute("a", "m", "u", key_file=os.path.join(d, "none")), {}), "")
        self.assertEqual(harness.read_key(harness.ModelRoute("a", "m", "u"), {}), "")


class Registry(unittest.TestCase):
    def test_pi_is_registered_and_an_unknown_name_is_refused(self):
        self.assertIs(harness.get("pi"), harness_pi.PI)
        self.assertIn("pi", harness.names())
        for bad in ("nope", "", "Pi", "../pi", "prompts", "cli"):
            with self.assertRaises(harness.HarnessError, msg=bad):
                harness.get(bad)

    def test_a_name_and_roles_are_checked_at_registration(self):
        class Bad(harness.Harness):
            name, roles = "Bad Name", frozenset({"fix"})
        with self.assertRaises(harness.HarnessError):
            harness.register(Bad())

        class Odd(harness.Harness):
            name, roles = "odd", frozenset({"fix", "dream"})
        with self.assertRaises(harness.HarnessError):
            harness.register(Odd())

    def test_the_fake_is_there_only_while_a_test_installs_it(self):
        self.assertNotIn("fakeh", harness.names())
        install_fake(self)
        self.assertIs(harness.get("fakeh").name, "fakeh")


class Capabilities(unittest.TestCase):
    DOC = os.path.join(HERE, "HARNESS_SEAMS.md")

    def table(self):
        with open(self.DOC) as f:
            text = f.read()
        body = text.split("<!-- capabilities:begin -->")[1].split("<!-- capabilities:end -->")[0]
        return [ln for ln in body.strip().splitlines() if ln.startswith("|")]

    def test_pi_declares_each_capability_and_a_new_one_defaults_to_no(self):
        got = harness.capabilities(harness_pi.PI)
        self.assertEqual(sorted(got), sorted(harness.CAPABILITIES))
        self.assertEqual(got, {"can_resume": True, "resume_after_kill": True, "enforces_readonly": True, "supports_hooks": False,
                               "reports_cost": True, "reports_usage": True, "supports_turn_cap": True, "native_system_prompt": True,
                               "prompt_via_stdin": False})
        self.assertFalse(any(harness.capabilities(harness.Harness()).values()), "nothing is claimed by default")

    def test_a_step_that_asks_for_what_the_harness_lacks_is_refused_with_the_reason(self):
        pi = harness_pi.PI
        self.assertEqual(harness.missing(pi, {"can_resume": True, "enforces_readonly": True, "roles": ["fix", "review"]}), [])
        self.assertEqual(harness.missing(pi, {"supports_hooks": True}),
                         [f"harness pi does not declare supports_hooks: {harness.CAPABILITIES['supports_hooks']}"])
        self.assertEqual(harness.missing(pi, {"supports_hooks": False}), [], "not asking is not a need")
        self.assertEqual(len(harness.missing(pi, {"prompt_via_stdin": True, "supports_hooks": True})), 2)
        self.assertIn("is not a capability", harness.missing(pi, {"telepathy": True})[0])

        class Mini(harness.Harness):
            name, roles = "mini", frozenset({"fix", "repro"})
        self.assertEqual(harness.missing(Mini(), {"roles": ["fix", "review", "advisor"]}),
                         ["harness mini does not serve the review role", "harness mini does not serve the advisor role"])

    def test_the_seams_doc_table_is_the_adapters(self):
        rows = self.table()
        self.assertEqual([c.strip() for c in rows[0].strip("|").split("|")], ["harness", "roles"] + list(harness.CAPABILITIES))
        self.assertEqual(set(rows[1].strip("|").split("|")), {" --- "}, "a markdown separator row")
        documented = {r.strip("|").split("|")[0].strip(): r for r in rows[2:]}
        for name in harness.names():
            if name in ("pi",):
                self.assertEqual(documented[name], harness.capability_row(harness.get(name)), name)
        self.assertEqual(set(documented) - set(harness.names()), set(), "a row for a harness that is not there")

    def test_the_seams_doc_names_every_seam_and_every_capability(self):
        with open(self.DOC) as f:
            text = f.read()
        for n in range(1, 12):
            self.assertRegex(text, rf"(?m)^### S{n} ")
        for k in harness.CAPABILITIES:
            self.assertIn(k, text)


class Runner(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)

    def test_a_timeout_kills_the_whole_group_and_keeps_what_came_out(self):
        marker = os.path.join(self.d, "alive")
        code, out = harness.run_process(f"echo first; (sleep 3; touch {marker}) & wait", self.d, timeout=1)
        self.assertEqual((code, out), (harness.TIMED_OUT, "first\n"))
        time.sleep(3.5)
        self.assertFalse(os.path.exists(marker), "the grandchild outlived the timeout")

    def test_check_raises_on_a_timeout_and_on_a_failure(self):
        with self.assertRaisesRegex(harness.ProcessFailed, "no result in 1s"):
            harness.run_process("sleep 5", self.d, timeout=1, check=True)
        with self.assertRaisesRegex(harness.ProcessFailed, "exit 3: boom"):
            harness.run_process("echo boom; exit 3", self.d, timeout=5, check=True)

    def test_the_prompt_goes_to_stdin_and_stdin_is_closed(self):
        cmd = [sys.executable, "-c", "import sys; print(sys.stdin.read().upper())"]
        for kw in ({}, {"tee_path": os.path.join(self.d, "s")}):
            self.assertEqual(harness.run_process(cmd, self.d, timeout=10, stdin="hello " * 40000, **kw)[1].strip(), ("HELLO " * 40000).strip())

    def test_the_turn_cap_kills_the_group_and_the_tee_holds_the_stream(self):
        tee = os.path.join(self.d, "episodes", "s.stream")
        script = "for i in 1 2 3 4 5; do echo \"turn $i\"; done; sleep 30; echo late"
        t0 = time.time()
        code, out = harness.run_process(["sh", "-c", script], self.d, timeout=60, max_turns=3,
                                        turn_ended=lambda line: line.startswith("turn"), tee_path=tee)
        self.assertEqual(code, harness.TURN_CAPPED)
        self.assertLess(time.time() - t0, 15)
        self.assertNotIn("late", out)
        with open(tee) as f:
            self.assertEqual(f.read(), out, "what the run printed is what the file holds")
        self.assertGreaterEqual(out.count("turn"), 3)

    def test_a_killed_run_leaves_its_stream(self):
        tee = os.path.join(self.d, "s")
        code, out = harness.run_process(["sh", "-c", "echo a; sleep 30"], self.d, timeout=1, tee_path=tee)
        self.assertEqual((code, out), (harness.TIMED_OUT, "a\n"))
        with open(tee) as f:
            self.assertEqual(f.read(), "a\n")

    def test_a_cap_needs_the_harness_s_own_turn_test(self):
        with self.assertRaises(ValueError):
            harness.run_process(["true"], self.d, max_turns=3)

    def test_a_turn_test_that_raises_does_not_stop_the_reading(self):
        def bad(line):
            raise RuntimeError("x")
        code, out = harness.run_process(["sh", "-c", "seq 1 5000"], self.d, timeout=30, max_turns=3, turn_ended=bad)
        self.assertEqual((code, out.count("\n")), (0, 5000))

    def test_fix_sh_is_this_runner_with_pi_s_turn_test_by_default(self):
        turn = json.dumps({"type": "message_end", "message": {"role": "assistant", "usage": {"output": 1}}})
        code, out = fix.sh(["sh", "-c", f"for i in 1 2 3 4 5; do echo '{turn}'; done; sleep 30"], ".", dict(os.environ), 60, max_turns=2)
        self.assertEqual(code, fix.TURN_CAPPED)
        with self.assertRaises(fix.Stop):
            fix.sh(["sh", "-c", "exit 3"], ".", dict(os.environ), 10, check=True)
        tee = os.path.join(self.d, "t")
        self.assertEqual(fix.sh(["echo", "x"], ".", dict(os.environ), 10, tee_path=tee), (0, "x\n"))
        with open(tee) as f:
            self.assertEqual(f.read(), "x\n")


class PiAdapter(unittest.TestCase):
    def stream(self, name):
        with open(os.path.join(PI_STREAMS, name)) as f:
            return f.read()

    def test_the_reviews_command_is_what_the_chain_always_sent(self):
        route = harness.ModelRoute(alias="r", model="fleet-r", base_url="u", endpoint="gw", meta={"pi_provider": "flotilla-gw"})
        head = ["pi", "-p", "--mode", "json", "--provider", "flotilla-gw", "--model", "fleet-r"]
        for tools, rules, want in (
                ("none", None, ["--no-tools"]),
                ("none", "/r.md", ["--no-tools", "--no-context-files"]),
                ("read", None, ["--tools", "read,grep,find,ls"]),
                ("read", "/r.md", ["--tools", "read,grep,find,ls", "--no-context-files", "--append-system-prompt", "/r.md"])):
            req = request("review", model=route, tools=tools, rules_path=rules, readonly=True, workdir="/wt", prompt="PROMPT")
            inv = harness_pi.PI.build(req)
            self.assertEqual((inv.argv, inv.env, inv.stdin, inv.cwd, inv.files), (head + want + ["PROMPT"], {}, None, "/wt", {}), (tools, rules))

    def test_the_provider_is_the_endpoints_unless_the_chain_names_one(self):
        route = harness.ModelRoute(alias="r", model="m", base_url="u", endpoint="gw")
        self.assertEqual(harness_pi.PI.build(request("review", model=route, tools="none")).argv[5], "flotilla-gw")

    def test_the_other_roles_run_through_pi_s_leaves(self):
        for role in ("fix", "repro", "verdict", "advisor"):
            with self.assertRaises(harness.HarnessError, msg=role):
                harness_pi.PI.build(request(role))

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
        self.assertEqual(t.tools, [{"name": "read", "arg": "src/a.js", "ok": True}])
        self.assertEqual(t.usage, harness.usage_dict(100, 12))
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


class Execute(unittest.TestCase):
    """harness.execute: build, write the files, run, read the outputs, parse; over the fake harness."""

    def setUp(self):
        self.h, self.cli = install_fake(self)
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.state = os.path.join(self.d, "state")
        os.makedirs(self.state)
        self.env = dict(os.environ, FAKE_STATE=self.state)

    def script(self, *steps):
        with open(os.path.join(self.state, "fakeh_script"), "w") as f:
            f.write("".join(json.dumps(s) + "\n" for s in steps))

    def calls(self):
        with open(os.path.join(self.state, "fakeh_calls")) as f:
            return [json.loads(x) for x in f]

    def req(self, **kw):
        return request(run_dir=self.d, workdir=self.d, **kw)

    def test_a_run_gives_the_episode_its_files_and_its_stream(self):
        self.script({"turns": [{"text": "a", "usage": {"input": 5, "output": 2}, "cost": 0.5}, {"text": "b"}], "answer": "all done"})
        ep, code, stream = harness.execute(self.h, self.req(prompt="the prompt", episode="001-fix", rules_path="/r.md"), self.env)
        self.assertEqual((code, ep.harness, ep.role, ep.answer, ep.session_id, ep.exit_reason), (0, "fakeh", "fix", "all done", "fakeh-sess-1", "completed"))
        self.assertEqual([t.text for t in ep.turns], ["a", "b"])
        self.assertEqual((ep.usage_total["output"], ep.cost_est), (2, 0.5))
        self.assertEqual(ep.notes, ["fakeh: rules not delivered"])
        self.assertEqual(ep.raw_path, "episodes/001-fix.stream")
        with open(os.path.join(self.d, ep.raw_path)) as f:
            self.assertEqual(f.read(), stream)
        cfg = os.path.join(self.d, "fakeh", "config.json")
        self.assertEqual(oct(os.stat(cfg).st_mode & 0o777), "0o600")
        c = self.calls()[0]
        self.assertEqual((c["stdin"], c["argv"][-2:]), ("the prompt", ["--tools", "all"]), "the prompt is on stdin, not in argv")
        check_contract(self, ep, self.h)

    def test_the_key_is_in_the_child_s_environment_only(self):
        kf = os.path.join(self.d, "key")
        with open(kf, "w") as f:
            f.write("sk-secret\n")
        req = self.req()
        req.model.key_file = kf
        harness.execute(self.h, req, self.env)
        c = self.calls()[0]
        self.assertEqual(c["key"], "sk-secret")
        self.assertNotIn("sk-secret", json.dumps(c["argv"]) + c["stdin"])

    def test_a_resume_passes_the_session_back(self):
        self.script({}, {"answer": "again"})
        ep1, _, _ = harness.execute(self.h, self.req(), self.env)
        ep2, _, _ = harness.execute(self.h, self.req(resume=self.h.resume_ref(ep1)), self.env)
        self.assertEqual((ep1.session_id, ep2.session_id, ep2.answer), ("fakeh-sess-1", "fakeh-sess-1", "again"))
        self.assertEqual(self.calls()[1]["argv"][-2:], ["--resume", "fakeh-sess-1"])

    def test_the_turn_cap_ends_the_run_and_the_episode_says_so(self):
        self.script({"turns": [{"text": str(i)} for i in range(6)], "sleep": 30})
        t0 = time.time()
        ep, code, _ = harness.execute(self.h, self.req(max_turns=3, episode="002-fix"), self.env)
        self.assertEqual((code, ep.exit_reason), (harness.TURN_CAPPED, "turn_cap"))
        self.assertLess(time.time() - t0, 15)
        self.assertGreaterEqual(len(ep.turns), 3)
        self.assertTrue(os.path.exists(os.path.join(self.d, "episodes", "002-fix.stream")))

    def test_a_timeout_and_a_crash_are_named(self):
        self.script({"turns": [{"text": "x"}], "sleep": 30}, {"exit": 3})
        ep, code, _ = harness.execute(self.h, self.req(timeout=1), self.env)
        self.assertEqual((code, ep.exit_reason, ep.turns[0].text), (harness.TIMED_OUT, "timeout", "x"))
        ep, code, _ = harness.execute(self.h, self.req(), self.env)
        self.assertEqual((code, ep.exit_reason, ep.error), (3, "error", "no final event"))

    def test_a_role_or_a_resume_the_harness_cannot_serve_is_refused_before_anything_runs(self):
        self.h.roles = frozenset({"fix"})
        with self.assertRaises(harness.HarnessError):
            harness.execute(self.h, self.req(role="review"), self.env)
        self.h.can_resume = False
        with self.assertRaises(harness.HarnessError):
            harness.execute(self.h, self.req(resume="s"), self.env)
        self.assertFalse(os.path.exists(os.path.join(self.state, "fakeh_calls")))

    def test_a_file_outside_the_run_directory_is_refused(self):
        class Evil(FakeH):
            def build(self, req):
                inv = super().build(req)
                inv.files = {"../escape": "x"}
                return inv
        with self.assertRaises(harness.HarnessError):
            harness.execute(Evil(self.cli), self.req(), self.env)
        self.assertFalse(os.path.exists(os.path.join(os.path.dirname(self.d), "escape")))

    def test_outputs_the_run_writes_reach_parse(self):
        seen = {}

        class Out(FakeH):
            def build(self, req):
                inv = super().build(req)
                inv.outputs = ["fakeh/config.json", "fakeh/missing.json"]
                return inv

            def parse(self, stream_text, files):
                seen.update(files)
                return super().parse(stream_text, files)
        harness.execute(Out(self.cli), self.req(), self.env)
        self.assertEqual(json.loads(seen["fakeh/config.json"]), {"alias": "bugfix-flash"})
        self.assertEqual(seen["fakeh/missing.json"], "")

    def test_pi_s_stream_is_not_written_beside_its_session_files(self):
        self.assertFalse(harness_pi.PI.tees)
        self.assertTrue(self.h.tees)


if __name__ == "__main__":
    unittest.main()
