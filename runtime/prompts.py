"""Prompts (ADR-0018): a routine's system prompt is a file, declared in its spec and hashed by flotillad.

The spec's `prompts:` maps a role (system, rules) to a path under the fleet's folder. flotillad hands the run
FLOTILLA_PROMPTS, {"<role>": {"path": ..., "sha256": ...}}, the hash the file had when the spec loaded, and a
run REFUSES a prompt whose file hashes to anything else: a prompt that is not the one the spec was reviewed
with never reaches the model. Without the variable (an evaluation run, a checkout) the caller's default path
is loaded, unverified.

Files are read from <code root>/fleets/<fleet>/<path>, the same place a fleet's skills are, under the same
rules as flotillad's -check (internal/prompts): a regular file, no symlink on the way, inside the fleet's
folder, UTF-8, not empty, at most MAX_BYTES. Each load is recorded (role, path, sha256). Stdlib only.
"""
import hashlib
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import loaded  # noqa: E402
from skills import code_root  # noqa: E402

MAX_BYTES = 64 * 1024
ROLE_RE = re.compile(r"^[a-z][a-z0-9_]{0,39}$")
PATH_RE = re.compile(r"^[A-Za-z0-9._/-]{1,200}$")


class PromptError(Exception):
    """A prompt that is missing, changed or malformed: refused, never replaced by a default."""


def declared(environ=None):
    """FLOTILLA_PROMPTS as a dict, or None when the run was not started by flotillad."""
    environ = os.environ if environ is None else environ
    raw = environ.get("FLOTILLA_PROMPTS")
    return json.loads(raw) if raw else None


def _check_path(role, rel):
    parts = rel.split("/") if isinstance(rel, str) else []
    if not (PATH_RE.match(rel or "") and rel.endswith((".md", ".txt")) and not rel.startswith("/")
            and all(p not in ("", ".", "..") for p in parts)):
        raise PromptError(f"prompt {role}: {rel!r} is not a .md or .txt path inside the fleet's folder")


def load(role, fleet, root=None, environ=None, default=None):
    """{role, path, sha256, text}: the file's text as it is, untrimmed."""
    if not ROLE_RE.match(role or ""):
        raise PromptError(f"prompt role {role!r} does not match {ROLE_RE.pattern}")
    spec = declared(environ)
    expect = None
    if spec is not None:
        entry = spec.get(role)
        if isinstance(entry, dict) and entry.get("path") and entry.get("sha256"):
            rel, expect = entry["path"], entry["sha256"]
        elif default:
            rel = default   # a routine whose spec declares no prompt for this role keeps its default
        else:
            raise PromptError(f"prompt {role}: the spec declares none (FLOTILLA_PROMPTS has {sorted(spec)})")
    elif default:
        rel = default
    else:
        raise PromptError(f"prompt {role}: no FLOTILLA_PROMPTS and no default path")
    _check_path(role, rel)
    base = os.path.join(root or code_root(), "fleets", fleet)
    cur = base
    for part in [""] + rel.split("/"):
        cur = os.path.join(cur, part) if part else cur
        if os.path.islink(cur):
            raise PromptError(f"prompt {role}: {rel} has a symlink; refused")
    if not os.path.isfile(cur):
        raise PromptError(f"prompt {role}: {rel} is not a file in fleet {fleet}")
    with open(cur, "rb") as f:
        raw = f.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise PromptError(f"prompt {role}: {rel} is over {MAX_BYTES} bytes (a prompt is never trimmed)")
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        raise PromptError(f"prompt {role}: {rel} is not UTF-8") from None
    if not text.strip():
        raise PromptError(f"prompt {role}: {rel} is empty")
    sha = hashlib.sha256(raw).hexdigest()
    text = text.replace("\r\n", "\n").replace("\r", "\n")   # the old read was text mode: the hash is of the bytes
    if expect is not None and sha != expect:
        raise PromptError(f"the {role} prompt differs from the one flotillad checked: {rel} expected {expect} got {sha}")
    loaded.record("playbook", "source", f"prompt {role}", f"{rel} (fleet {fleet})", sha, environ=environ)
    return {"role": role, "path": rel, "sha256": sha, "text": text}
