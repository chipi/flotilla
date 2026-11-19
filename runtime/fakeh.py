"""runtime/fakeh.py: the fake harness every adapter test and the CI seam end-to-end run on (it is not an adapter: it is
registered only while a test installs it), and `check_contract`, what every adapter's parse() promises (adapter.py's
"HOW TO ADD A HARNESS", point 5), used on each sample stream by every adapter's tests.

The fake's native format is the normalized events as JSON lines, one {"event": "turn", ...} per model call and a closing
{"event": "final", ...}. Its CLI (a script) takes the prompt on stdin, takes `--resume <session>`, and does what
FAKE_STATE/fakeh_script says for the call: a JSON line {"turns": [...], "files": {path: text}, "answer", "error",
"sleep", "exit", "remove": [path]}; without a line, one turn that says ok.
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
    for p in step.get("remove") or []:
        os.remove(os.path.join(os.getcwd(), p))
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
        return adapter.Episode(harness=self.name, session_id=final.get("session"), turns=turns, answer=answer, error=err,
                               exit_reason="error" if err else "completed", usage_total=adapter.sum_usage(turns),
                               cost_est=adapter.sum_cost(turns))


# The fake harness that speaks to a model, for the seam end-to-end that runs in CI (which has no pi): a CLI that reads its prompt on
# stdin, posts chat completions to the gateway it is told, executes the tool calls it gets back in its working directory, keeps its
# session under $FAKEH_HOME, and prints the same event lines. It offers the tools its --tools value allows (the names are pi's) and
# refuses a tool it did not offer, unless FAKEH_IGNORES_READONLY=1 (a harness that cannot enforce read-only). Unless FAKEH_OFFLINE=1
# it first pings a vendor host, as a CLI with an update check does: the seam end-to-end's S10 control.
FAKE_HTTP_CLI = textwrap.dedent('''\
    import json, os, subprocess, sys, time, urllib.request
    args = sys.argv[1:]
    def arg(name, default=""):
        return args[args.index(name) + 1] if name in args else default
    base, model, tools = arg("--base-url"), arg("--model"), arg("--tools", "all")
    system_file, resume = arg("--system"), arg("--resume")
    home, key = os.environ["FAKEH_HOME"], os.environ.get("FAKEH_KEY", "")
    prompt = sys.stdin.read()
    if os.environ.get("FAKEH_OFFLINE") != "1":
        try:
            urllib.request.urlopen("http://telemetry.invalid/ping", timeout=5)
        except Exception:
            pass
    TOOLS = {"all": ["bash", "edit", "read", "write"], "read": ["find", "grep", "ls", "read"], "none": []}
    offered = TOOLS["all"] if os.environ.get("FAKEH_IGNORES_READONLY") == "1" and tools != "none" else TOOLS[tools]
    os.makedirs(os.path.join(home, "sessions"), exist_ok=True)
    sid = resume or "fakehttp-%d" % int(time.time() * 1000)
    spath = os.path.join(home, "sessions", sid + ".json")
    messages = json.load(open(spath)) if resume and os.path.exists(spath) else None
    if messages is None:
        messages = [{"role": "system", "content": open(system_file).read()}] if system_file else []
    messages.append({"role": "user", "content": prompt})
    def save():
        with open(spath, "w") as f:
            json.dump(messages, f)
    save()
    while True:
        body = {"model": model, "messages": messages}
        if offered:
            body["tools"] = [{"type": "function", "function": {"name": t, "description": t, "parameters": {"type": "object", "properties": {
                "path": {"type": "string"}, "content": {"type": "string"}}}}} for t in offered]
        req = urllib.request.Request(base + "/chat/completions", data=json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
        resp = json.load(urllib.request.urlopen(req, timeout=120))
        msg, usage = resp["choices"][0]["message"], resp.get("usage") or {}
        messages.append(msg)
        calls, events = msg.get("tool_calls") or [], []
        for c in calls:
            name, a = c["function"]["name"], json.loads(c["function"].get("arguments") or "{}")
            path, ok, out = a.get("path") or "", True, "ok"
            if name not in offered:
                ok, out = False, "no such tool: " + name
            elif name == "write":
                os.makedirs(os.path.dirname(os.path.join(os.getcwd(), path)) or ".", exist_ok=True)
                open(os.path.join(os.getcwd(), path), "w").write(a.get("content") or "")
                out = "wrote " + path
            elif name == "read":
                try:
                    out = open(os.path.join(os.getcwd(), path)).read()
                except OSError:
                    ok, out = False, "cannot read " + path
            elif name == "bash":
                r = subprocess.run(a.get("command") or "true", shell=True, cwd=os.getcwd(), capture_output=True, text=True, timeout=30)
                out = (r.stdout + r.stderr)[-2000:]
            messages.append({"role": "tool", "tool_call_id": c["id"], "content": out})
            events.append({"name": name, "arg": path or a.get("command") or "", "ok": ok})
        save()
        print(json.dumps({"event": "turn", "stop": "tool_calls" if calls else "stop", "model": resp.get("model"), "text": msg.get("content") or "",
                          "usage": {"input": usage.get("prompt_tokens", 0), "output": usage.get("completion_tokens", 0)}, "tools": events}), flush=True)
        if not calls:
            break
    print(json.dumps({"event": "final", "session": sid, "answer": msg.get("content") or "", "error": ""}), flush=True)
    ''')


class FakeHttpH(FakeH):
    """FAKE_HTTP_CLI as an adapter. `enforces_readonly` is a constructor argument: True declares what a harness with switches
    would (the CLI offers only the read tools); False declares a harness that cannot, and `ignores_readonly` makes the CLI act
    like one (it offers every tool whatever it was told)."""
    name = "fakehttp"
    resume_after_kill = False   # its session is saved after each message, but it has no word for a line a kill cut
    reports_usage = True
    supports_turn_cap = True
    native_system_prompt = True
    prompt_via_stdin = True

    def __init__(self, cli, enforces_readonly=True, ignores_readonly=False):
        super().__init__(cli)
        self.enforces_readonly, self.ignores_readonly = enforces_readonly, ignores_readonly

    def build(self, req):
        argv = [sys.executable, self.cli, "--base-url", req.model.base_url, "--model", req.model.model, "--tools", req.tools]
        if req.system_path:
            argv += ["--system", req.system_path]
        if req.resume:
            argv += ["--resume", req.resume]
        env = {"FAKEH_KEY": adapter.read_key(req.model, os.environ), "FAKEH_HOME": os.path.join(req.run_dir, "fakeh")}
        if self.ignores_readonly:
            env["FAKEH_IGNORES_READONLY"] = "1"
        return adapter.Invocation(argv=argv, env=env, stdin=req.prompt, cwd=req.workdir)

    def phone_home_env(self):
        return {"FAKEH_OFFLINE": "1"}


def install_fake_http(tc, **kw):
    """Register FakeHttpH for this test (and take it away again); returns (harness, its CLI's path)."""
    d = tempfile.mkdtemp()
    tc.addCleanup(shutil.rmtree, d, True)
    cli = os.path.join(d, "fakehttp_cli.py")
    with open(cli, "w") as f:
        f.write(FAKE_HTTP_CLI)
    h = FakeHttpH(cli, **kw)
    adapter.register(h)
    tc.addCleanup(adapter.unregister, "fakehttp")
    return h, cli


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
