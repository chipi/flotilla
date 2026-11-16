"""bugfix/stepone.py: RFC-0013 §1, step one: before any model, check the bug can happen on the base at all.

Deterministic, on the worktree the fix already has, at the base commit. A wrong close loses a real bug, so every
rule only closes on strong evidence and anything else is "not checked" (the chain goes on to the verdict):

- names: only those the error says are missing (`x.name is not a function`, `name is not a function`,
  `name is not defined`, Python's `name 'n' is not defined`), and only names that look like the project's own: at
  least 6 characters, with an underscore or an inner capital, not a keyword, a global or a common built-in method
  (`undefined`, `map`, `get`, `forEach`...). An attribute error names an attribute of a wrong object, never searched.
- files: a source file the issue names (`src/...`, a `.svelte`, `.ts`, `.js` or `.py` path, or a bare
  `Component.svelte`), never a bundled path or a library's class. A close needs one that exists at the base.

Two closes, each `not_reproducible` with its evidence:
1. the failing code is not on main: no name is in the named files, nor anywhere in the tree
   (#659: StarPanel.svelte is on main, panel_tab_sky nowhere);
2. what the error says is missing is on main: every name is called in a named file and defined in another file
   (#651: SurfaceScene.svelte calls m.surface_true_scale_short, messages/*.json defines it).
A git command that fails or times out is "not checked", never a close.

Homelab's review of 444a153 (2026-11-16) found the first version closing real bugs: an AttributeError on `get`
(present in any repo), "undefined is not a function" (a keyword taken as a name), "items.map is not a function".
"""
import re
import subprocess

MISSING = [
    re.compile(r"(?:[\w$]+\.)*([A-Za-z_$][\w$]{2,})\s+is not (?:a function|defined)"),
    re.compile(r"name '([A-Za-z_]\w{2,})' is not defined"),
]
STOP = set("""
undefined null true false this self super window document globalThis console process module exports require
function object string number boolean symbol bigint arguments prototype constructor async await yield return
map filter reduce forEach find findIndex some every includes indexOf join split slice splice concat sort reverse
push pop shift unshift keys values entries get set has add delete clear then catch finally call apply bind
toString valueOf toFixed toLowerCase toUpperCase trim replace replaceAll match matchAll test exec charAt
addEventListener removeEventListener querySelector querySelectorAll getElementById appendChild createElement
getAttribute setAttribute getContext fetch json text parse stringify log error warn info debug assign freeze
from isArray length name items data value result response request callback handler listener options props
None True False print open len range list dict tuple str int float bool type object append extend update
""".split())
PATH = re.compile(r"(?:^|[\s`'\"(/])((?:src|lib|app|tests?)/[\w./-]+\.(?:svelte|tsx?|jsx?|mjs|py))\b")
COMPONENT = re.compile(r"(?<![\w/.-])([A-Z][\w-]*\.svelte)\b")
BUNDLED = re.compile(r"_app/immutable|/chunks/|node_modules|\.[A-Za-z0-9_-]{6,}\.m?js$")
TEST = re.compile(r"\.(test|spec)\.[cm]?[tj]sx?$|(^|/)tests?/|(^|/)test_[^/]*\.py$")


class GitError(Exception):
    pass


def git(wt, *args, ok=(0,)):
    try:
        p = subprocess.run(["git", "-C", wt, *args], capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as e:
        raise GitError(f"git {args[0]}: {type(e).__name__}") from e
    if p.returncode not in ok:
        raise GitError(f"git {args[0]}: exit {p.returncode}: {p.stderr.strip()[:120]}")
    return p.stdout


def searchable(name):
    """A name that looks like the project's own: not a keyword, global or built-in; long and specific enough."""
    return (name not in STOP and len(name) >= 6
            and ("_" in name.strip("_$") or re.search(r"[a-z][A-Z]", name) is not None))


def names_of(text):
    out = []
    for rx in MISSING:
        for n in rx.findall(text or ""):
            if searchable(n) and n not in out:
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
    tracked = git(wt, "ls-tree", "-r", "--name-only", base).splitlines()
    for p in named:
        hits = [t for t in tracked if t == p or (("/" not in p) and t.endswith("/" + p))]
        for h in hits or [p]:
            bucket = found if hits else absent
            if h not in bucket:
                bucket.append(h)
    return found, absent


def where(wt, base, name):
    """Non-test, non-Markdown files at the base that contain `name` as a word: [(path, line)]."""
    out = git(wt, "grep", "-n", "-w", "-F", "-e", name, base, "--", ".", ":!*.md", ok=(0, 1))   # 1: no match
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
        return "go", "step one: not checked (no project-specific name the error calls missing)"
    try:
        files, absent = files_of(issue_text, wt, base)
        if not files:
            return "go", f"step one: not checked (no named source file at the base; names: {', '.join(names)})"
        seen = {n: where(wt, base, n) for n in names}
    except GitError as e:
        return "go", f"step one: not checked ({e})"
    at = base[:12]
    searched = f"searched {', '.join(files)}" + (f" ({', '.join(absent)} not at the base)" if absent else "") + " and the tree"
    in_named = {n: [h for h in seen[n] if h[0] in files] for n in names}
    elsewhere = {n: [h for h in seen[n] if h[0] not in files] for n in names}
    if all(not seen[n] for n in names):
        verb = "appears" if len(names) == 1 else "appear"
        return "close", (f"step one: the failing code is not on main: {', '.join(names)} {verb} nowhere at {at} "
                         f"({searched}); the error came from code main does not have")
    if all(in_named[n] and elsewhere[n] for n in names):
        found = "; ".join(f"{n} called at {in_named[n][0][0]}:{in_named[n][0][1]}, defined at "
                          f"{elsewhere[n][0][0]}:{elsewhere[n][0][1]}" for n in names)
        return "close", (f"step one: what the error says is missing is on main at {at}: {found}; "
                         "the bug cannot happen there")
    return "go", f"step one: checked, no close ({searched}): " + ", ".join(
        f"{n} {'in the named file' if in_named[n] else 'not in the named file'}, "
        f"{'elsewhere too' if elsewhere[n] else 'nowhere else'}" for n in names)
