"""runtime/seam_e2e.py: the seam end-to-end of a harness (runtime/HARNESS_SEAMS.md): the harness's CLI, run through the station driver
(runtime/station_run.py) against a scripted model endpoint instead of a model.

A harness is adopted only when its real CLI binary, driven through the harness layer for stations of each `tools` value, shows
every seam S1-S11. The bugfix chain is not in the loop (bugfix/chain_seam_e2e.py runs the chain over the same infrastructure for
what stays the chain's: its spend metering, its kick-back, its checkpoint). The model is ScriptedEndpoint: a stdlib HTTP server that
speaks the protocol the harness uses and answers each request from a script (a tool call that edits a file, then a final text), and
records what it received: path, Authorization header, model, system text, the conversation, the tools offered. A RecordingProxy
stands where the network would be and records every host a CLI tries that is not the gateway. A run is killed mid-way (SIGTERM to
its process group, as the worker does at its timeout) and resumed by the session it left.

SeamSuite is the shared set of checks, one test method per seam; a harness's test_seam_<name>.py subclasses it, names its CLI and the
few things that differ, and runs it (test_seam_pi.py is the reference, test_seam_fakeh.py the same suite over a fake CLI that runs in
CI, which has no pi). Without the real CLI on PATH the suite is skipped, with the reason.

Protocols: POST /v1/chat/completions (pi, opencode, dsh, mini), streamed (server-sent events) or not. /v1/messages (Claude Code) and
/v1/responses (Codex) are added by the adoption of the harness that speaks them: View is the one place that reads a request and
Reply.render_chat the one that writes a response. Stdlib only; nothing here calls a model.
"""
import contextlib
import glob
import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

# built, not written: a literal that looks like a key is what the secrets scan looks for
KEY = "sk-" + "seam-" + "test-" + "key-0001"


class View:
    """One request as the seams are checked on it."""

    def __init__(self, path, headers, body, method="POST"):
        self.path, self.method, self.body = path, method, body
        self.headers = {k.lower(): v for k, v in headers.items()}
        self.t = time.time()

    @property
    def auth(self):
        return self.headers.get("authorization", "")

    @property
    def model(self):
        return self.body.get("model")

    @property
    def messages(self):
        return self.body.get("messages") or []

    @staticmethod
    def text_of(content):
        if isinstance(content, str):
            return content
        return "".join(p.get("text", "") for p in content or [] if isinstance(p, dict))

    @property
    def system(self):
        """The system text: every system (or developer) message of the conversation, in order."""
        return "\n".join(self.text_of(m.get("content")) for m in self.messages if m.get("role") in ("system", "developer"))

    @property
    def users(self):
        """The text of each user message, in order: the first is the chain's prompt, the later ones its follow-ups."""
        return [self.text_of(m.get("content")) for m in self.messages if m.get("role") == "user"]

    @property
    def new_tool_results(self):
        """The tool results after the last user message: how far into its current prompt the model is."""
        last = max((i for i, m in enumerate(self.messages) if m.get("role") == "user"), default=-1)
        return [m for m in self.messages[last + 1:] if m.get("role") == "tool"]

    @property
    def tool_names(self):
        return sorted((t.get("function") or {}).get("name") or t.get("name") or "" for t in self.body.get("tools") or [])


class Reply:
    """What the script answers: text, tool calls (name, arguments dict), both, a hold (the request is not answered for
    `hold` seconds: a run to be killed), or an HTTP error status."""

    def __init__(self, text="", tools=(), hold=0.0, status=200, usage=(100, 20)):
        self.text, self.tools, self.hold, self.status, self.usage = text, list(tools), hold, status, usage

    def render_chat(self, view):
        calls = [{"id": f"call_{i}", "type": "function", "function": {"name": n, "arguments": json.dumps(a)}}
                 for i, (n, a) in enumerate(self.tools)]
        finish = "tool_calls" if calls else "stop"
        usage = {"prompt_tokens": self.usage[0], "completion_tokens": self.usage[1], "total_tokens": sum(self.usage)}
        if not view.body.get("stream"):
            msg = {"role": "assistant", "content": self.text or None}
            if calls:
                msg["tool_calls"] = calls
            doc = {"id": "chatcmpl-seam", "object": "chat.completion", "created": 1, "model": view.model,
                   "choices": [{"index": 0, "message": msg, "finish_reason": finish}], "usage": usage}
            return "application/json", json.dumps(doc).encode()

        def chunk(delta, finish_reason=None, usage_doc=None):
            doc = {"id": "chatcmpl-seam", "object": "chat.completion.chunk", "created": 1, "model": view.model,
                   "choices": [{"index": 0, "delta": delta, "finish_reason": finish_reason}] if usage_doc is None else []}
            if usage_doc is not None:
                doc["usage"] = usage_doc
            return b"data: " + json.dumps(doc).encode() + b"\n\n"
        out = [chunk({"role": "assistant", "content": ""})]
        if self.text:
            out.append(chunk({"content": self.text}))
        for i, c in enumerate(calls):
            out.append(chunk({"tool_calls": [dict(c, index=i)]}))
        out += [chunk({}, finish), chunk({}, usage_doc=usage), b"data: [DONE]\n\n"]
        return "text/event-stream", b"".join(out)


class ScriptedEndpoint:
    """The gateway. `script(view) -> Reply` answers each POST, under a lock (a role's requests arrive in order); `requests`
    keeps every View, GETs included (the meter's /key/info reads the spend: `spend_per_post` for each POST answered)."""

    def __init__(self, script, spend_per_post=0.01):
        self.script, self.requests, self.lock, self.spend_per_post = script, [], threading.Lock(), spend_per_post
        endpoint = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def send(self, status, ctype, data):
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(n) if n else b""
                try:
                    body = json.loads(raw or b"{}")
                except ValueError:
                    body = {}
                view = View(self.path, dict(self.headers), body)
                with endpoint.lock:
                    endpoint.requests.append(view)
                    reply = endpoint.script(view)
                try:
                    if reply.hold:
                        time.sleep(reply.hold)
                    if reply.status != 200:
                        return self.send(reply.status, "application/json", json.dumps(
                            {"error": {"message": reply.text or "scripted error", "type": "scripted"}}).encode())
                    if not self.path.rstrip("/").endswith("/chat/completions"):
                        return self.send(404, "text/plain", b"")
                    self.send(200, *reply.render_chat(view))
                except OSError:   # the client was killed while it waited: what the kill tests mean
                    pass

            def do_GET(self):
                with endpoint.lock:
                    endpoint.requests.append(View(self.path, dict(self.headers), {}, "GET"))
                    spent = endpoint.spend()
                if self.path.endswith("/key/info"):
                    return self.send(200, "application/json", json.dumps({"info": {"spend": spent}}).encode())
                self.send(404, "text/plain", b"")

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.server.daemon_threads = True
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @property
    def base_url(self):
        return f"http://127.0.0.1:{self.server.server_address[1]}/v1"

    def posts(self):
        return [v for v in self.requests if v.method == "POST"]

    def gets(self):
        return [v for v in self.requests if v.method == "GET"]

    def spend(self):
        return round(len([v for v in self.requests if v.method == "POST"]) * self.spend_per_post, 6)

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class RecordingProxy:
    """Stands where the egress proxy would be (HTTP_PROXY), with the gateway on NO_PROXY: every request that reaches it is a
    host the CLI tried that is not the gateway. It refuses them all, and keeps them."""

    def __init__(self):
        self.seen = []
        proxy = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def refuse(self):
                proxy.seen.append(f"{self.command} {self.path}")
                self.send_response(403)
                self.send_header("Content-Length", "0")
                self.end_headers()
            do_GET = do_POST = do_PUT = do_HEAD = do_CONNECT = refuse

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.server.daemon_threads = True
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server.server_address[1]}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@contextlib.contextmanager
def stdin_closed():
    """The chain's process with no terminal and no held-open pipe on stdin, as the worker's has it: Chain.review starts the CLI
    without a stdin of its own, and `pi -p` waits for a stdin that never ends."""
    saved = os.dup(0)
    null = os.open(os.devnull, os.O_RDONLY)
    os.dup2(null, 0)
    try:
        yield
    finally:
        os.dup2(saved, 0)
        os.close(saved)
        os.close(null)


# what the scripted model does for the fixture workspace (src/a.js returns 1; a station is to make it return 2). It tells a request
# by the last user message of the conversation: no station name decides anything here either.

FIX_SOURCE = "module.exports = () => 2;\n"
FIX_SOURCE_AGAIN = "module.exports = () => 2; // again\n"
FIX_ANSWER = "Changed src/a.js so value() returns 2."
ASK_ANSWER = "I looked and it is fine."
SYSTEM_MARKER = "# SEAM-SYSTEM-MARKER: the system text of the version"
REPO_MARKER = "REPO-AGENTS-MD-MARKER"   # in the workspace's AGENTS.md: must never be read


class Script:
    """script(view) for the fixture workspace. The last user message says what to do:
      GRIND      never stops calling a tool (GRIND DETACH: and its first call starts two background processes, one of them in a session of its own);
      HOLD       the first request is not answered;
      KILLME     writes the file, and the second request is not answered while `hold_second` is set;
      WRITE and FOLLOWUP   write the file (FOLLOWUP: a variant), then say done;
      anything else   a plain answer."""

    def __init__(self):
        self.hold_second = False

    def __call__(self, view):
        last = (view.users or [""])[-1]
        fresh = len(view.new_tool_results)
        if "GRIND" in last:
            started = [("bash", {"command": "nohup sleep 300 >/dev/null 2>&1 &"}), ("bash", {"command": "setsid nohup sleep 300 >/dev/null 2>&1 &"})] if "DETACH" in last and not view.new_tool_results else []
            return Reply(tools=started + [("read", {"path": "src/a.js"})])
        if "HOLD" in last and "KILLME" not in last:
            return Reply(hold=120)
        if "KILLME" in last:
            if fresh and self.hold_second:
                return Reply(hold=120)
            return Reply(text="Looking.", tools=[("write", {"path": "src/a.js", "content": FIX_SOURCE})]) if not fresh else Reply(text=FIX_ANSWER)
        if "WRITE" in last or "FOLLOWUP" in last:
            if not fresh:
                return Reply(text="Looking.", tools=[("write", {"path": "src/a.js", "content": FIX_SOURCE_AGAIN if "FOLLOWUP" in last else FIX_SOURCE})])
            return Reply(text=FIX_ANSWER)
        return Reply(text=ASK_ANSWER)


ENV_KEYS = ("HOME", "PI_CODING_AGENT_DIR", "FAKEH_HOME", "HTTP_PROXY", "NO_PROXY", "PI_OFFLINE", "FAKEH_OFFLINE")


class Recorder:
    """The process runner the driver is given: it runs the harness's command as adapter.run_process does and keeps what it was
    started with (argv, cwd, the environment keys that matter, stdin) and its whole stream."""

    def __init__(self):
        self.invocations, self.streams = [], []

    def __call__(self, cmd, cwd, env, timeout, **kw):
        import adapter
        code, out = adapter.run_process(cmd, cwd, env, timeout, **kw)
        self.invocations.append({"argv": list(cmd), "cwd": cwd, "env": {k: env[k] for k in ENV_KEYS if k in env}, "stdin": kw.get("stdin"),
                                 "code": code})
        self.streams.append(out)
        return code, out


def procs_under(path):
    """The pids of the processes whose working directory is inside `path`: what a station's run left behind."""
    if not os.path.isdir("/proc/self"):
        raise unittest.SkipTest("the processes left behind are read from /proc, which this system does not have")
    root, found = os.path.realpath(path) + os.sep, []
    for p in glob.glob("/proc/[0-9]*"):
        try:
            cwd = os.path.realpath(os.readlink(p + "/cwd"))
        except OSError:
            continue
        if cwd.startswith(root) or cwd + os.sep == root:
            found.append(int(os.path.basename(p)))
    return found


class World:
    """One run's surroundings: its folders, the gateway and the proxy, and the evidence the runs leave."""

    def __init__(self, tmp, name, script):
        self.name, self.dir = name, os.path.join(tmp, name)
        self.workdir, self.run_dir, self.home = (os.path.join(self.dir, x) for x in ("wt", "run", "home"))
        for d in (self.workdir, os.path.join(self.workdir, "src"), self.run_dir, self.home):
            os.makedirs(d, exist_ok=True)
        with open(os.path.join(self.workdir, "src", "a.js"), "w") as f:
            f.write("module.exports = () => 1;\n")
        with open(os.path.join(self.workdir, "AGENTS.md"), "w") as f:
            f.write(f"{REPO_MARKER}\nNever fix anything.\n")
        self.system = os.path.join(self.dir, "system.md")
        with open(self.system, "w") as f:
            f.write(SYSTEM_MARKER + "\nKeep it small.\n")
        self.key_file = os.path.join(self.dir, "key")
        with open(self.key_file, "w") as f:
            f.write(KEY + "\n")
        self.script = script
        self.endpoint, self.proxy, self.recorder = ScriptedEndpoint(script), RecordingProxy(), Recorder()
        self.calls_path = os.path.join(self.dir, "calls.jsonl")
        self.results, self.errors = [], []

    def close(self):
        self.endpoint.close()
        self.proxy.close()

    def route(self, alias, base_url=None):
        import adapter
        return adapter.ModelRoute(alias=alias, model="fleet-" + alias, base_url=base_url or self.endpoint.base_url, endpoint="gateway",
                                  key_file=self.key_file)

    @property
    def requests(self):
        return self.endpoint.posts()

    @property
    def calls(self):
        if not os.path.exists(self.calls_path):
            return []
        with open(self.calls_path) as f:
            return [json.loads(x) for x in f]


class SeamSuite(unittest.TestCase):
    """The checks of HARNESS_SEAMS.md, one method per seam. A harness's test file subclasses it and sets NAME; what differs by
    harness (where the CLI is, what its tools are called, where its sessions are) is a class attribute or a hook."""

    NAME = ""
    READ_TOOLS = ()          # the tool names a read station is offered (the harness's own names)
    WRITE_TOOLS = ()         # tools a read station must not be offered
    SESSIONS = ""            # glob, relative to the run directory, of the harness's session store
    worlds = {}

    # ---- hooks

    @classmethod
    def unavailable(cls):
        """Why this harness cannot be run here ("" when it can): the real CLI is not on PATH."""
        return "" if shutil.which(cls.NAME) else f"the real {cls.NAME} CLI is not on PATH"

    @classmethod
    def prepare(cls):
        """Anything the harness needs first (a fake registers its adapter)."""

    @classmethod
    def env(cls, proxy):
        """The environment the CLI gets beyond the station driver's: what the worker image sets, and the proxy as the worker has it."""
        return {"HTTP_PROXY": proxy.url, "HTTPS_PROXY": proxy.url, "NO_PROXY": "127.0.0.1,localhost,::1", "NODE_USE_ENV_PROXY": "1"}

    @classmethod
    def session_ref_after_kill(cls, run_dir):
        """The session a killed run left, as the reference a resume takes (the harness's own: pi's id is in its file's first line)."""
        raise NotImplementedError

    # ---- the runs

    @classmethod
    def setUpClass(cls):
        why = cls.unavailable()
        if why:
            raise unittest.SkipTest(f"{why}: the seam end-to-end needs it (HARNESS_SEAMS.md, 'The minimum seam end-to-end'); CI's image does not have it")
        import adapter
        cls.prepare()
        cls.adapter = adapter
        cls.h = adapter.get(cls.NAME)
        cls.tmp = tempfile.mkdtemp()
        cls.addClassCleanup(shutil.rmtree, cls.tmp, True)
        cls.worlds = {}
        cls.main_world()
        cls.cap_world()
        cls.timeout_world()
        cls.detach_world()
        if cls.h.resume_after_kill:
            cls.killed_world()
        cls.addClassCleanup(lambda: [w.close() for w in cls.worlds.values()])

    @classmethod
    def world(cls, name, script=None):
        w = World(cls.tmp, name, script or Script())
        cls.worlds[name] = w
        return w

    @classmethod
    def station(cls, w, name, tools, prompt, alias="flash", **kw):
        import station_run
        return station_run.Station(name=name, harness=cls.NAME, model=w.route(alias), prompt=prompt, tools=tools,
                                   system_path=w.system, alias=alias, **kw)

    @classmethod
    def go(cls, w, st, slots=None, **kw):
        import station_run
        base = dict(os.environ, **cls.env(w.proxy))
        base["HOME"] = w.home
        kw.setdefault("calls_path", w.calls_path)
        res = station_run.run_station(st, slots or {}, run_dir=w.run_dir, workdir=w.workdir, base_env=base, run=w.recorder, **kw)
        w.results.append(res)
        return res

    @classmethod
    def main_world(cls):
        """Four stations of every `tools` value, one resumed by its session: the run most seams are read from."""
        w = cls.world("main")
        meter = w.endpoint.spend
        a1 = cls.go(w, cls.station(w, "alpha", "all", "WRITE the fix: make value() return {{value}}."), {"value": 2}, seq=1, meter=meter)
        cls.go(w, cls.station(w, "alpha", "all", "FOLLOWUP: {{note}}"), {"note": "a reviewer asked for changes"}, seq=2, resume=a1.resume_ref, meter=meter)
        cls.go(w, cls.station(w, "beta", "read", "ASK what does src/a.js return?"), seq=3, meter=meter)
        cls.go(w, cls.station(w, "gamma", "none", "ASK without any tool."), seq=4, meter=meter)
        w.elapsed = 0.0

    @classmethod
    def cap_world(cls):
        w = cls.world("cap")
        t0 = time.time()
        cls.go(w, cls.station(w, "delta", "all", "GRIND forever", max_turns=2, timeout=60), seq=1)
        w.elapsed = time.time() - t0

    @classmethod
    def detach_world(cls):
        """A capped station whose tool started a background process in the working directory, as a model that starts a dev server does."""
        w = cls.world("detach")
        cls.addClassCleanup(cls.reap_detached, w)
        cls.go(w, cls.station(w, "delta", "all", "GRIND DETACH forever", max_turns=2, timeout=60), seq=1)

    @staticmethod
    def reap_detached(w):
        """Kill what the detach scenario left (a `sleep 300` of this test's own, found by its working directory): the suite cleans up
        after itself whether or not the harness's runner did."""
        for pid in procs_under(w.dir):
            try:
                with open(f"/proc/{pid}/cmdline", "rb") as f:
                    if f.read().split(b"\0")[:2] == [b"sleep", b"300"]:
                        os.kill(pid, signal.SIGKILL)
            except (OSError, ProcessLookupError):
                pass

    @classmethod
    def timeout_world(cls):
        w = cls.world("timeout")
        cls.go(w, cls.station(w, "delta", "all", "HOLD the answer", timeout=4), seq=1)

    RUNNER = '''\
import json, os, sys
sys.path.insert(0, %r)
import adapter, station_run
station_run.exit_on_sigterm()
spec = json.load(open(sys.argv[1]))
st = station_run.Station(name=spec["name"], harness=spec["harness"], model=adapter.ModelRoute(**spec["route"]), prompt=spec["prompt"],
                         tools=spec["tools"], system_path=spec["system"], timeout=spec["timeout"], alias=spec["alias"])
station_run.run_station(st, {}, run_dir=spec["run_dir"], workdir=spec["workdir"], base_env=spec["env"], seq=1)
''' % HERE

    @classmethod
    def killed_world(cls):
        """The driver as a process, killed (SIGTERM to its process group, as the worker does at its timeout) while the station is in
        flight, and the station resumed by the session the killed run left."""
        script = Script()
        script.hold_second = True
        w = cls.world("killed", script)
        spec = os.path.join(w.dir, "spec.json")
        runner = os.path.join(w.dir, "runner.py")
        route = w.route("flash")
        with open(runner, "w") as f:
            f.write(cls.RUNNER)
        env = dict(os.environ, **cls.env(w.proxy), HOME=w.home)
        with open(spec, "w") as f:
            json.dump({"name": "alpha", "harness": cls.NAME, "route": {"alias": route.alias, "model": route.model, "base_url": route.base_url,
                                                                       "endpoint": route.endpoint, "key_file": route.key_file},
                       "prompt": "KILLME write the file", "tools": "all", "system": w.system, "timeout": 120, "alias": "flash",
                       "run_dir": w.run_dir, "workdir": w.workdir, "env": env}, f)
        p = subprocess.Popen([sys.executable, runner, spec], start_new_session=True, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT, text=True)
        cls.addClassCleanup(lambda: p.poll() is None and os.killpg(p.pid, signal.SIGKILL))
        deadline = time.time() + 90
        while time.time() < deadline and not [v for v in w.requests if v.new_tool_results]:
            time.sleep(0.1)
        w.held = bool([v for v in w.requests if v.new_tool_results])
        os.killpg(p.pid, signal.SIGTERM)
        w.killed_out, _ = p.communicate(timeout=30)
        w.killed_exit = p.returncode
        w.killed_requests = len(w.requests)
        w.run_dir_files = {}
        for root, _, names in os.walk(w.run_dir):
            for n in names:
                fp = os.path.join(root, n)
                try:
                    with open(fp, "rb") as fh:
                        w.run_dir_files[os.path.relpath(fp, w.run_dir)] = fh.read(2_000_000)
                except OSError:
                    pass
        w.sessions = sorted(os.path.relpath(x, w.run_dir) for x in glob.glob(os.path.join(w.run_dir, cls.SESSIONS)))
        w.ref = cls.session_ref_after_kill(w.run_dir)
        script.hold_second = False
        w.resumed = cls.go(w, cls.station(w, "alpha", "all", "FOLLOWUP: the run was interrupted, continue"), seq=2, resume=w.ref)

    # ---- the seams

    def test_s0_the_scripted_stations_ran(self):
        w = self.worlds["main"]
        self.assertEqual([r.episode.exit_reason for r in w.results], ["completed"] * 4)
        self.assertEqual([r.episode.station for r in w.results], ["alpha", "alpha", "beta", "gamma"], "the station name is the caller's label")
        self.assertEqual(len(w.recorder.invocations), 4)
        if self.h.resume_after_kill:
            self.assertTrue(self.worlds["killed"].held, "the killed run did reach the held request")

    def test_s1_the_prompt_is_the_callers_and_only_the_callers_and_not_in_argv(self):
        w = self.worlds["main"]
        first = w.requests[0]
        self.assertEqual(first.users, ["WRITE the fix: make value() return 2."], "the rendered text, as the first user turn")
        for inv in w.recorder.invocations:
            self.assertIsNotNone(inv["stdin"], "the prompt is on stdin")
            self.assertNotIn(inv["stdin"], " ".join(inv["argv"]), "and not in argv")
        self.assertEqual(w.results[2].episode.turns[0].texts[0], {"name": "prompt", "bytes": len("ASK what does src/a.js return?".encode()),
                                                                  "sha256": hashlib.sha256(b"ASK what does src/a.js return?").hexdigest()},
                         "the rendered text is on the episode, by hash and length")

    def test_s2_the_system_text_reaches_the_harness_and_the_workspaces_own_instructions_do_not(self):
        for name in ("main", "cap"):
            for v in self.worlds[name].requests:
                self.assertIn(SYSTEM_MARKER, v.system, name)
                self.assertNotIn(REPO_MARKER, v.system, f"{name}: the workspace's own AGENTS.md is never read")
        w = self.worlds["main"]
        self.assertEqual(w.results[0].episode.turns[0].texts[1]["name"], "system")

    def test_s3_the_model_is_reached_through_the_gateway_with_the_key_and_the_key_is_nowhere_else(self):
        for name, w in self.worlds.items():
            self.assertTrue(w.requests, name)
            for v in w.requests:
                self.assertEqual((v.path, v.auth, v.model), ("/v1/chat/completions", f"Bearer {KEY}", "fleet-flash"), name)
            for inv in w.recorder.invocations:
                self.assertNotIn(KEY, json.dumps(inv["argv"]) + (inv["stdin"] or ""), "the key is in no argv and not on stdin")
        leaked = []
        for w in self.worlds.values():
            for root, _, names in os.walk(w.dir):
                for n in names:
                    p = os.path.join(root, n)
                    if p == w.key_file:
                        continue
                    try:
                        with open(p, "rb") as fh:
                            if KEY.encode() in fh.read(2_000_000):
                                leaked.append(os.path.relpath(p, self.tmp))
                    except OSError:
                        pass
        self.assertEqual(leaked, [], "the key is in no file of any run, the workspace and the run directory included")

    def test_s4_the_stream_becomes_an_episode_and_a_valid_record(self):
        import adapter
        from fakeh import check_contract
        w = self.worlds["main"]
        eps = [self.h.parse(s, {}) for s in w.recorder.streams]
        for ep in eps:
            check_contract(self, ep, self.h)
        alpha1, alpha2, beta, gamma = eps
        self.assertEqual((len(alpha1.turns), alpha1.answer, alpha1.error), (2, FIX_ANSWER, ""))
        self.assertEqual((alpha1.usage_total["input"], alpha1.usage_total["output"]), (200, 40))
        self.assertEqual((alpha1.turns[0].stop, alpha1.turns[1].stop), ("tool_calls", "stop"))
        self.assertEqual([(t["name"], t["arg"], t.get("ok")) for t in alpha1.turns[0].tools], [("write", "src/a.js", True)])
        self.assertEqual(len(alpha2.turns), 2, "a resumed episode's turns are this call's, not the session's")
        self.assertEqual((beta.answer, gamma.answer), (ASK_ANSWER, ASK_ANSWER))
        for r in w.results:
            self.assertEqual(adapter.validate_episode(r.record), [], "every episode's record validates against the schema")
            self.assertEqual(r.record["station"], r.episode.station)
        with open(os.path.join(w.run_dir, w.results[0].record_path)) as f:
            self.assertEqual(json.load(f), w.results[0].record, "and the record is the file")
        self.assertEqual(w.results[0].record["turns"][0]["actions"][0]["name"], "write")

    def test_s5_the_cap_and_the_timeout_end_the_episode_and_the_process_group(self):
        cap, t = self.worlds["cap"], self.worlds["timeout"]
        self.assertEqual(cap.results[0].episode.exit_reason, "turn_cap")
        self.assertLess(cap.elapsed, 45, "the grinding station was stopped by the cap, not the 60 s timeout")
        self.assertLessEqual(len(cap.requests), 3)
        self.assertEqual(t.results[0].episode.exit_reason, "timeout")
        self.assertTrue(t.requests)
        main = self.worlds["main"]
        for stream in main.recorder.streams:
            ends = [ln for ln in stream.splitlines() if self.h.turn_ended(ln)]
            self.assertEqual(len(ends), len(self.h.parse(stream, {}).turns), "turn_ended counts the model calls of the real stream")
        for name in ("main", "cap", "timeout"):
            self.assertEqual(self.left_under(self.worlds[name].dir), [], f"{name}: no process of the harness is left")

    @staticmethod
    def left_under(path):
        deadline = time.time() + 10
        left = procs_under(path)
        while left and time.time() < deadline:
            time.sleep(0.2)
            left = procs_under(path)
        return left

    def test_s5_a_process_a_tool_detaches_is_not_left_either(self):
        """The capped station's tool started `nohup sleep 300 &`: after the cap's kill of the harness's group nothing of this run may
        still be running in its folder. The group kill does not reach a process a harness puts in a session of its own: that harness's
        runner sweeps what a tool detached into a session of its own (adapter._kill, Linux only: the test skips without /proc)."""
        w = self.worlds["detach"]
        self.assertGreaterEqual(len(w.requests[1].new_tool_results), 2, "the first turn's tools ran: the background process was started")
        self.assertEqual(self.left_under(w.dir), [], "a process the tool detached outlived the cap")

    def test_s6_a_read_station_is_offered_only_read_tools(self):
        w = self.worlds["main"]
        alpha, beta, gamma = w.requests[0], w.requests[4], w.requests[5]   # each station's first request: two for each alpha, one for the others
        self.assertEqual(beta.tool_names, sorted(self.READ_TOOLS))
        self.assertFalse(set(beta.tool_names) & set(self.WRITE_TOOLS))
        self.assertEqual(gamma.tool_names, [], "none: no tool at all")
        self.assertTrue(set(self.WRITE_TOOLS) <= set(alpha.tool_names), alpha.tool_names)

    def test_s7_a_session_continues_with_its_history_and_after_a_kill(self):
        w = self.worlds["main"]
        first, second = w.requests[0], w.requests[2]
        self.assertEqual(second.users[0], first.users[0], "the same session")
        self.assertEqual(second.users[1], "FOLLOWUP: a reviewer asked for changes", "the follow-up is the next message")
        self.assertGreater(len(second.messages), len(first.messages), "the earlier turns are in the conversation")
        self.assertEqual(w.results[1].episode.session_id, w.results[0].episode.session_id)
        if not self.h.resume_after_kill:
            return
        k = self.worlds["killed"]
        self.assertTrue(k.sessions, f"the killed run's session is in the run directory: {k.sessions}")
        self.assertTrue(k.ref)
        resumed_view = [v for v in k.requests[k.killed_requests:]][0]
        self.assertEqual(resumed_view.users[0], "KILLME write the file", "the resumed run is the killed run's session")
        self.assertEqual(resumed_view.users[1], "FOLLOWUP: the run was interrupted, continue")
        self.assertTrue([m for m in resumed_view.messages if m.get("role") == "tool"], "its tool results from before the kill are there")
        self.assertEqual(k.resumed.episode.exit_reason, "completed")

    def test_s8_every_model_call_is_a_line_of_the_calls_file(self):
        w = self.worlds["main"]
        self.assertEqual(len(w.calls), len(w.requests), "one line per model call")
        self.assertTrue(all(c["tokens_in"] == 100 and c["tokens_out"] == 20 for c in w.calls))
        self.assertEqual({c["station"] for c in w.calls}, {"alpha", "beta", "gamma"})
        self.assertEqual({c["harness"] for c in w.calls}, {self.NAME})
        self.assertEqual({c["role"] for c in w.calls}, {""}, "a line of a station says station, not role")
        self.assertEqual(sorted({c["episode_seq"] for c in w.calls}), [1, 2, 3, 4], "an episode keeps its order number")
        writes = [t for c in w.calls for t in c["tools"] if t["name"] == "write"]
        self.assertEqual([(t["arg"], t.get("ok")) for t in writes], [("src/a.js", True), ("src/a.js", True)])
        with_texts = [c for c in w.calls if "texts" in c]
        self.assertEqual(len(with_texts), 4, "the call that read the texts says which")
        for c in w.calls:
            for t in c["tools"]:
                self.assertLessEqual(set(t), {"name", "arg", "ok"}, "a tool in the calls file is name, arg, ok: the calls file is not the record")

    def test_s9_the_cost_is_the_gateways(self):
        w = self.worlds["main"]
        for r in w.results:
            self.assertEqual(r.record["cost"]["source"], "gateway")
        self.assertAlmostEqual(sum(r.record["cost"]["usd"] for r in w.results), 0.01 * len(w.requests))
        for r in w.results:   # the harness's own figure is an estimate beside it, never the bill
            self.assertNotEqual(r.record["cost"]["source"], "harness-estimate")

    def test_s10_nothing_but_the_gateway_is_contacted(self):
        for name, w in self.worlds.items():
            self.assertEqual(w.proxy.seen, [], f"{name}: a host other than the gateway was tried")
            self.assertEqual({v.path for v in w.endpoint.gets()}, set(), f"{name}: the gateway was only asked for completions")

    def test_s10_control_the_proxy_does_see_a_host_that_is_not_the_gateway(self):
        """Without this a silent proxy could mean a CLI that ignores it: the same station is run pointed at a host that is not on
        NO_PROXY, and the proxy must see it."""
        w = World(self.tmp, "control", Script())
        self.addCleanup(w.close)
        st = self.station(w, "probe", "none", "ASK anything", timeout=60)
        st.model = w.route("flash", base_url="http://gateway.invalid:4001/v1")
        try:
            self.go(w, st)
        except Exception:   # noqa: BLE001 -- the run fails: there is nowhere to go; what the proxy saw is the evidence
            pass
        self.assertTrue(any("gateway.invalid" in x for x in w.proxy.seen), "the CLI went round the proxy: S10's evidence means nothing for it")

    def test_s11_the_work_and_the_session_store_are_under_the_run_directory(self):
        for name, w in self.worlds.items():
            for inv in w.recorder.invocations:
                self.assertEqual(inv["cwd"], w.workdir, name)
                self.assertEqual(inv["env"]["HOME"], w.run_dir, f"{name}: HOME is the run directory")
            self.assertEqual(os.listdir(w.home), [], f"{name}: nothing in the worker's own HOME")
        main = self.worlds["main"]
        self.assertTrue(glob.glob(os.path.join(main.run_dir, self.SESSIONS)), "the session store is a place under the run directory")
        if self.h.resume_after_kill:
            self.assertTrue(self.worlds["killed"].run_dir_files, "the killed run's directory was kept and read")
