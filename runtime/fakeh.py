"""runtime/fakeh.py: the fake harness every adapter test and the CI seam end-to-end run on (it is not an adapter: it is
registered only while a test installs it), and `check_contract`, what every adapter's parse() promises (adapter.py's
"HOW TO ADD A HARNESS", point 5), used on each sample stream by every adapter's tests.

The fake's native format is the normalized events as JSON lines, one {"event": "turn", ...} per model call and a closing
{"event": "final", ...}. Its CLI (a script) takes the prompt on stdin, takes `--resume <session>`, and does what
FAKE_STATE/fakeh_script says for the call: a JSON line {"turns": [...], "files": {path: text}, "answer", "error",
"sleep", "exit"}; without a line, one turn that says ok.
"""
import json
import os
import shutil
import sys
import tempfile
import textwrap

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adapter  # noqa: E402


def check_contract(tc, ep, h=None):
    """What every adapter's parse() promises: used on each sample stream by every adapter's tests."""
    tc.assertIsInstance(ep, adapter.Episode)
    if h is not None:
        tc.assertEqual(ep.harness, h.name)
    tc.assertIn(ep.exit_reason, adapter.EXIT_REASONS)
    tc.assertIsInstance(ep.answer, str)
    tc.assertEqual(ep.answer, ep.answer.strip(), "the answer is stripped")
    tc.assertIsInstance(ep.error, str)
    tc.assertEqual([t.n for t in ep.turns], list(range(1, len(ep.turns) + 1)), "turns are numbered from 1")
    for t in ep.turns:
        tc.assertIsInstance(t.text, str)
        tc.assertIsInstance(t.stop, str)
        tc.assertTrue(t.cost_est is None or t.cost_est >= 0)
        if t.usage is not None:
            tc.assertEqual(sorted(t.usage), sorted(adapter.USAGE_KEYS))
            tc.assertTrue(all(isinstance(v, int) and v >= 0 for v in t.usage.values()), t.usage)
        for tool in t.tools:
            tc.assertIsInstance(tool["name"], str)
            tc.assertIsInstance(tool["arg"], str)
            tc.assertTrue("ok" not in tool or isinstance(tool["ok"], bool))
    tc.assertEqual(ep.usage_total, adapter.sum_usage(ep.turns))
    tc.assertEqual(adapter.Episode.from_dict(json.loads(json.dumps(ep.to_dict()))), ep, "an episode survives its JSON")
    rec = adapter.episode_record(ep)
    tc.assertEqual(adapter.validate_episode(rec), [], "the episode record validates against every schema")
    tc.assertTrue(rec["cost"]["source"], "the cost's source is always set: unknown is a value, not a gap")
    for t in rec["turns"]:
        for a in t.get("actions", []):
            for k in ("args", "result"):
                tc.assertFalse(k in a and k + "_sha256" in a, "a record holds a text or its hash, not both")
                tc.assertEqual(k + "_sha256" in a, k + "_bytes" in a, "a hash comes with its length")
            if "result" in a:
                tc.assertLessEqual(len(a["result"]), adapter.RESULT_CAP)


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


class FakeH(adapter.Harness):
    name = "fakeh"
    tools_served = frozenset(adapter.TOOLS)
    can_resume = True
    enforces_readonly = False

    def __init__(self, cli):
        self.cli = cli

    def build(self, req):
        argv = [sys.executable, self.cli, "--model", req.model.model, "--tools", req.tools]
        if req.resume:
            argv += ["--resume", req.resume]
        return adapter.Invocation(argv=argv, env={"FAKEH_KEY": adapter.read_key(req.model, os.environ)}, stdin=req.prompt,
                                  files={"fakeh/config.json": json.dumps({"alias": req.model.alias})},
                                  notes=["fakeh: rules not delivered"] if req.system_path else [])

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
                turns.append(adapter.Turn(n=len(turns) + 1, model=e.get("model"), text=e.get("text", ""), tools=e.get("tools") or [],
                                          usage=adapter.usage_dict(**u) if u else None, cost_est=e.get("cost"), stop=e.get("stop", "")))
            elif e.get("event") == "final":
                final = e
        answer = (final.get("answer") or next((t.text for t in reversed(turns) if t.text.strip()), "")).strip()
        err = final.get("error") or ("" if final else "no final event")
        return adapter.Episode(harness="fakeh", session_id=final.get("session"), turns=turns, answer=answer, error=err,
                               exit_reason="error" if err else "completed", usage_total=adapter.sum_usage(turns),
                               cost_est=adapter.sum_cost(turns))


def install_fake(tc):
    """Register the fake harness for this test (and take it away again); returns (harness, its CLI's path)."""
    d = tempfile.mkdtemp()
    tc.addCleanup(shutil.rmtree, d, True)
    cli = os.path.join(d, "fakeh_cli.py")
    with open(cli, "w") as f:
        f.write(FAKE_CLI)
    h = FakeH(cli)
    adapter.register(h)
    tc.addCleanup(adapter.unregister, "fakeh")
    return h, cli


def request(station="fix", **kw):
    route = adapter.ModelRoute(alias="bugfix-flash", model="fleet-bugfix-flash", base_url="http://127.0.0.1:9/v1",
                               endpoint="gateway-bugfix", key_file="/dev/null", meta=kw.pop("meta", {}))
    base = dict(station=station, prompt="fix the bug", system_path=None, model=route, tools="all", resume=None,
                max_turns=0, workdir=".", run_dir=".", timeout=60)
    base.update(kw)
    return adapter.Request(**base)
