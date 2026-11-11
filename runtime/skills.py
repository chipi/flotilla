"""Skills (ADR-0012, RFC-0005 v1 part 1): load a skill's instructions by name, and match a routine's rules.

A skill is a folder with a SKILL.md: frontmatter (`name` equal to the folder name, `description` of 1 to
1024 characters; every other field is ignored and grants nothing), then the instructions. Skills come
from two layers inside the image, each pinned by the image tag:

  flotilla   <code root>/skills/<name>/SKILL.md           flotilla's default skills
  fleet      <code root>/fleets/<fleet>/skills/<name>/     the fleet's own skills

A name exists in exactly one layer; no layer shadows another. Only SKILL.md is read, never a symlink,
never references/ or scripts/ (skills never grant anything: RFC-0005 §5). A skill is never trimmed: one
over MAX_BYTES is refused. flotillad's -check applies the same rules before a deploy (internal/skills).

A routine's rules (its spec's `skills.rule`, handed to the run as JSON in FLOTILLA_SKILLS) are matched in
order against the structured fields the routine's code put in its case; every rule that matches loads,
each skill once. Each load is recorded (name, layer, version, sha256) and is a `skill.load` span when the
OpenTelemetry SDK is present. Stdlib otherwise.
"""
import hashlib
import json
import os
import re

NAME_RE = re.compile(r"^[a-z][a-z0-9-]{0,62}$")
MAX_BYTES = 16384
MAX_DESCRIPTION = 1024


class SkillError(Exception):
    """A skill that is missing, ambiguous or malformed: the installation's mistake, never skipped."""


def code_root():
    """FLOTILLA_CODE_ROOT (the worker image sets /opt/flotilla), else this file's repo."""
    return os.environ.get("FLOTILLA_CODE_ROOT") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def layers(fleet, root=None):
    root = root or code_root()
    return [("flotilla", os.path.join(root, "skills")), ("fleet", os.path.join(root, "fleets", fleet, "skills"))]


def parse(text, name):
    """(description, body) of a SKILL.md, or SkillError."""
    if not text.startswith("---\n"):
        raise SkillError(f"skill {name}: SKILL.md must start with a --- frontmatter line")
    end = text.find("\n---\n", 3)
    if end < 0:
        raise SkillError(f"skill {name}: frontmatter is not closed by a --- line")
    fields = {}
    for line in text[4:end].splitlines():
        k, sep, v = line.partition(":")
        if sep and k.strip() in ("name", "description"):
            fields[k.strip()] = v.strip().strip('"').strip("'")
    if fields.get("name") != name:
        raise SkillError(f"skill {name}: frontmatter name is {fields.get('name')!r}, not the folder's name")
    desc = fields.get("description", "")
    if not 1 <= len(desc) <= MAX_DESCRIPTION:
        raise SkillError(f"skill {name}: description must be 1 to {MAX_DESCRIPTION} characters")
    body = text[end + 5:].strip()
    if not body:
        raise SkillError(f"skill {name}: no instructions after the frontmatter")
    return desc, body


def find(name, fleet, root=None):
    """(layer, path) of the skill's SKILL.md, or SkillError."""
    if not NAME_RE.match(name or ""):
        raise SkillError(f"skill name {name!r} does not match {NAME_RE.pattern}")
    found = []
    for layer, base in layers(fleet, root):
        d = os.path.join(base, name)
        p = os.path.join(d, "SKILL.md")
        if os.path.islink(d) or os.path.islink(p):
            raise SkillError(f"skill {name}: {layer} layer has a symlink; refused")
        if os.path.isfile(p):
            found.append((layer, p))
    if not found:
        raise SkillError(f"skill {name}: in no layer (fleet {fleet})")
    if len(found) > 1:
        raise SkillError(f"skill {name}: in more than one layer ({', '.join(l for l, _ in found)})")
    return found[0]


def load(name, fleet, root=None, version=None):
    """{name, layer, version, sha256, text}: the instructions as a load inserts them."""
    layer, path = find(name, fleet, root)
    with open(path, "rb") as f:
        raw = f.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise SkillError(f"skill {name}: SKILL.md is over {MAX_BYTES} bytes (a skill is never trimmed)")
    _, body = parse(raw.decode("utf-8"), name)
    rec = {"name": name, "layer": layer, "version": version or os.environ.get("FLOTILLA_VERSION") or "dev",
           "sha256": hashlib.sha256(raw).hexdigest()}
    _span(rec)
    return dict(rec, text=body)


def rules_from_env(default_path=None, environ=None):
    """The routine's rules: FLOTILLA_SKILLS (flotillad, from the spec) or, without it, the default file."""
    environ = os.environ if environ is None else environ
    raw = environ.get("FLOTILLA_SKILLS")
    if raw:
        rules = (json.loads(raw) or {}).get("rule") or []
    elif default_path:
        with open(default_path) as f:
            rules = json.load(f)
    else:
        rules = []
    for r in rules:
        if not isinstance(r.get("when"), dict) or not r["when"] or not isinstance(r.get("load"), list):
            raise SkillError(f"skills rule {r!r}: needs a non-empty when and a load list")
    return rules


def match(rules, fields):
    """The skills to load for these case fields: rules in order, equality on every `when` field, each once."""
    out = []
    for r in rules:
        if all(fields.get(k) == v for k, v in r["when"].items()):
            for name in r["load"]:
                if name not in out:
                    out.append(name)
    return out


def _span(rec):
    try:
        from opentelemetry import trace
    except ImportError:
        return
    with trace.get_tracer("flotilla.skills").start_as_current_span("skill.load", attributes={
            "flotilla.skill.name": rec["name"], "flotilla.skill.layer": rec["layer"],
            "flotilla.skill.version": rec["version"], "flotilla.skill.sha256": rec["sha256"]}):
        pass
