"""bugfix/seam_e2e.py: the seam end-to-end of a harness (bugfix/HARNESS_SEAMS.md): the real CLI, run by the chain against a
scripted model endpoint instead of a model.

A harness is adopted only when its real CLI binary, driven by bugfix/fix.py's Chain for the roles fix, repro and review
over a tiny fixture repo (one failing test, fixed by one edit), shows every seam S1-S11. The model is ScriptedEndpoint:
a stdlib HTTP server that speaks the protocol the harness uses and answers each request from a script (a tool call that
edits a file, then a final text), and records what it received: path, Authorization header, model, system text, the
conversation, the tools offered. A RecordingProxy stands where the network would be and records every host a CLI tries
that is not the gateway. A run is killed mid-way (SIGTERM to its process group, as the worker does at its timeout) and
resumed, as ADR-0019 describes.

SeamSuite is the shared set of checks, one test method per seam; a harness's test_seam_<name>.py subclasses it, names
its CLI and the few things that differ, and runs it (test_seam_pi.py is the reference, and the shape of every next one).
The real CLI is not in CI's image: without it on PATH the suite is skipped, with the reason.

Protocols: POST /v1/chat/completions (pi, opencode, dsh, mini), streamed (server-sent events) or not. /v1/messages (Claude
Code) and /v1/responses (Codex) are added by the adoption of the harness that speaks them: View is the one place that
reads a request and Reply.render_chat the one that writes a response. Stdlib only; nothing here calls a model.
"""
import contextlib
import glob
import io
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
import unittest.mock
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


# what the scripted model does for the fixture repo (test_fix.py's: src/a.js returns 1, the issue says "value() returns 1,
# want 2", the failing test is "expect src/a.js 2"). A role is told apart by the first words of its prompt.

FIX_TEST = "const a = require('./a.js')\nmsg value() returns 1, want 2\nexpect src/a.js 2\n"
FIX_SOURCE = "module.exports = () => 2;\n"
FIX_SOURCE_REVIEWED = "module.exports = () => 2; // reviewed\n"
FIX_ANSWER = "Changed src/a.js so value() returns 2."
REPRO_ANSWER = {"repro": "test", "files": ["src/a.test.js"], "evidence_quoted": "value() returns 1, want 2",
                "expect_failure_match": "returns 1, want 2"}
REVIEW_ASKS = "The test only restates the change.\nVERDICT: request-changes CLASS: test-proves-nothing"
REVIEW_APPROVES = "The change is small and correct.\nVERDICT: approve"
RULES_MARKER = "# Rules for this run (flotilla's bugfix fleet)"   # the first line of bugfix/fleet_rules.md
REPO_RULES_MARKER = "REPO-AGENTS-MD-MARKER"                      # in the fixture repo's AGENTS.md: must never be read
DECLARED_MARKER = "# DECLARED-RULES-MARKER"                      # in a rules file a spec declares and hashes


def role_of(view):
    first = (view.users or [""])[0]
    if first.startswith("You are fixing a bug"):
        return "fix"
    if first.lstrip().startswith("# reproducer"):
        return "repro"
    if first.startswith("Review this automated fix"):
        return "review"
    return "other"


class Script:
    """script(view) for the fixture repo, with a mode per run. Fix: on each prompt (the first, or a follow-up: a review's, the
    interruption's) write the source, then say done. Repro: write the failing test, then the JSON block the chain parses.
    Review: the first asks for changes when `review_asks`, then approves.
    modes: "grind" (the fix never stops calling a tool), "hold" (the fix's first request is never answered),
    "hold_second" (its second is not: a run killed in the middle of the fix; the mode can be set to "" for the resume)."""

    def __init__(self, review_asks=False, mode=""):
        self.review_asks, self.mode, self.reviews = review_asks, mode, 0

    def __call__(self, view):
        role, fresh = role_of(view), len(view.new_tool_results)
        if role == "fix":
            if self.mode == "hold" or (self.mode == "hold_second" and fresh):
                return Reply(hold=120)
            if self.mode == "grind":
                return Reply(tools=[("read", {"path": "src/a.js"})])
            if not fresh:
                reviewed = any(u.startswith("A REVIEW OF YOUR FIX ASKED FOR CHANGES") for u in view.users)
                return Reply(text="Looking.", tools=[("write", {"path": "src/a.js", "content": FIX_SOURCE_REVIEWED if reviewed else FIX_SOURCE})])
            return Reply(text=FIX_ANSWER)
        if role == "repro":
            if not fresh:
                return Reply(tools=[("write", {"path": "src/a.test.js", "content": FIX_TEST})])
            return Reply(text="done\n```json\n" + json.dumps(REPRO_ANSWER) + "\n```")
        if role == "review":
            self.reviews += 1
            return Reply(text=REVIEW_ASKS if self.review_asks and self.reviews == 1 else REVIEW_APPROVES)
        return Reply(text="unexpected request")


RECORDER = """\
import json, os, sys
keys = ("HOME", "PI_CODING_AGENT_DIR", "PI_RESUME_SESSION", "BUGFIX_PI_RULES", "HTTP_PROXY", "NO_PROXY", "PI_OFFLINE")
path = os.path.join(os.environ["SEAM_STATE"], "argv.jsonl")
n = len(open(path).readlines()) if os.path.exists(path) else 0
with open(path, "a") as f:
    f.write(json.dumps({"seq": n, "argv": sys.argv[1:], "cwd": os.getcwd(), "pid": os.getppid(),
                        "env": {k: os.environ[k] for k in keys if k in os.environ}}) + "\\n")
print(n)
"""

WRAPPER = """\
#!/usr/bin/env bash
# the real CLI, with what it was started with and its whole stdout kept
set -u
N=$(python3 "$SEAM_REC" "$@")
"$SEAM_REAL" "$@" | tee "$SEAM_STATE/stream_$N.jsonl"
exit ${PIPESTATUS[0]}
"""


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


class Run:
    """The evidence of one chain run."""

    def __init__(self, **kw):
        self.plan, self.out, self.elapsed = None, "", 0.0
        self.requests, self.gets, self.invocations, self.streams = [], [], [], []
        self.calls, self.spend, self.proxy_seen, self.review_prompts = [], None, [], []
        self.home_left, self.run_dir_files = [], {}
        self.__dict__.update(kw)

    def by_role(self, role):
        return [v for v in self.requests if role_of(v) == role]


class SeamSuite(unittest.TestCase):
    """The checks of HARNESS_SEAMS.md, one method per seam. A harness's test file subclasses it and sets NAME and BINARY; what
    differs by harness (flags, the sessions' place, what its tools are called) is a class attribute or a hook."""

    NAME = ""
    BINARY = ""
    READ_TOOLS = ()          # the tool names a read role is offered (the harness's own names)
    WRITE_TOOLS = ()         # tools a read role must not be offered
    SESSIONS = ""            # glob, relative to the run directory, of the harness's session store
    runs = {}

    # ---- hooks

    @classmethod
    def flags(cls):
        """The chain flags that put fix, repro and review on this harness (none for pi, which is the default)."""
        return []

    @classmethod
    def env(cls, ep, proxy, key_file):
        """The environment of the CLI beyond the chain's: what the worker image sets, and the proxy as the worker has it."""
        return {"HTTP_PROXY": proxy.url, "HTTPS_PROXY": proxy.url, "NO_PROXY": "127.0.0.1,localhost,::1", "NODE_USE_ENV_PROXY": "1"}

    @classmethod
    def parse(cls, stream_text):
        import harness
        return harness.get(cls.NAME).parse(stream_text, {})

    # ---- the runs

    @classmethod
    def setUpClass(cls):
        cls.real = shutil.which(cls.BINARY)
        if not cls.real:
            raise unittest.SkipTest(f"the real {cls.BINARY} CLI is not on PATH: the seam end-to-end needs it "
                                    f"(HARNESS_SEAMS.md, 'The minimum seam end-to-end'); CI's image does not have it")
        cls.tmp = tempfile.mkdtemp()
        cls.addClassCleanup(shutil.rmtree, cls.tmp, True)
        cls.runs = {}
        cls.version = subprocess.run([cls.real, "--version"], capture_output=True, text=True).stdout.strip()
        print(f"seam end-to-end: {cls.NAME} {cls.version} at {cls.real}", file=sys.stderr)
        cls.runs["main"] = cls.run_chain("main", Script(review_asks=True), "--review-kickback-max", "1", calls=True, spend=True)
        cls.runs["read"] = cls.run_chain("read", Script(), "--review-tools", "read")
        cls.runs["cap"] = cls.run_chain("cap", Script(mode="grind"), "--fix-max-turns", "1", "--kickback-max", "0", "--no-repro",
                                        "--episode-timeout", "60")
        cls.runs["timeout"] = cls.run_chain("timeout", Script(mode="hold"), "--episode-timeout", "4", "--kickback-max", "0", "--no-repro")
        cls.runs["declared"] = cls.run_chain("declared", Script(), env=cls.declared_rules(DECLARED_MARKER, True))
        cls.runs["undeclared_hash"] = cls.run_chain("undeclared_hash", Script(), "--no-repro", env=cls.declared_rules(DECLARED_MARKER, False))
        cls.runs["killed"], cls.runs["resumed"] = cls.run_killed_and_resumed()

    @classmethod
    def declared_rules(cls, marker, honest):
        """The environment of a spec that declares its `rules` prompt (ADR-0018): a file under a code root's fleets/bugfix/ and
        the hash flotillad loaded it with; `honest` False gives a hash that is not the file's."""
        import hashlib
        root = tempfile.mkdtemp(dir=cls.tmp)
        path = os.path.join(root, "fleets", "bugfix", "playbooks", "rules.md")
        os.makedirs(os.path.dirname(path))
        text = f"{marker}\nKeep it small.\n"
        with open(path, "w") as fh:
            fh.write(text)
        sha = hashlib.sha256(text.encode()).hexdigest() if honest else "0" * 64
        return {"FLOTILLA_CODE_ROOT": root,
                "FLOTILLA_PROMPTS": json.dumps({"rules": {"path": "playbooks/rules.md", "sha256": sha}})}

    @classmethod
    def fixture(cls, name, ep, proxy):
        import test_fix as tf
        f = tf.Fixture(["actionable", "actionable", "actionable"], [])
        cls.addClassCleanup(f.close)
        state = os.path.join(cls.tmp, name)
        os.makedirs(state)
        # the real leaves for the roles under test; the verdict and the advisor stay the test's (wave 2 of the plan)
        bakeoff = os.path.join(os.path.dirname(HERE), "bugfix-fleet")
        for leaf in ("pi.sh", "repro.sh"):
            shutil.copy(os.path.join(bakeoff, "bakeoff", "harnesses", leaf), os.path.join(f.bakeoff, "harnesses", leaf))
        os.makedirs(os.path.join(f.d, "agents"), exist_ok=True)
        shutil.copy(os.path.join(bakeoff, "agents", "repro.md"), os.path.join(f.d, "agents", "repro.md"))
        # the repository's own rules file, which no harness may read: the fleet's rules replace it
        remote = os.path.join(f.d, "remotes", "example", "shop")
        work = os.path.join(f.d, "work")
        git = lambda *a, cwd=work: subprocess.run(["git", *a], cwd=cwd, check=True, capture_output=True,  # noqa: E731
                                                  env=dict(os.environ, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t",
                                                           GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t"))
        git("clone", "-q", remote, work, cwd=f.d)
        with open(os.path.join(work, "AGENTS.md"), "w") as fh:
            fh.write(f"{REPO_RULES_MARKER}\nNever fix anything.\n")
        git("add", "-A")
        git("commit", "-qm", "rules")
        git("push", "-q", "origin", "HEAD:main")
        f.base = subprocess.run(["git", "-C", remote, "rev-parse", "main"], capture_output=True, text=True).stdout.strip()
        key_file = os.path.join(state, "key")
        with open(key_file, "w") as fh:
            fh.write(KEY + "\n")
        models = {"endpoints": {"gateway-bugfix": {"base_url": ep.base_url, "kind": "litellm", "key_file": key_file}},
                  "aliases": {"bugfix-flash": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-flash"},
                              "bugfix-reviewer": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-reviewer"}}}
        rec = os.path.join(state, "seam_rec.py")
        with open(rec, "w") as fh:
            fh.write(RECORDER)
        os.remove(os.path.join(f.d, "bin", cls.BINARY)) if os.path.exists(os.path.join(f.d, "bin", cls.BINARY)) else None
        os.makedirs(os.path.join(f.d, "bin"), exist_ok=True)
        wrapper = os.path.join(f.d, "bin", cls.BINARY)
        with open(wrapper, "w") as fh:
            fh.write(WRAPPER)
        os.chmod(wrapper, 0o755)
        f.extra_env = dict(cls.env(ep, proxy, key_file), FLOTILLA_MODELS=json.dumps(models), SEAM_STATE=state, SEAM_REC=rec,
                           SEAM_REAL=cls.real, PI_OFFLINE="1", PI_SKIP_VERSION_CHECK="1")
        f.repro = True
        return f, state

    @classmethod
    def evidence(cls, run, ep, proxy, state, f):
        run.requests, run.gets, run.proxy_seen = ep.posts(), ep.gets(), list(proxy.seen)
        path = os.path.join(state, "argv.jsonl")
        if os.path.exists(path):
            with open(path) as fh:
                run.invocations = [json.loads(x) for x in fh]
        for inv in run.invocations:
            sp = os.path.join(state, f"stream_{inv['seq']}.jsonl")
            with open(sp) as fh:
                run.streams.append(fh.read()) if os.path.exists(sp) else run.streams.append("")

    @classmethod
    def run_chain(cls, name, script, *extra, calls=False, spend=False, env=None):
        import fix
        ep, proxy = ScriptedEndpoint(script), RecordingProxy()
        cls.addClassCleanup(ep.close)
        cls.addClassCleanup(proxy.close)
        f, state = cls.fixture(name, ep, proxy)
        f.extra_env.update(env or {})
        home = os.path.join(cls.tmp, name + "-home")
        os.makedirs(home)
        env = {"HOME": home}
        if calls:
            env["FLOTILLA_CALLS_FILE"] = os.path.join(state, "calls.jsonl")
        prompts, real = [], fix.Chain.review_prompt

        def spy(chain, *a, **k):
            prompts.append(real(chain, *a, **k))
            return prompts[-1]
        out, t0 = io.StringIO(), time.time()
        more = ["--spend-file", ".spend", "--spend-poll", "0.05", "--spend-settle", "2", "--spend-stable", "0.2"] if spend else []
        with unittest.mock.patch.dict(os.environ, env), unittest.mock.patch.object(fix.Chain, "review_prompt", spy), \
                contextlib.redirect_stdout(out), stdin_closed():
            plan, _ = f.run(*cls.flags(), *more, *extra)
        run = Run(plan=plan, out=out.getvalue(), elapsed=time.time() - t0, review_prompts=prompts, fixture=f, home=home)
        cls.evidence(run, ep, proxy, state, f)
        if calls:
            with open(env["FLOTILLA_CALLS_FILE"]) as fh:
                run.calls = [json.loads(x) for x in fh]
        if spend:
            with open(os.path.join(home, ".spend")) as fh:
                run.spend = float(fh.read())
        run.home_left = sorted(x for x in os.listdir(home) if x != ".spend")
        return run

    @classmethod
    def run_killed_and_resumed(cls):
        """The chain as a process, killed (SIGTERM to its process group, as the worker does at its timeout) while the fix is
        in flight, and started again with FLOTILLA_RESUME (ADR-0019)."""
        import fix
        script = Script(mode="hold_second")
        ep, proxy = ScriptedEndpoint(script), RecordingProxy()
        cls.addClassCleanup(ep.close)
        cls.addClassCleanup(proxy.close)
        f, state = cls.fixture("killed", ep, proxy)
        home = os.path.join(cls.tmp, "killed-home")
        os.makedirs(home)
        calls = os.path.join(state, "calls.jsonl")
        plan = os.path.join(f.d, "plan.json")
        env = dict(os.environ, FLOTILLA_PLAN_FILE=plan, FAKE_STATE=f.state, GIT_ALLOW_PROTOCOL="file", GIT_REMOTE_BASE=f.remotes,
                   PATH=os.path.join(f.d, "bin") + os.pathsep + os.environ["PATH"], HOME=home, BUGFIX_BAKEOFF_DIR=f.bakeoff,
                   GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_CASE=json.dumps({"id": "case-20261115-abc", "kind": "code_bug", "target": f"repo:{f.repo}/issues/12",
                                             "data": {"issue": {"repo": f.repo, "number": 12, "title": "value is 1",
                                                                "body": "value() returns 1, want 2", "url": "u"}}}),
                   FLOTILLA_CALLS_FILE=calls, **f.extra_env)
        cmd = [sys.executable, os.path.join(HERE, "fix.py"), "--repo", f.repo, "--worker-model", "bugfix-flash",
               "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer", "--setup", "true",
               "--test-cmd", f"python3 {f.suite} run %s", "--related-cmd", f"python3 {f.suite} related %s",
               "--scratch", f.scratch, *cls.flags()]
        p = subprocess.Popen(cmd, env=env, start_new_session=True, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT, text=True)
        cls.addClassCleanup(lambda: p.poll() is None and os.killpg(p.pid, signal.SIGKILL))
        deadline = time.time() + 90
        while time.time() < deadline and not [v for v in ep.posts() if role_of(v) == "fix" and v.new_tool_results]:
            time.sleep(0.1)
        held = [v for v in ep.posts() if role_of(v) == "fix" and v.new_tool_results]
        os.killpg(p.pid, signal.SIGTERM)
        out_a, _ = p.communicate(timeout=30)
        run_dir = os.path.join(f.scratch, "runs", "case-20261115-abc")
        killed = Run(out=out_a, held=bool(held), exit=p.returncode, fixture=f, home=home)
        cls.evidence(killed, ep, proxy, state, f)
        killed.checkpoint = None
        if os.path.exists(os.path.join(run_dir, "checkpoint.json")):
            with open(os.path.join(run_dir, "checkpoint.json")) as fh:
                killed.checkpoint = json.load(fh)
        killed.run_dir = run_dir
        killed.run_dir_files = {}
        for root, _, names in os.walk(run_dir):
            for n in names:
                fp = os.path.join(root, n)
                try:
                    with open(fp, "rb") as fh:
                        killed.run_dir_files[os.path.relpath(fp, run_dir)] = fh.read(2_000_000)
                except OSError:
                    pass
        killed.sessions = sorted(os.path.relpath(x, run_dir) for x in glob.glob(os.path.join(run_dir, cls.SESSIONS)))
        before = len(ep.posts())
        script.mode = ""
        env["FLOTILLA_RESUME"] = json.dumps({"run_id": "r1", "spend_usd": 0})
        p2 = subprocess.run(cmd, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                            timeout=300)
        resumed = Run(out=p2.stdout, exit=p2.returncode, fixture=f, home=home)
        cls.evidence(resumed, ep, proxy, state, f)
        resumed.requests = ep.posts()[before:]
        resumed.invocations = resumed.invocations[len(killed.invocations):]
        resumed.streams = resumed.streams[len(killed.invocations):]
        for attr, path, load in (("plan", plan, json.load), ("calls", calls, lambda fh: [json.loads(x) for x in fh])):
            if os.path.exists(path):
                with open(path) as fh:
                    setattr(resumed, attr, load(fh))
        return killed, resumed

    # ---- the seams

    def fix_prompt(self):
        import harness_prompts as hp
        frozen = ("\n\nIMPORTANT: the failing test src/a.test.js reproduces the bug and is frozen. Fix the source so that it "
                  "passes; do not change, move or delete it (you may add other tests).")
        return hp.fix_prompt("make value() return 2" + frozen)

    def repro_prompt(self):
        import harness_prompts as hp
        with open(os.path.join(os.path.dirname(HERE), "bugfix-fleet", "agents", "repro.md")) as fh:
            body = hp.agent_def_body(fh.read())
        return hp.repro_prompt(body, "THE ISSUE AS FILED:\nvalue is 1\n\nvalue() returns 1, want 2\n\nTHE TASK THE VERDICT STEP WROTE FROM "
                                     "IT (it can be wrong; never take a phrase from it):\nmake value() return 2")

    def test_s0_the_scripted_chain_ran_to_a_plan(self):
        main = self.runs["main"]
        self.assertEqual(main.plan["outcome"], "plan", main.plan)
        self.assertEqual(sorted({role_of(v) for v in main.requests}), ["fix", "repro", "review"])
        self.assertTrue(self.runs["killed"].held, "the killed run did reach the held request")
        self.assertEqual(self.runs["resumed"].plan["outcome"], "plan", self.runs["resumed"].out[-600:])

    def test_s1_the_prompt_is_the_chains_and_only_the_chains(self):
        main = self.runs["main"]
        self.assertEqual(main.by_role("repro")[0].users, [self.repro_prompt()])
        self.assertEqual(main.by_role("fix")[0].users, [self.fix_prompt()])
        self.assertEqual(main.by_role("review")[0].users, [main.review_prompts[0]])
        self.assertTrue(main.review_prompts[0].startswith("Review this automated fix for example/shop#12"))

    def test_s2_the_fleets_rules_reach_the_harness_and_the_repositorys_do_not(self):
        main, rules = self.runs["main"], os.path.join(HERE, "fleet_rules.md")
        with open(rules) as fh:
            text = fh.read().strip().splitlines()[2]
        for role in ("fix", "repro"):
            for v in main.by_role(role):
                self.assertIn(RULES_MARKER, v.system, role)
                self.assertIn(text, v.system, role)
                self.assertNotIn(REPO_RULES_MARKER, v.system, role)
        for v in main.by_role("review"):
            self.assertNotIn(REPO_RULES_MARKER, v.system)
            self.assertNotIn(RULES_MARKER, v.system, "a review with no tools gets no rules about tools")
        for v in self.runs["read"].by_role("review"):
            self.assertIn(RULES_MARKER, v.system, "a review with tools does")
            self.assertNotIn(REPO_RULES_MARKER, v.system)
        declared = self.runs["declared"]
        self.assertEqual(declared.plan["outcome"], "plan", declared.plan)
        for v in declared.by_role("fix"):
            self.assertIn(DECLARED_MARKER, v.system, "the spec's declared rules are the rules")
            self.assertNotIn(RULES_MARKER, v.system, "and replace the default file")
        refused = self.runs["undeclared_hash"]
        self.assertEqual(refused.requests, [], "a rules file that does not hash to the declaration is refused before any model call")
        self.assertEqual(refused.plan["outcome"], "escalate")
        self.assertTrue(refused.plan["detail"].startswith("prompt: the rules prompt differs from the one flotillad checked"), refused.plan)

    def test_s3_the_model_is_reached_through_the_gateway_with_the_key_and_the_key_is_nowhere_else(self):
        for name in ("main", "read", "cap", "timeout", "declared"):
            run = self.runs[name]
            self.assertTrue(run.requests, name)
            for v in run.requests:
                self.assertEqual((v.path, v.auth), ("/v1/chat/completions", f"Bearer {KEY}"), name)
                self.assertEqual(v.model, "fleet-bugfix-reviewer" if role_of(v) == "review" else "fleet-bugfix-flash", name)
            for inv in run.invocations:
                self.assertNotIn(KEY, json.dumps(inv), "the key is in no argv or environment the chain set")
        killed = self.runs["killed"]
        leaked = [p for p, data in killed.run_dir_files.items() if KEY.encode() in data]
        self.assertEqual(leaked, [], "the key is in no file of the run directory")
        self.assertTrue(killed.run_dir_files, "the killed run's directory was kept and read")

    def test_s4_the_stream_becomes_an_episode(self):
        import test_harness as th
        main = self.runs["main"]
        eps = [self.parse(s) for s in main.streams]
        for ep in eps:
            th.check_contract(self, ep)
        # invocation order: repro, fix, review (asks for changes), fix (resumed), review (approves)
        repro, fix1, rev1, fix2, rev2 = eps
        self.assertEqual((len(repro.turns), repro.exit_reason), (2, "completed"))
        self.assertEqual(json.loads(repro.answer.split("```json")[1].split("```")[0]), REPRO_ANSWER)
        self.assertEqual((len(fix1.turns), fix1.answer, fix1.error), (2, FIX_ANSWER, ""))
        self.assertEqual(fix1.usage_total["input"], 200)
        self.assertEqual(fix1.usage_total["output"], 40)
        self.assertEqual(fix1.turns[0].stop, "tool_calls")
        self.assertEqual(fix1.turns[1].stop, "stop")
        self.assertEqual([(t["name"], t["arg"], t.get("ok")) for t in fix1.turns[0].tools], [("write", "src/a.js", True)])
        self.assertEqual(rev1.answer, REVIEW_ASKS)
        self.assertEqual(rev2.answer, REVIEW_APPROVES)
        self.assertEqual(len(fix2.turns), 2, "a resumed episode's turns are this call's, not the session's")
        import fix
        self.assertEqual(fix.usage(main.streams[1]), (2, 40), "the chain's own count of pi's stream agrees")

    def test_s5_the_cap_and_the_timeout_end_the_episode_and_the_process_group(self):
        import harness_pi
        main = self.runs["main"]
        for stream, ep_turns in zip(main.streams, (2, 2, 1, 2, 1)):
            ends = [ln for ln in stream.splitlines() if harness_pi.turn_ended(ln)]
            self.assertEqual(len(ends), ep_turns, "turn_ended counts the model calls of the real stream")
        cap = self.runs["cap"]
        self.assertIn("the fix episode reached its turn cap (1) with no change", cap.plan["detail"])
        self.assertLess(cap.elapsed, 45, "the grinding fix was stopped by the cap, not the 60 s timeout")
        self.assertLessEqual(len(cap.by_role("fix")), 2)
        t = self.runs["timeout"]
        self.assertIn("the fix episode ran out of time with no change", t.plan["detail"])
        self.assertTrue(t.by_role("fix"))
        deadline = time.time() + 10
        alive = list(t.invocations + cap.invocations)
        while alive and time.time() < deadline:
            alive = [i for i in alive if os.path.exists(f"/proc/{i['pid']}")]
            time.sleep(0.1)
        self.assertEqual(alive, [], "no process of a capped or timed-out episode is left")

    def test_s6_a_read_role_is_offered_only_read_tools(self):
        main, read = self.runs["main"], self.runs["read"]
        for v in main.by_role("review"):
            self.assertEqual(v.tool_names, [], "a review with no tools")
        for v in read.by_role("review"):
            self.assertEqual(v.tool_names, sorted(self.READ_TOOLS))
            self.assertFalse(set(v.tool_names) & set(self.WRITE_TOOLS))
        for role in ("fix", "repro"):
            for v in main.by_role(role):
                self.assertTrue(set(self.WRITE_TOOLS) <= set(v.tool_names), (role, v.tool_names))

    def test_s7_a_session_continues_with_its_history_after_a_review_and_after_a_kill(self):
        main, killed, resumed = self.runs["main"], self.runs["killed"], self.runs["resumed"]
        first, second = main.by_role("fix")[0], main.by_role("fix")[2]
        self.assertEqual(second.users[0], first.users[0], "the same session")
        self.assertTrue(second.users[1].startswith("A REVIEW OF YOUR FIX ASKED FOR CHANGES."))
        self.assertIn(REVIEW_ASKS.splitlines()[0], second.users[1], "the whole review is the next message")
        self.assertGreater(len(second.messages), len(first.messages), "the earlier turns are in the conversation")
        self.assertEqual(killed.checkpoint["phase"], "attempt")
        self.assertEqual(killed.checkpoint["harness"], "pi")
        self.assertEqual(len(killed.sessions), 2, f"the killed run's sessions (repro, fix) are in the run directory: {killed.sessions}")
        fixes = resumed.by_role("fix")
        self.assertTrue(fixes, resumed.out[-500:])
        self.assertEqual(fixes[0].users[0], killed.by_role("fix")[0].users[0], "the resumed fix is the killed fix's session")
        self.assertTrue(fixes[0].users[1].startswith("The run was interrupted. The worktree is as you left it; continue."))
        self.assertTrue([m for m in fixes[0].messages if m.get("role") == "tool"], "its tool results from before the kill are there")
        self.assertIn("resumed from checkpoint", resumed.out)
        self.assertEqual(resumed.plan["outcome"], "plan")
        self.assertEqual(resumed.by_role("repro"), [], "the frozen proof is not written again")

    def test_s8_every_model_call_is_a_line_of_the_calls_file(self):
        main = self.runs["main"]
        lines = [c for c in main.calls if c.get("kind") != "step"]
        self.assertEqual(len(lines), len(main.requests), "one line per model call")
        self.assertTrue(all(c["tokens_in"] == 100 and c["tokens_out"] == 20 for c in lines))
        seqs = {}
        for c in lines:
            seqs.setdefault(c["episode"], set()).add(c["episode_seq"])
        self.assertEqual(sorted(len(v) for v in seqs.values()), [1, 1, 1, 1], "an episode keeps its order number")
        self.assertEqual(sorted(x for v in seqs.values() for x in v), [1, 2, 3, 4],
                         "an episode per pi session: the repro's, the fix's (resumed in the same one) and two reviews")
        self.assertTrue(all("harness" not in c for c in lines), "pi's lines carry no harness field (absent means pi)")
        writes = [t for c in lines for t in c["tools"] if t["name"] == "write"]
        self.assertEqual([(t["arg"], t.get("ok")) for t in writes], [("src/a.test.js", True), ("src/a.js", True), ("src/a.js", True)])
        self.assertEqual({c["role"] for c in lines}, {"coder", "reviewer"})

    def test_s9_the_cost_is_the_gateways(self):
        main = self.runs["main"]
        self.assertAlmostEqual(main.spend, 0.01 * len(main.requests))
        self.assertIn("source=gateway", main.out)

    def test_s10_nothing_but_the_gateway_is_contacted(self):
        for name, run in self.runs.items():
            self.assertEqual(run.proxy_seen, [], f"{name}: a host other than the gateway was tried")
            self.assertEqual({v.path for v in run.gets}, {"/key/info"} if run.gets else set(), f"{name}: only the spend read")
        self.assertEqual(self.runs["killed"].proxy_seen + self.runs["resumed"].proxy_seen, [])

    def test_s10_control_the_proxy_does_see_a_host_that_is_not_the_gateway(self):
        """Without this a silent proxy could mean a CLI that ignores it. The CLI is pointed at a host that is not on NO_PROXY."""
        self.assertTrue(self.control_sees_other_hosts(), "the CLI went round the proxy: S10's evidence means nothing for it")

    @classmethod
    def control_sees_other_hosts(cls):
        raise NotImplementedError("the harness's control: start the CLI against a gateway.invalid base URL and return whether the proxy saw it")

    def test_s11_the_work_and_the_session_store_are_under_the_run_directory(self):
        main, killed = self.runs["main"], self.runs["killed"]
        for inv in killed.invocations + main.invocations:
            self.assertTrue(inv["cwd"].endswith("/wt"), inv["cwd"])
            self.assertEqual(inv["env"]["HOME"], os.path.dirname(inv["cwd"]), "HOME is the run directory")
        self.assertTrue(killed.sessions, "the session store is a place under the run directory")
        for name, run in self.runs.items():
            self.assertEqual(run.home_left, [], f"{name}: nothing in the worker's own HOME but the spend file")
        self.assertTrue(any(n.startswith("wt/") for n in killed.run_dir_files), "the worktree is in the run directory")
