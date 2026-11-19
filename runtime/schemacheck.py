"""A small JSON Schema checker for flotilla's own schemas (runtime/input.schema.json), standard library only.

A real validator (the jsonschema package) is a dependency this repository does not take, so this checks the subset the schemas use
and refuses the rest: a schema that uses a keyword the checker does not implement is an error when it is loaded, never a constraint
that is silently not checked.

Supported keywords: type (a name or a list of names), const, enum, required, properties, additionalProperties (false, true or a
schema), items (one schema), minItems, maxItems, minLength, maxLength, pattern (searched, as JSON Schema does), minimum, maximum.
Annotations ($schema, $id, title, description, $comment, examples, default) are read and ignored. No $ref, no combinators.

    python3 runtime/schemacheck.py runtime/input.schema.json export/input.json ...   # PASS or FAIL per file; exit 1 on a FAIL
    errors = schemacheck.validate(doc, schema)                                       # [] when it validates
"""
import json
import re
import sys

ANNOTATIONS = {"$schema", "$id", "title", "description", "$comment", "examples", "default"}
KEYWORDS = ANNOTATIONS | {"type", "const", "enum", "required", "properties", "additionalProperties", "items", "minItems", "maxItems",
                          "minLength", "maxLength", "pattern", "minimum", "maximum"}
TYPES = {"object", "array", "string", "integer", "number", "boolean", "null"}


class SchemaError(ValueError):
    """The schema itself is not one this checker can honour."""


def check_schema(schema, path="$"):
    """Refuse a schema that uses anything outside the supported subset (so nothing is silently unchecked)."""
    if schema is True or schema is False:
        return
    if not isinstance(schema, dict):
        raise SchemaError(f"{path}: a schema is an object or a boolean")
    for k in schema:
        if k not in KEYWORDS:
            raise SchemaError(f"{path}: the keyword {k!r} is not supported by this checker")
    t = schema.get("type")
    for name in ([t] if isinstance(t, str) else t or []):
        if name not in TYPES:
            raise SchemaError(f"{path}: unknown type {name!r}")
    for k, v in schema.get("properties", {}).items():
        check_schema(v, f"{path}.properties.{k}")
    if "additionalProperties" in schema:
        check_schema(schema["additionalProperties"], f"{path}.additionalProperties")
    if "items" in schema:
        check_schema(schema["items"], f"{path}.items")
    if "pattern" in schema:
        try:
            re.compile(schema["pattern"])
        except re.error as e:
            raise SchemaError(f"{path}: pattern {schema['pattern']!r}: {e}") from e
    if "required" in schema and not (isinstance(schema["required"], list) and all(isinstance(r, str) for r in schema["required"])):
        raise SchemaError(f"{path}: required is a list of names")
    if "enum" in schema and not isinstance(schema["enum"], list):
        raise SchemaError(f"{path}: enum is a list")


def _is(value, name):
    if name == "object":
        return isinstance(value, dict)
    if name == "array":
        return isinstance(value, list)
    if name == "string":
        return isinstance(value, str)
    if name == "boolean":
        return isinstance(value, bool)
    if name == "null":
        return value is None
    if name == "integer":
        return isinstance(value, int) and not isinstance(value, bool)
    if name == "number":
        return isinstance(value, (int, float)) and not isinstance(value, bool)
    return False


def _same(a, b):
    """JSON equality: 1 is not true, and 1 equals 1.0."""
    if isinstance(a, bool) or isinstance(b, bool):
        return isinstance(a, bool) and isinstance(b, bool) and a == b
    return a == b


def _walk(value, schema, path, errors):
    if schema is True:
        return
    if schema is False:
        errors.append(f"{path}: nothing is allowed here")
        return
    if "type" in schema:
        names = [schema["type"]] if isinstance(schema["type"], str) else schema["type"]
        if not any(_is(value, n) for n in names):
            errors.append(f"{path}: want {' or '.join(names)}, got {type(value).__name__}")
            return
    if "const" in schema and not _same(value, schema["const"]):
        errors.append(f"{path}: must be {json.dumps(schema['const'])}")
    if "enum" in schema and not any(_same(value, e) for e in schema["enum"]):
        errors.append(f"{path}: must be one of {json.dumps(schema['enum'])}")
    if isinstance(value, str):
        if "minLength" in schema and len(value) < schema["minLength"]:
            errors.append(f"{path}: shorter than {schema['minLength']}")
        if "maxLength" in schema and len(value) > schema["maxLength"]:
            errors.append(f"{path}: longer than {schema['maxLength']}")
        if "pattern" in schema and not re.search(schema["pattern"], value):
            errors.append(f"{path}: does not match {schema['pattern']}")
    if _is(value, "number"):
        if "minimum" in schema and value < schema["minimum"]:
            errors.append(f"{path}: below {schema['minimum']}")
        if "maximum" in schema and value > schema["maximum"]:
            errors.append(f"{path}: above {schema['maximum']}")
    if isinstance(value, list):
        if "minItems" in schema and len(value) < schema["minItems"]:
            errors.append(f"{path}: fewer than {schema['minItems']} items")
        if "maxItems" in schema and len(value) > schema["maxItems"]:
            errors.append(f"{path}: more than {schema['maxItems']} items")
        if "items" in schema:
            for i, item in enumerate(value):
                _walk(item, schema["items"], f"{path}[{i}]", errors)
    if isinstance(value, dict):
        for name in schema.get("required", []):
            if name not in value:
                errors.append(f"{path}: {name} is required")
        props = schema.get("properties", {})
        for k, v in value.items():
            if k in props:
                _walk(v, props[k], f"{path}.{k}", errors)
            elif "additionalProperties" in schema:
                extra = schema["additionalProperties"]
                if extra is False:
                    errors.append(f"{path}: {k} is not allowed")
                else:
                    _walk(v, extra, f"{path}.{k}", errors)


def validate(value, schema):
    """The errors, as 'path: what is wrong', of value against schema; [] when it validates. SchemaError for a schema outside the
    supported subset."""
    check_schema(schema)
    errors = []
    _walk(value, schema, "$", errors)
    return errors


def main(argv):
    if len(argv) < 3:
        print("usage: schemacheck.py SCHEMA FILE...", file=sys.stderr)
        return 2
    with open(argv[1]) as f:
        schema = json.load(f)
    bad = 0
    for path in argv[2:]:
        try:
            with open(path) as f:
                doc = json.load(f)
            errors = validate(doc, schema)
        except (OSError, ValueError) as e:
            errors = [str(e)]
        if errors:
            bad += 1
            print(f"FAIL {path}")
            for e in errors:
                print(f"  {e}")
        else:
            print(f"PASS {path}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
