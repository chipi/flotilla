"""bugfix/declared_prompts.py: the prompt files a bugfix routine's spec declares (ADR-0018).

The bugfix worker image has no runtime/, so this carries the checks of runtime/prompts.py that a run needs
(bugfix/test_declared_prompts.py keeps the two in agreement, as callrec_pi's scrubber is kept): the spec's
`prompts:` reaches the run as FLOTILLA_PROMPTS, {"<role>": {"path", "sha256"}}, and a run refuses a file that
does not hash to what flotillad was loaded with. pi takes its rules as a path (--append-system-prompt), so
resolve() returns the verified file's path.
"""
import hashlib
import json
import os
import re

MAX_BYTES = 64 * 1024
PATH_RE = re.compile(r"^[A-Za-z0-9._/-]{1,200}$")


class PromptError(Exception):
    pass


def resolve(role, environ, code_root, fleet):
    """The absolute path of the declared prompt `role`, verified, or None when the spec declares none."""
    raw = environ.get("FLOTILLA_PROMPTS")
    if not raw:
        return None
    entry = (json.loads(raw) or {}).get(role)
    if not isinstance(entry, dict) or not entry.get("path") or not entry.get("sha256"):
        return None
    rel, expect = entry["path"], entry["sha256"]
    parts = rel.split("/")
    if not (PATH_RE.match(rel) and rel.endswith((".md", ".txt")) and not rel.startswith("/")
            and all(p not in ("", ".", "..") for p in parts)):
        raise PromptError(f"prompt {role}: {rel!r} is not a .md or .txt path inside the fleet's folder")
    cur = os.path.join(code_root, "fleets", fleet)
    for part in [""] + parts:
        cur = os.path.join(cur, part) if part else cur
        if os.path.islink(cur):
            raise PromptError(f"prompt {role}: {rel} has a symlink; refused")
    if not os.path.isfile(cur):
        raise PromptError(f"prompt {role}: {rel} is not a file in fleet {fleet}")
    with open(cur, "rb") as f:
        data = f.read(MAX_BYTES + 1)
    if len(data) > MAX_BYTES:
        raise PromptError(f"prompt {role}: {rel} is over {MAX_BYTES} bytes (a prompt is never trimmed)")
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        raise PromptError(f"prompt {role}: {rel} is not UTF-8") from None
    if not text.strip():
        raise PromptError(f"prompt {role}: {rel} is empty")
    sha = hashlib.sha256(data).hexdigest()
    if sha != expect:
        raise PromptError(f"the {role} prompt differs from the one flotillad checked: {rel} expected {expect} got {sha}")
    return cur
