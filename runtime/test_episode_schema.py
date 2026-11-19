"""python3 runtime/test_episode_schema.py: runtime/episode.schema.json, the episode record every adapter's Episode serializes to
(adapter.episode_record), and what keeps a record valid, masked and bounded.

The golden files in runtime/testdata: episode-*.json are valid records (episode-pi-1.0.0.json is what the real pi 1.0.0 stream
beside it parses to, regenerated with UPDATE_GOLDEN=1); episode-invalid.json is a list of {name, doc, error}, each a document the
schema must refuse with that error.

eval-harness's generic episode schema is pending (v2-design.md section 6). The test helper validates against a LIST of schemas
(adapter.SCHEMAS, today only flotilla's); when their file exists, add it to that list and a copy of it under testdata/, and
every golden and every adapter's episode is validated against it too. No placeholder file stands in for it.
"""
import glob
import json
import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import adapter  # noqa: E402
import fakeh  # noqa: E402
import harness_pi  # noqa: E402
import schemacheck  # noqa: E402
from fakeh import check_contract  # noqa: E402

DATA = os.path.join(HERE, "testdata")
PI_SAMPLES = os.path.join(HERE, "..", "bugfix", "testdata", "pi")
TOKEN = "abcdefgh" + "12345678" + "ijkl"   # built here: a literal in a header line is what the secrets scan looks for


def load(path):
    with open(path) as f:
        return json.load(f)


def pi_stream(name):
    with open(os.path.join(DATA if name.startswith("pi-1.0.0") else PI_SAMPLES, name)) as f:
        return f.read()


def minimal(**kw):
    return dict({"schema_version": 1, "harness": "pi", "station": "decide", "exit_reason": "completed",
                 "cost": {"usd": None, "source": "unknown"}, "turns": []}, **kw)


class Goldens(unittest.TestCase):
    def test_every_valid_golden_validates(self):
        files = sorted(set(glob.glob(os.path.join(DATA, "episode-*.json"))) - {os.path.join(DATA, "episode-invalid.json")})
        self.assertGreaterEqual(len(files), 4)
        for p in files:
            with self.subTest(os.path.basename(p)):
                self.assertEqual(adapter.validate_episode(load(p)), [])

    def test_every_invalid_golden_is_refused_with_its_error(self):
        cases = load(os.path.join(DATA, "episode-invalid.json"))
        self.assertGreaterEqual(len(cases), 10)
        for c in cases:
            with self.subTest(c["name"]):
                errors = adapter.validate_episode(c["doc"])
                self.assertTrue(errors, "refused")
                self.assertTrue(any(c["error"] in e for e in errors), (c["error"], errors))

    def test_the_real_pi_stream_parses_to_its_golden_record(self):
        ep = harness_pi.PI.parse(pi_stream("pi-1.0.0-stream.jsonl"), {})
        ep.station = "decide"
        got = adapter.episode_record(ep, seq=1, version=2, spec_hash="0123456789ab")
        golden = os.path.join(DATA, "episode-pi-1.0.0.json")
        if os.environ.get("UPDATE_GOLDEN"):
            with open(golden, "w") as f:
                json.dump(got, f, indent=2, sort_keys=True)
                f.write("\n")
        self.assertEqual(got, load(golden))
        self.assertEqual(adapter.validate_episode(got), [])
        a = got["turns"][0]["actions"][0]
        self.assertEqual((a["name"], a["ok"], a["result"], "duration_s" in a), ("write", True, "Successfully wrote to out.txt", False),
                         "pi's stream has no duration for a tool call: none is made up")
        self.assertTrue(got["turns"][0]["time"].endswith("Z"))


class Adapters(unittest.TestCase):
    """Every adapter's parsed sample is a valid record (check_contract does it), whichever streams it is given."""

    def test_every_pi_sample_is_a_valid_record(self):
        names = [os.path.basename(p) for p in sorted(glob.glob(os.path.join(PI_SAMPLES, "stream_*.jsonl")))] + ["pi-1.0.0-stream.jsonl"]
        self.assertEqual(len(names), 5)
        for name in names:
            with self.subTest(name):
                check_contract(self, harness_pi.PI.parse(pi_stream(name), {}), harness_pi.PI)

    def test_garbage_and_an_empty_stream_are_valid_records_too(self):
        for s in ("", None, "not json\n[1,2]\n"):
            check_contract(self, harness_pi.PI.parse(s, {}), harness_pi.PI)

    def test_the_fake_harness_is_a_valid_record(self):
        h, _ = fakeh.install_fake(self)
        stream = "\n".join(json.dumps(e) for e in (
            {"event": "turn", "stop": "tool_calls", "model": "m", "text": "a", "usage": {"input": 3, "output": 2}, "cost": 0.5,
             "tools": [{"name": "read", "arg": "x", "ok": True}]},
            {"event": "final", "session": "s", "answer": "done", "error": ""}))
        ep = h.parse(stream, {})
        check_contract(self, ep, h)
        self.assertEqual(adapter.episode_record(ep)["cost"], {"usd": 0.5, "source": "harness-estimate"})

    def test_a_cost_nobody_reported_is_the_explicit_unknown(self):
        ep = adapter.Episode(harness="x", station="s")
        self.assertEqual(adapter.episode_record(ep)["cost"], {"usd": None, "source": "unknown"})
        self.assertEqual(adapter.episode_record(ep, cost={"usd": 0.02, "source": "gateway"})["cost"], {"usd": 0.02, "source": "gateway"})

    def test_what_an_adapter_cannot_supply_is_left_out_not_made_up(self):
        rec = adapter.episode_record(adapter.Episode(harness="x"))
        self.assertEqual(sorted(rec), ["cost", "exit_reason", "harness", "schema_version", "station", "turns"])
        t = adapter.episode_record(adapter.Episode(harness="x", turns=[adapter.Turn(n=1)]))["turns"][0]
        self.assertEqual(t, {"n": 1})


class RecordsAreMaskedAndBounded(unittest.TestCase):
    def test_a_credential_in_a_result_an_argument_the_answer_or_a_turn_is_masked(self):
        auth = f"Authorization: Bearer {TOKEN}"
        ep = adapter.Episode(harness="x", station="s", answer=f"used {auth}", turns=[adapter.Turn(
            n=1, text=f"saw {auth}", tools=[adapter.action("bash", {"command": f"curl -H '{auth}' http://x"}, f"out {auth}", ok=True)])])
        blob = json.dumps(adapter.episode_record(ep))
        self.assertNotIn(TOKEN, blob)
        self.assertIn("<redacted>", blob)

    def test_an_address_and_an_e_mail_in_a_result_are_masked_too(self):
        a = adapter.action("bash", {"command": "x"}, "from ann@example.com at 10.1.2.3")
        self.assertEqual(a["result"], "from <email> at <ip>")

    def test_a_result_over_the_cap_is_a_hash_and_a_length_never_a_cut_text(self):
        for n in (adapter.RESULT_CAP, adapter.RESULT_CAP + 1):
            a = adapter.action("read", {"path": "p"}, "x" * n)
            if n == adapter.RESULT_CAP:
                self.assertEqual((len(a["result"]), "result_sha256" in a), (n, False), "at the cap it is kept whole")
            else:
                self.assertEqual((a.get("result"), a["result_bytes"], len(a["result_sha256"])), (None, n, 64), "over it, only the hash and the length")

    def test_the_hash_is_of_the_masked_text_so_a_secret_does_not_leave_through_it(self):
        import hashlib
        a = adapter.action("read", {"path": "p"}, f"Authorization: Bearer {TOKEN} " + "y" * adapter.RESULT_CAP)
        masked = adapter.bounded("result", f"Authorization: Bearer {TOKEN} " + "y" * adapter.RESULT_CAP)
        self.assertEqual(a["result_sha256"], masked["result_sha256"])
        self.assertNotEqual(a["result_sha256"], hashlib.sha256((f"Authorization: Bearer {TOKEN} " + "y" * adapter.RESULT_CAP).encode()).hexdigest())

    def test_long_arguments_are_bounded_the_same_way(self):
        a = adapter.action("write", {"path": "p", "content": "z" * (adapter.RESULT_CAP + 5)})
        self.assertNotIn("args", a)
        self.assertEqual(a["args_bytes"] > adapter.RESULT_CAP, True)
        self.assertEqual(a["arg"], "p", "the one-line argument is still there")

    def test_an_ok_and_a_duration_the_stream_does_not_have_are_not_invented(self):
        self.assertEqual(adapter.action("ls", {"path": "."}), {"name": "ls", "arg": ".", "args": {"path": "."}})
        self.assertEqual(adapter.action("ls", None, "r", 1.5, True), {"name": "ls", "arg": "", "result": "r", "duration_s": 1.5, "ok": True})

    def test_a_time_is_rfc_3339_or_nothing(self):
        self.assertEqual(adapter.iso_utc(1795103782756), "2026-11-19T15:56:22.756Z")
        for bad in (None, 0, -1, "x", True):
            self.assertIsNone(adapter.iso_utc(bad))


class SeveralSchemas(unittest.TestCase):
    """The validation helper takes a list: adding eval-harness's schema later is one entry."""

    def test_a_second_schema_is_checked_too_and_named_in_the_error(self):
        other = os.path.join(DATA, "two-schemas-fixture.schema.json")
        self.assertTrue(os.path.exists(other))
        doc = load(os.path.join(DATA, "episode-minimal.json"))
        self.assertEqual(adapter.validate_episode(doc), [])
        with mock.patch.object(adapter, "SCHEMAS", adapter.SCHEMAS + [("generic", other)]):
            errors = adapter.validate_episode(doc)
        self.assertTrue(errors and all(e.startswith("generic: ") for e in errors), errors)

    def test_the_schema_is_in_the_subset_the_checker_honours(self):
        for _, path in adapter.SCHEMAS:
            schemacheck.check_schema(load(path))   # raises on any keyword the checker does not implement

    def test_the_episode_schema_stays_open_so_a_generic_field_is_not_refused(self):
        doc = load(os.path.join(DATA, "episode-minimal.json"))
        doc["extra"] = {"a": 1}
        doc["turns"] = [{"n": 1, "future_field": True, "actions": [{"name": "x", "other": 1}]}]
        self.assertEqual(adapter.validate_episode(doc), [])


if __name__ == "__main__":
    unittest.main()
