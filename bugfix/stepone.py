"""bugfix/stepone.py: RFC-0013 §1, step one, advisory: what the base has of what the error says is missing.

Before any model, on the worktree the fix already has, at the base commit, it records facts on the chain; it never
closes a case (operator, 2026-11-17). Two rounds of close rules closed real bugs (homelab's reviews of 444a153 and
0731108: an AttributeError on `get`, `undefined is not a function`, `items.map`, a NameError whose name is defined in
another file, i.e. a missing import, a computed name, a name only in a test, a doc or a generated file), while the cases
it was built for, dev-branch errors such as #651 and #659, are already left out by intake (`is_dev`) and by triage.

What it records:
- the names the error says are missing (`x.name is not a function`, `name is not a function`, `name is not defined`,
  Python's `name 'n' is not defined`) that look like the project's own (6+ characters, an underscore or an inner
  capital, not a keyword, global or built-in method);
- for each: whether a source file the issue names has it, and where else at the base it appears (code, tests and docs
  alike), or that it appears nowhere at the base;
- the named files the base does not have, and a sign of computed names (`obj[name](`, a string that starts the name);
- "not checked" when there is nothing to search or a git command fails.
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
COMPUTED = re.compile(r"\[\s*[A-Za-z_$][\w$]*\s*\]\s*\(")
NOTE_MAX = 1200   # the plan body keeps its tail (BODY_MAX): a long note must not push the chain's head out


class GitError(Exception):
    pass


def git(wt, *args, ok=(0,)):
    try:
        p = subprocess.run(["git", "-C", wt, *args], capture_output=True, text=True, errors="replace", timeout=60)
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
    """Every tracked file at the base that contains `name` as a word, tests and docs included: [(path, line)]."""
    out = git(wt, "grep", "-n", "-w", "-F", "-e", name, base, ok=(0, 1))   # 1: no match
    hits = []
    for ln in out.splitlines():
        parts = ln.split(":", 3)   # base:path:line:text
        if len(parts) >= 3:
            hits.append((parts[1], parts[2]))
    return hits


def computed(wt, base, files, names):
    """Whether a named file calls through a computed name or holds a string that starts one of the names."""
    for f in files:
        text = git(wt, "show", f"{base}:{f}")
        if COMPUTED.search(text):
            return True
        for n in names:
            stem = n[: n.rfind("_") + 1] if "_" in n.strip("_") else ""
            if len(stem) >= 4 and re.search(r"['\"`]" + re.escape(stem), text):
                return True
    return False


def check(issue_text, wt, base):
    """('note', facts) for the chain's log. Advisory: it never decides anything, and nothing it meets ends the case."""
    try:
        note = _check(issue_text, wt, base)
    except Exception as e:
        note = f"step one (advisory): not checked ({e if isinstance(e, GitError) else type(e).__name__})"
    return "note", note if len(note) <= NOTE_MAX else note[:NOTE_MAX - 12] + " [cut short]"


def _check(issue_text, wt, base):
    names = names_of(issue_text)
    if not names:
        return "step one (advisory): not checked (no project-specific name the error calls missing)"
    try:
        files, absent = files_of(issue_text, wt, base)
        seen = {n: where(wt, base, n) for n in names}
        dynamic = computed(wt, base, files, names) if files else False
    except GitError as e:
        return f"step one (advisory): not checked ({e})"
    facts = []
    for n in names:
        in_named = [h for h in seen[n] if h[0] in files]
        others = [h for h in seen[n] if h[0] not in files]
        if not seen[n]:
            facts.append(f"{n} appears nowhere at {base[:12]}")
        else:
            where_ = ", ".join(f"{p}:{line}" for p, line in (in_named + others)[:3])
            facts.append(f"{n} {'in a named file' if in_named else 'not in a named file'}; at {where_}")
    extra = []
    if files:
        extra.append(f"named files on main: {', '.join(files)}")
    if absent:
        extra.append(f"named files not on main: {', '.join(absent)}")
    if dynamic:
        extra.append("a named file computes names (obj[name]( or a string that starts a name)")
    return "step one (advisory): " + "; ".join(facts + extra)
