"""python3 runtime/test_sops.py: a fleet's SOPs (runtime/sops.py): parse refusals, the loader's hash and cap, matching by specificity (a tie
matches none), the registry gate on `fix`, and the draft's text parsing back to itself."""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import skills  # noqa: E402
import sops  # noqa: E402

GOOD = """---
name: {n}
description: Use when a backup's age passes its limit on a host with a backup job.
applies_to: {applies}
diagnosis: [{{"read":"metric","name":"backup_age"}}]
fix: [{{"action":"backup.rerun"}}]
verify: {{"check":"backup_newer_than_execution","within":"60m"}}
rollback: []
rung: 2
evidence: ["case-1"]
---
The human steps, and why.
"""
ANY = '{"kinds":["failed_backup"],"hosts":["*"],"alerts":[]}'
REGISTRY = {"backup.rerun": {"name": "backup.rerun", "for_kinds": ["failed_backup"], "check": {"verify": "backup_newer_than_execution", "within": "60m"}},
            "other.fix": {"name": "other.fix", "for_kinds": ["failed_backup"], "check": {"verify": "other_check", "within": "5m"}}}


def text(n="s", applies=ANY, **swap):
    t = GOOD.format(n=n, applies=applies)
    for old, new in swap.items():
        assert old in t, old
        t = t.replace(old, new)
    return t


def sop(n="s", applies=ANY):
    return sops.parse(text(n, applies), n)


class Parse(unittest.TestCase):
    def test_a_good_sop(self):
        s = sop("backup-stale")
        self.assertEqual((s.name, s.rung, s.fix, s.verify["within"], s.evidence), ("backup-stale", 2, [{"action": "backup.rerun"}], "60m", ["case-1"]))
        self.assertEqual(s.applies_to, {"kinds": ["failed_backup"], "hosts": ["*"], "alerts": []})
        self.assertEqual(s.body, "The human steps, and why.")

    def test_defaults_for_the_optional_keys(self):
        t = "\n".join(ln for ln in text().splitlines() if not ln.startswith(("diagnosis", "rollback", "evidence")))
        s = sops.parse(t, "s")
        self.assertEqual((s.diagnosis, s.rollback, s.evidence), ([], [], []))

    def test_refusals(self):
        def drop(key):
            return "\n".join(ln for ln in text().splitlines() if not ln.startswith(key + ":"))
        bad = {
            "bad json": (text(**{"rung: 2": "rung: [2"}), "rung is not JSON"),
            "unknown key": (text(**{"rung: 2": "rung: 2\nowner: me"}), "not `key: value`"),
            "key twice": (text(**{"rung: 2": "rung: 2\nrung: 1"}), "rung is given twice"),
            "a line that is no key": (text(**{"rung: 2": "rung: 2\njust text"}), "not `key: value`"),
            "name is not the folder": (text("other"), "not the folder's name"),
            "no name": (drop("name"), "missing name"),
            "no fix": (drop("fix"), "missing fix"),
            "no verify": (drop("verify"), "missing verify"),
            "no rung": (drop("rung"), "missing rung"),
            "no applies_to": (drop("applies_to"), "missing applies_to"),
            "rung above 3": (text(**{"rung: 2": "rung: 4"}), "rung is a whole number"),
            "rung a bool": (text(**{"rung: 2": "rung: true"}), "rung is a whole number"),
            "rung a text": (text(**{"rung: 2": 'rung: "2"'}), "rung is a whole number"),
            "no kinds": (text(applies='{"hosts":["*"]}'), "kinds at least"),
            "empty kinds": (text(applies='{"kinds":[]}'), "one or more"),
            "an applies key it does not know": (text(applies='{"kinds":["k"],"teams":["a"]}'), "applies_to is"),
            "a host that is not text": (text(applies='{"kinds":["k"],"hosts":[1]}'), "applies_to.hosts"),
            "a fix step with params": (text(**{'[{"action":"backup.rerun"}]': '[{"action":"backup.rerun","params":{"host":"x"}}]'}), "fix step"),
            "an empty fix": (text(**{'fix: [{"action":"backup.rerun"}]': "fix: []"}), "one or more actions"),
            "a fix that is not an action name": (text(**{'"backup.rerun"': '"rm -rf /"'}), "fix step"),
            "a host read": (text(**{'"read":"metric"': '"read":"host"'}), "no host read exists"),
            "verify without within": (text(**{',"within":"60m"': ""}), "verify is"),
            "verify within in words": (text(**{'"60m"': '"an hour"'}), "verify is"),
            "no instructions": (text().split("\n---\n")[0] + "\n---\n \n", "no instructions"),
            "no frontmatter": ("x\n", "frontmatter"),
        }
        for name, (t, want) in bad.items():
            with self.subTest(name), self.assertRaisesRegex(sops.SopError, want):
                sops.parse(t, "s")

    def test_a_trailing_newline_is_not_part_of_a_name_a_duration_or_an_action(self):
        for swap, want in (({'"60m"': '"60m\\n"'}, "verify is"), ({'"backup.rerun"': '"backup.rerun\\n"'}, "fix step"),
                           ({'"name":"backup_age"': '"name":"backup_age\\n"'}, "diagnosis is")):
            with self.subTest(swap), self.assertRaisesRegex(sops.SopError, want):
                sops.parse(text(**swap), "s")

    def test_both_worker_images_carry_what_the_sop_routines_import(self):
        root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        def copies(image):
            with open(os.path.join(root, "deploy", "docker", image)) as f:
                return {ln.split()[2] if ln.startswith("COPY") else "" for ln in f}
        ops, triage = copies("worker-ops.Dockerfile"), copies("worker-triage.Dockerfile")
        self.assertIn("/opt/flotilla/runtime", ops, "sop_act and sop_draft import runtime/sops.py")
        self.assertIn("/opt/flotilla/ops", triage, "sop_investigate imports ops/sop_act.py")
        self.assertIn("/opt/flotilla/host/registry.json", triage, "load_registry's default path must exist where investigate runs")
        self.assertIn("/opt/flotilla/host/registry.json", ops)

    def test_a_sop_error_is_a_skill_error(self):
        self.assertTrue(issubclass(sops.SopError, skills.SkillError))

    def test_render_parses_back_to_itself(self):
        s = sop("backup-stale")
        again = sops.parse(sops.render(s), "backup-stale")
        for k in ("name", "description", "applies_to", "diagnosis", "fix", "verify", "rollback", "rung", "evidence", "body"):
            self.assertEqual(getattr(again, k), getattr(s, k), k)


class Loader(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()

    def put(self, name, body=None, folder=None):
        p = os.path.join(self.d, folder or name)
        os.makedirs(p, exist_ok=True)
        with open(os.path.join(p, "SKILL.md"), "w") as f:
            f.write(body if body is not None else text(name))

    def test_load_all_has_each_sops_hash_and_is_sorted(self):
        self.put("b-two")
        self.put("a-one")
        got = sops.load_all(self.d, version="sha-test")
        self.assertEqual([s.name for s in got], ["a-one", "b-two"])
        import hashlib
        with open(os.path.join(self.d, "a-one", "SKILL.md"), "rb") as f:
            self.assertEqual(got[0].sha256, hashlib.sha256(f.read()).hexdigest())
        self.assertTrue(got[0].path.endswith("a-one/SKILL.md"))
        self.assertIn("sha256:" + got[0].sha256, got[0].cite())

    def test_no_folder_is_no_sops(self):
        self.assertEqual(sops.load_all(os.path.join(self.d, "nothing")), [])

    def test_a_loaded_sop_is_recorded_as_a_load(self):
        self.put("a-one")
        lf = os.path.join(self.d, "loaded.jsonl")
        old = os.environ.get("FLOTILLA_LOADED_FILE")
        os.environ["FLOTILLA_LOADED_FILE"] = lf
        try:
            sops.load_all(self.d)
        finally:
            os.environ.pop("FLOTILLA_LOADED_FILE") if old is None else os.environ.__setitem__("FLOTILLA_LOADED_FILE", old)
        with open(lf) as f:
            line = json.loads(f.readline())
        self.assertEqual((line["kind"], line["trust"], line["title"]), ("skill", "source", "a-one"))
        self.assertIn("fleet sop", line["source"])

    def test_the_skills_layers_refusals_apply(self):
        self.put("too-big", text("too-big") + "x" * skills.MAX_BYTES)
        with self.assertRaisesRegex(skills.SkillError, "never trimmed"):
            sops.load_all(self.d)

    def test_a_broken_sop_refuses_the_lot(self):
        self.put("fine")
        self.put("broken", "x\n")
        with self.assertRaisesRegex(sops.SopError, "frontmatter"):
            sops.load_all(self.d)

    def test_a_folder_with_no_skill_md_and_a_bad_folder_name(self):
        os.makedirs(os.path.join(self.d, "empty"))
        with self.assertRaisesRegex(sops.SopError, "no SKILL.md"):
            sops.load_all(self.d)
        os.rmdir(os.path.join(self.d, "empty"))
        self.put("x", folder="Bad_Name")
        with self.assertRaisesRegex(sops.SopError, "does not match"):
            sops.load_all(self.d)

    def test_a_symlinked_sop_is_refused(self):
        self.put("real")
        elsewhere = tempfile.mkdtemp()
        with open(os.path.join(elsewhere, "SKILL.md"), "w") as f:
            f.write(text("linked"))
        os.symlink(elsewhere, os.path.join(self.d, "linked"))
        with self.assertRaisesRegex(skills.SkillError, "symlink"):
            sops.load_all(self.d)

    def test_a_stray_file_is_not_an_sop(self):
        self.put("fine")
        with open(os.path.join(self.d, "README.md"), "w") as f:
            f.write("notes")
        self.assertEqual([s.name for s in sops.load_all(self.d)], ["fine"])


def case(kind="failed_backup", target="host:backup-01", **data):
    return {"id": "case-1", "kind": kind, "target": target, "data": data}


class Match(unittest.TestCase):
    def test_the_kind_must_be_listed(self):
        s = sop()
        self.assertEqual(sops.match([s], case())[0], s)
        got, why = sops.match([s], case(kind="other"))
        self.assertEqual((got, why), (None, "no SOP applies"))

    def test_the_host_is_a_glob_on_the_target(self):
        a, b = sop("a-glob", '{"kinds":["failed_backup"],"hosts":["backup-*"]}'), sop("b-other", '{"kinds":["failed_backup"],"hosts":["web-*"]}')
        self.assertEqual(sops.match([a, b], case())[0], a)
        self.assertEqual(sops.match([a, b], case(target="host:web-02"))[0], b)
        self.assertEqual(sops.match([a, b], case(target="host:db-1"))[0], None)

    def test_a_case_with_no_host_matches_only_a_sop_for_any_host(self):
        anyhost, pinned = sop("any-host"), sop("pinned", '{"kinds":["failed_backup"],"hosts":["backup-01"]}')
        self.assertEqual(sops.match([anyhost, pinned], case(target="repo:o/r"))[0], anyhost)
        self.assertEqual(sops.match([pinned], case(target="repo:o/r"))[0], None)

    def test_an_alert_must_be_the_cases_alert(self):
        s = sop("alerted", '{"kinds":["failed_backup"],"hosts":["*"],"alerts":["backup-stale"]}')
        self.assertEqual(sops.match([s], case(alert="backup-stale"))[0], s)
        self.assertEqual(sops.match([s], case(alert="disk-full"))[0], None)
        self.assertEqual(sops.match([s], case())[0], None, "a SOP that names an alert needs the case to have one")

    def test_the_most_specific_wins(self):
        generic = sop("generic")
        glob = sop("globbed", '{"kinds":["failed_backup"],"hosts":["backup-*"]}')
        exact = sop("exact", '{"kinds":["failed_backup"],"hosts":["backup-01"]}')
        alerted = sop("alerted", '{"kinds":["failed_backup"],"hosts":["*"],"alerts":["stale"]}')
        self.assertEqual(sops.match([generic, glob, exact], case())[0], exact)
        self.assertEqual(sops.match([generic, glob], case())[0], glob)
        self.assertEqual(sops.match([generic, exact, alerted], case(alert="stale"))[0], alerted, "an alert outranks a host")
        self.assertEqual(sops.match([generic, exact, alerted], case(alert="other"))[0], exact)

    def test_an_exact_tie_matches_none_and_says_so(self):
        a, b = sop("a-one"), sop("b-two")
        got, why = sops.match([a, b], case())
        self.assertIsNone(got)
        self.assertIn("ambiguous", why)
        self.assertIn("a-one, b-two", why)
        self.assertEqual(sops.match([b, a], case())[1], why, "the order SOPs are given in does not change the answer")

    def test_a_tie_below_a_more_specific_sop_is_no_tie(self):
        a, b, exact = sop("a-one"), sop("b-two"), sop("exact", '{"kinds":["failed_backup"],"hosts":["backup-01"]}')
        self.assertEqual(sops.match([a, b, exact], case())[0], exact)

    def test_the_cases_data_may_be_json_text(self):
        s = sop("alerted", '{"kinds":["failed_backup"],"hosts":["*"],"alerts":["stale"]}')
        c = {"kind": "failed_backup", "target": "host:h", "data": json.dumps({"alert": "stale"})}
        self.assertEqual(sops.match([s], c)[0], s)


class Registry(unittest.TestCase):
    def test_fix_actions_returns_the_registrys_declarations(self):
        acts = sops.fix_actions(sop(), REGISTRY, kind="failed_backup")
        self.assertEqual([a["name"] for a in acts], ["backup.rerun"])

    def test_an_action_the_registry_lacks_is_refused(self):
        s = sops.parse(text(**{'"backup.rerun"': '"backup.wipe"'}), "s")
        with self.assertRaisesRegex(sops.SopError, "backup.wipe is not in the registry.*never adds a command"):
            sops.fix_actions(s, REGISTRY)

    def test_every_fix_step_is_checked_not_only_the_first(self):
        s = sops.parse(text(**{'[{"action":"backup.rerun"}]': '[{"action":"backup.rerun"},{"action":"made.up"}]'}), "s")
        with self.assertRaisesRegex(sops.SopError, "made.up"):
            sops.fix_actions(s, REGISTRY)

    def test_a_rollback_the_registry_lacks_is_refused(self):
        s = sops.parse(text(**{"rollback: []": 'rollback: [{"action":"made.up"}]'}), "s")
        with self.assertRaisesRegex(sops.SopError, "made.up"):
            sops.fix_actions(s, REGISTRY)

    def test_an_action_for_another_kind_is_refused(self):
        with self.assertRaisesRegex(sops.SopError, "not an action for process_change"):
            sops.fix_actions(sop(), REGISTRY, kind="process_change")

    def test_the_registrys_check_decides_what_verifies(self):
        s = sops.parse(text(**{"backup_newer_than_execution": "something_else"}), "s")
        with self.assertRaisesRegex(sops.SopError, "registry's check"):
            sops.fix_actions(s, REGISTRY)

    def test_load_registry_joins_files_and_refuses_a_duplicate(self):
        d = tempfile.mkdtemp()
        paths = []
        for i, names in enumerate((["a.one"], ["b.two"], ["a.one"])):
            p = os.path.join(d, f"r{i}.json")
            with open(p, "w") as f:
                json.dump({"actions": [{"name": n} for n in names]}, f)
            paths.append(p)
        self.assertEqual(sorted(sops.load_registry(paths[:2])), ["a.one", "b.two"])
        with self.assertRaisesRegex(sops.SopError, "declared twice"):
            sops.load_registry(paths)
        self.assertEqual(sops.registry_paths({"FLOTILLA_REGISTRY": os.pathsep.join(paths[:2])}), paths[:2])

    def test_the_shipped_example_sop_parses_and_fits_the_shipped_registry(self):
        root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        loaded = sops.load_all(os.path.join(root, "fleets", "operator", "sops"))
        self.assertTrue(loaded, "fleets/operator/sops holds the example SOP")
        reg = sops.load_registry([os.path.join(root, "host", "registry.json")])
        for s in loaded:
            sops.fix_actions(s, reg, kind=s.applies_to["kinds"][0])


if __name__ == "__main__":
    unittest.main()
