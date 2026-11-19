"""python3 runtime/test_stations.py: a version's stations as a run receives them (runtime/stations.py): the texts loaded under the
prompt rules and refused when they do not hash to what flotillad checked, `{{slot}}` rendering that fails before any model call, the
rendered text's record (masked, capped), and a calls line that says station and texts only when stations are in play."""
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
os.environ.setdefault("SF_OBSERV_DISABLED", "1")
import model_client as mc  # noqa: E402
import stations  # noqa: E402

TOKEN = "abcdefgh" + "12345678" + "ijkl"   # built here: a literal in a header line is what the secrets scan looks for


def sha(b):
    return hashlib.sha256(b).hexdigest()


class World:
    """A code root with fleets/triage/cycle/v1/task.md and the FLOTILLA_STATIONS flotillad would hand a run of it."""

    def __init__(self, tc, text=b"Decide {{case}} now.\n"):
        self.tmp = tempfile.TemporaryDirectory()
        tc.addCleanup(self.tmp.cleanup)
        self.root = self.tmp.name
        d = os.path.join(self.root, "fleets", "triage", "cycle", "v1")
        os.makedirs(d)
        self.file = os.path.join(d, "task.md")
        with open(self.file, "wb") as f:
            f.write(text)
        doc = {"schema_version": 1, "version_limits": {"max_probes": 3},
               "stations": {"decide": {"harness": "flotilla", "model": "flash", "tools": "none",
                                       "texts": {"task": {"path": "cycle/v1/task.md", "sha256": sha(text)}}}}}
        self.env = {"FLOTILLA_STATIONS": json.dumps(doc)}
        self.st = stations.load(self.env)

    def render(self, slots=None, **kw):
        return self.st.render("decide", "task", {"case": "c-1"} if slots is None else slots, fleet="triage", root=self.root, environ=self.env, **kw)


class Load(unittest.TestCase):
    def test_none_without_the_variable_and_an_error_for_garbage(self):
        self.assertIsNone(stations.load({}))
        for bad in ("{", "[]", json.dumps({"schema_version": 2, "stations": {}}), json.dumps({"schema_version": 1})):
            with self.assertRaises(stations.StationsError, msg=bad):
                stations.load({"FLOTILLA_STATIONS": bad})

    def test_names_and_the_versions_limits(self):
        w = World(self)
        self.assertEqual((w.st.names(), w.st.limits()), (["decide"], {"max_probes": 3}))
        with self.assertRaisesRegex(stations.StationsError, "no station 'nope'"):
            w.st.station("nope")


class Render(unittest.TestCase):
    def test_a_text_is_rendered_from_its_slots(self):
        r = World(self).render()
        self.assertEqual((r.text, r.station, r.name), ("Decide c-1 now.\n", "decide", "task"))

    def test_a_missing_slot_fails_before_any_model_call(self):
        with self.assertRaisesRegex(stations.StationsError, "needs the slot.*case"):
            World(self).render({})

    def test_a_slot_the_text_does_not_use_is_refused_too(self):
        with self.assertRaisesRegex(stations.StationsError, "does not use"):
            World(self).render({"case": 1, "extra": 2})

    def test_a_text_the_version_does_not_declare_fails_the_same_way(self):
        w = World(self)
        with self.assertRaisesRegex(stations.StationsError, "declares no text 'other'"):
            w.st.render("decide", "other", {}, fleet="triage", root=w.root, environ=w.env)

    def test_a_file_that_does_not_hash_to_what_flotillad_checked_is_refused(self):
        w = World(self)
        with open(w.file, "wb") as f:
            f.write(b"Decide something else {{case}}.\n")
        with self.assertRaisesRegex(prompts_error(), "differs from the one flotillad checked"):
            w.render()

    def test_the_prompt_rules_hold_for_a_station_text(self):
        for name, content, err in (("empty", b"  \n", "is empty"), ("latin", b"caf\xe9 {{case}}", "not UTF-8"),
                                   ("big", b"x" * (stations.TEXT_CAP + 1), "over 65536 bytes")):
            w = World(self, content)
            with self.assertRaisesRegex(prompts_error(), err, msg=name):
                w.render()

    def test_a_symlinked_text_is_refused(self):
        w = World(self)
        os.remove(w.file)
        other = os.path.join(w.root, "elsewhere.md")
        with open(other, "w") as f:
            f.write("Decide {{case}} now.\n")
        os.symlink(other, w.file)
        with self.assertRaisesRegex(prompts_error(), "symlink"):
            w.render()

    def test_a_rendered_text_is_a_loaded_record(self):
        w = World(self)
        out = os.path.join(w.root, "loaded.jsonl")
        w.env["FLOTILLA_LOADED_FILE"] = out
        w.render()
        with open(out) as f:
            rec = json.loads(f.readline())
        self.assertEqual((rec["kind"], rec["trust"], rec["title"]), ("playbook", "source", "prompt decide.task"))


def prompts_error():
    import prompts
    return prompts.PromptError


class Records(unittest.TestCase):
    def setUp(self):
        stations.forget_sent()

    def test_the_ref_is_the_hash_and_the_length_of_what_the_model_read(self):
        r = World(self, "Résumé {{case}}\n".encode()).render()
        raw = r.text.encode()
        self.assertEqual(r.ref(), {"name": "task", "sha256": sha(raw), "bytes": len(raw)})

    def test_a_credential_in_a_rendered_text_is_masked_in_what_is_kept(self):
        r = World(self).render({"case": f"Authorization: Bearer {TOKEN}"})
        rec = r.record()
        self.assertNotIn(TOKEN, json.dumps(rec))
        self.assertTrue(rec["redacted"] and "<redacted>" in rec["text"])
        self.assertEqual(rec["sha256"], r.ref()["sha256"], "the hash is of what the model read")

    def test_a_body_is_sent_once_per_hash_and_a_reference_after(self):
        """A run of many calls must not carry the same body on each (the worker's result has a size limit)."""
        r = World(self).render()
        first, again, third = r.record(), r.record(), r.record()
        self.assertEqual(first["text"], r.text)
        self.assertEqual(again, r.ref())
        self.assertEqual(third, r.ref())
        self.assertNotIn("text", again)
        other = stations.Rendered("decide", "task", "another text", "h").record()
        self.assertIn("text", other, "a different hash is sent in full once")
        stations.forget_sent()
        self.assertIn("text", r.record(), "a new run starts again")
        size = sum(len(json.dumps(r.record())) for _ in range(5000))
        self.assertLess(size, 5000 * 200, "5000 calls carry references, not bodies")

    def test_a_text_over_the_cap_is_a_hash_and_a_length_never_a_cut_text(self):
        r = stations.Rendered("s", "t", "x" * (stations.TEXT_CAP + 1), "h")
        rec = r.record()
        self.assertEqual((rec["text"], rec["over_cap"], rec["bytes"]), (None, True, stations.TEXT_CAP + 1))
        ok = stations.Rendered("s", "t", "x" * stations.TEXT_CAP, "h").record()
        self.assertEqual((ok["over_cap"], len(ok["text"])), (False, stations.TEXT_CAP))


class Calls(unittest.TestCase):
    def client(self, path):
        class T:
            def __call__(self, url, data, headers, timeout):
                return 200, {}, json.dumps({"choices": [{"message": {"content": "{}"}}], "model": "m", "usage": {"prompt_tokens": 1, "completion_tokens": 1}})
        cfg = {"endpoints": {"gw": {"base_url": "http://gw/v1", "key_env": "K"}}, "aliases": {"flash": {"endpoint": "gw", "model": "m"}}}
        return mc.Client(mc.load(cfg), transport=T(), environ={"K": "sk-test", "FLOTILLA_CALLS_FILE": path}, tracer=None)

    def test_a_line_says_station_and_texts_only_when_they_are_given(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "calls.jsonl")
            c = self.client(path)
            r = World(self).render()
            c.chat("flash", [], station="decide", texts=[r.ref()])
            c.chat("flash", [], station="decide")
            c.chat("flash", [])
            with open(path) as f:
                lines = [json.loads(x) for x in f]
        self.assertEqual((lines[0]["station"], lines[0]["texts"]), ("decide", [r.ref()]))
        self.assertEqual(("station" in lines[1], "texts" in lines[1]), (True, False))
        self.assertEqual(("station" in lines[2], "texts" in lines[2]), (False, False), "a call of a run without stations is the line it always was")

    def test_what_the_station_asks_is_not_sent_to_the_endpoint(self):
        sent = []

        class T:
            def __call__(self, url, data, headers, timeout):
                sent.append(data)
                return 200, {}, json.dumps({"choices": [{"message": {"content": "{}"}}], "model": "m", "usage": {}})
        cfg = {"endpoints": {"gw": {"base_url": "http://gw/v1", "key_env": "K"}}, "aliases": {"flash": {"endpoint": "gw", "model": "m"}}}
        mc.Client(mc.load(cfg), transport=T(), environ={"K": "sk-test"}, tracer=None).chat("flash", [], station="decide", texts=[{"name": "t"}])
        self.assertNotIn(b"station", sent[0])
        self.assertNotIn(b"texts", sent[0])


class Span(unittest.TestCase):
    def test_a_stations_model_call_span_carries_its_name_and_only_then(self):
        seen = []

        class Tracer:
            def start_as_current_span(self, name, attributes=None):
                import contextlib
                seen.append(dict(attributes))
                return contextlib.nullcontext(mock.Mock())

        class T:
            def __call__(self, url, data, headers, timeout):
                return 200, {}, json.dumps({"choices": [{"message": {"content": "{}"}}], "model": "m", "usage": {}})
        cfg = {"endpoints": {"gw": {"base_url": "http://gw/v1", "key_env": "K"}}, "aliases": {"flash": {"endpoint": "gw", "model": "m"}}}
        c = mc.Client(mc.load(cfg), transport=T(), environ={"K": "sk-test"}, tracer=Tracer())
        c.chat("flash", [], station="decide")
        c.chat("flash", [])
        self.assertEqual(seen[0]["flotilla.station"], "decide")
        self.assertNotIn("flotilla.station", seen[1])


class WorkerProcess(unittest.TestCase):
    """A real process started the way a worker starts a routine's command: FLOTILLA_STATIONS and the code root in its environment."""

    SCRIPT = ("import sys; sys.path.insert(0, %r); import stations\n"
              "st = stations.load()\n"
              "slots = {} if len(sys.argv) > 1 and sys.argv[1] == 'bad' else {'case': 'c-9'}\n"
              "print(st.render('decide', 'task', slots, fleet='triage').text, end='')\n" % HERE)

    def run_it(self, w, *args):
        env = dict(os.environ, FLOTILLA_CODE_ROOT=w.root, **w.env)
        return subprocess.run([sys.executable, "-c", self.SCRIPT, *args], env=env, capture_output=True, text=True, timeout=60)

    def test_a_run_renders_its_text_and_a_slot_error_ends_it_before_it_does_anything_else(self):
        w = World(self)
        p = self.run_it(w)
        self.assertEqual((p.returncode, p.stdout), (0, "Decide c-9 now.\n"), p.stderr)
        p = self.run_it(w, "bad")
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("StationsError", p.stderr)
        self.assertEqual(p.stdout, "")


if __name__ == "__main__":
    unittest.main()
