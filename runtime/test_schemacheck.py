"""python3 runtime/test_schemacheck.py: the stdlib schema checker, and runtime/input.schema.json against its golden inputs.

The golden files in runtime/testdata (input-*.json valid, input-invalid.json each with the error it must give) are read by this
test and by the Go test of internal/input, so the schema, the checker and flotillad's envelope agree.
"""
import glob
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import schemacheck as sc  # noqa: E402

SCHEMA = os.path.join(HERE, "input.schema.json")
DATA = os.path.join(HERE, "testdata")


def load(path):
    with open(path) as f:
        return json.load(f)


class Checker(unittest.TestCase):
    def v(self, value, schema):
        return sc.validate(value, schema)

    def test_types(self):
        for name, good, bad in [("string", "a", 1), ("integer", 3, 3.5), ("number", 3.5, "3"), ("boolean", True, 1),
                                ("object", {}, []), ("array", [], {}), ("null", None, 0)]:
            self.assertEqual(self.v(good, {"type": name}), [], name)
            self.assertTrue(self.v(bad, {"type": name}), name)

    def test_a_bool_is_not_a_number(self):
        self.assertTrue(self.v(True, {"type": "integer"}))
        self.assertTrue(self.v(False, {"type": "number"}))
        self.assertEqual(self.v(1.0, {"type": "number"}), [])

    def test_a_list_of_types(self):
        self.assertEqual(self.v(None, {"type": ["string", "null"]}), [])
        self.assertTrue(self.v(1, {"type": ["string", "null"]}))

    def test_const_and_enum_tell_true_from_one(self):
        self.assertEqual(self.v(1, {"const": 1}), [])
        self.assertTrue(self.v(True, {"const": 1}))
        self.assertTrue(self.v(1, {"const": True}))
        self.assertEqual(self.v("a", {"enum": ["a", "b"]}), [])
        self.assertTrue(self.v("c", {"enum": ["a", "b"]}))
        self.assertTrue(self.v(True, {"enum": [1]}))

    def test_strings(self):
        s = {"type": "string", "minLength": 2, "maxLength": 3, "pattern": "^a"}
        self.assertEqual(self.v("ab", s), [])
        self.assertTrue(self.v("a", s))
        self.assertTrue(self.v("abcd", s))
        self.assertTrue(self.v("ba", s))
        self.assertEqual(self.v("xxa", {"pattern": "a"}), [], "a pattern is searched, not matched whole")

    def test_numbers(self):
        s = {"type": "integer", "minimum": 1, "maximum": 3}
        self.assertEqual(self.v(2, s), [])
        self.assertTrue(self.v(0, s))
        self.assertTrue(self.v(4, s))

    def test_arrays(self):
        s = {"type": "array", "minItems": 1, "maxItems": 2, "items": {"type": "integer"}}
        self.assertEqual(self.v([1], s), [])
        self.assertTrue(self.v([], s))
        self.assertTrue(self.v([1, 2, 3], s))
        errs = self.v([1, "x"], s)
        self.assertEqual(errs, ["$[1]: want integer, got str"])

    def test_objects(self):
        s = {"type": "object", "required": ["a"], "additionalProperties": False, "properties": {"a": {"type": "integer"}}}
        self.assertEqual(self.v({"a": 1}, s), [])
        self.assertEqual(self.v({}, s), ["$: a is required"])
        self.assertEqual(self.v({"a": 1, "b": 2}, s), ["$: b is not allowed"])
        self.assertEqual(self.v({"a": "x"}, s), ["$.a: want integer, got str"])

    def test_additional_properties_as_a_schema(self):
        s = {"type": "object", "additionalProperties": {"type": "string"}}
        self.assertEqual(self.v({"a": "x", "b": "y"}, s), [])
        self.assertEqual(self.v({"a": 1}, s), ["$.a: want string, got int"])

    def test_a_path_names_where(self):
        s = {"type": "object", "properties": {"a": {"type": "array", "items": {"type": "object", "properties": {"b": {"type": "integer"}}}}}}
        self.assertEqual(self.v({"a": [{"b": 1}, {"b": "x"}]}, s), ["$.a[1].b: want integer, got str"])

    def test_boolean_schemas(self):
        self.assertEqual(self.v(1, True), [])
        self.assertTrue(self.v(1, False))
        self.assertEqual(self.v({"a": 1}, {"type": "object", "properties": {"a": True}}), [])

    def test_annotations_are_ignored(self):
        s = {"$schema": "x", "$id": "y", "title": "t", "description": "d", "$comment": "c", "examples": [1], "default": 2, "type": "integer"}
        self.assertEqual(self.v(1, s), [])

    def test_a_keyword_outside_the_subset_is_an_error_never_silently_skipped(self):
        for kw in ("$ref", "oneOf", "anyOf", "allOf", "not", "if", "format", "patternProperties", "uniqueItems", "multipleOf",
                   "exclusiveMinimum", "dependentRequired", "contains", "prefixItems"):
            with self.subTest(kw), self.assertRaises(sc.SchemaError):
                sc.validate(1, {kw: 1})
        with self.assertRaises(sc.SchemaError):
            sc.validate({}, {"type": "object", "properties": {"a": {"oneOf": []}}})
        with self.assertRaises(sc.SchemaError):
            sc.validate([], {"type": "array", "items": {"format": "date-time"}})

    def test_a_bad_schema_is_an_error(self):
        for bad in ({"type": "map"}, {"pattern": "("}, {"required": "a"}, {"enum": 1}, "string", {"additionalProperties": {"type": "blob"}}):
            with self.subTest(str(bad)), self.assertRaises(sc.SchemaError):
                sc.validate(1, bad)


class InputSchema(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.schema = load(SCHEMA)

    def test_the_schema_is_within_the_checkers_subset(self):
        sc.check_schema(self.schema)
        self.assertEqual(self.schema["properties"]["schema_version"], {"const": 1})

    def test_every_golden_input_validates(self):
        files = sorted(glob.glob(os.path.join(DATA, "input-*.json")))
        names = [os.path.basename(f) for f in files if os.path.basename(f) != "input-invalid.json"]
        self.assertGreaterEqual(len(names), 7, names)
        for f in files:
            if os.path.basename(f) == "input-invalid.json":
                continue
            with self.subTest(os.path.basename(f)):
                self.assertEqual(sc.validate(load(f), self.schema), [])

    def test_a_dataset_built_input_needs_only_its_identity(self):
        d = load(os.path.join(DATA, "input-dataset.json"))
        self.assertEqual(sorted(d), ["case", "input_key", "kind", "routine", "schema_version"])
        for k in ("ledger", "live_case_keys", "state_snapshot", "commit", "version", "started_at"):
            self.assertNotIn(k, d)
        self.assertEqual(sc.validate(d, self.schema), [])

    def test_every_invalid_input_gives_its_error(self):
        cases = load(os.path.join(DATA, "input-invalid.json"))
        self.assertGreaterEqual(len(cases), 30)
        for c in cases:
            with self.subTest(c["name"]):
                errs = sc.validate(c["doc"], self.schema)
                self.assertTrue(errs, "it validated")
                self.assertTrue(any(c["error"] in e for e in errs), f"want {c['error']!r} in {errs}")

    def test_the_names_of_the_invalid_cases_are_unique(self):
        names = [c["name"] for c in load(os.path.join(DATA, "input-invalid.json"))]
        self.assertEqual(len(names), len(set(names)))

    def test_the_cli_says_pass_and_fail(self):
        import io
        import contextlib
        buf = io.StringIO()
        good = os.path.join(DATA, "input-tick.json")
        with contextlib.redirect_stdout(buf):
            rc = sc.main(["x", SCHEMA, good])
        self.assertEqual((rc, buf.getvalue().strip()), (0, "PASS " + good))
        buf = io.StringIO()
        bad = os.path.join(DATA, "input-invalid.json")   # an array, not an object
        with contextlib.redirect_stdout(buf):
            rc = sc.main(["x", SCHEMA, good, bad])
        self.assertEqual(rc, 1)
        self.assertIn("FAIL " + bad, buf.getvalue())
        self.assertIn("want object", buf.getvalue())
        self.assertEqual(sc.main(["x", SCHEMA]), 2)


if __name__ == "__main__":
    unittest.main(verbosity=1)
