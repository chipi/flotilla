"""bugfix/fix: the acting routine for code_bug cases (RFC-0010 §2), fleetd's chain.go on the platform.

Started by flotillad with the case (FLOTILLA_CASE, the issue in its data). It runs the chain's steps in
chain.go's order, calling the bake-off's leaves through LiteLLM, and decides:

  1. verdict (triage_run.sh): actionable, or it escalates (needs-info, rejected: with the reason)
  2. the base: the repo at main (or --base), installed
  3. up to 1 + kickback_max rounds of: a fix (harnesses/<--harness>.sh) from the base, then the gates,
     which run only what the fix touches (RFC-0010 §9: the PR's CI runs the whole suite)
       the scope: a test file in the diff; nothing in CI, hooks, the repo's config, manifest or lockfile
       repro: the fix's test files, on the base with the fix's source left out, load and fail; with the
         fix they pass (a test that cannot load on the base, because it imports what the fix adds, is
         noted, not counted as a failure)
       related: the tests related to the changed source files show no failure the base did not have
     a failing gate kicks it back: the advisor (advisor_run.sh) pins the owner once per round unless the
     fix was already at a pin (the acceptance transition), then the verdict is asked again with that
     evidence; needs-info or rejected escalates
  4. a review of the clean diff: request-changes sends it back once (--review-kickback-max), with the whole
     review and the previous diff, straight to the fix step (no new verdict: the review did not dispute that
     the bug is actionable); a review that did not run is said on the plan, never read as an approval; then:
       {"outcome": "plan", "action": "github.open_pr", "params": {repo, base, title, body, diff}}
     or {"outcome": "escalate", "detail": why}

Nothing leaves the worker: the plan goes to flotillad's policy (shadow records it; at propose a person
approves it and bugfix/open-pr opens the PR). The case's directory on the scratch volume (runs/<case id>/) is
deleted when the run ends with a plan or an escalation; a run cut short (killed, restarted, an error) leaves it,
with a checkpoint written at each boundary of the chain, for the case's next run to continue when flotillad
says so (FLOTILLA_RESUME, ADR-0019). The npm cache stays for the next run.

Whether the fixes are any good is phase B (RFC-0010 §8). This routine is the form: the same steps, gates
and limits as the chain the bake-off measured.

  python3 bugfix/fix.py --repo example/shop --worker-model bugfix-flash --advisor-model bugfix-reviewer \\
      --reviewer-model bugfix-reviewer
Environment: FLOTILLA_CASE, FLOTILLA_PLAN_FILE, FLOTILLA_MODELS (the worker's); BUGFIX_SCRATCH (the
scratch volume); GIT_REMOTE_BASE (default https://github.com, a test's local path).
"""
import argparse
import fcntl
import glob
import hashlib
import json
import os
import re
import shlex
import shutil
import signal
import subprocess
import sys
import threading
import time
import traceback

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import callrec_pi  # noqa: E402
import declared_prompts  # noqa: E402
import harness  # noqa: E402
import harness_pi  # noqa: E402
import pi_config  # noqa: E402
import spend  # noqa: E402
import stepone  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
BAKEOFF = os.environ.get("BUGFIX_BAKEOFF_DIR", os.path.join(os.path.dirname(HERE), "bugfix-fleet", "bakeoff"))
READ_ONLY_TOOLS = harness_pi.READ_ONLY_TOOLS   # SR-0001 H8: what the steps that only read get (harness_pi)
REVIEW_PROMPT_MAX = 100_000   # bytes; pi takes the prompt as one argument and Linux caps one at 128 KiB
REQUEST_CHANGES = re.compile(r"VERDICT:\s*\**\s*request[- ]changes", re.I)
VERDICT_LINE = re.compile(r"^[\s*_>#-]*VERDICT:", re.I)
VERDICT_LINE_REQUESTS = re.compile(r"^[\s*_>#-]*VERDICT:\s*\**\s*request[- ]changes", re.I)
# The objections a review can judge from what it is given (C4b, the advisor's review of 2026-11-16): only these send a
# fix back, since a kick-back costs a whole fix round (#335: $0.11 -> $0.31 under C4a for "uninvestigated, unverified,
# invented"); anything else the review raises goes on the plan for the operator.
KICK_CLASSES = ("hides-symptom", "over-broad", "test-proves-nothing", "breaks-behaviour", "wrong-place")
REVIEW_CLASS = re.compile(r"CLASS:\s*\**\s*([a-z-]+)", re.I)
TEST_FILE = re.compile(r"\.(test|spec)\.[cm]?[tj]sx?$|(^|/)tests?/|(^|/)test_[^/]*\.py$")
# CI and hooks, and what `vitest related` cannot see: the repo's config, manifest and lockfile (the
# advisor's review, 2026-11-15); a spec adds its repo's own with --out-of-scope
OUT_OF_SCOPE = re.compile(r"^(\.github/|\.husky/|\.gitlab-ci|\.circleci/)|(^|/)(package\.json|package-lock\.json|"
                          r"npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|tsconfig[^/]*\.json|"
                          r"(vitest|vite|svelte|playwright)\.config\.[cm]?[tj]s)$")
# What a pytest run executes or is configured by, wherever it sits in the tree: conftest.py and the
# sitecustomize/.pth hooks are imported at collection or at interpreter start, with the worker's
# environment; the config, manifest, lock and constraints files decide what is installed and how it runs.
# Under --test-format pytest the scope is an allowlist (pytest_scope); this blocklist only gives its
# error message the more specific reason for these.
PYTEST_OUT_OF_SCOPE = re.compile(r"(^|/)(conftest\.py|sitecustomize\.py|usercustomize\.py|[^/]*\.pth|\.?pytest\.(ini|toml)|tox\.ini|"
                                 r"\.coveragerc|setup\.(py|cfg)|pyproject\.toml|Makefile|\.pre-commit-config\.yaml|"
                                 r"requirements[^/]*\.txt|constraints[^/]*\.txt|[^/]*\.lock)$|^requirements/")
DIFF_MAX = 16000   # the registry's max for github.open_pr's diff
BODY_MAX = 8000
TITLE_MAX = 200


def review_requests_changes(review):
    """The review's decision is its LAST line that starts with `VERDICT:` (the prompt asks it to end with
    one); a review that quotes the instruction or names a rejected alternative mid-text is not a request.
    With no such line, the match anywhere is kept: a reviewer that buried its verdict in a sentence still
    asked for changes, and a spurious kick-back costs one round while a missed one ships the objection."""
    lines = [ln for ln in (review or "").splitlines() if VERDICT_LINE.match(ln)]
    if lines:
        return bool(VERDICT_LINE_REQUESTS.match(lines[-1]))
    return bool(REQUEST_CHANGES.search(_unquoted(review or "")))


ASSERTION = re.compile(r"\b(expect|assert)\w*\s*\(|\bassert\s|\.(not\.)?to[A-Z]\w*\(|\bself\.assert\w+\(|\bpytest\.raises\(")


def changed_assertions(wt, base, run_dir, env):
    """B11: [(test file, [assertion lines the fix removed or rewrote])] for test files that exist at the base. A fix
    that inverts or drops an assertion of a test on main can make its own gates pass while the bug stays (#553 on
    2026-11-16: `.not.toBeNull()` became `.toBeNull()` in a test whose comment said no filter ships without evidence)."""
    _, names = sh(["git", "-C", wt, "diff", "--cached", "--name-only", "--no-renames", "-z", "--diff-filter=M", base],
                  run_dir, env, 60)
    out = []
    for f in [n for n in names.split("\0") if n.strip() and TEST_FILE.search(n)]:
        _, d = sh(["git", "-C", wt, "diff", "--cached", "-U0", base, "--", f], run_dir, env, 60)
        removed = [ln[1:].strip() for ln in d.splitlines() if ln.startswith("-") and not ln.startswith("---")]
        added = {ln[1:].strip() for ln in d.splitlines() if ln.startswith("+") and not ln.startswith("+++")}
        gone = [ln for ln in removed if ASSERTION.search(ln) and ln not in added]
        if gone:
            out.append((f, gone))
    return out


READS_FILE = re.compile(r"readFileSync\(|readFile\(|fs\.read|\.read_text\(|\bopen\(")
SOURCE_NAMED = re.compile(r"""['"`][^'"`]*\.(?:svelte|[cm]?[tj]sx?|py)['"`]""")


def source_text_tests(wt, base, run_dir, env):
    """[(test file, line)]: lines a fix adds to a test file that read a source file's text (not a .json/.yaml fixture)
    instead of running its code. Such a test fails on the base and passes with the fix by construction (#553 on
    2026-11-17: a regex over AudioOverlay.svelte's text, the review approved it); of the 18 plans recorded until then,
    no other has one."""
    _, names = sh(["git", "-C", wt, "diff", "--cached", "--name-only", "--no-renames", "-z", "--diff-filter=AM", base],
                  run_dir, env, 60)
    out = []
    for f in [n for n in names.split("\0") if n.strip() and TEST_FILE.search(n)]:
        _, d = sh(["git", "-C", wt, "diff", "--cached", "-U0", base, "--", f], run_dir, env, 60)
        added = [ln[1:] for ln in d.splitlines() if ln.startswith("+") and not ln.startswith("+++")]
        hit = reads_source_text(added)
        if hit:
            out.append((f, hit))
    return out


IMPORT_LINE = re.compile(r"^\s*(?:import\b|export\b.*\bfrom\b|from\s+\S+\s+import\b)|\brequire\(|\bimport\(|\bvi\.mock\(")


def reads_source_text(added):
    """The first added test line that reads a file's text when the test names a source file outside an import (the
    path may sit on the line before, `const p = 'src/x.svelte'; readFileSync(p)`), else ""."""
    named = any(SOURCE_NAMED.search(ln) and not TEST_FILE.search(ln) and not IMPORT_LINE.search(ln) for ln in added)
    for ln in added:
        if READS_FILE.search(ln) and not IMPORT_LINE.search(ln) and (named or SOURCE_NAMED.search(ln)):
            return ln.strip()[:160]
    return ""


# a test file, or an assertion in code form (no space before its bracket): a criterion's prose may say "assert zero
# pageerrors" (#466) or "assert (a) the returned promise rejects" (#517, 2026-11-17) for a new test
TEST_NAMED = re.compile(r"[\w./-]+\.(?:test|spec)\.[cm]?[tj]sx?\b|\btest_\w+\.py\b"
                        r"|\.(?:not\.)?to[A-Z]\w*\(|\bexpect\(|\bassert\w*\(|\bself\.assert\w+\(")


def test_change_criteria(v):
    """The acceptance criteria of a re-verdict that name a test file or an assertion: a criterion the issue never states,
    written after a failed round. Over the recorded verdicts (2026-11-16/17) it names 5 of #553's 6 actionable kick-back
    verdicts ("the test ... is changed to assert toBeNull()") and none of #335's or #466's; the sixth is left to the
    assertion gate in attempt()."""
    acc = ((v.get("problem") or {}).get("acceptance") or []) if isinstance(v, dict) else []
    out = []
    for a in acc:
        text = a if isinstance(a, str) else str((a or {}).get("criterion") or "")
        if TEST_NAMED.search(text):
            out.append(text.strip()[:160])
    return out


def kick_classes(frozen_proof):
    """The review classes that send a fix back. With a frozen proof, wrong-place is a note: it is about code the diff
    does not show, and the fix already passes a proof it could not write (#335, 2026-11-18)."""
    return tuple(c for c in KICK_CLASSES if not (frozen_proof and c == "wrong-place"))


def review_class(review):
    """The class the review's last VERDICT line names (CLASS: x), "" without one."""
    lines = [ln for ln in (review or "").splitlines() if VERDICT_LINE.match(ln)]
    m = REVIEW_CLASS.search(lines[-1]) if lines else None
    return m.group(1).lower() if m else ""


def _unquoted(text):
    """The text without what only quotes the instruction: `>` lines, code fences, `inline code` and "double quotes"."""
    text = re.sub(r"```.*?```", " ", text, flags=re.S)
    text = "\n".join(ln for ln in text.splitlines() if not ln.lstrip().startswith(">"))
    text = re.sub(r"`[^`\n]*`", " ", text)
    return re.sub(r"[\"\u201c][^\"\u201d\n]*[\"\u201d]", " ", text)


class Stop(Exception):
    """Ends the chain with an outcome: escalate with a reason."""


class Terminated(BaseException):
    """The worker's SIGTERM at the routine's timeout (a BaseException: no step's error handling may catch it).
    The worker SIGKILLs 10 s later (worker/flotilla_worker.py KILL_GRACE_S), so main has that long to stop
    the step in flight and report the run's spend."""


DASHES = str.maketrans({c: "-" for c in "‐‑‒–—―−"} |
                       {c: "'" for c in "‘’‚‛"} | {c: '"' for c in "“”„‟"})


def norm_phrase(s):
    """A phrase as the reproducer's check compares it: lower case, runs of whitespace as one space, every dash or minus
    as "-" and curly quotes as straight ones (#335 on 2026-11-18: the issue's "~1–2", en dash, typed as "~1-2"), and
    markdown's bold and code marks dropped (an offered "Actual: ..." comes from the issue's "**Actual:** ..."), and its
    escapes too (#335's "\\`mission.flight.events\\`" was copied as "mission.flight.events", 2026-11-18)."""
    s = re.sub(r"\\([\\`*_{}\[\]()#+\-.!|~<>])", r"\1", str(s).translate(DASHES))
    return " ".join(s.replace("**", "").replace("`", "").lower().split())


SYMPTOM_LINE = re.compile(r"^\W*(?:actual|expected|observed|symptom|signal|error|steps?|repro\w*)\b", re.I)


def symptom_phrases(issue, limit=12):
    """The issue's own lines that state the symptom (Actual:, Expected:, Symptom:, Signal:, an error line), markdown
    marks stripped, offered to the reproducer as the only phrases it may name as its failure text."""
    out = []
    for ln in (issue or "").splitlines():
        # only the line's markdown frame goes (list, quote and heading marks, bold): the text itself stays verbatim,
        # `addEL_hook` with its underscore, or the check would refuse the phrase it was offered
        t = re.sub(r"^\s*(?:[-*+>]\s+|#+\s*|\d+\.\s+)*", "", ln).replace("**", "").strip()
        if (len(t.split()) >= 3 and len(t) <= 300 and (SYMPTOM_LINE.match(t) or re.search(r"\b\w*Error\b:", t))
                and t not in out):
            out.append(t)
    return out[:limit]


ERROR_TEXT = re.compile(r"\b\w*(?:Error|Exception)\b|\btraceback\b", re.I)
NOT_REPO = re.compile(r"(^|/)(node_modules|\.svelte-kit|\.venv|venv|site-packages|dist-packages|\.pnpm)/")
JS_IMPORT = re.compile(r"""(?:\bfrom\s+|\bimport\s*\(?\s*|\brequire\s*\(\s*)['"]([^'"]+)['"]""")
PY_IMPORT = re.compile(r"^\s*(?:from\s+([\w.]+)\s+import|import\s+([\w.]+))", re.M)
FRAME = re.compile(r"(?:file://)?((?:/|\b)[\w.@$+-]+(?:/[\w.@$+-]+)*\.(?:[cm]?[tj]sx?|svelte|vue|py)):\d+")


def repo_imports(text, wt, fmt="vitest"):
    """The modules of this repository (not its tests, not a package) that a test file imports. #553's fake proof on
    2026-11-18 imported only vitest and @sentry/browser-utils: it tested a library, not the repository."""
    out = []
    if fmt == "pytest":
        for m in PY_IMPORT.finditer(text):
            top = (m.group(1) or m.group(2)).split(".")[0]
            if top and not top.startswith(("test", "conftest")) and any(
                    os.path.exists(os.path.join(wt, d, top + ext)) for d in ("", "src") for ext in ("", ".py")):
                out.append(m.group(1) or m.group(2))
        return out
    code = "\n".join(ln for ln in text.splitlines() if not ln.lstrip().startswith(("//", "/*", "*", "#")))
    for spec in JS_IMPORT.findall(code):
        # an import names a test without its extension ('./x.repro.test'): judge it as the .ts file it loads
        if spec.startswith(("./", "../", "$lib/", "src/", "~/", "@/")) and not TEST_FILE.search(
                spec if re.search(r"\.[cm]?[tj]sx?$", spec) else spec + ".ts"):
            out.append(spec)
    return out


def repo_frames(messages, wt):
    """The source files of this repository (not a test, not a package) in failed tests' stacks or tracebacks."""
    root = os.path.realpath(wt) + "/"
    out = []
    for path in FRAME.findall("\n".join(messages)):
        if path.startswith("/"):
            path = os.path.realpath(path)
            if not path.startswith(root):
                continue
            path = path[len(root):]
        if (not NOT_REPO.search("/" + path) and not TEST_FILE.search(path) and path not in out
                and os.path.isfile(os.path.join(wt, path))):
            out.append(path)
    return out


def file_sha(path):
    """sha256 of a file's bytes, "" when it does not exist (a frozen reproducing test that was deleted)."""
    try:
        with open(path, "rb") as f:
            return hashlib.sha256(f.read()).hexdigest()
    except OSError:
        return ""


def cap_bytes(s, n):
    """s cut to at most n UTF-8 bytes, keeping its start, the cut marked."""
    b = s.encode()
    if len(b) <= n:
        return s
    return b[:max(n - 80, 0)].decode(errors="ignore") + f"\n[... cut: {len(b) - n + 80} more bytes]"


def tail(s, n):
    s = (s or "").strip()
    return s if len(s) <= n else "…" + s[-(n - 1):]


TURN_CAPPED = harness.TURN_CAPPED


def sh(cmd, cwd, env=None, timeout=None, check=False, max_turns=None, turn_ended=None, tee_path=None, stdin=None):
    """Run a command in its own process group; (exit code, combined output). A timeout kills the whole
    group and is exit 124 (harness.run_process, which holds the runner every harness shares). With max_turns the output
    is read as it comes, and the group is killed once that many turns have ended: exit TURN_CAPPED. A turn is a line
    turn_ended(line) accepts; the default is pi's, an assistant message_end of its JSON stream, as usage() counts them.
    tee_path: every line is also written there as it arrives. stdin: written to the command, None leaves it inherited."""
    try:
        return harness.run_process(cmd, cwd, env, timeout, check=check, max_turns=max_turns,
                                   turn_ended=turn_ended or harness_pi.turn_ended, tee_path=tee_path, stdin=stdin)
    except harness.ProcessFailed as e:
        raise Stop(str(e)) from None


# A fetch GitHub refused or that broke off: worth trying again. Anonymous fetches were refused in bursts
# on a worker host ("could not read Username": a 401 to an anonymous request), 5 of 8 runs in 3 minutes on
# 2026-11-15 while the same checkout a minute later and the next day succeeded.
FETCH_REFUSED = re.compile(r"could not read Username|unable to access|RPC failed|early EOF|unexpected disconnect|"
                           r"The requested URL returned error: (429|5\d\d)", re.I)
FETCH_RETRY_PAUSES = (30, 90)


ERR_LINE = re.compile(r"(npm (error|ERR!)|gyp ERR!|^error|Error:|ERR_)", re.I)


def errors_of(out, n=400):
    """The error lines of a tool's output, not its last lines: npm ends a failed install with an upgrade
    notice, which is all a tail showed (shop #490 on the host, 2026-11-15)."""
    lines = [ln.strip() for ln in (out or "").splitlines() if ERR_LINE.search(ln)]
    return tail(" | ".join(lines), n) if lines else tail(out, n)


class Tests:
    """One test run's result, from a vitest or a pytest JSON report: the failed tests (file::name), the files
    that failed to load or ran no test (vitest: no assertions and a failed status; pytest: a failed
    collection, or every test skipped), the files that ran, and the failed tests' messages (Chain.repro reads them)."""

    def __init__(self, fails, unloaded, ran, messages=None):
        self.fails, self.unloaded, self.ran, self.messages = fails, unloaded, ran, messages or []


def run_tests(wt, cmd, files, env, timeout, fmt="vitest"):
    """cmd with %s for the report and the files appended. A red run is data, not an error; a run that did
    not finish, or wrote no readable report, is an error (it names which). fmt: the report's shape."""
    report = os.path.join(wt, ".flotilla-suite.json")
    if os.path.exists(report):
        os.remove(report)
    code, out = sh(cmd.replace("%s", report) + "".join(" " + shlex.quote(f) for f in files), wt, env, timeout)
    try:
        if code == 124:
            raise Stop(f"the tests did not finish in {timeout} s: {' '.join(files)[:200]}")
        try:
            with open(report) as f:
                rep = json.load(f)
        except (OSError, ValueError) as e:
            raise Stop(f"the tests wrote no readable report (exit {code}, {e}): {errors_of(out, 300)}")
    finally:
        if os.path.exists(report):
            os.remove(report)
    if fmt == "pytest":
        return parse_pytest(rep, files)
    return parse_vitest(rep, wt)


def parse_vitest(rep, wt):
    fails, unloaded, ran, messages = set(), set(), set(), []
    for tr in rep.get("testResults") or []:
        name = os.path.relpath(tr.get("name", ""), wt)
        ran.add(name)
        assertions = tr.get("assertionResults") or []
        if not assertions and tr.get("status") == "failed":
            unloaded.add(name)
        for ar in assertions:
            if ar.get("status") == "failed":
                fails.add(name + "::" + ar.get("fullName", ""))
                messages.extend(str(m) for m in ar.get("failureMessages") or [])
    return Tests(fails, unloaded, ran, messages)


def parse_pytest(rep, files):
    """pytest-json-report's {"tests": [{nodeid, outcome}], "collectors": [{nodeid, outcome}]}. A skipped
    test is not a pass: api's integration files guard themselves with pytest.importorskip, so a
    broken install skips them and a run that only skipped must not look green. A file with no test that
    passed or failed, a collector that did not pass, and a requested file with no result at all are all
    unloaded (not run)."""
    fails, unloaded, seen, messages = set(), set(), {}, []
    for t in rep.get("tests") or []:
        nodeid = t.get("nodeid") or ""
        file, _, name = nodeid.partition("::")
        outcome = t.get("outcome")
        # "error" is a failed setup or teardown (a fixture that does not exist on the base, say), not a
        # test that ran and failed: it is a failure only when its call phase failed, else not run
        if outcome == "error" and (t.get("call") or {}).get("outcome") == "failed":
            outcome = "failed"
        executed = outcome in ("passed", "xpassed", "failed")
        seen[file] = seen.get(file, False) or executed
        if outcome == "failed":
            fails.add(nodeid)
            messages.append(str((t.get("call") or {}).get("longrepr") or ""))
        elif outcome == "error":
            unloaded.add(nodeid)
    unloaded |= {f for f, executed in seen.items() if not executed}
    for c in rep.get("collectors") or []:
        if c.get("outcome") != "passed":
            unloaded.add((c.get("nodeid") or "").partition("::")[0] or "(collection)")
    unloaded |= {f for f in files if f.endswith(".py") and f not in seen}
    ran = {f for f, executed in seen.items() if executed} - unloaded
    return Tests(fails, unloaded, ran, messages)


def assistant_text(stream):
    """The assistant's final text from a pi --mode json stream: the last message_end of the assistant."""
    return harness_pi.assistant_text(stream)


def stream_error(stream):
    """The error of a pi stream's last assistant message, "" when it did not end in one."""
    return harness_pi.stream_error(stream)


def usage(stream):
    """turns and output tokens of a pi stream (chain.go's parseUsageFull), for the record."""
    return harness_pi.usage(stream)


def issue_for_case(issue, repo, environ):
    """The issue as filed: intake cuts long bodies so its whole report fits flotillad's 64 KiB (body_cut), and the
    fix step should not reason from a cut report. When the body was cut, read it whole from GitHub's public issues
    API for `repo` (GITHUB_API, default https://api.github.com; the bugfix proxy allows api.github.com, no credential).
    Intake gives every detection the same small share (869 bytes), so most bodies are cut and this read is the fix's
    only full view of the issue: after one retry (BUGFIX_BODY_RETRY_S, 5 s) a failure stops the case, visibly, rather
    than fixing from the first few hundred bytes (the ops repo's review, 2026-11-16; the unauthenticated API allows 60 reads
    an hour per address). Returns the issue and a note for the chain's log ("" when nothing was done)."""
    if not issue.get("body_cut"):
        return issue, ""
    api = (environ.get("GITHUB_API") or "https://api.github.com").rstrip("/")
    url = f"{api}/repos/{repo}/issues/{int(issue['number'])}"
    why = ""
    for attempt in range(2):
        if attempt:
            time.sleep(float(environ.get("BUGFIX_BODY_RETRY_S") or 5))
        try:
            import urllib.request
            req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json",
                                                       "User-Agent": "flotilla-bugfix-fix"})
            with urllib.request.urlopen(req, timeout=20) as r:
                body = json.load(r).get("body")
        except Exception as e:  # noqa: BLE001 -- any failure: try once more, then stop
            why = f"{type(e).__name__}: {e}"
            continue
        if isinstance(body, str):
            return dict(issue, body=body, body_cut=False), f"issue body: read the full body ({len(body)} chars) from GitHub"
        why = "no body in GitHub's answer"
    raise Stop(f"the issue's body was cut to fit intake's report and could not be read whole from GitHub ({tail(why, 160)}): "
               "not fixing from its first few hundred bytes")


def step_row(step, detail):
    """(role, text) of a note of the chain as a step row of the run's calls: the role it concerns (verdict, reviewer,
    advisor; gates for a round's outcome, harness for any other hand-off) and its short text."""
    line = f"{step}: {detail}" if detail else step
    if step == "verdict":
        return "verdict", line
    if step == "review":
        outcome = re.sub(r"^[\s*_>#-]*VERDICT:\s*", "", detail or "", flags=re.I).strip(" *_")
        return "reviewer", f"review: {outcome}"
    if step == "advisor":
        return "advisor", f"pin: {detail}"
    if step == "acceptance gap":
        return "gates", line
    if re.fullmatch(r"round \d+", step):
        return ("reviewer" if (detail or "").startswith("the review") else "gates"), line
    return "harness", line


# Durable runs (ADR-0019). A case's work lives in one directory on the scratch volume, runs/<case id>/, with a
# checkpoint.json written after each boundary of the chain. A run that is cut short (flotillad restarted, the
# worker restarted, an error) leaves it; flotillad may start the case's next run with FLOTILLA_RESUME, and only
# then is the checkpoint used. Any other start deletes it, and so does a normal end (a plan or an escalation).
CHECKPOINT = "checkpoint.json"
CHECKPOINT_VERSION = 1
CHECKPOINT_MAX_AGE = 24 * 3600   # seconds: an older checkpoint is not resumed, and an older run directory is deleted
CHECKPOINT_PHASES = ("checkout", "attempt", "fixed", "gated", "reviewed")
INTERRUPTED = "The run was interrupted. The worktree is as you left it; continue."


def case_run_dir(scratch, case):
    cid = str(case.get("id") or "case")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,80}", cid):
        cid = "case-" + hashlib.sha256(cid.encode()).hexdigest()[:12]
    return os.path.join(scratch, "runs", cid)


LOCK = ".lock"
LOCK_WAIT_S = 120
EXIT_BUSY = 75   # EX_TEMPFAIL: flotillad reads it as "this run never started" (internal/sched ExitBusy)


class RunBusy(Exception):
    """Another run of the case holds runs/<case id>/.lock and did not let go within the wait."""


class RunDirStuck(Exception):
    """The case's run directory still holds what could not be deleted."""


def _open_up(path):
    """Make path and what is under it writable and searchable, so that it can be deleted (a Go module cache is
    read-only by design, and a repo's tools chmod too)."""
    try:
        if os.path.islink(path):
            return
        os.chmod(path, 0o700 if os.path.isdir(path) else 0o600)
        if os.path.isdir(path):
            for name in os.listdir(path):
                _open_up(os.path.join(path, name))
    except OSError:
        pass


def rmtree_force(path):
    """Delete path, getting past read-only directories; what still cannot go (a file owned by another user) stays
    and the caller finds it."""
    if os.path.islink(path) or not os.path.isdir(path):   # a file, a link, or nothing
        try:
            os.remove(path)
        except OSError:
            pass
        return
    try:
        shutil.rmtree(path)
        return
    except OSError:
        pass
    _open_up(path)
    shutil.rmtree(path, ignore_errors=True)


def remove_run_dir(scratch, run_dir):
    rmtree_force(run_dir)
    rmtree_force(os.path.join(scratch, "venvs", os.path.basename(run_dir)))


def clear_run_dir(scratch, run_dir):
    """The directory of a run that starts fresh: everything in it is deleted but the lock this run holds (a waiting
    run locks the file at the path, so it must stay). Leftovers that cannot be deleted are an error, said plainly:
    a fresh run on top of them would fail in some later step."""
    rmtree_force(os.path.join(scratch, "venvs", os.path.basename(run_dir)))
    os.makedirs(run_dir, exist_ok=True)
    for name in os.listdir(run_dir):
        if name != LOCK:
            rmtree_force(os.path.join(run_dir, name))
    left = [n for n in os.listdir(run_dir) if n != LOCK]
    if left:
        raise RunDirStuck(f"{run_dir} cannot be cleared: {', '.join(sorted(left)[:5])} stays (permissions or another user's file)")


def lock_run_dir(run_dir, wait_s=LOCK_WAIT_S, poll_s=0.5):
    """An exclusive flock on runs/<case id>/.lock for the whole run, so two runs of one case never share its worktree
    (a resume started while the interrupted run is still being stopped, a restart's cancel not yet delivered). Waits up
    to wait_s; then RunBusy, with nothing in the directory touched but the empty lock file. The file is checked to be
    the one at the path once locked: the run that held it may have ended and deleted the directory. Returns the fd;
    closing it (any way the run ends, a kill included) releases the lock."""
    path = os.path.join(run_dir, LOCK)
    deadline = time.monotonic() + wait_s
    while True:
        os.makedirs(run_dir, exist_ok=True)
        fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
        try:
            while True:
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= deadline:
                        raise RunBusy(f"another run of this case is still running (runs/{os.path.basename(run_dir)}/{LOCK} "
                                      f"was held for {wait_s:g} s): this run ends without touching its checkpoint") from None
                    time.sleep(poll_s)
            try:
                if os.stat(path).st_ino == os.fstat(fd).st_ino:
                    return fd
            except FileNotFoundError:
                pass
        except BaseException:
            os.close(fd)
            raise
        os.close(fd)


def lock_held(run_dir):
    """Whether another process holds the directory's lock now (a live run): the sweep leaves such a directory."""
    try:
        fd = os.open(os.path.join(run_dir, LOCK), os.O_RDONLY)
    except OSError:
        return False
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return False
    except BlockingIOError:
        return True
    finally:
        os.close(fd)


def sweep_stale(scratch, now=None):
    """Delete the run directories (and their venvs) nobody has touched for CHECKPOINT_MAX_AGE: runs that were
    cut short and never resumed, and the per-run directories of before ADR-0019. The newest file of a directory is
    its checkpoint, or its pi sessions, while a run is alive. A directory whose lock is held is a live run's."""
    now = time.time() if now is None else now
    for d in glob.glob(os.path.join(scratch, "runs", "*")):
        try:
            newest = max([os.path.getmtime(d)] + [os.path.getmtime(p) for p in
                                                   (os.path.join(d, CHECKPOINT),) if os.path.exists(p)])
        except OSError:
            continue
        if now - newest > CHECKPOINT_MAX_AGE and not lock_held(d):
            remove_run_dir(scratch, d)


def write_checkpoint(run_dir, doc):
    """Atomically: written beside, flushed to disk, renamed over; a kill leaves the old checkpoint or the new one."""
    path = os.path.join(run_dir, CHECKPOINT)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(doc, f)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)
    fd = os.open(run_dir, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def inside(run_dir, rel):
    """The absolute path of rel (a path relative to run_dir) when it exists and, symlinks resolved, stays inside
    run_dir; else ValueError."""
    if not isinstance(rel, str) or not rel or os.path.isabs(rel):
        raise ValueError(f"path {rel!r}")
    root = os.path.realpath(run_dir)
    p = os.path.realpath(os.path.join(run_dir, rel))
    if not p.startswith(root + os.sep) or not os.path.exists(p):
        raise ValueError(f"path {rel!r} is not a file of the run directory")
    return os.path.join(run_dir, rel)


def load_checkpoint(run_dir, case, repo, ref, now=None):
    """(checkpoint, "") when run_dir holds one this case's next run may continue; (None, why) else. The case and the
    repo and ref match, the worktree is at the base the checkpoint names, it is younger than CHECKPOINT_MAX_AGE, and
    every path in it is inside the run directory."""
    now = time.time() if now is None else now
    try:
        with open(os.path.join(run_dir, CHECKPOINT)) as f:
            ck = json.load(f)
        if not isinstance(ck, dict) or ck.get("version") != CHECKPOINT_VERSION:
            raise ValueError("not a version-%d checkpoint" % CHECKPOINT_VERSION)
        if ck.get("case") != case.get("id") or ck.get("repo") != repo or ck.get("ref") != ref:
            raise ValueError("it is another case's, repo's or ref's")
        saved = ck.get("saved")
        if not isinstance(saved, (int, float)) or isinstance(saved, bool) or saved > now + 60 or now - saved > CHECKPOINT_MAX_AGE:
            raise ValueError("older than %d h" % (CHECKPOINT_MAX_AGE // 3600))
        if ck.get("phase") not in CHECKPOINT_PHASES:
            raise ValueError(f"phase {ck.get('phase')!r}")
        base = ck.get("base")
        if not isinstance(base, str) or not re.fullmatch(r"[0-9a-f]{40}", base):
            raise ValueError("no base commit")
        code, head = sh(["git", "-C", os.path.join(run_dir, "wt"), "rev-parse", "HEAD"], run_dir, None, 30)
        if code != 0 or head.strip() != base:
            raise ValueError("the worktree is not at its base")
        st = ck.get("state")
        if not isinstance(st, dict):
            raise ValueError("no state")
        rnd, back = st.get("round"), st.get("reviewed_back")
        if not all(isinstance(x, int) and not isinstance(x, bool) and x >= 0 for x in (rnd, back)):
            raise ValueError("round counts")
        if not (isinstance(st.get("pins"), list) and all(isinstance(p, str) for p in st["pins"])):
            raise ValueError("pins")
        if ck["phase"] != "checkout":
            for k in ("prior", "manifest"):
                inside(run_dir, st.get(k))
        if st.get("resume") is not None:
            r = st["resume"]
            inside(run_dir, r.get("session"))
            inside(run_dir, r.get("patch"))
            if not isinstance(r.get("feedback"), str):
                raise ValueError("resume feedback")
        k = st.get("kept")
        if k is not None and not (isinstance(k, dict) and isinstance(k.get("round"), int) and all(
                isinstance(k.get(x), str) for x in ("diff", "review", "checked")) and isinstance(k.get("unloaded"), list)):
            raise ValueError("kept")
        if not (isinstance(ck.get("log"), list) and all(isinstance(x, str) for x in ck["log"])):
            raise ValueError("log")
        if not isinstance(ck.get("checked"), str) or not isinstance(ck.get("unloaded"), list) or not isinstance(ck.get("kb_round"), int):
            raise ValueError("gate notes")
        if ck["phase"] == "attempt":
            before = (ck.get("attempt") or {}).get("before")
            if not isinstance(before, dict):
                raise ValueError("no session list")
            for rel in before:
                if not isinstance(rel, str) or os.path.isabs(rel) or ".." in rel.split("/"):
                    raise ValueError("session list")
        if ck["phase"] == "fixed":
            x = ck.get("fixed") or {}
            inside(run_dir, x.get("patch"))
            inside(run_dir, x.get("stream"))
            if x.get("session") is not None:
                inside(run_dir, x["session"])
        if ck["phase"] in ("gated", "reviewed"):
            g = ck.get("gated") or {}
            if not (isinstance(g.get("reason"), str) and isinstance(g.get("files"), list) and all(isinstance(x, str) for x in g["files"])
                    and all(isinstance(g.get(x), int) for x in ("turns", "out_tok"))):
                raise ValueError("gated state")
            inside(run_dir, g.get("stream"))
            inside(run_dir, g.get("patch"))
            if g.get("session") is not None:
                inside(run_dir, g["session"])
        if ck["phase"] == "reviewed" and not isinstance(ck.get("review"), str):
            raise ValueError("no review")
    except (OSError, ValueError, TypeError, AttributeError) as e:
        return None, f"{type(e).__name__}: {e}"
    return ck, ""


def trim_session(path):
    """Drop a last line a kill cut short, so pi finds a session of whole JSON lines."""
    try:
        with open(path, "rb") as f:
            data = f.read()
        if not data or data.endswith(b"\n"):
            return
        cut = data.rfind(b"\n") + 1
        with open(path, "wb") as f:
            f.write(data[:cut])
    except OSError:
        pass


class Chain:
    def __init__(self, a, case, run_dir, environ, meter=None, restored=None):
        self.a, self.case, self.run_dir = a, case, run_dir
        self.restored, self.snap, self.in_flight, self.fixed_ok = restored, None, None, False
        self.resumed_from = ""   # the plan's detail says so when the run continued an earlier one
        # each pi episode's turns go to the run's calls file once it ends (role: verdict, coder, reviewer, advisor),
        # with the episode's share of the gateway's bill when there is a meter; the chain's notes go there as steps
        self.calls = callrec_pi.Recorder(os.path.join(run_dir, "pi"), environ, case.get("id"), meter)
        self.issue = ((case.get("data") or {}).get("issue")) or {}
        if not self.issue.get("number"):
            raise Stop("the case carries no issue (its detection's data has none)")
        self.repo = self.issue.get("repo") or a.repo
        if self.repo != a.repo:
            raise Stop(f"the case's issue is in {self.repo}, not {a.repo}")
        self.n = int(self.issue["number"])
        self.tid = f"real-{self.repo.split('/')[1]}-{self.n}"
        self.root = os.path.join(run_dir, "bakeoff")       # BAKEOFF_ROOT: the leaves' results
        self.wt = os.path.join(run_dir, "wt")              # BAKEOFF_WT: the worktree
        self.triaged = os.path.join(run_dir, "triaged")    # BAKEOFF_TRIAGED_DIR: the manifests
        self.log = []
        self.repro_patch, self.repro_files = "", {}   # the frozen reproducing test (repro), none without --repro
        self.issue, body_note = issue_for_case(self.issue, self.repo, environ)
        if body_note:
            self.note(body_note)
        self.rel_targets, self.rel_unmapped, self.rel_considered = [], [], []
        self.venv = os.path.join(a.scratch, "venvs", os.path.basename(run_dir))
        upstream = dict(x.split("=", 1) for x in a.upstream)
        cfg = pi_config.write(os.path.join(run_dir, "pi"), sorted({a.worker_model, a.advisor_model, a.reviewer_model}), environ, upstream,
                              min_output=a.min_output or None)
        self.cfg = cfg
        self.provider, self.worker = pi_config.pick(cfg, a.worker_model)
        _, self.advisor = pi_config.pick(cfg, a.advisor_model)
        _, self.reviewer = pi_config.pick(cfg, a.reviewer_model)
        self.env = dict(environ)
        # every pi step reads the fleet's rule file, not the repo's AGENTS.md: on shop that file is ~30k tokens
        # in every call (pi loads it by default), 44% of the input tokens of 2026-11-15/16's sessions
        if a.pi_rules:
            self.env["BUGFIX_PI_RULES"] = a.pi_rules
        else:
            self.env.pop("BUGFIX_PI_RULES", None)
        self.env.update({"PI_CODING_AGENT_DIR": os.path.join(run_dir, "pi"), "PI_PROVIDER": self.provider,
                         "BAKEOFF_ROOT": self.root, "BAKEOFF_WT": self.wt, "BAKEOFF_TRIAGED_DIR": self.triaged,
                         "TRIAGE_MODEL": self.worker, "ADVISOR_MODEL": self.advisor,
                         "PI_MODEL": self.worker, "npm_config_cache": os.path.join(a.scratch, "npm-cache"),
                         "HOME": run_dir,
                         # a Python repo's venv and pip cache: outside the worktree; both are the run's own
                         # and deleted with it (a cache shared between runs would let a wheel that one
                         # run's test code planted be installed by the next)
                         "BUGFIX_VENV": self.venv, "PIP_CACHE_DIR": os.path.join(run_dir, "pip-cache"),
                         # no git hook runs, in any git this run starts: the repo cache is shared between
                         # runs, and a hook planted by one run's code would run in the next (the review,
                         # docs/security-reviews)
                         "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": "/dev/null"})
        self.env.pop("FLOTILLA_PLAN_FILE", None)  # the leaves write nothing of the run's own
        if restored:
            self.adopt(restored)

    def adopt(self, ck):
        """Take over the chain a checkpoint holds: the worktree is already at its base (load_checkpoint checked), the
        notes so far are this run's too, and the sessions the earlier run recorded are not recorded again."""
        self.base = ck["base"]
        self.env["TRIAGE_BASE"] = self.base
        self.log = list(ck["log"])
        self.checked, self.unloaded_on_base, self.kb_round = ck["checked"], list(ck["unloaded"]), ck["kb_round"]
        self.cache = os.path.join(self.a.scratch, "repos", self.repo.replace("/", "__") + ".git")
        self.scrub_cache(self.cache, self.env.get("GIT_REMOTE_BASE", "https://github.com").rstrip("/") + "/" + self.repo)
        self.pin_cache_config()
        pi = os.path.join(self.run_dir, "pi")
        before = (ck.get("attempt") or {}).get("before") or {}
        self.in_flight, grown = None, []
        if ck["phase"] == "attempt":
            now = self.sessions()
            grown = [f for f, n in now.items() if before.get(os.path.relpath(f, self.run_dir)) != n]
            self.in_flight = max(grown, key=os.path.getmtime) if grown else None
        self.calls.skip_existing(keep=set(grown))
        self.resumed_from = f"phase {ck['phase']}, round {ck['state']['round']}, saved {ck.get('saved_at', '?')}"

    def save(self, phase, state, **extra):
        """The checkpoint of a boundary of the chain. `state` is the loop's: its round, pins, the paths of the verdict and
        manifest it goes on from, the review kick-backs spent, the last clean fix the review sent back, and the resume
        tuple (session, patch, feedback) the next attempt takes. Paths are relative to the run directory. A write that
        fails is noted and the run goes on: the checkpoint is for the next run, and the run in hand does not need it."""
        rel = lambda p: os.path.relpath(p, self.run_dir)  # noqa: E731
        try:
            state = state or {"round": 0, "pins": [], "prior": None, "manifest": None, "reviewed_back": 0, "resume": None}
            st = dict(state, prior=rel(state["prior"]) if state["prior"] else None,
                      manifest=rel(state["manifest"]) if state["manifest"] else None)
            kept = getattr(self, "kept", None)
            st["kept"] = {"round": kept[0], "diff": kept[1], "review": kept[2], "checked": kept[3] or "",
                          "unloaded": list(kept[4] or [])} if kept else None
            if getattr(self, "repro_files", None):   # the frozen proof: a resumed run holds its rounds to the same test
                with open(os.path.join(self.run_dir, "checkpoint.repro.patch"), "w") as f:
                    f.write(self.repro_patch)
                st["repro"] = {"patch": "checkpoint.repro.patch", "files": dict(self.repro_files)}
            if state.get("resume"):
                session, patch, feedback = state["resume"]
                with open(os.path.join(self.run_dir, "checkpoint.resume.patch"), "w") as f:
                    f.write(patch)
                st["resume"] = {"session": rel(session), "patch": "checkpoint.resume.patch", "feedback": feedback}
            now = time.time()
            doc = {"version": CHECKPOINT_VERSION, "case": self.case.get("id"), "repo": self.repo, "ref": self.a.base or "main",
                   "base": self.base, "saved": now, "saved_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now)),
                   "phase": phase, "log": self.log, "checked": getattr(self, "checked", "") or "",
                   "unloaded": list(getattr(self, "unloaded_on_base", []) or []), "kb_round": getattr(self, "kb_round", 0),
                   "state": st}
            doc.update(extra)
            write_checkpoint(self.run_dir, doc)
        except (OSError, ValueError, TypeError) as e:
            print(f"BUGFIX FIX #{self.n} no checkpoint at {phase}: {type(e).__name__}: {e}", flush=True)

    def note(self, step, detail=""):
        line = f"{step}: {detail}" if detail else step
        self.log.append(line)
        print(f"BUGFIX FIX #{self.n} {line}", flush=True)
        self.calls.step(*step_row(step, detail))

    # 2 · the base
    def checkout(self):
        """main, one commit deep, its large blobs fetched only for the paths checked out: shop's history is
        14 GB and its tree 6.8 GB, most of it images the suite does not read. --exclude-path leaves such trees out;
        a test that needs them fails on the base too, so it is in the base's red set, not a new failure."""
        remote = self.env.get("GIT_REMOTE_BASE", "https://github.com").rstrip("/") + "/" + self.repo
        ref = self.a.base or "main"
        # One partial clone per repo on the scratch volume, kept between runs; each run fetches only what is
        # new and checks out a worktree of its own from it, and the large blobs a checkout fetched stay for
        # the next run. Before, each run cloned 850 MB of shop anonymously, and GitHub refused some of the
        # checkout's fetches that followed one another (#643 and #648 on a worker host, 2026-11-15, "could not read
        # Username": a 6 KB request answered 4.5 KB, where #646's same request had been answered 140 MB).
        # blob:limit, not blob:none: with no blobs at all a checkout asks for every file in one batch, and
        # GitHub answered such a request with HTTP 400 (#643 on the host, 2026-11-15).
        cache = os.path.join(self.a.scratch, "repos", self.repo.replace("/", "__") + ".git")
        self.cache = cache
        if not os.path.isdir(cache):
            os.makedirs(os.path.dirname(cache), exist_ok=True)
            # not --bare: with per-worktree config a bare repo's worktrees inherit core.bare=true
            sh(["git", "init", "-q", cache], self.run_dir, self.env, 60, check=True)
            sh(["git", "-C", cache, "remote", "add", "origin", remote], self.run_dir, self.env, 60, check=True)
            sh(["git", "-C", cache, "config", "extensions.worktreeConfig", "true"], self.run_dir, self.env, 60, check=True)
        self.scrub_cache(cache, remote)
        self.check_cache_config(cache)
        sh(["git", "-C", cache, "fetch", "-q", "--depth", "1", f"--filter=blob:limit={self.a.blob_limit}", "origin", ref],
           self.run_dir, self.env, 1200, check=True)
        sh(["git", "-C", cache, "worktree", "add", "-q", "--no-checkout", "--detach", self.wt, "FETCH_HEAD"],
           self.run_dir, self.env, 120, check=True)
        if self.a.exclude_path:
            sh(["git", "-C", self.wt, "sparse-checkout", "set", "--no-cone", "/*"] + [f"!/{x.strip('/')}/" for x in self.a.exclude_path],
               self.run_dir, self.env, 60, check=True)
        self.with_fetch_retry(["git", "-C", self.wt, "checkout", "-q", "--detach", "HEAD"], 1200)
        _, base = sh(["git", "-C", self.wt, "rev-parse", "HEAD"], self.run_dir, self.env, 30, check=True)
        self.base = base.strip()
        self.env["TRIAGE_BASE"] = self.base  # the leaves reset to it
        self.note("base", self.base[:12])
        # The worktrees share the cache's config, and a repo's own scripts write to it: shop's `prepare`
        # runs `git config core.hooksPath .husky` on every npm ci (8 runs escalated by the check above on
        # a worker host, 2026-11-15). Nothing runs from it (GIT_CONFIG_* pins hooksPath for every git the run
        # starts); what must not happen is that it stays for the next run. So the config checked above is
        # put back after the setup and when the run ends, and the check above stays for what got past.
        self.cfg_path = os.path.join(cache, ".git", "config")
        with open(self.cfg_path) as f:
            self.cfg_good = f.read()
        if self.a.setup:
            code, out = sh(self.a.setup, self.wt, self.env, self.a.setup_timeout)
            self.restore_config("setup")
            if code != 0:
                raise Stop(f"the repo's setup failed on the base (exit {code}): {errors_of(out)}")

    def scrub_cache(self, cache, remote):
        """What the shared cache is put back to before anything reads it: also on a resume, which does not check out,
        because the run that was cut short ran the repo's code beside the cache and a hook or another origin it left
        would otherwise be pinned as the config to restore."""
        sh(["git", "-C", cache, "worktree", "prune"], self.run_dir, self.env, 60)  # a run that died left its worktree
        # what an earlier run's code could have left in the shared cache: hooks, another origin, a URL rewrite
        rmtree_force(os.path.join(cache, ".git", "hooks"))
        sh(["git", "-C", cache, "remote", "set-url", "origin", remote], self.run_dir, self.env, 60, check=True)

    def check_cache_config(self, cache):
        _, cfg = sh(["git", "-C", cache, "config", "--local", "--list"], self.run_dir, self.env, 60)
        bad = [ln for ln in cfg.splitlines() if re.match(r"(url\.|core\.(hookspath|fsmonitor|sshcommand)|protocol\.)", ln, re.I)]
        if bad:
            raise Stop(f"the repo cache's config was changed ({'; '.join(bad)[:200]}): remove {cache} by hand")

    def pin_cache_config(self):
        """A resumed run does not check out again: the cache's config is checked as checkout() does, and what it is now
        is what restore_config puts back at the end."""
        self.check_cache_config(self.cache)
        self.cfg_path = os.path.join(self.cache, ".git", "config")
        with open(self.cfg_path) as f:
            self.cfg_good = f.read()

    def restore_config(self, when):
        """The cache's config back to what checkout() checked; the lines that differed are noted."""
        if not getattr(self, "cfg_path", None):
            return
        with open(self.cfg_path) as f:
            now = f.read()
        if now == self.cfg_good:
            return
        changed = sorted(set(now.splitlines()) ^ set(self.cfg_good.splitlines()))
        with open(self.cfg_path, "w") as f:
            f.write(self.cfg_good)
        self.note(f"{when} changed the repo cache's config; put back", "; ".join(ln.strip() for ln in changed)[:200])

    def with_fetch_retry(self, cmd, timeout):
        """Run a git command that may fetch (a partial clone's checkout fetches the blobs it lacks); a refused
        or broken fetch is tried again after FETCH_RETRY_PAUSES, and one that fails every time stops the run."""
        for pause in (0,) + FETCH_RETRY_PAUSES:
            if pause:
                self.note("fetch refused; trying again", f"in {pause}s")
                time.sleep(pause)
            code, out = sh(cmd, self.run_dir, self.env, timeout)
            if code == 0:
                return out
            if not FETCH_REFUSED.search(out):
                break
        raise Stop(f"{' '.join(cmd)}: exit {code}: {tail(out, 300)}")

    def reset(self):
        sh(["git", "-C", self.wt, "reset", "-q", "--hard", self.base], self.run_dir, self.env, 60, check=True)
        sh(["git", "-C", self.wt, "clean", "-fdq"], self.run_dir, self.env, 60, check=True)  # not -x: node_modules stays

    def put_repro(self):
        """Put the frozen reproducing test (repro) back on the reset worktree."""
        if not self.repro_patch:
            return
        pf = os.path.join(self.run_dir, "repro.patch")
        with open(pf, "w") as f:
            f.write(self.repro_patch)
        sh(["git", "-C", self.wt, "apply", "--index", "--binary", pf], self.run_dir, self.env, 60, check=True)

    # 1 · the verdict
    def verdict(self, evidence=None, prior=None, acceptance_gap=False):
        ticket = os.path.join(self.run_dir, "ticket.json")
        with open(ticket, "w") as f:
            json.dump({"id": self.tid, "repo": self.repo.split("/")[1], "level": "L0-real", "source_url": self.issue.get("url", ""),
                       "description": (self.issue.get("title") or "") + "\n\n" + (self.issue.get("body") or ""),
                       "oracle_test_file": "", "code_files": []}, f)
        args = [os.path.join(BAKEOFF, "triage_run.sh"), ticket, "pi"] + ([evidence, prior] if evidence else [])
        env = dict(self.env, ACCEPTANCE_GAP="1" if acceptance_gap else "0", PI_TOOLS=READ_ONLY_TOOLS)
        results = os.path.join(self.root, "results")
        started = time.time() - 1
        code, out = sh(args, BAKEOFF, env, self.a.episode_timeout + 120)
        self.calls.record("verdict", self.a.worker_model)
        # the leaf names its directory from the prior verdict's kickback_round, which the model writes:
        # take the verdict this call wrote, not the one the round number predicts
        fresh = []
        for d in os.listdir(results) if os.path.isdir(results) else []:
            f = os.path.join(results, d, "pi", "triage.json")
            if d.startswith(f"{self.tid}-triage") and os.path.exists(f) and os.path.getmtime(f) >= started:
                fresh.append((os.path.getmtime(f), f))
        if not fresh:
            raise Stop(f"the verdict step gave no verdict (exit {code}): {tail(out, 300)}")
        path = max(fresh)[1]
        try:
            with open(path) as f:
                v = json.load(f)
        except (OSError, ValueError):
            raise Stop(f"the verdict step's verdict is not JSON (exit {code}): {tail(out, 300)}")
        kb = re.search(r"-kb(\d+)$", os.path.basename(os.path.dirname(os.path.dirname(path))))
        self.kb_round = int(kb.group(1)) if kb else 0
        # the verdict prompt's own contract (agents/triage.md: "missing": only for needs-info): an actionable verdict
        # that lists what is unknown is needs-info (#553: 2 of 3 first passes on 2026-11-16/17, each fix then wrong)
        missing = (v.get("missing") if isinstance(v, dict) else None) or []
        if isinstance(v, dict) and v.get("verdict") == "actionable" and (missing if isinstance(missing, list) else str(missing).strip()):
            v = dict(v, verdict="needs-info")
            self.note("downgrade", "the verdict said actionable but listed what is missing: needs-info")
        return v, path

    # 2 · the reproducing test, before any fix
    def repro(self, manifest):
        """A separate agent writes the failing test from the issue's evidence (harnesses/repro.sh, agents/repro.md), or
        says what is missing; it never fixes. The proof must not come from the agent that chose the fix: on 2026-11-17
        five approved #553 plans were five different guesses, each proven by its own fixer's test (fleet-eval-data
        projects/bugfix-cost, the advisor's review). Checked here: test files only, no assertion on main changed, no
        source text read, the failure text it promises is the issue's own, and the test fails on the base with it.
        The test is then frozen: every fix round starts from it and may not change it (attempt), and it is the repro
        gate's test (gates). No test: the case ends as needs-info, before any fix session."""
        with open(manifest) as f:
            task = json.load(f).get("description") or ""
        self.reset()
        issue = (self.issue.get("title") or "") + "\n\n" + (self.issue.get("body") or "")
        phrases = symptom_phrases(issue)
        offer = ("\n\nTHE PHRASES YOU MAY USE as expect_failure_match (lines of the issue as filed that state the symptom; "
                 "copy one, or a 3-8 word part of one, exactly; never a phrase from the task):\n"
                 + "\n".join(f"- {p}" for p in phrases)) if phrases else ""
        desc = cap_bytes(f"THE ISSUE AS FILED:\n{issue}{offer}\n\nTHE TASK THE VERDICT STEP WROTE FROM IT (it can be wrong; "
                         f"never take a phrase from it):\n{task}", REVIEW_PROMPT_MAX)
        code, stream = sh([os.path.join(BAKEOFF, "harnesses", "repro.sh"), self.wt, desc], BAKEOFF, self.env,
                          self.a.episode_timeout, max_turns=self.a.repro_max_turns)
        said = assistant_text(stream or "")
        blocks = re.findall(r"```json\s*(\{.*?\})\s*```", said, re.S)
        try:
            ans = json.loads(blocks[-1]) if blocks else None
        except ValueError:
            ans = None
        if not isinstance(ans, dict):
            raise Stop(f"the reproducer gave no answer (exit {code}): {tail(' '.join(said.split()), 200)}")
        if ans.get("repro") == "none":
            raise Stop(f"needs info from the reporter: no failing test can be written from the issue: "
                       f"{str(ans.get('why') or '')[:300]}; needed: {str(ans.get('needed') or '')[:300]}")
        self.stage()
        _, names = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", "--no-renames", "-z", self.base],
                      self.run_dir, self.env, 60, check=True)
        files = [x for x in names.split("\0") if x.strip() and not x.startswith(".flotilla-suite")]
        other = [x for x in files if not TEST_FILE.search(x)]
        if other:
            raise Stop(f"the reproducer changed files that are not tests: {', '.join(other)[:300]}")
        # new files only: on 2026-11-18 a reproducer replaced an existing test of the file it added to (#335)
        _, added = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", "--no-renames", "-z", "--diff-filter=A",
                       self.base], self.run_dir, self.env, 60, check=True)
        edited = sorted(set(files) - {x for x in added.split("\0") if x.strip()})
        if edited:
            raise Stop(f"the reproducer edited a file that is on main (it must add a new test file): {', '.join(edited)[:300]}")
        tests = [x for x in files if os.path.exists(os.path.join(self.wt, x))]
        if self.a.test_format == "pytest":
            tests = [x for x in tests if x.endswith(".py") and not PYTEST_OUT_OF_SCOPE.search(x)]
        if not tests:
            raise Stop("the reproducer wrote no test")
        changed = changed_assertions(self.wt, self.base, self.run_dir, self.env)
        if changed:
            raise Stop("the reproducer removed or rewrote an assertion of a test on main: "
                       + "; ".join(f"{f}: {' | '.join(lines[:3])}" for f, lines in changed)[:300])
        reads = source_text_tests(self.wt, self.base, self.run_dir, self.env)
        if reads:
            raise Stop("the reproducer's test reads a source file's text: " + "; ".join(f"{f}: {ln}" for f, ln in reads)[:300])

        norm = norm_phrase
        match = norm(ans.get("expect_failure_match") or "")
        if not match or match not in norm(issue):
            raise Stop(f"the reproducer's failure text is not the issue's own: {match[:200]!r}")
        res = self.tests(self.a.test_cmd, tests)
        if not res.fails:
            raise Stop(f"the reproducing test does not fail on the base ({', '.join(tests)}): it does not reproduce the bug")
        if match not in norm(" ".join(res.messages)):
            raise Stop(f"the reproducing test fails on the base, but not with the issue's symptom: {match[:120]!r} "
                       f"is not in its failure ({tail(' '.join(norm(m) for m in res.messages), 200)})")
        # the symptom must come out of this repository's code, not be written into the test (#553 on 2026-11-18:
        # the issue's TypeError was an assertion's label on a test of @sentry/browser-utils; the advisor's review)
        imported = []
        for x in tests:
            with open(os.path.join(self.wt, x), errors="replace") as f:
                imported += repo_imports(f.read(), self.wt, self.a.test_format)
        if not imported:
            raise Stop(f"the reproducing test imports nothing of this repository ({', '.join(tests)}): it tests a "
                       f"library or itself, not the code the issue is about")
        if ERROR_TEXT.search(match):
            raised = [m for m in res.messages if match in norm(m) and repo_frames([m], self.wt)]
            if not raised:
                raise Stop(f"the issue's error is quoted by the reproducing test, not raised by the repository's code: "
                           f"no failure carrying {match[:80]!r} has a frame in the repository's source")
        _, self.repro_patch = sh(["git", "-C", self.wt, "diff", "--cached", "--binary", self.base, "--", ".",
                                  ":(exclude).flotilla-suite.json"], self.run_dir, self.env, 60, check=True)
        self.repro_files = {x: file_sha(os.path.join(self.wt, x)) for x in tests}
        self.note("repro", f"{', '.join(tests)} fails on the base with the issue's symptom ({match[:80]!r})")

    # 3 · one fix attempt and its gates
    def sessions(self):
        """{session file: size} of pi's saved sessions in this run (all steps share PI_CODING_AGENT_DIR)."""
        out = {}
        for f in glob.glob(os.path.join(self.run_dir, "pi", "sessions", "*", "*.jsonl")):
            try:
                out[f] = os.path.getsize(f)
            except OSError:
                pass
        return out

    def apply(self, patch):
        """(ok, output) of putting a diff on the worktree, staged."""
        pf = os.path.join(self.run_dir, "resume.patch")
        with open(pf, "w") as f:
            f.write(patch)
        code, out = sh(["git", "-C", self.wt, "apply", "--index", "--binary", pf], self.run_dir, self.env, 60)
        return code == 0, out

    def save_fixed(self, stream):
        """pi's episode is over and the gates are not: the fix and the stream go in the checkpoint, so a kill in the gates
        (minutes of tests) costs the gates again and not the episode."""
        try:
            for name, text in (("checkpoint.fix.patch", self.last_patch), ("checkpoint.stream", stream or "")):
                with open(os.path.join(self.run_dir, name), "w") as f:
                    f.write(text)
            self.fixed_ok = True
        except OSError as e:
            print(f"BUGFIX FIX #{self.n} no checkpoint after the fix: {e}", flush=True)
            return
        sess = os.path.relpath(self.last_session, self.run_dir) if self.last_session else None
        self.snap("fixed", fixed={"patch": "checkpoint.fix.patch", "stream": "checkpoint.stream", "session": sess})

    def attempt(self, manifest, resume=None, cont=None):
        """One fix attempt. `resume` = (session file, the previous round's diff, the feedback): the worktree is put
        back as that round left it and its session goes on with the feedback as the next message, where a new session
        would explore the repo again from nothing (44-99 calls a session on 2026-11-15/16; the installation's research repo
        projects/bugfix-cost, the advisor's review). If the diff does not apply, a new session from the manifest.
        `cont` (ADR-0019) = what an earlier run left of this attempt: {"session": the session it was killed in the
        middle of, "patch": None} goes on in that session with the worktree as it is, and a message that says so;
        {"session", "patch", "stream"} is a fix pi had finished and the gates had not: the tree is put back from the
        patch and the gates run, with no new pi episode."""
        with open(manifest) as f:
            desc = json.load(f)["description"]
        if self.repro_files:
            frozen = ", ".join(sorted(self.repro_files))
            note = (f"\n\nIMPORTANT: the failing test {frozen} reproduces the bug and is frozen. Fix the source so that it "
                    "passes; do not change, move or delete it (you may add other tests).")
        else:
            note = "\n\nIMPORTANT: write a failing regression test FIRST (repro-first), then fix. Both must be in your final diff."
        env, resumed_from, finished, self.fixed_ok = self.env, None, False, False
        if cont and cont.get("patch") is not None:   # a saved fix holds the frozen test: its diff is from the base
            self.reset()
            ok, out = self.apply(cont["patch"])
            if ok:
                finished, self.fixed_ok = True, True
            else:
                self.note("resume", f"the saved fix does not apply ({tail(out, 120)}): a new session")
                self.reset()
                cont, resume = None, None
        elif not cont:
            self.reset()
        if cont and not finished:
            # the worktree as the killed session left it; the frozen test goes back only if it is gone
            if any(not os.path.exists(os.path.join(self.wt, x)) for x in self.repro_files):
                self.put_repro()
            trim_session(cont["session"])
            desc, env = INTERRUPTED + note, dict(self.env, PI_RESUME_SESSION=cont["session"])
        elif finished:
            pass
        elif resume:
            session, patch, feedback = resume
            ok = True
            if patch.strip():
                ok, out = self.apply(patch)
                if not ok:
                    self.note("resume", f"the previous round's diff does not apply ({tail(out, 120)}): a new session")
                    self.reset()
            if not (ok and patch.strip()):   # a round's diff holds the frozen test; without one, put it back
                self.put_repro()
            if ok:
                desc = feedback + "\n\nThe worktree is as your previous attempt left it." + note
                env, resumed_from = dict(self.env, PI_RESUME_SESSION=session), patch
            else:
                desc += note
        else:
            self.put_repro()
            desc += note
        harness = os.path.join(BAKEOFF, "harnesses", self.a.harness + ".sh")
        if not re.fullmatch(r"[a-z0-9-]+", self.a.harness) or not os.path.exists(harness):
            raise Stop(f"no harness {self.a.harness!r} in {os.path.dirname(harness)}")
        desc = cap_bytes(desc, REVIEW_PROMPT_MAX)   # one argv string to pi: Linux caps it at 128 KiB
        before = self.sessions()
        if finished:
            code, stream, self.last_session = 0, cont["stream"], cont.get("session")
        else:
            code, stream = sh([harness, self.wt, desc], BAKEOFF, env, self.a.episode_timeout, max_turns=self.a.fix_max_turns)
            self.calls.record("coder", self.a.worker_model)
            after = self.sessions()
            grown = [f for f, n in after.items() if before.get(f) != n]
            self.last_session = max(grown, key=os.path.getmtime) if grown else (cont["session"] if cont else None)
        turns, out_tok = usage(stream)
        self.stage()
        # -z: names raw, never git's quoted form for a non-ASCII path; --no-renames: both sides of a rename
        # are listed, so a file moved out of an out-of-scope path is still seen
        _, names = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", "--no-renames", "-z", self.base],
                      self.run_dir, self.env, 60, check=True)
        files = [x for x in names.split("\0") if x.strip() and not x.startswith(".flotilla-suite")]
        _, self.last_patch = sh(["git", "-C", self.wt, "diff", "--cached", "--binary", self.base, "--", ".",
                                 ":(exclude).flotilla-suite.json"], self.run_dir, self.env, 60, check=True)
        stats = (turns, out_tok)
        if not finished and self.snap:
            self.save_fixed(stream)
        moved = [x for x, sha in sorted(self.repro_files.items()) if file_sha(os.path.join(self.wt, x)) != sha]
        if moved:
            return f"the fix changed the frozen reproducing test: {', '.join(moved)}", files, stream, stats
        own = [x for x in files if x not in self.repro_files]   # what the fix itself changed
        if resumed_from is not None and self.last_patch == resumed_from:   # the tree still holds the last round's change
            return "the fix changed nothing since its last round", files, stream, stats
        if code == 124 and not own:
            return "the fix episode ran out of time with no change", files, stream, stats
        if code == TURN_CAPPED and not own:
            return f"the fix episode reached its turn cap ({self.a.fix_max_turns}) with no change", files, stream, stats
        if not own:
            return "the fix changed nothing", files, stream, stats
        pytest = self.a.test_format == "pytest"
        bad = []
        for x in files:
            if OUT_OF_SCOPE.search(x) or any(re.search(o, x) for o in self.a.out_of_scope):
                bad.append(x)
            elif pytest and x.startswith('"'):   # git's quoted form of an odd name: fail closed
                bad.append(f"{x} (a quoted path name: it cannot be checked against the scope)")
            elif pytest and self.pytest_scope(x):
                bad.append(f"{x} ({self.pytest_scope(x)})")
        # under pytest the scope comes first: a file pytest imports must be named, not hidden behind "no test"
        if bad and pytest:
            return f"out of scope: the diff touches {', '.join(bad)}", files, stream, stats
        # the related gates run what is left: a round that deletes the test its change breaks (or moves it out
        # of a test path: --no-renames lists that as a delete) would pass them
        _, gone = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", "--no-renames", "-z", "--diff-filter=D", self.base],
                     self.run_dir, self.env, 60, check=True)
        deleted = [x for x in gone.split("\0") if x.strip() and TEST_FILE.search(x)]
        if deleted:
            return f"out of scope: a fix may not delete a test: {', '.join(deleted)}", files, stream, stats
        # nor remove or invert an assertion of a test on main: that alone made a symptom-hiding filter pass the repro
        # gate (#553, 2026-11-16/17: `.not.toBeNull()` -> `.toBeNull()` in all 5 filter plans; 0 in the 7 #335 plans)
        changed = changed_assertions(self.wt, self.base, self.run_dir, self.env)
        if changed:
            what = "; ".join(f"{f}: " + " | ".join(lines[:4]) for f, lines in changed)
            return (f"out of scope: a fix may not remove or rewrite an assertion of a test on main (add a test instead): "
                    f"{what[:600]}"), files, stream, stats
        reads = source_text_tests(self.wt, self.base, self.run_dir, self.env)
        if reads:
            return ("test-proves-nothing: a test reads a source file's text instead of running its code: "
                    + "; ".join(f"{f}: {line}" for f, line in reads)[:600]), files, stream, stats
        tests = [x for x in files if TEST_FILE.search(x) and os.path.exists(os.path.join(self.wt, x))]
        if pytest:   # only .py files go to pytest; a conftest or config is never a test
            tests = [x for x in tests if x.endswith(".py") and not PYTEST_OUT_OF_SCOPE.search(x)]
        if self.repro_files:   # the repro gate's test is the reproducer's, never one the fixer wrote
            tests = sorted(self.repro_files)
        if not tests:
            return "repro-first violated: no test in the diff", files, stream, stats
        if bad:
            return f"out of scope: the diff touches {', '.join(bad)}", files, stream, stats
        return self.gates(files, tests), files, stream, stats

    def pytest_scope(self, x):
        """Why a changed file may not be part of a pytest fix ("" when it may): an ALLOWLIST, a .py under
        --source-root or a test_*.py / *_test.py under --test-root; every other changed file anywhere (config,
        data, docs, helpers) is out. Python that pytest imports outside a test is code that runs with the
        worker's environment: the repo root is on sys.path (cwd under `python -m pytest`, and PYTHONPATH), so a
        top-level json.py or pytest_socket.py shadows the installed module, a tests/ __init__.py runs at
        package collection, a helper module when a test imports it."""
        if PYTEST_OUT_OF_SCOPE.search(x):
            return "pytest config, collection or install file"
        src, tst = self.a.source_root.rstrip("/") + "/", self.a.test_root.rstrip("/") + "/"
        if x.endswith(".py") and x.startswith(src):
            return ""
        if x.startswith(tst) and x.endswith(".py"):
            if re.fullmatch(r"test_[^/]*\.py|[^/]*_test\.py", os.path.basename(x)):
                return ""
            return (f"only test files (test_*.py, *_test.py) may change under {self.a.test_root}: "
                    "__init__.py and helper modules run at import")
        if x.endswith(".py"):
            return (f"a .py outside {self.a.source_root} and {self.a.test_root} is imported by pytest and may shadow "
                    "an installed module")
        return (f"only a .py under {self.a.source_root} or a test_*.py / *_test.py under {self.a.test_root} may change: "
                "config, data and other files are a person's")

    def gates(self, files, tests):
        """RFC-0010 §9: repro (the tests fail on the base, pass with the fix) and related (no failure the base
        did not have, in the tests related to the changed source files). Leaves the tree as the fix made it."""
        patch = os.path.join(self.run_dir, "fix.patch")
        _, d = sh(["git", "-C", self.wt, "diff", "--cached", "--binary", "--no-renames", self.base], self.run_dir, self.env, 60, check=True)
        with open(patch, "w") as f:
            f.write(d)
        src = [x for x in files if not TEST_FILE.search(x)]
        t = self.a.test_timeout

        def tree(with_source):
            self.reset()
            args = ["git", "-C", self.wt, "apply", "--binary"]
            if not with_source:
                args += [f"--include={x}" for x in tests]
            sh(args + [patch], self.run_dir, self.env, 60, check=True)

        tree(with_source=False)
        base = self.tests(self.a.test_cmd, tests)
        base_src = [x for x in src if os.path.exists(os.path.join(self.wt, x))]
        base_rel = self.related(base_src, say=False)
        tree(with_source=True)
        fixed = self.tests(self.a.test_cmd, tests)
        rel = self.related(src, say=True)
        self.unloaded_on_base = sorted(base.unloaded)
        for x in tests:
            if x not in base.unloaded and not any(f.startswith(x + "::") for f in base.fails):
                return f"repro: {x} passes on the base without the fix: it does not reproduce the bug"
        # RFC-0010 §9: "a repro test must load and fail on the base". A file that cannot load there shows no
        # behaviour; it is noted, and at least one other test must load and fail.
        if all(x in base.unloaded for x in tests):
            return (f"repro: no test loads and fails on the base ({', '.join(sorted(tests))} does not load there): "
                    "nothing shows the bug before the fix")
        if fixed.unloaded or fixed.fails:
            return f"the fix's own tests fail with the fix: {tail('; '.join(sorted(fixed.unloaded | fixed.fails)), 300)}"
        if self.a.related == "pytest-dirs" and self.rel_considered and not self.rel_targets:
            return (f"related: no changed source file under {self.a.source_root} maps to a test directory under "
                    f"{self.a.test_root} ({', '.join(self.mirror_hint(x) for x in self.rel_unmapped)}): no related test ran, "
                    "so nothing shows the change breaks none")
        new = sorted((rel.fails | rel.unloaded) - (base_rel.fails | base_rel.unloaded))
        if new:
            return f"{len(new)} new failures in the related tests: {tail('; '.join(new), 300)}"
        self.checked = f"{len(tests)} test files fail on the base and pass with the fix; {len(rel.ran)} related files, none newly failing"
        if self.rel_unmapped:
            self.checked += f"; no mirrored test directory for {', '.join(self.rel_unmapped)}"
        return ""

    def tests(self, cmd, files):
        return run_tests(self.wt, cmd, files, self.env, self.a.test_timeout, self.a.test_format)

    def mirror(self, src):
        """pytest-dirs: (the existing mirrored test paths, the changed files under the source root with none,
        the changed files under the source root). src/<root>/a/b.py -> <test-root>/a/; a module straight under
        the source root -> <test-root>/test_<module>.py; a non-.py file has no mirror."""
        root, troot = self.a.source_root.rstrip("/") + "/", self.a.test_root.rstrip("/")
        targets, unmapped, considered = set(), [], [x for x in src if x.startswith(root)]
        for x in considered:
            rel = x[len(root):]
            t = None
            if x.endswith(".py"):
                parent = os.path.dirname(rel)
                t = os.path.join(troot, parent) if parent else os.path.join(troot, "test_" + os.path.basename(rel))
            if t and os.path.exists(os.path.join(self.wt, t)):
                targets.add(t)
            else:
                unmapped.append(x)
        return sorted(t for t in targets if not any(t.startswith(o + "/") for o in targets)), unmapped, considered

    def mirror_path(self, x):
        """Where the tests of source file x belong: its mirror under --test-root ("" for a non-.py file)."""
        if not x.endswith(".py"):
            return ""
        rel = x[len(self.a.source_root.rstrip("/") + "/"):]
        parent = os.path.dirname(rel)
        troot = self.a.test_root.rstrip("/")
        return os.path.join(troot, parent) + "/" if parent else os.path.join(troot, "test_" + os.path.basename(rel))

    def mirror_hint(self, x):
        return f"{x}: its tests belong in {self.mirror_path(x)}" if self.mirror_path(x) else f"{x}: a non-.py file has no test mirror"

    def related(self, src, say):
        """The tests related to the changed source files. vitest: --related-cmd on them. pytest-dirs: --test-cmd
        on the mirrored test directories (see mirror); a file with no mirror is noted and left out, never
        widened to the whole suite, and when none maps the gate fails (gates). say: the final tree's call."""
        if not src:
            return Tests(set(), set(), set())
        if self.a.related != "pytest-dirs":
            return run_tests(self.wt, self.a.related_cmd, src, self.env, self.a.test_timeout, self.a.test_format)
        targets, unmapped, considered = self.mirror(src)
        if say:
            self.rel_targets, self.rel_unmapped, self.rel_considered = targets, unmapped, considered
            for x in unmapped:
                self.note("related", f"no mirrored test directory for {x}: skipped")
            if targets:
                self.note("related", ", ".join(targets))
        if not targets:
            return Tests(set(), set(), set())
        return self.tests(self.a.test_cmd, targets)

    def stage(self):
        """Stage the worktree. The trees --exclude-path leaves out of the sparse checkout are left out here too: a build
        the agent runs can write into them (#553 on 2026-11-17: `vite build` made thumbnails in static/images/), and a
        plain `git add -A` then fails on paths outside the sparse checkout, ending the case on git's error ($0.24)."""
        sh(["git", "-C", self.wt, "add", "-A", "--", "."] + [f":(exclude){x.strip('/')}" for x in self.a.exclude_path],
           self.run_dir, self.env, 60, check=True)

    def diff(self):
        self.stage()
        _, d = sh(["git", "-C", self.wt, "diff", "--cached", "--no-renames", self.base, "--", ".", ":(exclude).flotilla-suite.json"],
                  self.run_dir, self.env, 60, check=True)
        return d

    def review_prompt(self, diff, task="", summary=""):
        """The review's whole input: the issue as filed, the diff, and the files the fix touched as they are after it.
        With --review-tools none the reviewer judges from this alone, in one call: on 2026-11-15/16 a review explored
        the repo for 5 to 38 calls on glm-5.2, the fleet's dearest model (the research repo's projects/bugfix-cost, C4).
        pi takes the prompt as one argument, which Linux caps at 128 KiB (MAX_ARG_STRLEN): the parts are cut to fit
        REVIEW_PROMPT_MAX, the diff first, then the issue, then the files, each cut said where it is."""
        head = (f"Review this automated fix for {self.repo}#{self.n} ({self.issue.get('title', '')}).\n\n")
        frozen = ""
        repro_files = getattr(self, "repro_files", None)
        if repro_files:
            # #335 on 2026-11-18: the review spent itself on the proof and asked for a panel test the issue's own
            # criterion made impossible (the panel shows past events by design), and turned down a fix the
            # reference fix's tests pass (the advisor's review)
            frozen = (f"THE FROZEN PROOF: {', '.join(sorted(repro_files))} was written from the issue by a separate "
                      "agent before any fix, accepted by the chain's checks, fails on the base and passes with this fix. "
                      "It is not under review and the fixer could not change it: judge the fix against it and the issue. "
                      "A request for another test, or a test at another layer, is a note, never a request for changes. "
                      "That the fix is in the wrong place is a note too, naming the file the issue points at.\n\n")
        ask = ("\n\nGive a concise review: correctness risks, edge cases the fix misses, "
               + ("" if frozen else "test adequacy, ") + "and whether it fixes "
               "the issue as filed rather than hiding its symptom. Ask for changes only for a fault you can show from what "
               "is above, and name its class: " + ", ".join(kick_classes(bool(repro_files))) + ". A fix that does not meet an acceptance "
               "criterion the issue states, or that stops the error by silently dropping behaviour (skipping, swallowing, "
               "filtering it out), is such a fault: class hides-symptom; the issue's acceptance criteria are the ones in the issue "
               "as filed, not in the task. A fix that weakens or inverts an assertion of a test that is on main is class "
               "test-proves-nothing. Write anything else (what could not be "
               "checked from here, style, follow-ups) as notes and approve. End with one line: VERDICT: approve, or "
               "VERDICT: request-changes CLASS: <one of the classes>.")
        if self.a.review_tools == "none":
            ask = "\n\nYou have no tools: judge from what is above." + ask
        room = REVIEW_PROMPT_MAX - len((head + ask).encode())

        def cut(text, n):
            b = text.encode()
            return text if len(b) <= n else b[:max(n - 80, 0)].decode(errors="ignore") + f"\n[... cut: {len(b) - n + 80} more bytes]"

        parts = []
        head += frozen
        room -= len(frozen.encode())
        gates = getattr(self, "checked", "") or ""
        if gates:
            parts.append(("THE GATES (run by the chain on this diff): ", gates + "\n\n"))
            room -= len(gates.encode()) + 60
        self.changed_tests = changed_assertions(self.wt, self.base, self.run_dir, self.env)
        if self.changed_tests:
            text = "\n".join(f"{f}: " + " | ".join(lines[:8]) for f, lines in self.changed_tests)
            parts.append(("THE FIX REMOVES OR REWRITES ASSERTIONS OF TESTS THAT ARE ON MAIN (found by the chain): ",
                          cut(text, min(room, 3000)) + "\n\n"))
            room -= len(parts[-1][1].encode()) + 90
        parts.append(("DIFF:\n", cut(diff, min(room, 40000))))
        room -= len(parts[-1][1].encode()) + 20
        if task:
            parts.append(("\n\nTHE TASK THE FIX WAS GIVEN (the verdict step's analysis; it can be wrong: judge the fix against "
                          "the issue as filed above, not against this task):\n", cut(task, min(room, 6000))))
            room -= len(parts[-1][1].encode()) + 70
        if summary:
            parts.append(("\n\nTHE FIX AGENT'S OWN SUMMARY:\n", cut(summary, min(room, 3000))))
            room -= len(parts[-1][1].encode()) + 40
        body = self.issue.get("body") or ""
        parts.append(("\n\nTHE ISSUE AS FILED:\n", cut(body, min(room, 8000))))
        room -= len(parts[-1][1].encode()) + 30
        _, names = sh(["git", "-C", self.wt, "diff", "--cached", "--name-only", "--no-renames", "-z", "--diff-filter=d", self.base],
                      self.run_dir, self.env, 60)
        files = []
        for x in [n for n in names.split("\0") if n.strip() and not n.startswith(".flotilla-suite")]:
            if room < 200:
                files.append(f"=== {x} === [not included: no room left]")
                continue
            path = os.path.join(self.wt, x)
            # only a regular file, read no further than the room left: a fix that adds a symlink to the routine's key
            # file (/run/flotilla/model-*.key) must not put the key in a prompt the provider sees, and a FIFO or
            # /dev/zero must not block or fill the run (the ops repo's review of the series, 2026-11-16)
            inside = os.path.realpath(path).startswith(os.path.realpath(self.wt) + os.sep)
            if os.path.islink(path) or not inside or not os.path.isfile(path):
                files.append(f"=== {x} === [not included: not a regular file]")
                room -= len(x) + 60
                continue
            try:
                with open(path, "rb") as f:
                    text = f.read(max(room, 0) + 1).decode("utf-8", errors="replace")
            except OSError:
                continue
            piece = cut(text, room - len(x) - 20)
            files.append(f"=== {x} ===\n{piece}")
            room -= len(piece.encode()) + len(x) + 20
        if files:
            parts.append(("\n\nTHE CHANGED FILES, AFTER THE FIX:\n", "\n".join(files)))
        # the room goes diff first, but the reviewer reads the issue first and the task last: a one-call review anchors on
        # what it reads first, and on #553 each review deferred to a task the issue does not support (2026-11-16/17)
        order = ["THE ISSUE AS FILED", "THE GATES", "THE FIX REMOVES", "DIFF", "THE CHANGED FILES", "THE FIX AGENT", "THE TASK"]
        parts.sort(key=lambda p: next(i for i, k in enumerate(order) if p[0].lstrip("\n").startswith(k)))
        return head + "\n\n".join(a.lstrip("\n") + b.rstrip("\n") for a, b in parts) + ask

    def review(self, diff, task="", summary=""):
        prompt = self.review_prompt(diff, task, summary)
        # the review has always been sent with the worker's provider (self.provider), not the reviewer alias's own
        route = harness.route(self.cfg, self.a.reviewer_model, {"pi_provider": self.provider})
        req = harness.RoleRequest(role="review", prompt=prompt, rules_path=self.env.get("BUGFIX_PI_RULES") or None, model=route,
                                  tools="none" if self.a.review_tools == "none" else "read", readonly=True, resume=None,
                                  max_turns=0, workdir=self.wt, run_dir=self.run_dir, timeout=self.a.review_timeout)
        ep, code, _ = harness.execute(harness.get("pi"), req, self.env, run=sh)
        self.calls.record("reviewer", self.a.reviewer_model)
        if ep.answer:
            return ep.answer
        return f"(no review: {tail(ep.error, 300)})" if ep.error else f"(no review: the reviewer episode ended {code} with no text)"

    def run(self):
        self.kept = None   # the last gate-clean round the review sent back: (rnd, diff, review, checked, unloaded)
        try:
            return self.rounds()
        except (Stop, OSError, subprocess.SubprocessError, ValueError) as e:
            if not self.kept:
                raise
            # a good fix is not thrown away because a later round failed (a gate, the verdict, or an error a
            # round raised): the operator gets the last clean one with the review's requested changes in it,
            # and the reason the later rounds ended
            rnd, d, review, checked, unloaded = self.kept
            where = ""
            if not isinstance(e, Stop):   # an error a round raised may be a bug: keep where it came from
                where = " | " + tail(" | ".join(traceback.format_exc().strip().splitlines()[-20:]), 1500)
            self.note("kept", f"round {rnd}'s clean fix; the run ended ({type(e).__name__}): {tail(str(e), 300)}{where}")
            return self.plan(rnd, d, review, checked, unloaded,
                             unresolved="the later rounds did not resolve what the review asked for")

    def read_saved(self, rel):
        with open(os.path.join(self.run_dir, rel)) as f:
            return f.read()

    def rounds(self):
        ck = self.restored
        if ck:
            phase = ck["phase"]
            self.note("resumed from checkpoint", self.resumed_from)
        else:
            self.checkout()
            # 0 · step one (RFC-0013 §1), advisory: what the base has of what the error calls missing. It never closes a
            # case (operator, 2026-11-17): its close rules closed real bugs, and dev-branch errors are left out upstream
            if self.a.step_one:
                try:
                    _, facts = stepone.check((self.issue.get("title") or "") + "\n\n" + (self.issue.get("body") or ""),
                                             self.wt, self.base)
                except Exception as e:   # advisory: a fault in it must not end the case
                    facts = f"not checked ({type(e).__name__})"
                self.note("step one", facts.removeprefix("step one (advisory): "))
            phase = "checkout"
            self.save("checkout", None)
        self.last_session, self.last_patch = None, ""
        start, after, cont, saved_review, gated = 0, None, None, None, None
        if phase == "checkout":
            # 1 · the verdict, on the base
            v, vpath = self.verdict()
            self.note("verdict", v.get("verdict", "none"))
            if v.get("verdict") != "actionable":
                raise Stop(self.not_actionable(v))
            manifest = os.path.join(self.triaged, f"{self.tid}-triaged.json")
            # 2 · the reproducing test, by its own agent, before any fix (--no-repro: the fixer writes its own, as
            # before). A run killed here does it again: the checkpoint is still "checkout"; every later one carries it
            if self.a.repro:
                if not os.path.exists(manifest):
                    raise Stop("stuck: the verdict step wrote no triaged manifest")
                self.repro(manifest)
            pins, prior, reviewed_back, resume = [], vpath, 0, None
        else:
            st = ck["state"]
            rp = st.get("repro")
            if rp:   # the frozen proof this case's fix rounds were held to
                self.repro_patch, self.repro_files = self.read_saved(rp["patch"]), dict(rp["files"])
            start, pins, reviewed_back = st["round"], list(st["pins"]), st["reviewed_back"]
            prior, manifest = os.path.join(self.run_dir, st["prior"]), os.path.join(self.run_dir, st["manifest"])
            r = st["resume"]
            resume = (os.path.join(self.run_dir, r["session"]), self.read_saved(r["patch"]), r["feedback"]) if r else None
            k = st["kept"]
            self.kept = (k["round"], k["diff"], k["review"], k["checked"], k["unloaded"]) if k else None
            if phase == "attempt" and self.in_flight:
                cont = {"session": self.in_flight, "patch": None}
            elif phase == "fixed":
                x = ck["fixed"]
                cont = {"session": os.path.join(self.run_dir, x["session"]) if x.get("session") else None,
                        "patch": self.read_saved(x["patch"]), "stream": self.read_saved(x["stream"])}
            elif phase in ("gated", "reviewed"):
                gated = ck["gated"]
                after = (gated["reason"], gated["files"], self.read_saved(gated["stream"]), gated["turns"], gated["out_tok"])
                self.last_session = os.path.join(self.run_dir, gated["session"]) if gated.get("session") else None
                self.last_patch = self.read_saved(gated["patch"])
                if not gated["reason"]:   # the tree as the gates left it: put back, a verdict step may have reset it since
                    self.reset()
                    ok, out = self.apply(self.last_patch)
                    if not ok:
                        raise Stop(f"the checkpoint's clean fix does not apply to the base: {tail(out, 200)}")
                if phase == "reviewed":
                    saved_review = ck["review"]

        def snap(phase, **extra):
            self.save(phase, {"round": rnd, "pins": pins, "prior": prior, "manifest": manifest,
                              "reviewed_back": reviewed_back, "resume": resume}, **extra)
        self.snap = snap
        for rnd in range(start, self.a.kickback_max + 1):
            if not os.path.exists(manifest):
                raise Stop(f"stuck: the verdict step wrote no triaged manifest for round {rnd}")
            if after is None:
                if cont is None:   # a continued attempt keeps the checkpoint it was continued from, until its own fix is saved
                    snap("attempt", attempt={"before": {os.path.relpath(f, self.run_dir): n for f, n in self.sessions().items()}})
                reason, files, stream, (turns, out_tok) = (self.attempt(manifest, resume, cont=cont) if cont
                                                           else self.attempt(manifest, resume))
                cont, resume = None, None
                gated = {"reason": reason, "files": files, "turns": turns, "out_tok": out_tok, "stream": "checkpoint.stream",
                         "patch": "checkpoint.fix.patch",
                         "session": os.path.relpath(self.last_session, self.run_dir) if self.last_session else None}
                if self.fixed_ok:
                    snap("gated", gated=gated)
            else:
                reason, files, stream, turns, out_tok = after
                after, resume = None, None
            from_review = False
            if not reason:
                d = self.diff()
                if len(d) > DIFF_MAX:
                    raise Stop(f"the clean fix's diff is {len(d)} characters, more than the {DIFF_MAX} a plan may carry")
                with open(manifest) as mf:
                    task = json.load(mf).get("description") or ""
                if saved_review is not None:
                    review, saved_review = saved_review, None   # its note is in the log the checkpoint kept
                else:
                    review = self.review(d, task, assistant_text(stream or ""))
                    self.note("review", tail(review.splitlines()[-1] if review else "", 80))
                    snap("reviewed", gated=gated, review=review)
                if review.startswith("(no review"):   # a failed review is not an approval: the plan says so
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review, unresolved="the review did not run: " + tail(review, 200))
                if not any(VERDICT_LINE.match(ln) for ln in review.splitlines()):
                    # text without a verdict (a cut-off or timed-out review) is not an approval either
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review, unresolved="the review gave no verdict: " + tail(" ".join(review.split()), 200))
                # the review saw what the gates did not on 3 of phase A's 4 plans: request-changes sends the fix
                # back while the budget lasts; after that the operator sees the plan with the review in it
                if review_requests_changes(review) and review_class(review) == "wrong-place" and self.repro_files:
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    self.note("review note", "the review says the fix is in the wrong place; with the frozen proof passing "
                                             "that is a note for the operator, not a kick-back (see the review below)")
                    return self.plan(rnd, d, review)
                if review_requests_changes(review) and review_class(review) not in KICK_CLASSES:
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review, unresolved=(
                        f"the review asked for changes it cannot show from the diff (class {review_class(review) or 'none'}), "
                        "so the fix was not sent back"))
                if review_requests_changes(review) and reviewed_back < self.a.review_kickback_max \
                        and rnd < self.a.kickback_max:
                    reviewed_back += 1
                    self.kept = (rnd, d, review, self.checked, self.unloaded_on_base)
                    from_review = True
                    # one line: the reason goes into result.tsv and the chain's log; the fix step gets the whole review
                    reason = "the review requested changes: " + tail(" ".join(review.split()), 400)
                    self.note(f"round {rnd}", reason)
                    manifest = self.review_manifest(manifest, rnd, d, review)
                    if self.a.resume and self.last_session:
                        resume = (self.last_session, self.last_patch,
                                  f"A REVIEW OF YOUR FIX ASKED FOR CHANGES. Address what it says that holds; if a point is "
                                  f"wrong, keep your fix and say why in the test or a comment.\n\nTHE REVIEW:\n{review[:12000]}")
                    continue
                else:
                    self.note(f"round {rnd}", f"gates clean ({len(files)} files, {turns} turns, {out_tok} output tokens): {self.checked}")
                    return self.plan(rnd, d, review, unresolved=(
                        "the review's kick-back budget was spent, so nothing changed after it"
                        if review_requests_changes(review) else ""))
            self.note(f"round {rnd}", reason)
            if rnd == self.a.kickback_max:
                raise Stop(f"stuck: kick-back limit ({self.a.kickback_max}) reached; last: {reason}")
            # the kick-back evidence, as chain.go writes it for the leaves
            ev = os.path.join(self.root, "results", f"{self.tid}-r{rnd}")
            os.makedirs(ev, exist_ok=True)
            touched = [x for x in files if not TEST_FILE.search(x)]
            with open(os.path.join(ev, "touched.txt"), "w") as f:
                f.write("".join(x + "\n" for x in touched))
            with open(os.path.join(ev, "harness.json"), "w") as f:
                f.write(stream or "")
            with open(os.path.join(ev, "result.tsv"), "w") as f:
                f.write(f"{self.tid}\tpi\tFAIL ({reason})\t0\t0\t{turns}\t{out_tok}\tno\t0\n")
            gap = next((p for p in pins if p in touched), "")
            if gap:
                self.note("acceptance gap", f"fixed at the pin {gap} and still failing")
            elif self.a.advisor and not from_review:   # a review's objection is its own evidence: no pin needed
                code, out = sh([os.path.join(BAKEOFF, "advisor_run.sh"), os.path.join(self.run_dir, "ticket.json"), ev],
                               BAKEOFF, dict(self.env, PI_TOOLS=READ_ONLY_TOOLS), self.a.episode_timeout)
                self.calls.record("advisor", self.a.advisor_model)
                try:
                    with open(os.path.join(ev, "advisor.json")) as f:
                        adv = json.load(f)
                    pin, lost = adv.get("file") or "", str(adv.get("not_locatable") or "")
                except (OSError, ValueError, AttributeError):
                    pin, lost = "", ""
                self.note("advisor", pin or (f"not locatable: {lost[:200]}" if lost else f"no usable pin (exit {code})"))
                if pin:
                    pins.append(pin)
                elif lost:
                    # a fix failed and the advisor cannot find where the fault is: another verdict and round cost
                    # more and on #553 ended in needs-info anyway ($0.52 and $0.88, 2026-11-17)
                    raise Stop(f"needs info from the reporter after round {rnd}: the advisor could not locate the fault "
                               f"in this repository: {lost[:400]}")
            kv, kpath = self.verdict(ev, prior, bool(gap))
            if gap and kv.get("verdict") == "actionable":  # acceptance mode may not re-pin (chain.go's downgrade)
                kv = dict(kv, verdict="needs-info")
                self.note("downgrade", "an acceptance gap's verdict came back actionable: needs-info")
            if kv.get("verdict") != "actionable":
                raise Stop(self.not_actionable(kv, after=f"after round {rnd}"))
            prescribed = test_change_criteria(kv)
            if prescribed:
                raise Stop(f"after round {rnd} the verdict's acceptance criteria prescribe changing a test, which the issue "
                           f"does not: {'; '.join(prescribed)[:400]}")
            manifest = os.path.join(self.triaged, f"{self.tid}-triaged-kb{self.kb_round}.json")
            prior = kpath
            if self.a.resume and self.last_session and os.path.exists(manifest):
                try:
                    with open(manifest) as f:
                        task = json.load(f).get("description") or ""
                    resume = (self.last_session, self.last_patch,
                              f"YOUR FIX DID NOT PASS ITS GATES: {reason}\n\nTHE TASK, AS THE VERDICT STEP NOW SEES IT:\n{task}")
                except (OSError, ValueError, AttributeError) as e:   # the next round starts a new session instead
                    self.note("resume", f"the renewed task could not be read ({type(e).__name__}): a new session")

    def review_manifest(self, manifest, rnd, d, review):
        """The next fix's instructions after a review's request-changes: the same task, the whole review and the
        diff it judged (the next attempt starts from the base again). No new verdict: it re-ran ~30 calls of
        exploration to re-judge actionability, which the review did not dispute, and the review reached the fix
        step only as 400 characters of result.tsv through it (the research repo's projects/bugfix-cost, the advisor's
        review: a likely reason #553's later rounds changed nothing)."""
        with open(manifest) as f:
            m = json.load(f)
        m["description"] = (m.get("description") or "") + (
            f"\n\nA REVIEW OF YOUR PREVIOUS ATTEMPT (round {rnd}) ASKED FOR CHANGES. Address what it says that holds; "
            f"if a point is wrong, keep your fix and say why in the test or a comment.\n\nTHE REVIEW:\n{review[:12000]}"
            f"\n\nTHE DIFF IT REVIEWED (the worktree is back at the base):\n{d[:16000]}")
        out = os.path.join(self.triaged, f"{self.tid}-triaged-review{rnd}.json")
        with open(out, "w") as f:
            json.dump(m, f)
        return out

    def not_actionable(self, v, after=""):
        verdict = v.get("verdict") or "none"
        missing = v.get("missing") or v.get("Missing") or []
        why = "; ".join(str(m) for m in missing) if isinstance(missing, list) else str(missing)
        head = {"needs-info": "needs info from the reporter", "reject": "rejected as not a bug",
                "rejected": "rejected as not a bug"}.get(verdict, f"no usable verdict ({verdict})")
        return " ".join(x for x in [head, after, (": " + why) if why else ""] if x).replace(" :", ":")

    def plan(self, rnd, d, review, checked=None, unloaded=None, unresolved=""):
        checked = self.checked if checked is None else checked
        unloaded = self.unloaded_on_base if unloaded is None else unloaded
        title = f"fix #{self.n}: {self.issue.get('title', '')}"[:TITLE_MAX]
        flag = (f"unresolved: the review did not run; round {rnd}'s clean fix kept"
                if unresolved.startswith("the review did not run") else
                f"unresolved: the review gave no verdict; round {rnd}'s clean fix kept"
                if unresolved.startswith("the review gave no verdict") else
                f"unresolved: the review raised points it cannot show from the diff; round {rnd}'s clean fix kept"
                if unresolved.startswith("the review asked for changes it cannot show") else
                f"unresolved: the review asked for changes; round {rnd}'s clean fix kept")
        head = f"**{flag}**: {unresolved} (see the review below).\n\n" if unresolved else ""
        body = "\n".join([
            f"An automated fix for {self.repo}#{self.n} by flotilla's bugfix fleet (RFC-0010), in beta evaluation.",
            "", f"Gates (base {self.base[:12]}, round {rnd}): {checked}; nothing in CI, hooks, config or the lockfile. "
            "The whole suite is this PR's CI.",
            *([f"Not loadable on the base (they import what the fix adds): {', '.join(unloaded)}."]
              if unloaded else []),
            *([f"Assertions of tests on main removed or rewritten by the fix: "
               + "; ".join(f"{f} ({len(lines)})" for f, lines in getattr(self, "changed_tests", []))]
              if getattr(self, "changed_tests", None) else []),
            "", "The chain:", *[f"- {x}" for x in self.log],
            "", "Review (fleet reviewer, comment only):", tail(review, 3000),
            "", "The operator merges, always: the fleet never does."])
        return {"outcome": "plan", "action": "github.open_pr",
                "detail": f"#{self.n}: " + (f"{flag}; " if unresolved else "") + f"a clean fix in round {rnd} at {self.base[:12]}"
                          + (f" (resumed from a checkpoint: {self.resumed_from})" if self.resumed_from else ""),
                # the flag goes on after the cut: tail() keeps the end, and it must never cut the flag away
                "params": {"repo": self.repo, "base": self.base, "title": title,
                           "body": head + tail(body, BODY_MAX - len(head)), "diff": d}}


def parse(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--worker-model", required=True, help="an alias of FLOTILLA_MODELS")
    ap.add_argument("--advisor-model", required=True)
    ap.add_argument("--reviewer-model", required=True)
    ap.add_argument("--setup", default="npm ci --no-audit --no-fund && (npm run --if-present i18n:compile)")
    ap.add_argument("--test-cmd", default="npx vitest run --reporter=json --outputFile=%s", help="%%s: the report; the files follow")
    ap.add_argument("--related-cmd", default="npx vitest related --run --reporter=json --outputFile=%s")
    ap.add_argument("--test-format", choices=("vitest", "pytest"), default="vitest",
                    help="the shape of the report --test-cmd writes: vitest's JSON, or pytest-json-report's")
    ap.add_argument("--related", choices=("vitest", "pytest-dirs"), default="vitest",
                    help="vitest: --related-cmd on the changed source files; pytest-dirs: --test-cmd on the test "
                         "directories that mirror them (--source-root, --test-root)")
    ap.add_argument("--source-root", default="", help="pytest-dirs: the source tree the mirror starts at, e.g. src/pkg")
    ap.add_argument("--test-root", default="", help="pytest-dirs: the test tree that mirrors it, e.g. tests/unit/pkg")
    ap.add_argument("--out-of-scope", action="append", default=[], help="a regex of paths a fix may not touch")
    ap.add_argument("--base", default="", help="the commit to fix (default: main), for replays (phase B)")
    ap.add_argument("--harness", default="pi", help="harnesses/<name>.sh (phase B arms)")
    ap.add_argument("--exclude-path", action="append", default=[], help="a tree of the repo not to check out (sparse)")
    ap.add_argument("--blob-limit", default="256k", help="blobs up to this size come with the clone (git --filter=blob:limit)")
    ap.add_argument("--upstream", action="append", default=[], metavar="LITELLM_MODEL=OPENROUTER_MODEL",
                    help="the model LiteLLM routes an alias's model to, for pi's catalog (pi_config)")
    # a kick-back roughly doubles a case's cost (a new fix session, and before it a new verdict): 2 and 1 bound it
    ap.add_argument("--min-output", type=int, default=32768,
                    help="a floor for each model's reply limit pi is told (its catalog gives flash 4096, which reasoning used up before the answer); 0 keeps the catalog's")
    ap.add_argument("--no-repro", dest="repro", action="store_false",
                    help="no separate reproducer: the fixer writes its own failing test, as before 2026-11-18 (for comparing "
                         "the two in an eval). Default: a reproducer agent writes the failing test from the issue's evidence "
                         "before any fix, and the test is frozen for every fix round")
    ap.add_argument("--no-step-one", dest="step_one", action="store_false",
                    help="skip RFC-0013 §1's advisory step one (bugfix/stepone.py), which notes on the chain what the base "
                         "has of what the error calls missing; it never closes a case")
    ap.add_argument("--no-resume", dest="resume", action="store_false",
                    help="a kick-back starts a new fix session (before C10) instead of resuming the previous round's")
    ap.add_argument("--kickback-max", type=int, default=2)
    ap.add_argument("--review-kickback-max", type=int, default=1,
                    help="times the review's request-changes sends a gate-clean fix back (within --kickback-max)")
    ap.add_argument("--no-advisor", dest="advisor", action="store_false")
    ap.add_argument("--episode-timeout", type=int, default=1200)
    ap.add_argument("--repro-max-turns", type=int, default=40,
                    help="stop the reproducer after this many assistant turns (0: no cap). On 2026-11-18 a #553 reproducer "
                         "explored 90 turns ($0.15) and wrote nothing; the #335 ones wrote their test in fewer")
    ap.add_argument("--fix-max-turns", type=int, default=90,
                    help="stop a fix episode after this many assistant turns (0: no cap). Of the eval runs of 2026-11-16/17 "
                         "every round whose fix passed the oracle took at most 67 turns; #553's round 0s took 105 to 194 "
                         "($0.14-0.36 each) and none gave a right fix")
    ap.add_argument("--test-timeout", type=int, default=900)
    ap.add_argument("--setup-timeout", type=int, default=1200)
    ap.add_argument("--review-timeout", type=int, default=600)
    ap.add_argument("--review-tools", choices=("none", "read"), default="none",
                    help="none: the reviewer judges from the issue, the diff and the changed files it is given, in one "
                         "call; read: it also gets pi's read tools and explores (the behaviour before C4)")
    ap.add_argument("--scratch", default=os.environ.get("BUGFIX_SCRATCH", "/scratch"))
    ap.add_argument("--pi-rules", default=None,
                    help="the rule file every pi step gets instead of the repo's AGENTS.md (--no-context-files); '' keeps "
                         "pi's default of loading the repo's AGENTS.md. Not given: the spec's declared `rules` prompt "
                         "(FLOTILLA_PROMPTS, hash-checked), else bugfix/fleet_rules.md")
    ap.add_argument("--spend-file", default="", help="the routine's spend_file, relative to HOME: the run writes its cost "
                    "there (the gateway's bill for its keys; bugfix/spend.py)")
    ap.add_argument("--spend-poll", type=float, default=5.0, help="seconds between the spend reads after the run")
    ap.add_argument("--spend-settle", type=float, default=60.0, help="most seconds to wait for the spend to settle")
    ap.add_argument("--spend-stable", type=float, default=20.0,
                    help="seconds the spend must hold still to count as settled (LiteLLM records calls late)")
    ap.add_argument("--lock-wait", type=float, default=LOCK_WAIT_S,
                    help="seconds to wait for another run of the same case to let go of its directory before ending in error")
    a = ap.parse_args(argv)
    if (a.related == "pytest-dirs" or a.test_format == "pytest") and not (a.source_root and a.test_root):
        ap.error("--related pytest-dirs and --test-format pytest need --source-root and --test-root (the scope rules use them)")
    return a


def pi_rules(a, environ):
    """The rule file for pi: --pi-rules when given; else the spec's declared `rules` prompt, which must hash to
    what flotillad was loaded with (ADR-0018); else the fleet_rules.md beside this file, as before."""
    if a.pi_rules is not None:
        return a.pi_rules
    root = environ.get("FLOTILLA_CODE_ROOT") or os.path.dirname(HERE)
    fleet = (environ.get("FLOTILLA_ROUTINE") or "bugfix/fix").split("/")[0]
    return declared_prompts.resolve("rules", environ, root, fleet) or os.path.join(HERE, "fleet_rules.md")


def main(argv=None, environ=None):
    environ = dict(os.environ if environ is None else environ)
    a = parse(argv)
    case = json.loads(environ["FLOTILLA_CASE"])
    try:
        a.pi_rules = pi_rules(a, environ)
    except declared_prompts.PromptError as e:
        # fail closed, and end as a setup failure ends: an escalation with the reason
        out = {"outcome": "escalate", "detail": f"prompt: {e}"}
        with open(environ["FLOTILLA_PLAN_FILE"], "w") as f:
            json.dump(out, f)
        print(f"BUGFIX FIX case={case.get('id')} outcome=escalate {out['detail']}")
        return out
    run_dir = case_run_dir(a.scratch, case)
    # One run of a case at a time, for the whole run: a resume can start while the run it continues is still being
    # stopped. A run that cannot get the lock ends here as an error, having touched nothing.
    lock = lock_run_dir(run_dir, a.lock_wait)
    try:
        return run_case(a, environ, case, run_dir)
    finally:
        os.close(lock)


def _number(v):
    try:
        return max(float(v), 0.0)
    except (TypeError, ValueError):
        return 0.0


def run_case(a, environ, case, run_dir):
    sweep_stale(a.scratch)
    # ADR-0019: flotillad says whether this run may continue the case's last one (FLOTILLA_RESUME); the checkpoint
    # decides whether it can. Without the word, or without a checkpoint that holds, the case starts fresh.
    restored = None
    try:
        resumption = json.loads(environ.get("FLOTILLA_RESUME") or "null")
    except ValueError:
        resumption = None
    if isinstance(resumption, dict) and os.path.isdir(run_dir):
        restored, why = load_checkpoint(run_dir, case, a.repo, a.base or "main")
        if restored is None:
            print(f"BUGFIX FIX case={case.get('id')} resume of run {resumption.get('run_id')}: no usable checkpoint ({why}): starting fresh",
                  flush=True)
    if restored is None:
        clear_run_dir(a.scratch, run_dir)
    # a resumed run reports its own spend, not its earlier run's: the pi sessions already in the directory are the
    # baseline (taken here, so that a Chain that cannot even start does not count them again). What the interrupted
    # run did not get reported (flotillad's restart reads no spend) is carried onto this run's: pi's figure for those
    # sessions less what flotillad recorded for the run (its spend_usd in FLOTILLA_RESUME), never below 0.
    baseline = spend.pi_cost(os.path.join(run_dir, "pi")) if restored else 0.0
    carry = max(baseline - _number((resumption or {}).get("spend_usd")), 0.0) if restored else 0.0
    chain = None
    meter = None
    terminated = []
    finished = False

    def on_term(signum, frame):
        terminated.append(signum)
        raise Terminated()

    # before the first spend reads: a SIGTERM during them still leaves a number (no model has run: 0)
    old_term = signal.signal(signal.SIGTERM, on_term)
    if a.spend_file:
        try:
            cfg = json.loads(environ.get("FLOTILLA_MODELS") or "{}")
        except ValueError:
            cfg = {}
        try:
            meter = spend.Meter(spend.keys_of(cfg, {a.worker_model, a.advisor_model, a.reviewer_model}, environ),
                                a.spend_poll, a.spend_settle, stable_s=a.spend_stable)
        except Terminated:
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            spend.write(os.path.join(environ.get("HOME", "/"), a.spend_file), 0.0)
            if restored is None:   # a run that was to continue one leaves what it was to continue
                remove_run_dir(a.scratch, run_dir)
            raise
    try:
        chain = Chain(a, case, run_dir, environ, meter, restored)
        out = chain.run()
        finished = True
    except Stop as e:
        out = {"outcome": "escalate", "detail": str(e)}
        finished = True
    except pi_config.ConfigError as e:
        out = {"outcome": "escalate", "detail": f"config: {e}"}
        finished = True
    finally:
        # from here a SIGTERM (the worker's, at the routine's timeout; it SIGKILLs 10 s later) must not cut the report
        # short: it only hurries the spend read, here or already under way
        def on_late_term(signum, frame):
            terminated.append(signum)
            if meter is not None:
                meter.hurry()

        signal.signal(signal.SIGTERM, on_late_term)
        if terminated and meter is not None:
            meter.hurry()
        if meter is not None:   # before the run directory, which holds pi's sessions, is deleted
            # pi's figure first, so a SIGKILL during the gateway read still leaves a number; then the bill over it
            spend.write(os.path.join(environ.get("HOME", "/"), a.spend_file), spend.pi_cost(os.path.join(run_dir, "pi"), baseline) + carry)
            # every episode's settled spend is already known when the chain measured them all: nothing to wait for again
            quiet = chain is not None and chain.calls.measures and not terminated
            usd, source, why = meter.cost(os.path.join(run_dir, "pi"), quiet=quiet, baseline=baseline)
            usd += carry
            spend.write(os.path.join(environ.get("HOME", "/"), a.spend_file), usd)
            print(f"BUGFIX SPEND case={case.get('id')} usd={usd:.6f} source={source}" + (f" ({why})" if why else "") +
                  (f" carried={carry:.6f} of the interrupted run" if carry else ""))
        if chain is not None:
            chain.restore_config("the run")
        # A run cut short by a crash, a restart or an error leaves its directory and checkpoint for the case's next run
        # (ADR-0019), unless there is no checkpoint to continue or its own timeout ended it (status timeout, which the
        # routine does not resume): the directory holds a worktree and a venv, gigabytes on shop. A cancel's SIGTERM
        # cannot be told from a restart's, so it keeps the directory until the 24 h sweep.
        deadline = _number(environ.get("FLOTILLA_RUN_DEADLINE"))
        timed_out = bool(terminated) and deadline > 0 and time.time() >= deadline - 5
        if finished or timed_out or not os.path.exists(os.path.join(run_dir, CHECKPOINT)):
            remove_run_dir(a.scratch, run_dir)
        if chain is not None and getattr(chain, "cache", None):
            sh(["git", "-C", chain.cache, "worktree", "prune"], a.scratch, environ, 60)
        if not terminated:
            signal.signal(signal.SIGTERM, old_term)
    with open(environ["FLOTILLA_PLAN_FILE"], "w") as f:
        json.dump(out, f)
    print(f"BUGFIX FIX case={case.get('id')} outcome={out['outcome']} {out.get('detail', '')}")
    return out


if __name__ == "__main__":
    try:
        main()
    except RunBusy as e:
        print(f"BUGFIX FIX {e}", flush=True)
        sys.exit(EXIT_BUSY)
