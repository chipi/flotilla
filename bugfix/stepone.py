"""bugfix/stepone.py: RFC-0013 §1, step one: before any model, check the bug can happen on the base at all.

Deterministic, on the worktree the fix already has, at the base commit. It searches only names it can search there:
- a name the error says is missing: `x.name is not a function`, `name is not defined`, Python's `name 'n' is not
  defined` and `has no attribute 'n'`;
- a source file the issue names (`src/...`, a `.svelte`, `.ts`, `.js` or `.py` path, or a bare `Component.svelte`),
  never a bundled path (`_app/immutable/...`, a hashed `33.FFOEfYOl.js`, `node_modules`), never a library's class.

Two closes, each as `not_reproducible` with its evidence:
1. the failing code is not on main: no searched name appears in the named files nor anywhere in the tree
   (#659: StarPanel.svelte exists at the base, its call to panel_tab_sky does not);
2. what the error says is missing is there: every name the error calls missing is present at the base
   (#651: surface_true_scale_short is a key of messages/*.json).
With nothing searchable it records "not checked" and the chain goes on (#648, #656: a library class and bundle paths).
"""
import re
import subprocess

MISSING = [
    re.compile(r"(?:[\w$]+\.)*([A-Za-z_$][\w$]{2,})\s+is not (?:a function|defined)"),
    re.compile(r"name '([A-Za-z_]\w{2,})' is not defined"),
    re.compile(r"has no attribute '([A-Za-z_]\w{2,})'"),
]
PATH = re.compile(r"(?:^|[\s`'\"(/])((?:src|lib|app|tests?)/[\w./-]+\.(?:svelte|tsx?|jsx?|mjs|py))\b")
COMPONENT = re.compile(r"(?<![\w/.-])([A-Z][\w-]*\.svelte)\b")
BUNDLED = re.compile(r"_app/immutable|/chunks/|node_modules|\.[A-Za-z0-9_-]{6,}\.m?js$")
TEST = re.compile(r"\.(test|spec)\.[cm]?[tj]sx?$|(^|/)tests?/|(^|/)test_[^/]*\.py$")


def git(wt, *args):
    p = subprocess.run(["git", "-C", wt, *args], capture_output=True, text=True, timeout=60)
    return p.returncode, p.stdout


def names_of(text):
    out = []
    for rx in MISSING:
        for n in rx.findall(text or ""):
            if n not in out:
                out.append(n)
    return out


def files_of(text, wt, base):
    """(paths at the base the issue names, paths it names that the base does not have)."""
    named, found, absent = [], [], []
    for p in PATH.findall(text or "") + COMPONENT.findall(text or ""):
        p = p.lstrip("/")
        if BUNDLED.search(p) or p in named:
            continue
        named.append(p)
    _, tracked = git(wt, "ls-tree", "-r", "--name-only", base)
    tracked = tracked.splitlines()
    for p in named:
        hits = [t for t in tracked if t == p or (("/" not in p) and t.endswith("/" + p))]
        for h in hits or [p]:
            bucket = found if hits else absent
            if h not in bucket:
                bucket.append(h)
    return found, absent


def where(wt, base, name):
    """Non-test, non-Markdown files at the base that contain `name` as a word: [(path, line)]."""
    _, out = git(wt, "grep", "-n", "-w", "-F", "-e", name, base, "--", ".", ":!*.md")
    hits = []
    for ln in out.splitlines():
        parts = ln.split(":", 3)   # base:path:line:text
        if len(parts) >= 3 and not TEST.search(parts[1]):
            hits.append((parts[1], parts[2]))
    return hits


def check(issue_text, wt, base):
    """('close', evidence) for a not_reproducible close, else ('go', what was checked)."""
    names = names_of(issue_text)
    if not names:
        return "go", "step one: not checked (no name the error calls missing)"
    files, absent = files_of(issue_text, wt, base)
    at = base[:12]
    seen = {n: where(wt, base, n) for n in names}
    searched = f"searched {', '.join(files) if files else 'no named file'}" + (
        f" ({', '.join(absent)} not at the base)" if absent else "") + " and the tree"
    if all(not seen[n] for n in names):
        verb = "appears" if len(names) == 1 else "appear"
        return "close", (f"step one: the failing code is not on main: {', '.join(names)} {verb} nowhere at {at} "
                         f"({searched}); the error came from code main does not have")
    if all(seen[n] for n in names):
        found = "; ".join(f"{n} at {seen[n][0][0]}:{seen[n][0][1]}" for n in names)
        return "close", (f"step one: what the error says is missing is on main at {at}: {found} ({searched}); "
                         "the bug cannot happen there")
    return "go", f"step one: checked, the code is on main ({searched}): " + ", ".join(
        f"{n} {'found' if seen[n] else 'absent'}" for n in names)
