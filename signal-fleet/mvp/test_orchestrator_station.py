"""python3 test_orchestrator_station.py: the station-fed triage (orchestrator_station.py) against the code the flat routine runs.

The proof AGENTS.md rule 3 asks for, on the repo: the old path (triage.py as it is, reading its settings from the environment and
harness-default.json) and the new one (orchestrator_station.py, reading the decide station of fleets/triage/cycle/v1 from
FLOTILLA_STATIONS as flotillad would hand it) are sent the same signals against the same scripted model endpoint. The requests the model
receives must be byte for byte the same, and so must the decisions, the way a model failure ends and the calls file's tokens. Each path runs
in a process of its own (the new one rewires the triage module), as a driver of this same file: `test_orchestrator_station.py --driver old|station OUTFILE`.
What this cannot prove is decision parity on real signals and a real model: that is signal-fleet/mvp/gate_stations_replay.py, run by the
installation on its private corpus.
"""
import ast
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
import urllib.error

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, "..", ".."))
GATEWAY = "http://litellm:4000/v1"
MODEL = "fleet-triage-flash"   # what the old path sends as the model, and what the alias triage-flash resolves to
RUNENV = os.path.join(ROOT, "internal", "sched", "testdata", "runenv_shipped.json")


def shipped_env(routine="triage/cycle@v1"):
    """The environment flotillad hands a run of the shipped version (the runenv golden: a Go test pins it to the loader)."""
    with open(RUNENV) as f:
        return json.load(f)[routine]


def models_config():
    return json.dumps({
        "endpoints": {"gateway": {"base_url": GATEWAY, "kind": "litellm", "key_env": "OPENROUTER_API_KEY",
                                  "defaults": {"tools": "native", "json": "object", "cost": "reported"}}},
        "aliases": {"triage-flash": {"endpoint": "gateway", "model": MODEL}, "triage-pro": {"endpoint": "gateway", "model": "fleet-triage-pro"}}})


# ---- the scenarios: a signal, the model's scripted replies, and what each probe answers ----
def sig(name, alert, summary="", labels=None):
    return {"fingerprint": f"fp-{name}", "occurrence_id": f"oc-{name}", "source": "glitchtip", "alertname": alert,
            "labels": dict({"project": "shop"}, **(labels or {})), "summary": summary, "startsAt": "2026-11-19T00:00:00Z", "raw": {}}


PROBE = lambda name, **a: json.dumps({"action": "probe", "probe": name, "probe_args": a})  # noqa: E731
FILE = json.dumps({"action": "decide", "disposition": "file", "reason": "r", "certainty": "high",
                   "file": {"work_type": "bug", "title": "t", "symptom": "s", "area": "a", "evidence": ["e"],
                            "acceptance": [{"criterion": "c", "intent_source": "reporter"}]}})
DISMISS = json.dumps({"action": "decide", "disposition": "dismiss", "reason": "noise", "certainty": "high",
                      "dismissal_evidence": "logs show a single retry that succeeded"})
CLEANUP = json.dumps({"action": "decide", "disposition": "cleanup", "reason": "smoke", "marker": "smoke", "certainty": "high"})
ESCALATE = json.dumps({"action": "decide", "disposition": "escalate", "reason": "unclear", "question": "q?", "certainty": "low"})
LONG = "x" * 3000
HTTP = lambda code: ("raise", code)  # noqa: E731

SCENARIOS = [
    ("transient-hint", sig("a", "Deepgram request timed out after 30s"), [PROBE("service_logs", service="shop"), DISMISS]),
    ("file", sig("b", "KeyError in checkout"), [PROBE("event_detail"), PROBE("metric", query="up"), FILE]),
    ("environment-hint", sig("c", "OSError: [Errno 28] No space left on device"), [PROBE("occurrence_history"), ESCALATE]),
    ("bad-json-and-long-echo", sig("d", "TypeError in cart"), ["this is not json " + "y" * 900, PROBE("trace", trace_id="t1"), FILE]),
    ("empty-completion", sig("e", "ValueError in search"), ["", PROBE("source_state", path="a.py"), FILE]),
    ("probe-budget-and-final-call", sig("f", "IndexError in feed"),
     [PROBE("event_detail"), PROBE("metric", query="a"), PROBE("trace", trace_id="x"), PROBE("service_logs", service="s"), DISMISS]),
    ("unknown-probe", sig("g", "RuntimeError in worker"), [PROBE("shell", cmd="ls"), ESCALATE]),
    ("long-result", sig("h", "MemoryError in export"), [PROBE("service_logs", service="export"), ESCALATE]),
    ("cleanup", sig("i", "smoke test failed", labels={"environment": "test"}), [CLEANUP]),
    ("operational", sig("j", "cost soft cap exceeded for key x"), []),
    ("http-500", sig("k", "ConnectionError in api"), [HTTP(500)]),
    ("http-401", sig("l", "ConnectionError in api"), [HTTP(401)]),
    ("no-decision", sig("m", "Exception in cron"), ["{}", "{}", "{}", "{}"]),
]
PROBE_RESULTS = {"event_detail": {"data": {"result": "stack ..."}}, "occurrence_history": {"count": 1, "level": "info"},
                 "service_logs": LONG, "metric": {"data": {"result": [1]}}, "trace": "span ok", "source_state": "def f(): pass"}


# ---- the driver: one path, every scenario, in this process ----
def driver(mode, outpath):
    os.environ.setdefault("SF_OBSERV_DISABLED", "1")
    sys.path.insert(0, HERE)
    if mode == "station":
        import orchestrator_station
        orchestrator_station.install()
    import observ
    import triage
    observ.new_trace_id = lambda: "trace-fixed"
    out, state = [], {"replies": [], "sent": []}

    def post(url, body, headers=None, timeout=60):
        state["sent"].append({"url": url, "data": json.dumps(body), "has_auth": bool((headers or {}).get("Authorization"))})
        r = state["replies"].pop(0)
        if isinstance(r, tuple):
            raise urllib.error.HTTPError(url, r[1], "scripted", {}, None)
        return {"choices": [{"message": {"content": r}}], "usage": {"prompt_tokens": 100 + len(state["sent"]), "completion_tokens": 7},
                "model": MODEL}

    triage.post_json = post
    triage._probe = lambda signal, name, args, table: PROBE_RESULTS[name]
    for name, signal, replies in SCENARIOS:
        state["replies"], state["sent"] = list(replies), []
        try:
            res = ("decided", triage.triage(dict(signal)))
        except Exception as e:  # noqa: BLE001
            res = ("raised", type(e).__name__ + ": " + str(e))
        out.append({"name": name, "sent": state["sent"], "result": json.loads(json.dumps(res, default=str)), "left": len(state["replies"])})
    with open(outpath, "w") as f:
        json.dump(out, f)


def run_driver(mode, tmp):
    env = {"PATH": os.environ.get("PATH", ""), "HOME": tmp, "SF_OBSERV_DISABLED": "1", "OPENROUTER_API_KEY": "k-secret",
           "FLOTILLA_CALLS_FILE": os.path.join(tmp, f"calls-{mode}.jsonl"), "PYTHONHASHSEED": "0"}
    if mode == "old":
        env.update(SF_OPENROUTER_URL=GATEWAY + "/chat/completions", SF_TRIAGE_MODEL=MODEL)
    else:
        shipped = shipped_env()
        env.update(FLOTILLA_STATIONS=shipped["FLOTILLA_STATIONS"], FLOTILLA_SKILLS=shipped["FLOTILLA_SKILLS"], FLOTILLA_MODELS=models_config())
    outpath = os.path.join(tmp, f"out-{mode}.json")
    p = subprocess.run([sys.executable, os.path.abspath(__file__), "--driver", mode, outpath], env=env, cwd=HERE, capture_output=True, text=True)
    if p.returncode != 0:
        raise AssertionError(f"the {mode} driver failed:\n{p.stderr[-3000:]}")
    with open(outpath) as f:
        return json.load(f), env["FLOTILLA_CALLS_FILE"]


def calls(path):
    if not os.path.exists(path):
        return []
    with open(path) as f:
        return [json.loads(x) for x in f if x.strip()]


class RequestEquivalence(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        cls.old, cls.old_calls = run_driver("old", cls.tmp)
        cls.new, cls.new_calls = run_driver("station", cls.tmp)

    def test_every_request_the_model_receives_is_the_same_bytes(self):
        self.assertEqual([s["name"] for s in self.old], [s[0] for s in SCENARIOS])
        sent = 0
        for o, n in zip(self.old, self.new):
            with self.subTest(o["name"]):
                self.assertEqual(o["sent"], n["sent"])
                self.assertEqual(o["left"], n["left"])
                sent += len(o["sent"])
        self.assertGreater(sent, 20, "the scenarios sent almost nothing: the comparison proves nothing")

    def test_every_decision_and_failure_is_the_same(self):
        for o, n in zip(self.old, self.new):
            with self.subTest(o["name"]):
                got = n["result"]
                if got[0] == "raised":   # a transport failure's text names the model alias, which is the station's: the one difference
                    got = [got[0], got[1].replace("triager call failed: triage-flash:", "triager call failed: fleet-triage-flash:")]
                self.assertEqual(o["result"], got)
        kinds = {s["result"][0] for s in self.old}
        self.assertEqual(kinds, {"decided", "raised"})
        raised = [s["result"][1].split(":")[0] for s in self.old if s["result"][0] == "raised"]
        self.assertEqual(sorted(raised), ["TriageDeferred", "TriagerDown"])
        decisions = {s["result"][1]["disposition"] for s in self.old if s["result"][0] == "decided"}
        self.assertEqual(decisions, {"dismiss", "file", "escalate", "cleanup"})

    def test_the_hint_the_class_loads_and_the_trimmed_result_are_in_the_request(self):
        by = {s["name"]: s for s in self.new}
        first = json.loads(by["transient-hint"]["sent"][0]["data"])
        self.assertIn("transient", first["messages"][1]["content"].lower())
        last = json.loads(by["long-result"]["sent"][-1]["data"])
        self.assertIn("[truncated", last["messages"][-1]["content"])
        self.assertTrue(all(s["has_auth"] for s in by["file"]["sent"]), "the alias's key reaches the request")

    def test_the_calls_file_has_the_same_tokens_and_the_station_and_its_text(self):
        key = lambda c: (c["tokens_in"], c["tokens_out"], c["step"], c["decision_key"], c["tools"])  # noqa: E731
        old, new = calls(self.old_calls), calls(self.new_calls)
        self.assertEqual([key(c) for c in old], [key(c) for c in new])
        self.assertTrue(old and all("station" not in c for c in old))
        self.assertTrue(all(c["station"] == "decide" and c["alias"] == "triage-flash" and c["texts"][0]["name"] == "system" for c in new))
        bodies = [c["texts"][0] for c in new if "text" in c["texts"][0]]
        self.assertEqual(len(bodies), 1, "the system text's body is in the calls once per run, a reference after")
        self.assertIn("INVESTIGATE", bodies[0]["text"])
        self.assertTrue(all(c["texts"][0]["sha256"] == bodies[0]["sha256"] for c in new))


class GateStationsReplay(unittest.TestCase):
    """gate_stations_replay.py, the installation's gate, on a capture made here from the scripted scenarios: it passes on a faithful
    capture and fails on a request that is one byte off. Its real corpus is the installation's, never run here."""

    def capture(self, tmp, damage=False):
        old, _ = run_driver("old", tmp)
        scenario = {n: (r, s) for n, _, r in SCENARIOS for s in old if s["name"] == n}
        turns = []
        for name in ("file", "bad-json-and-long-echo", "probe-budget-and-final-call"):
            replies, got = scenario[name]
            for i, s in enumerate(got["sent"]):
                turns.append({"step": i, "request": s["data"] + (" " if damage and not turns else ""), "reply": replies[i],
                              "usage": {"prompt_tokens": 100 + i, "completion_tokens": 7}, "model": MODEL})
        d = os.path.join(tmp, "captures" + ("-bad" if damage else ""))
        os.makedirs(d)
        with open(os.path.join(d, "c1.json"), "w") as f:
            json.dump({"exact": {"turns": turns}}, f)
        shipped = shipped_env()
        contract = os.path.join(tmp, "contract.json")
        with open(contract, "w") as f:
            json.dump({"env": {"FLOTILLA_STATIONS": shipped["FLOTILLA_STATIONS"], "FLOTILLA_SKILLS": shipped["FLOTILLA_SKILLS"],
                               "FLOTILLA_MODELS": models_config()}}, f)
        return d, contract

    def gate(self, d, contract):
        env = {"PATH": os.environ.get("PATH", ""), "HOME": d, "SF_OBSERV_DISABLED": "1"}
        return subprocess.run([sys.executable, os.path.join(HERE, "gate_stations_replay.py"), d, "--contract", contract,
                               "--baseline", os.path.join(HERE, "triage.py")], env=env, cwd=HERE, capture_output=True, text=True)

    def test_a_faithful_capture_passes_and_a_damaged_one_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            ok = self.gate(*self.capture(tmp))
            self.assertEqual(ok.returncode, 0, ok.stdout + ok.stderr)
            self.assertIn("STATIONS REPLAY: PASS", ok.stdout)
            self.assertRegex(ok.stdout, r"history: \d+ recorded model turns on fleet-triage-flash replayed, 0 on another model")
        with tempfile.TemporaryDirectory() as tmp:
            bad = self.gate(*self.capture(tmp, damage=True))
            self.assertEqual(bad.returncode, 1, bad.stdout + bad.stderr)
            self.assertIn("STATIONS REPLAY: FAIL", bad.stdout)

    def test_a_corpus_of_another_model_is_no_data(self):
        with tempfile.TemporaryDirectory() as tmp:
            d, contract = self.capture(tmp)
            p = os.path.join(d, "c1.json")
            with open(p) as f:
                raw = f.read().replace(MODEL, "some-other-model")
            with open(p, "w") as f:
                f.write(raw)
            r = self.gate(d, contract)
            self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
            self.assertIn("NO DATA", r.stdout)


class StationIsTheOnlySource(unittest.TestCase):
    def env(self, **change):
        e = shipped_env()
        e = {k: e[k] for k in ("FLOTILLA_STATIONS", "FLOTILLA_SKILLS")}
        e.update(FLOTILLA_MODELS=models_config(), OPENROUTER_API_KEY="k", FLOTILLA_CODE_ROOT=ROOT)
        e.update(change)
        return e

    def settings(self, env):
        sys.path.insert(0, HERE)
        os.environ.setdefault("SF_OBSERV_DISABLED", "1")
        os.environ.setdefault("OPENROUTER_API_KEY", "k")
        import orchestrator_station
        return orchestrator_station, orchestrator_station.settings(env)

    def doc(self, mutate):
        d = json.loads(shipped_env()["FLOTILLA_STATIONS"])
        mutate(d)
        return {"FLOTILLA_STATIONS": json.dumps(d)}

    def refuses(self, env, text):
        mod, _ = self.settings(self.env())   # the unmutated one loads
        with self.assertRaises(Exception) as cm:
            mod.settings(env)
        self.assertIn(text, str(cm.exception))

    def test_the_shipped_station_gives_the_harness_the_first_fleets_loop(self):
        _, (cfg, text, reply_cap, call_timeout, station, rendered) = self.settings(self.env())
        with open(os.path.join(ROOT, "fleets", "triage", "harness-default.json")) as f:
            default = json.load(f)
        self.assertEqual(cfg["tools"], default["tools"])
        self.assertEqual(cfg["gates"], default["gates"])
        self.assertEqual(cfg["limits"], default["limits"])
        self.assertEqual(cfg["quirks"], default["quirks"])
        self.assertEqual(cfg["hooks"], default["hooks"])
        self.assertEqual((reply_cap, call_timeout, station["model"]), (8192, 90, "triage-flash"))
        self.assertEqual(rendered.text, text)

    def test_no_stations_is_refused(self):
        self.refuses(self.env(FLOTILLA_STATIONS=""), "FLOTILLA_STATIONS is not set")

    def test_a_tool_a_gate_a_turn_count_or_a_limit_that_is_not_triages_is_refused(self):
        def drop_tool(d):
            d["stations"]["decide"]["tool_names"].pop()
        self.refuses(self.env(**self.doc(drop_tool)), "tool_names must be the probe menu's")

        def bad_gate(d):
            d["stations"]["decide"]["gates"].append("nonesuch")
        self.refuses(self.env(**self.doc(bad_gate)), "gates ['nonesuch'] not registered")

        def turns(d):
            d["stations"]["decide"]["limits"]["max_turns"] = 6
        self.refuses(self.env(**self.doc(turns)), "not MAX_PROBES")

        def no_flow_limit(d):
            del d["version_limits"]["result_chars"]
        self.refuses(self.env(**self.doc(no_flow_limit)), "version limits.result_chars must be a positive whole number")

        def unknown_hook(d):
            d["stations"]["decide"]["hooks"]["after_tool"].append({"name": "nonesuch", "mode": "observe"})
        self.refuses(self.env(**self.doc(unknown_hook)), "nonesuch")

    def test_a_skills_rule_that_loads_a_skill_the_station_does_not_list_is_refused(self):
        def fewer(d):
            d["stations"]["decide"]["skills"].remove("hint-environment")
        self.refuses(self.env(**self.doc(fewer)), "hint-environment, which station decide does not list")

    def test_a_system_text_that_is_not_the_one_flotillad_hashed_is_refused(self):
        def tamper(d):
            d["stations"]["decide"]["texts"]["system"]["sha256"] = "0" * 64
        self.refuses(self.env(**self.doc(tamper)), "differs from the one flotillad checked")

    def test_a_slot_the_text_does_not_have_or_needs_is_refused_before_a_call(self):
        mod, _ = self.settings(self.env())
        import stations
        st = stations.load(self.env())
        with self.assertRaises(stations.StationsError):
            st.render("decide", "system", {"max_probes": "3"}, fleet="triage", root=ROOT)
        with self.assertRaises(stations.StationsError):
            st.render("decide", "system", {"max_probes": "3", "probe_menu": "m", "extra": "x"}, fleet="triage", root=ROOT)

    def test_the_system_text_renders_as_the_playbook_does(self):
        # fleets/triage/playbooks/triage.md (what triage.py reads without flotillad) and the version's system.md are one text
        mod, (_, text, _, _, _, _) = self.settings(self.env())
        import triage
        self.assertEqual(text, triage._sys())
        slots = set()
        for v in ("v1", "v2"):
            with open(os.path.join(ROOT, "fleets", "triage", "cycle", v, "system.md")) as f:
                slots |= set(re.findall(r"\{\{\s*(\w+)\s*\}\}", f.read()))
        self.assertEqual(slots, {"max_probes", "probe_menu"})


class NoModelInTheCode(unittest.TestCase):
    """The model, its endpoint and its limits are the version's: the module holds none of them as a literal."""
    FORBIDDEN = re.compile(r"(?i)flash|deepseek|openrouter|gpt-|claude|fleet-triage|triage-(pro|candidate)|\bpro\b|8192|\bSF_TRIAGE")

    def test_no_string_in_orchestrator_station_names_a_model_or_a_limit(self):
        with open(os.path.join(HERE, "orchestrator_station.py")) as f:
            tree = ast.parse(f.read())
        docs = set()
        for node in ast.walk(tree):
            if isinstance(node, (ast.Module, ast.FunctionDef, ast.ClassDef)) and ast.get_docstring(node, clean=False) is not None:
                docs.add(id(node.body[0].value))
        hits = [n.value for n in ast.walk(tree) if isinstance(n, ast.Constant) and isinstance(n.value, (str, int)) and id(n) not in docs
                and self.FORBIDDEN.search(str(n.value))]
        self.assertEqual(hits, [])


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "--driver":
        driver(sys.argv[2], sys.argv[3])
    else:
        unittest.main(verbosity=1)
