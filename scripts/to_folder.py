#!/usr/bin/env python3
"""One-off mover: a flat routine spec (fleets/<fleet>/<routine>.yaml) becomes a routine folder
(ADR-0020 section 1):

    <routine>/routine.yaml       identity: every top-level key a version does not own
    <routine>/v1/spec.yaml       the keys a version owns (run, explain, steps, skills, harness, prompts, models)
    <routine>/v1/<file>          a prompt file the spec names, copied from the fleet's folder (the path stays
                                 inside v1/; the source file stays where it is, other routines may read it)

The text is split by top-level key and written as it was: comments, folded blocks and key order stay. The
only edit is the deprecated level word: `autonomy: shadow` is written `record` (a routine.yaml never writes
the old word). Not shipped in any image; run it from the repo root:

    python3 scripts/to_folder.py fleets/jobs/backup-monitor.yaml [...]    # git rm of the flat file is left to you
"""
import os
import re
import sys

OWNED = {"run", "explain", "steps", "skills", "harness", "prompts", "models"}
KEY = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*):")


def blocks(text):
    """(key, lines) per top-level key; the comment and blank lines before a key go with it."""
    out, pend, cur = [], [], None
    for line in text.splitlines(keepends=True):
        m = KEY.match(line)
        if m:
            cur = (m.group(1), pend + [line])
            pend = []
            out.append(cur)
        elif cur is not None and line[:1] in (" ", "-", "\t"):
            cur[1].extend(pend + [line])
            pend = []
        else:
            pend.append(line)
    if pend:
        if cur is None:
            raise SystemExit("no keys")
        cur[1].extend(pend)
    return out


def move(path):
    fleet_dir = os.path.dirname(path)
    name = os.path.basename(path)[: -len(".yaml")]
    dest = os.path.join(fleet_dir, name)
    if os.path.exists(dest):
        raise SystemExit(f"{dest} exists")
    routine, version, copies = [], [], []
    for key, lines in blocks(open(path).read()):
        if key in OWNED:
            if key == "prompts":
                fixed = []
                for ln in lines:
                    m = re.match(r"^(\s+[a-z_]+:\s*)(\S+)\s*$", ln)
                    if m and "/" in m.group(2):
                        copies.append(m.group(2))
                        ln = m.group(1) + os.path.basename(m.group(2)) + "\n"
                    fixed.append(ln)
                lines = fixed
            version.extend(lines)
        else:
            if key in ("autonomy", "max_autonomy"):
                lines = [re.sub(r"^(\w+:\s*)shadow(\s*)$", r"\1record\2", ln) for ln in lines]
            routine.extend(lines)
    os.makedirs(os.path.join(dest, "v1"))
    open(os.path.join(dest, "routine.yaml"), "w").write("".join(routine))
    open(os.path.join(dest, "v1", "spec.yaml"), "w").write("".join(version))
    for rel in copies:
        with open(os.path.join(fleet_dir, rel), "rb") as src, open(os.path.join(dest, "v1", os.path.basename(rel)), "wb") as dst:
            dst.write(src.read())
    return dest


if __name__ == "__main__":
    for p in sys.argv[1:]:
        print(move(p))
