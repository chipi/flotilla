"""python3 bugfix/test_fix.py: bugfix/fix's chain (RFC-0010 §2) on fake leaves, a fake pi, a fake suite
and a local git remote. The models are replaced, not the flow: the verdicts and fixes each leaf gives
are scripted per call, and the chain's steps, gates and limits are what is tested."""
import json
import re
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
import time
import textwrap
import time
import unittest
import unittest.mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fix  # noqa: E402

MODELS = {"endpoints": {"gateway-bugfix": {"base_url": "http://127.0.0.1:9/v1", "kind": "litellm", "key_file": "/dev/null"}},
          "aliases": {"bugfix-flash": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-flash"},
                      "bugfix-reviewer": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-reviewer"}}}

# Each fake leaf pops its next scripted answer from a file in FAKE_STATE and appends what it was called with.
POP = textwrap.dedent('''\
    pop() { f="$FAKE_STATE/$1"; head -n1 "$f"; tail -n +2 "$f" > "$f.n"; mv "$f.n" "$f"; }
    ''')

# with FAKE_SESS set, a leaf saves a pi session of one assistant message that called a tool, as pi does
SESS = textwrap.dedent('''\
    sess() { [ -n "${FAKE_SESS:-}" ] || return 0; mkdir -p "$PI_CODING_AGENT_DIR/sessions/--wt--"
      printf '{"type":"session","version":3,"id":"sess-%s"}\\n{"type":"message","message":{"role":"assistant","model":"m","usage":{"input":10,"output":2,"cost":{"total":0.5}},"stopReason":"toolUse","content":[{"type":"toolCall","name":"read"}]}}\\n' "$1-$RANDOM" > "$PI_CODING_AGENT_DIR/sessions/--wt--/$1-$RANDOM.jsonl"; }
    ''')

TRIAGE = POP + SESS + textwrap.dedent('''\
    sess verdict
    BUG_JSON="$1"; EV="${3:-}"; PRIOR="${4:-}"
    ID=$(jq -r .id "$BUG_JSON")
    V=$(pop verdicts)
    echo "verdict ${BUGFIX_PI_RULES:-none}" >> "$FAKE_STATE/rules"
    IMG=no; [ -e "$BAKEOFF_WT/static/images/x.png" ] && IMG=yes
    echo "triage $V ev=${EV:+yes} gap=$ACCEPTANCE_GAP model=$TRIAGE_MODEL provider=$PI_PROVIDER img=$IMG tools=${PI_TOOLS:-all} rows=$( [ -n "$EV" ] && wc -l < "$EV/result.tsv" | tr -d " " || echo 0)" >> "$FAKE_STATE/calls"
    KB=0; [ -n "$EV" ] && KB=$(( $(jq -r '.kickback_round // 0' "$PRIOR") + 1 ))
    SUF=""; [ "$KB" -gt 0 ] && SUF="-kb$KB"
    OUT="$BAKEOFF_ROOT/results/${ID}-triage${SUF}/pi"; mkdir -p "$OUT"
    # "missing" is for needs-info (agents/triage.md); actionable-missing is the shape that contract forbids
    MISSING='["the expected value"]'; [ "$V" = actionable ] && MISSING='[]'
    [ "$V" = actionable-missing ] && V=actionable-x
    printf '{"verdict": "%s", "kickback_round": %s, "missing": %s, "problem": %s}' "${V%-x}" "$KB" "$MISSING" \
      "$( [ -f "$FAKE_STATE/acceptance" ] && cat "$FAKE_STATE/acceptance" || echo '{}')" > "$OUT/triage.json"
    V=${V%-x}
    if [ "$V" = actionable ]; then
      mkdir -p "$BAKEOFF_TRIAGED_DIR"
      printf '{"id": "%s", "description": "make value() return 2"}' "$ID" > "$BAKEOFF_TRIAGED_DIR/${ID}-triaged${SUF}.json"
    fi
    git -C "$BAKEOFF_WT" reset -q --hard "$TRIAGE_BASE"
    ''')

PI_HARNESS = POP + SESS + textwrap.dedent('''\
    sess coder
    WT="$1"; F=$(pop fixes)
    printf '%s' "$2" > "$FAKE_STATE/fix_desc"
    # pi's saved session: a new one per fresh run, appended to on a resume (what the chain resumes is logged)
    if [ -n "${PI_RESUME_SESSION:-}" ]; then
      echo "resume $(basename "$PI_RESUME_SESSION") changed=$(git -C "$WT" status --porcelain | wc -l | tr -d ' ') desc=$(printf '%s' "$2" | head -c 34)" >> "$FAKE_STATE/resumes"
      echo '{}' >> "$PI_RESUME_SESSION"
    else
      mkdir -p "$PI_CODING_AGENT_DIR/sessions/--wt--"; echo '{}' > "$PI_CODING_AGENT_DIR/sessions/--wt--/s-$F-$RANDOM.jsonl"
    fi
    echo "fix ${BUGFIX_PI_RULES:-none}" >> "$FAKE_STATE/rules"
    echo "fix $F model=$PI_MODEL tools=${PI_TOOLS:-all}" >> "$FAKE_STATE/calls"
    cd "$WT"
    case "$F" in
      good)     echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js ;;
      srconly)  echo 'module.exports = () => 2;' > src/a.js ;;
      srcplus)  echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/b.js 1' > src/b.test.js ;;
      wrongsrc) echo 'module.exports = () => 3;' > src/a.js ;;
      hang)     echo $$ > "$FAKE_STATE/harness.pid"; sleep 60 ;;
      notest)   echo 'module.exports = () => 2;' > src/a.js ;;
      chatty)   for i in 1 2 3 4 5; do echo '{"type": "message_end", "message": {"role": "assistant", "usage": {"output": 1}}}'; done
                sleep 60 ;;
      srctext)  echo 'module.exports = () => 2;' > src/a.js
                printf "const p = 'src/a.js';\nconst t = readFileSync(p, 'utf-8');\nexpect src/a.js 2\n" > src/a.test.js ;;
      built)    echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js
                mkdir -p static/images; echo thumb > static/images/x.thumb.jpg ;;
      flip)     echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js
                echo '// expect(value()).toBeNull()' > guard.test.js ;;
      breaks)   echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js; echo 'module.exports = () => 2;' > src/b.js ;;
      norepro)  echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/b.js 1' > src/a.test.js ;;
      halffix)  echo 'module.exports = () => 3;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js ;;
      newmod)   echo 'module.exports = () => 3;' > src/c.js; printf 'import src/c.js\nexpect src/c.js 3\n' > src/c.test.js ;;
      goodnewmod) echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js
                echo 'module.exports = () => 3;' > src/c.js; printf 'import src/c.js\nexpect src/c.js 3\n' > src/c.test.js ;;
      lockfile) echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js; echo '{}' > package-lock.json ;;
      workflow) echo 'expect src/a.js 2' > src/a.test.js; mkdir -p .github/workflows; echo on: push > .github/workflows/x.yml ;;
      pygood)   echo 'def value(): return 2' > src/api/pkg/mod.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pybreaks) echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'def value(): return 2' > src/api/pkg/other.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pylone)   echo 'def value(): return 2' > src/api/lone/z.py
                echo 'expect src/api/lone/z.py 2' > tests/unit/api/test_z.py ;;
      pynested) echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'def value(): return 1  # x' > src/api/pkg/sub/s.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pypartial) echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'def value(): return 1  # x' > src/api/lone/z.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyinit)   echo 'def value(): return 2' > src/api/__init__.py
                echo 'expect src/api/__init__.py 2' > tests/unit/api/test_i.py ;;
      pyconftest) echo 'def value(): return 2' > src/api/pkg/mod.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py; echo 'import os' > tests/unit/api/pkg/conftest.py ;;
      pyconftestonly) echo 'import os' > tests/unit/api/pkg/conftest.py ;;
      pyjson)   echo 'def value(): return 2' > src/api/pkg/mod.py; mkdir -p tests/unit/api/pkg/data; echo '{}' > tests/unit/api/pkg/data/x.json
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyfixture) echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'F = 1' > src/api/pkg/newf.py
                printf 'fixture src/api/pkg/newf.py\\nexpect src/api/pkg/mod.py 2\\n' > tests/unit/api/pkg/test_mod.py ;;
      pyshadow) echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'x = 1' > json.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyinitfile) echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'import os' > tests/unit/api/pkg/__init__.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyhelper) echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'import os' > tests/unit/api/pkg/helpers.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pysuffix) echo 'def value(): return 2' > src/api/pkg/mod.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/mod_test.py ;;
      pyrename) echo 'def value(): return 2' > src/api/pkg/mod.py; git mv conftest.py src/api/pkg/moved.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyunicode) echo 'def value(): return 2' > src/api/pkg/mod.py; mkdir -p tests/unit/api/é; echo 'import os' > tests/unit/api/é/conftest.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyunicodesrc) echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'x' > 'src/api/pkg/é.txt'
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyconfigs) echo 'def value(): return 2' > src/api/pkg/mod.py; echo '[pytest]' > .pytest.ini; echo '[pytest]' > pytest.toml; echo '[pytest]' > .pytest.toml
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pydata)   echo 'def value(): return 2' > src/api/pkg/mod.py; echo '{}' > src/api/pkg/data.json; echo 'hi' > README.md
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyquote)  echo 'def value(): return 2' > src/api/pkg/mod.py; echo 'x = 1' > '"q".py'
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyrenamesrc) echo 'def value(): return 2' > src/api/pkg/mod.py; git mv src/api/lone/z.py src/api/pkg/z.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pydeltest) echo 'def value(): return 2' > src/api/pkg/mod.py; rm tests/unit/api/pkg/test_keep.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py ;;
      pyskip)   echo 'def value(): return 2' > src/api/pkg/mod.py; printf 'SKIP\\nexpect src/api/pkg/mod.py 2\\n' > tests/unit/api/pkg/test_mod.py ;;
      pyconf)   echo 'def value(): return 2' > src/api/pkg/mod.py
                echo 'expect src/api/pkg/mod.py 2' > tests/unit/api/pkg/test_mod.py; echo '[project]' > pyproject.toml ;;
      jsrename) echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js; git mv package.json src/moved.json ;;
      jsdeltest) echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js; rm keep.test.js ;;
      jsmovetest) echo 'module.exports = () => 2;' > src/a.js; echo 'expect src/a.js 2' > src/a.test.js; mkdir -p docs; git mv keep.test.js docs/keep.md ;;
      nothing)  ;;
    esac
    printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"output":7}}}\\n'
    ''')

# the reproducer (harnesses/repro.sh): pops a kind from $FAKE_STATE/repros, writes its test in the worktree and answers
# with a pi message_end carrying the JSON block. The issue (fixture) says "value() returns 1, want 2".
REPRO_HARNESS = textwrap.dedent('''\
    WT="$1"; K=$(head -n 1 "$FAKE_STATE/repros"); sed -i 1d "$FAKE_STATE/repros"
    echo "repro $K" >> "$FAKE_STATE/calls"
    printf '%s' "$2" > "$FAKE_STATE/repro_desc"
    python3 - "$WT" "$K" <<'PY'
    import json, os, sys
    wt, k = sys.argv[1], sys.argv[2]
    def w(p, t):
        open(os.path.join(wt, p), "w").write(t)
    ans = {"repro": "test", "files": ["src/a.test.js"], "evidence_quoted": "value() returns 1, want 2",
           "expect_failure_match": "returns 1, want 2"}
    if k == "good":
        w("src/a.test.js", "const a = require('./a.js')\\nmsg value() returns 1, want 2\\nexpect src/a.js 2\\n")
    elif k == "none":
        ans = {"repro": "none", "why": "a minified frame only", "needed": "a source map"}
    elif k == "src":
        w("src/a.test.js", "const a = require('./a.js')\\nmsg value() returns 1, want 2\\nexpect src/a.js 2\\n"); w("src/a.js", "module.exports = () => 2;\\n")
    elif k == "nomatch":
        w("src/a.test.js", "const a = require('./a.js')\\nexpect src/a.js 2\\n"); ans["expect_failure_match"] = "a crash the issue never mentions"
    elif k == "passes":
        w("src/a.test.js", "const a = require('./a.js')\\nmsg value() returns 1, want 2\\nexpect src/b.js 1\\n")
    elif k == "othersymptom":
        w("src/a.test.js", "const a = require('./a.js')\\nexpect src/a.js 2\\n")
    elif k == "noanswer":
        ans = None
    elif k == "libonly":   # #553 on 2026-11-18: a test of a library, labelled with the issue's text
        w("src/a.test.js", "const lib = require('some-lib')\\nmsg value() returns 1, want 2\\nexpect src/a.js 2\\n")
    elif k == "quotederror":   # the issue's error as an assertion's label: no frame of the repository
        w("src/a.test.js", "const a = require('./a.js')\\nmsg TypeError: value is null\\nexpect src/a.js 2\\n")
        ans["expect_failure_match"] = "TypeError: value is null"
    elif k == "raisederror":   # the error thrown by the repository's code: its stack has src/a.js
        w("src/a.test.js", "const a = require('./a.js')\\nmsg TypeError: value is null at value (src/a.js:1:9)\\n"
                           "expect src/a.js 2\\n")
        ans["expect_failure_match"] = "TypeError: value is null"
    elif k == "editexisting":
        w("keep.test.js", open(os.path.join(wt, "keep.test.js")).read() + "msg value() returns 1, want 2\\nexpect src/a.js 2\\n")
        ans["files"] = ["keep.test.js"]
    elif k == "chatty":
        turn = {"type": "message_end", "message": {"role": "assistant", "content": [{"type": "text", "text": "looking"}],
                                                   "usage": {"output": 1}}}
        for _ in range(5):
            print(json.dumps(turn), flush=True)
        import time; time.sleep(60)
    text = ("done\\n```json\\n" + json.dumps(ans) + "\\n```") if ans else "I could not decide."
    print(json.dumps({"type": "message_end", "message": {"role": "assistant", "content": [{"type": "text", "text": text}],
                                                         "usage": {"output": 5}}}))
    PY
    ''')

ADVISOR = SESS + textwrap.dedent('''\
    sess advisor
    echo "advisor tools=${PI_TOOLS:-all}" >> "$FAKE_STATE/calls"
    if [ -f "$FAKE_STATE/advisor.json" ]; then cp "$FAKE_STATE/advisor.json" "$2/advisor.json"; else
    printf '{"file": "src/a.js", "function": "value", "decoy": "src/b.js", "rationale": "it computes it"}' > "$2/advisor.json"; fi
    ''')

# the reviewer is pi itself, on PATH
PI_BIN = POP + SESS + textwrap.dedent('''\
    sess reviewer
    T=all; prev=; RULES=none; for a in "$@"; do [ "$prev" = --tools ] && T="$a"; [ "$prev" = --append-system-prompt ] && RULES="$a"; prev="$a"; done
    NC=no; for a in "$@"; do [ "$a" = --no-context-files ] && NC=yes; [ "$a" = --no-tools ] && T=none; done
    echo "review $RULES no-context-files=$NC" >> "$FAKE_STATE/rules"
    for a in "$@"; do last="$a"; done; printf '%s' "$last" > "$FAKE_STATE/review_prompt"
    R=approve; [ -s "$FAKE_STATE/reviews" ] && R=$(pop reviews)
    echo "review model=$6 tools=$T verdict=$R" >> "$FAKE_STATE/calls"
    [ "$R" = fail ] && exit 1
    V="$R"; [ "$R" = request-changes ] && V="request-changes CLASS: test-proves-nothing"
    [ "$R" = request-changes-unverified ] && V="request-changes CLASS: unverified"
    [ "$R" = request-changes-wrongplace ] && V="request-changes CLASS: wrong-place"
    if [ "$R" = noverdict ]; then   # a review cut off before its verdict
      printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The test only checks a const"}]}}\\n'
      exit 0
    fi
    printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The test only checks a constant.\\\\nVERDICT: %s"}]}}\\n' "$V"
    ''')

# a vitest-shaped runner. `run REPORT FILES`: those test files; `related REPORT SOURCES`: every *.test.js
# that names one of the sources. A test file's lines: "import F" (a missing F: the file fails to load),
# "expect F N" (passes when F returns N), "FAIL" (always fails).
SUITE = textwrap.dedent('''\
    import json, os, sys
    mode, report, args = sys.argv[1], sys.argv[2], sys.argv[3:]
    def all_tests():
        for root, _, files in os.walk("."):
            if ".git" not in root:
                for f in files:
                    if f.endswith(".test.js"):
                        yield os.path.relpath(os.path.join(root, f))
    if mode == "run":
        files = args
    else:
        files = [t for t in all_tests() if any(a in open(t).read() for a in args)]
    res = []
    for f in files:
        p = os.path.abspath(f)
        lines = open(p).read().splitlines()
        if any(ln.startswith("import ") and not os.path.exists(ln.split()[1]) for ln in lines):
            res.append({"name": p, "status": "failed", "assertionResults": []})
            continue
        st = "passed"
        for ln in lines:
            if ln == "FAIL":
                st = "failed"
            elif ln.startswith("expect "):
                _, target, n = ln.split()
                if not os.path.exists(target) or f"=> {n};" not in open(target).read():
                    st = "failed"
        res.append({"name": p, "status": st, "assertionResults": [{"status": st, "fullName": f,
                    "failureMessages": [" | ".join(lines)] if st == "failed" else []}]})
    json.dump({"testResults": res}, open(report, "w"))
    ''')


# a pytest-json-report-shaped runner: `PYSUITE REPORT ARGS`, the args test files or directories (every
# test_*.py below). A test file's lines are SUITE's, plus "SKIP" (every test in it is skipped, as a
# pytest.importorskip that found nothing does). A file that fails to load is a failed collector.
PYSUITE = textwrap.dedent('''\
    import json, os, sys
    report, args = sys.argv[1], sys.argv[2:]
    files = []
    for a in args:
        if os.path.isdir(a):
            for root, _, fs in os.walk(a):
                files += [os.path.join(root, f) for f in sorted(fs) if f.startswith("test_") and f.endswith(".py")]
        else:
            files.append(a)
    tests, collectors = [], [{"nodeid": "", "outcome": "passed", "result": []}]
    if not args:   # pytest with no path argument runs the whole suite: fail loudly, a gate must never do that
        tests.append({"nodeid": "WHOLE_SUITE::test_0", "outcome": "failed"})
    for f in sorted(set(files)):
        if not f.endswith(".py"):   # a data file handed to pytest: fail loudly
            tests.append({"nodeid": f"NON_PY::{f}", "outcome": "failed"})
            continue
        lines = open(f).read().splitlines()
        no_fixture = any(ln.startswith("fixture ") and not os.path.exists(ln.split()[1]) for ln in lines)
        if any(ln.startswith("import ") and not os.path.exists(ln.split()[1]) for ln in lines):
            collectors.append({"nodeid": f, "outcome": "failed", "longrepr": "ImportError"})
            continue
        collectors.append({"nodeid": f, "outcome": "passed", "result": []})
        for i, ln in enumerate(l for l in lines if l == "FAIL" or l.startswith("expect ")):
            ok = True
            if ln == "FAIL":
                ok = False
            else:
                _, target, n = ln.split()
                ok = os.path.exists(target) and f"return {n}" in open(target).read()
            out = "skipped" if "SKIP" in lines else ("passed" if ok else "failed")
            t = {"nodeid": f"{f}::test_{i}", "outcome": out}
            if no_fixture:   # a fixture that is not there: pytest-json-report's "error", the setup phase failed
                t.update(outcome="error", setup={"outcome": "failed"})
            tests.append(t)
    json.dump({"tests": tests, "collectors": collectors}, open(report, "w"))
    ''')


def write(path, text, mode=0o755):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write("#!/usr/bin/env bash\nset -eu\n" + text if path.endswith(".sh") or path.endswith("/pi") else text)
    os.chmod(path, mode)


def git(*a, cwd):
    subprocess.run(["git", *a], cwd=cwd, check=True, capture_output=True)


class Fixture:
    def __init__(self, verdicts, fixes, py=False):
        self.py = py
        self.repo = "example/api" if py else "example/shop"
        self.d = tempfile.mkdtemp()
        b = os.path.join(self.d, "bakeoff")
        write(os.path.join(b, "triage_run.sh"), TRIAGE)
        write(os.path.join(b, "advisor_run.sh"), ADVISOR)
        write(os.path.join(b, "harnesses", "pi.sh"), PI_HARNESS)
        write(os.path.join(b, "harnesses", "repro.sh"), REPRO_HARNESS)
        write(os.path.join(self.d, "bin", "pi"), PI_BIN)
        self.suite = os.path.join(self.d, "suite.py")
        write(self.suite, PYSUITE if py else SUITE, 0o644)
        self.state = os.path.join(self.d, "state")
        os.makedirs(self.state)
        for name, items in (("verdicts", verdicts), ("fixes", fixes)):
            with open(os.path.join(self.state, name), "w") as f:
                f.write("".join(x + "\n" for x in items))
        # the remote: example/shop with one passing test and one already red on main
        src = os.path.join(self.d, "src")
        os.makedirs(os.path.join(src, "src"))
        os.makedirs(os.path.join(src, "static", "images"))
        files = (("src/a.js", "module.exports = () => 1;\n"), ("src/b.js", "module.exports = () => 1;\n"),
                 ("keep.test.js", "expect src/b.js 1\n"), ("red.test.js", "expect src/a.js 9\n"),
                 ("guard.test.js", "// expect(value()).not.toBeNull()\n"),   # an assertion on main the suite ignores
                 ("static/images/x.png", "a big image\n"), ("package.json", '{"name": "x"}\n'))
        if py:
            # api's shape: src/<pkg>/<a>/<b>.py, tests mirrored under tests/unit/<pkg>/<a>/
            pkg, tests = "src/api", "tests/unit/api"
            files = ((f"{pkg}/pkg/mod.py", "def value(): return 1\n"), (f"{pkg}/pkg/other.py", "def value(): return 1\n"),
                     (f"{pkg}/__init__.py", "\n"), (f"{pkg}/lone/z.py", "def value(): return 1\n"), (f"{pkg}/pkg/sub/s.py", "def value(): return 1\n"),
                     (f"{tests}/pkg/sub/test_s.py", f"expect {pkg}/pkg/sub/s.py 1\n"),
                     (f"{tests}/pkg/test_keep.py", f"expect {pkg}/pkg/other.py 1\n"),
                     (f"{tests}/pkg/test_red.py", f"expect {pkg}/pkg/mod.py 9\n"),
                     (f"{tests}/test_top.py", f"expect {pkg}/pkg/other.py 1\n"), ("conftest.py", "# root\n"))
        for rel, text in files:
            os.makedirs(os.path.dirname(os.path.join(src, rel)), exist_ok=True)
            with open(os.path.join(src, rel), "w") as f:
                f.write(text)
        git("init", "-q", "-b", "main", cwd=src)
        git("-c", "user.email=t@t", "-c", "user.name=t", "add", "-A", cwd=src)
        git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base", cwd=src)
        remote = os.path.join(self.d, "remotes", *self.repo.split("/"))
        os.makedirs(os.path.dirname(remote))
        git("clone", "-q", "--bare", src, remote, cwd=self.d)
        git("config", "uploadpack.allowfilter", "true", cwd=remote)  # GitHub allows --filter; a local bare repo must be told
        self.base = subprocess.run(["git", "-C", remote, "rev-parse", "main"], capture_output=True, text=True).stdout.strip()
        self.bakeoff, self.remotes = b, "file://" + os.path.join(self.d, "remotes")  # file://: --depth and --filter work
        self.scratch = os.path.join(self.d, "scratch")

    def run(self, *extra, resume=None):
        plan = os.path.join(self.d, "plan.json")
        if os.path.exists(plan):
            os.remove(plan)
        env = dict(os.environ, FLOTILLA_PLAN_FILE=plan, FLOTILLA_MODELS=json.dumps(MODELS), FAKE_STATE=self.state, GIT_ALLOW_PROTOCOL="file",
                   GIT_REMOTE_BASE=self.remotes, PATH=os.path.join(self.d, "bin") + os.pathsep + os.environ["PATH"],
                   GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_CASE=json.dumps({"id": "case-20261115-abc", "kind": "code_bug", "target": f"repo:{self.repo}/issues/12",
                                             "data": {"issue": {"repo": self.repo, "number": 12, "title": "value is 1",
                                                                "body": getattr(self, "body", "value() returns 1, want 2"),
                                                                "url": "u"}}}))
        env.update(getattr(self, "extra_env", {}))
        if resume:   # what flotillad hands a run it lets continue the case's last (ADR-0019)
            env["FLOTILLA_RESUME"] = json.dumps(resume)
        old = fix.BAKEOFF
        fix.BAKEOFF = self.bakeoff
        try:
            cmds = (["--test-cmd", f"python3 {self.suite} %s", "--test-format", "pytest", "--related", "pytest-dirs",
                     "--source-root", "src/api", "--test-root", "tests/unit/api"] if self.py else
                    ["--test-cmd", f"python3 {self.suite} run %s", "--related-cmd", f"python3 {self.suite} related %s"])
            out = fix.main(["--repo", self.repo, "--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer",
                            "--reviewer-model", "bugfix-reviewer", "--setup", "true", *cmds,
                            "--scratch", self.scratch, *([] if getattr(self, "repro", False) else ["--no-repro"]),
                            *extra], environ=env)
        finally:
            fix.BAKEOFF = old
        with open(plan) as f:
            return json.load(f), out

    def calls(self):
        with open(os.path.join(self.state, "calls")) as f:
            return [ln.split()[0] for ln in f if ln.strip()]

    def close(self):
        shutil.rmtree(self.d, ignore_errors=True)


class Streams(unittest.TestCase):
    def test_the_last_message_s_text_or_error(self):
        ok = '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"VERDICT: approve"}],"stopReason":"stop"}}'
        bad = ('{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error",'
               '"errorMessage":"400: UnsupportedParamsError reasoning_effort"}}')
        self.assertEqual((fix.assistant_text(ok), fix.stream_error(ok)), ("VERDICT: approve", ""))
        self.assertEqual((fix.assistant_text(bad), fix.stream_error(bad)), ("", "400: UnsupportedParamsError reasoning_effort"))


class Timeouts(unittest.TestCase):
    def test_a_timeout_kills_the_whole_group(self):
        """A grandchild (pi under bash pi.sh, vitest under npx) dies with the timeout."""
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        marker = os.path.join(d, "alive")
        code, _ = fix.sh(f"(sleep 3; touch {marker}) & wait", d, timeout=1)
        self.assertEqual(code, 124)
        import time
        time.sleep(3.5)
        self.assertFalse(os.path.exists(marker), "the grandchild outlived the timeout")
        self.assertEqual(fix.sh(["true"], d, timeout=5), (0, ""))


class Errors(unittest.TestCase):
    def test_npm_s_error_lines_not_its_notice(self):
        out = ("added 1 package\nnpm error code 1\nnpm error path /wt/node_modules/canvas\n"
               "npm error gyp ERR! stack Error: getaddrinfo EAI_AGAIN nodejs.org\n"
               "npm notice New major version of npm available! 10.9.9 -> 12.2.2\n")
        got = fix.errors_of(out)
        self.assertIn("npm error path /wt/node_modules/canvas", got)
        self.assertIn("nodejs.org", got)
        self.assertNotIn("New major version", got)
        self.assertEqual(fix.errors_of("all fine\nthe end"), "all fine\nthe end", "no error line: the tail")


def pyrep(tests=(), collectors=()):
    return {"tests": [{"nodeid": n, "outcome": o, **({"setup": {"outcome": "failed"}} if o == "error" else {})} for n, o in tests],
            "collectors": [{"nodeid": n, "outcome": o} for n, o in collectors]}


class PytestReport(unittest.TestCase):
    """pytest-json-report's shape read into the one internal per-file result vitest's gives."""

    def test_passed_failed_collector_error_and_all_skipped(self):
        rep = pyrep(
            tests=[("tests/a/test_ok.py::test_1", "passed"), ("tests/a/test_ok.py::test_2[x-1]", "passed"),
                   ("tests/a/test_bad.py::TestC::test_1", "failed"), ("tests/a/test_bad.py::test_2", "passed"),
                   ("tests/a/test_err.py::test_1", "error"),
                   ("tests/a/test_skip.py::test_1", "skipped"), ("tests/a/test_skip.py::test_2", "skipped"),
                   ("tests/a/test_mixed.py::test_1", "skipped"), ("tests/a/test_mixed.py::test_2", "passed"),
                   ("tests/a/test_xf.py::test_1", "xfailed")],
            collectors=[("", "passed"), ("tests/a/test_ok.py", "passed"), ("tests/a/test_load.py", "failed"),
                        ("tests/a/test_modskip.py", "skipped")])
        t = fix.parse_pytest(rep, [])
        self.assertEqual(t.fails, {"tests/a/test_bad.py::TestC::test_1"})
        self.assertEqual(t.unloaded, {"tests/a/test_load.py", "tests/a/test_skip.py", "tests/a/test_modskip.py", "tests/a/test_xf.py",
                                      "tests/a/test_err.py", "tests/a/test_err.py::test_1"})
        self.assertEqual(t.ran, {"tests/a/test_ok.py", "tests/a/test_bad.py", "tests/a/test_mixed.py"})

    def test_a_setup_or_teardown_error_is_not_a_failure_and_not_a_pass(self):
        """pytest-json-report: a failed setup is outcome "error" with setup.outcome failed and no call; a failed
        teardown after a passed call is "error" too. A fixture that exists only in the fixed source errors on
        the base: that must not read as the test failing there."""
        rep = {"tests": [{"nodeid": "t/test_a.py::test_ok", "outcome": "passed"},
                         {"nodeid": "t/test_a.py::test_setup", "outcome": "error", "setup": {"outcome": "failed"}},
                         {"nodeid": "t/test_a.py::test_td", "outcome": "error",
                          "setup": {"outcome": "passed"}, "call": {"outcome": "passed"}, "teardown": {"outcome": "failed"}},
                         {"nodeid": "t/test_a.py::test_both", "outcome": "error", "call": {"outcome": "failed"}}],
               "collectors": []}
        t = fix.parse_pytest(rep, [])
        self.assertEqual(t.fails, {"t/test_a.py::test_both"}, "only a failed call phase is a failure")
        self.assertEqual(t.unloaded, {"t/test_a.py::test_setup", "t/test_a.py::test_td"})
        self.assertEqual(t.ran, {"t/test_a.py"})

    def test_a_file_whose_collector_failed_did_not_run(self):
        rep = pyrep(tests=[("tests/test_b.py::test_1", "passed")], collectors=[("tests/test_b.py", "failed")])
        t = fix.parse_pytest(rep, [])
        self.assertEqual((t.unloaded, t.ran), ({"tests/test_b.py"}, set()))

    def test_all_skipped_is_not_a_pass(self):
        t = fix.parse_pytest(pyrep(tests=[("tests/test_i.py::test_1", "skipped")]), ["tests/test_i.py"])
        self.assertEqual((t.unloaded, t.ran, t.fails), ({"tests/test_i.py"}, set(), set()))

    def test_a_requested_file_with_no_result_did_not_run(self):
        t = fix.parse_pytest(pyrep(tests=[("tests/test_a.py::test_1", "passed")]), ["tests/test_a.py", "tests/test_gone.py", "tests/dir"])
        self.assertEqual(t.unloaded, {"tests/test_gone.py"}, "a directory argument names no file; a missing file is unloaded")

    def test_a_collection_error_without_a_file_is_named(self):
        t = fix.parse_pytest(pyrep(collectors=[("", "failed")]), [])
        self.assertEqual(t.unloaded, {"(collection)"})


class Chain(unittest.TestCase):
    def fixture(self, verdicts, fixes):
        f = Fixture(verdicts, fixes)
        self.addCleanup(f.close)
        return f

    def test_a_clean_fix_is_a_pr_plan(self):
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run()
        self.assertEqual((plan["outcome"], plan["action"]), ("plan", "github.open_pr"), plan)
        p = plan["params"]
        self.assertEqual((p["repo"], p["base"]), ("example/shop", f.base))
        self.assertEqual(p["title"], "fix #12: value is 1")
        self.assertIn("+++ b/src/a.test.js", p["diff"])
        self.assertIn("+module.exports = () => 2;", p["diff"])
        self.assertNotIn("flotilla-suite", p["diff"])
        self.assertIn("1 test files fail on the base and pass with the fix", p["body"])
        self.assertIn("none newly failing", p["body"], "red.test.js fails on the base too: not a new failure")
        self.assertIn("VERDICT: approve", p["body"])
        self.assertEqual(f.calls(), ["triage", "fix", "review"])
        self.assertEqual(os.listdir(os.path.join(f.scratch, "runs")), [], "the run's directory is deleted")
        cache = os.path.join(f.scratch, "repos", "example__shop.git")
        wl = subprocess.run(["git", "-C", cache, "worktree", "list"], capture_output=True, text=True).stdout.splitlines()
        self.assertEqual(len(wl), 1, f"the cache stays and the run's worktree is gone: {wl}")
        # a second case reuses the cache: it fetches what is new, not the whole repo again
        with open(os.path.join(f.state, "verdicts"), "w") as fh:
            fh.write("needs-info\n")
        plan2, _ = f.run()
        self.assertEqual(plan2["outcome"], "escalate")
        self.assertTrue(os.path.isdir(cache))

    def test_excluded_paths_are_not_checked_out_and_not_in_the_diff(self):
        """shop's images are left out of the checkout (--exclude-path); their absence is not a deletion."""
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run("--exclude-path", "static/images")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertNotIn("static/images", plan["params"]["diff"])
        with open(os.path.join(f.state, "calls")) as fh:
            self.assertIn("img=no", fh.read())
        g = self.fixture(["needs-info"], [])
        g.run()
        with open(os.path.join(g.state, "calls")) as fh:
            self.assertIn("img=yes", fh.read(), "without --exclude-path the whole tree is checked out")

    def test_a_build_that_writes_into_an_excluded_tree_does_not_end_the_case(self):
        # #553 on 2026-11-17: the agent ran `vite build`, which wrote thumbnails into static/images/ (left out of the
        # sparse checkout); `git add -A` failed on them and the case escalated on git's error after $0.24
        f = self.fixture(["actionable"], ["built"])
        plan, _ = f.run("--exclude-path", "static/images")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertNotIn("static/images", plan["params"]["diff"])
        self.assertIn("+++ b/src/a.js", plan["params"]["diff"])

    def test_a_tampered_cache_is_refused_and_no_hook_runs(self):
        """The repo cache is shared between runs: a URL rewrite left in its config stops the next run, and a
        hook left in it does not run."""
        f = self.fixture(["needs-info", "needs-info"], [])
        f.run()
        cache = os.path.join(f.scratch, "repos", "example__shop.git")
        hook = os.path.join(cache, ".git", "hooks", "post-checkout")
        os.makedirs(os.path.dirname(hook), exist_ok=True)
        marker = os.path.join(f.d, "hook-ran")
        with open(hook, "w") as fh:
            fh.write(f"#!/bin/sh\ntouch {marker}\n")
        os.chmod(hook, 0o755)
        subprocess.run(["git", "-C", cache, "config", "url.https://evil.example/.insteadOf", "https://github.com/"], check=True)
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("the repo cache's config was changed", plan["detail"])
        subprocess.run(["git", "-C", cache, "config", "--unset", "url.https://evil.example/.insteadOf"], check=True)
        os.makedirs(os.path.dirname(hook), exist_ok=True)
        with open(hook, "w") as fh:
            fh.write(f"#!/bin/sh\ntouch {marker}\n")
        os.chmod(hook, 0o755)
        with open(os.path.join(f.state, "verdicts"), "w") as fh:
            fh.write("needs-info\n")
        f.run()
        self.assertFalse(os.path.exists(marker), "a hook in the shared cache ran")

    def test_the_repo_s_setup_writing_the_cache_config_is_put_back(self):
        """shop's `prepare` writes core.hooksPath into the shared cache's config on every npm ci: the run puts
        the config back, so the next run is not refused (8 escalations on a worker host, 2026-11-15). The fix's own
        change made after the setup is put back too, when the run ends."""
        f = self.fixture(["needs-info", "needs-info"], [])
        plan, _ = f.run("--setup", "git config core.hooksPath .husky")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertNotIn("config was changed", plan["detail"])
        cache = os.path.join(f.scratch, "repos", "example__shop.git")
        got = subprocess.run(["git", "-C", cache, "config", "--local", "--get", "core.hooksPath"], capture_output=True, text=True)
        self.assertEqual(got.returncode, 1, f"core.hooksPath stayed in the cache: {got.stdout!r}")
        plan2, _ = f.run("--setup", "git config core.hooksPath .husky")
        self.assertIn("needs info", plan2["detail"], "the second run is not refused by the cache check")

    def test_a_refused_fetch_is_tried_again(self):
        """GitHub refusing the checkout's blob fetch (anonymous, in a burst) is tried again; a refusal every
        time still stops the run, and another failure is not retried."""
        f = self.fixture(["needs-info", "needs-info", "needs-info"], [])
        real = subprocess.run(["sh", "-c", "command -v git"], capture_output=True, text=True).stdout.strip()
        count = os.path.join(f.d, "checkouts")
        wrapper = os.path.join(f.d, "bin", "git")
        os.makedirs(os.path.dirname(wrapper), exist_ok=True)

        def fail(times, msg):
            with open(wrapper, "w") as fh:
                fh.write(f"""#!/bin/sh
case "$*" in *"checkout -q --detach HEAD"*)
  n=$(cat {count} 2>/dev/null || echo 0); n=$((n+1)); echo $n > {count}
  if [ $n -le {times} ]; then echo "fatal: {msg}" >&2; exit 128; fi;;
esac
exec {real} "$@"
""")
            os.chmod(wrapper, 0o755)
            if os.path.exists(count):
                os.remove(count)
        old = fix.FETCH_RETRY_PAUSES
        fix.FETCH_RETRY_PAUSES = (0, 0)
        try:
            fail(2, "could not read Username for 'https://github.com': No such device or address")
            plan, _ = f.run()
            self.assertIn("needs info", plan["detail"], "refused twice, then fetched: the run goes on")
            fail(3, "could not read Username for 'https://github.com': No such device or address")
            plan, _ = f.run()
            self.assertIn("could not read Username", plan["detail"], "refused every time: the run stops")
            fail(1, "invalid reference: HEAD")
            plan, _ = f.run()
            with open(count) as fh:
                self.assertEqual(fh.read().strip(), "1", "a failure that is not a refused fetch is not retried")
        finally:
            fix.FETCH_RETRY_PAUSES = old
            os.remove(wrapper)

    def test_not_actionable_escalates_with_the_reason(self):
        f = self.fixture(["needs-info"], [])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("needs info from the reporter: the expected value", plan["detail"])
        self.assertEqual(f.calls(), ["triage"])

    def test_no_test_kicks_back_to_the_advisor_and_round_two_ships(self):
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("round 0: repro-first violated: no test in the diff", plan["params"]["body"])
        self.assertIn("advisor: src/a.js", plan["params"]["body"])
        self.assertEqual(f.calls(), ["triage", "fix", "advisor", "triage", "fix", "review"])

    def test_every_episode_leaves_its_turns_in_the_calls_file(self):
        """The calls file the worker returns holds each pi episode's turns with the episode's role, its order in
        the run and the tools it called; without FLOTILLA_CALLS_FILE nothing is written."""
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        path = os.path.join(f.d, "calls.jsonl")
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1", "FLOTILLA_CALLS_FILE": path}):
            plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        with open(path) as fh:
            lines = [json.loads(x) for x in fh]
        turns = [c for c in lines if c.get("kind") != "step"]
        read = [{"name": "read", "arg": ""}]
        self.assertEqual([(c["role"], c["episode_seq"], c["tools"], c["step"], c["alias"], c["decision_key"]) for c in turns],
                         [("verdict", 1, read, 1, "bugfix-flash", "case-20261115-abc"),
                          ("coder", 2, read, 1, "bugfix-flash", "case-20261115-abc"),
                          ("advisor", 3, read, 1, "bugfix-reviewer", "case-20261115-abc"),
                          ("verdict", 4, read, 1, "bugfix-flash", "case-20261115-abc"),
                          ("coder", 5, read, 1, "bugfix-flash", "case-20261115-abc"),
                          ("reviewer", 6, read, 1, "bugfix-reviewer", "case-20261115-abc")])
        # no meter (no --spend-file): the bill of an episode is not known, and pi's own figure (0.5 in these sessions) is not used
        self.assertTrue(all(c["cost_source"] == "" and c["cost_usd"] is None and c["finish_reason"] == "tool_calls" for c in turns))
        g = self.fixture(["actionable"], ["good"])
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1"}):
            g.run()
        self.assertFalse(os.path.exists(os.path.join(g.d, "calls.jsonl")))

    def test_the_chain_s_notes_are_step_rows_among_the_calls_in_the_order_they_happened(self):
        """Each note of the chain (verdict, a round's outcome, the advisor's pin, the review) is a line of kind step, with
        the role it concerns, its short text and its position among the steps; the episodes' turns sit between them."""
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        path = os.path.join(f.d, "calls.jsonl")
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1", "FLOTILLA_CALLS_FILE": path}):
            plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        with open(path) as fh:
            lines = [json.loads(x) for x in fh]
        steps = [c for c in lines if c.get("kind") == "step"]
        self.assertEqual([c["episode_seq"] for c in steps], list(range(1, len(steps) + 1)))
        for c in steps:
            self.assertEqual((c["alias"], c["model"], c["tokens_in"], c["tokens_out"], c["cost_usd"], c["cost_source"], c["finish_reason"]),
                             ("", "", None, None, None, "", ""), c)
        by_role = [(c["role"], c["detail"]) for c in steps]
        self.assertIn(("verdict", "verdict: actionable"), by_role)
        self.assertIn(("gates", "round 0: repro-first violated: no test in the diff"), by_role)
        self.assertIn(("advisor", "pin: src/a.js"), by_role)
        self.assertTrue(any(r == "reviewer" and d.startswith("review: ") and "VERDICT" not in d for r, d in by_role), by_role)
        self.assertTrue(any(r == "gates" and d.startswith("round 1: gates clean") for r, d in by_role), by_role)
        self.assertTrue(any(r == "harness" and d.startswith("base: ") for r, d in by_role), by_role)
        order = [(c["role"], c.get("kind", "call")) for c in lines]
        self.assertLess(order.index(("coder", "call")), order.index(("gates", "step")), "a round's outcome follows its episode")
        self.assertLess(order.index(("advisor", "call")), order.index(("advisor", "step")), "the pin follows the advisor's episode")

    def test_each_episode_takes_its_gateway_bill_from_the_meter_and_the_run_reports_their_sum_without_waiting_again(self):
        """main hands its meter to the chain: each pi episode is measured when it ends (spend.Meter.episode), its turns
        carry that bill, and the run's cost is read with quiet=True once every episode was measured."""
        class Meter:
            made = []

            def __init__(m, keys, *a, **kw):
                m.bills, m.quiet, m.hurried = [0.10, 0.20, 0.05, 0.10, 0.30, 0.25], None, False
                Meter.made.append(m)

            def episode(m):
                return m.bills.pop(0)

            def cost(m, agent_dir, quiet=False, baseline=0.0):
                m.quiet = quiet
                return 1.0, "gateway", ""

            def hurry(m, *a, **kw):
                m.hurried = True
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        path = os.path.join(f.d, "calls.jsonl")
        home = os.path.join(f.d, "home")
        os.makedirs(home)
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1", "FLOTILLA_CALLS_FILE": path, "HOME": home}), \
                unittest.mock.patch.object(fix.spend, "Meter", Meter):
            plan, _ = f.run("--spend-file", ".spend")
        self.assertEqual(plan["outcome"], "plan", plan)
        (m,) = Meter.made
        self.assertIs(m.quiet, True, "every episode was measured: nothing is waited for again")
        with open(path) as fh:
            turns = [c for c in map(json.loads, fh) if c.get("kind") != "step"]
        self.assertEqual([c["cost_usd"] for c in turns], [0.10, 0.20, 0.05, 0.10, 0.30, 0.25])
        self.assertEqual({c["cost_source"] for c in turns}, {"gateway-share"})
        with open(os.path.join(home, ".spend")) as fh:
            self.assertEqual(float(fh.read()), 1.0)

    def rfixture(self, verdicts, fixes, repros):
        """A fixture with the reproducer on (no --no-repro), its answers queued."""
        f = self.fixture(verdicts, fixes)
        with open(os.path.join(f.state, "repros"), "w") as fh:
            fh.write("".join(x + "\n" for x in repros))
        f.repro = True
        return f

    def test_the_reproducers_test_is_the_proof_and_the_fixer_only_fixes(self):
        f = self.rfixture(["actionable"], ["srconly"], ["good"])
        f.body = "value() returns 1, want 2\n\n**Actual:** value() returns 1, want 2"
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        with open(os.path.join(f.state, "repro_desc")) as fh:
            desc = fh.read()
        self.assertIn("THE PHRASES YOU MAY USE as expect_failure_match", desc)
        self.assertIn("\n- Actual: value() returns 1, want 2", desc, "the issue's own symptom line is offered")
        self.assertEqual(f.calls(), ["triage", "repro", "fix", "review"])
        self.assertIn("repro: src/a.test.js fails on the base with the issue's symptom ('returns 1, want 2')",
                      plan["params"]["body"])
        self.assertIn("+++ b/src/a.test.js", plan["params"]["diff"], "the frozen test ships with the fix")
        with open(os.path.join(f.state, "fix_desc")) as fh:
            self.assertIn("the failing test src/a.test.js reproduces the bug and is frozen", fh.read())

    def test_no_reproducing_test_is_needs_info_before_any_fix(self):
        f = self.rfixture(["actionable"], [], ["none"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate", plan)
        self.assertEqual(plan["detail"], "needs info from the reporter: no failing test can be written from the issue: "
                                         "a minified frame only; needed: a source map")
        self.assertEqual(f.calls(), ["triage", "repro"], "no fix session")

    def test_the_reproducers_output_is_checked(self):
        for kind, want in (("src", "the reproducer changed files that are not tests: src/a.js"),
                           ("nomatch", "the reproducer's failure text is not the issue's own"),
                           ("passes", "the reproducing test does not fail on the base (src/a.test.js)"),
                           ("othersymptom", "fails on the base, but not with the issue's symptom: 'returns 1, want 2'"),
                           ("noanswer", "the reproducer gave no answer"),
                           # #335 on 2026-11-18: a reproducer replaced an existing test in the file it added to
                           ("editexisting", "the reproducer edited a file that is on main (it must add a new test file): "
                                            "keep.test.js"),
                           # #553 on 2026-11-18: the proof tested @sentry/browser-utils and quoted the issue's error
                           ("libonly", "the reproducing test imports nothing of this repository (src/a.test.js)"),
                           ("quotederror", "the issue's error is quoted by the reproducing test, not raised by the "
                                           "repository's code")):
            f = self.rfixture(["actionable"], [], [kind])
            if kind.endswith("error"):
                f.body = "value() throws TypeError: value is null"
            plan, _ = f.run()
            self.assertEqual(plan["outcome"], "escalate", (kind, plan))
            self.assertIn(want, plan["detail"], kind)
            self.assertEqual(f.calls(), ["triage", "repro"], kind)

    def test_phrases_compare_across_dashes_and_quotes(self):
        # #335 on 2026-11-18: the issue's "~1–2" (en dash) was typed "~1-2"
        issue = "**Actual:** only ~1–2 events visible — first Venus flyby and maybe Launch."
        self.assertIn(fix.norm_phrase("only ~1-2 events visible - first venus flyby"), fix.norm_phrase(issue))
        self.assertIn(fix.norm_phrase('the "label" is wrong'), fix.norm_phrase("The “label” is wrong"))
        self.assertNotIn(fix.norm_phrase("labelled 'flyby' with empty notes"), fix.norm_phrase(issue))
        # #335 on 2026-11-18: the issue escapes its backticks, the reproducer copied the plain words
        escaped = r"**Expected:** all 6 \`mission.flight.events\` rows surface (Launch through Saturn)"
        self.assertIn(fix.norm_phrase("Expected: all 6 mission.flight.events rows surface"), fix.norm_phrase(escaped))
        self.assertIn(fix.norm_phrase(r"all 6 \`mission.flight.events\` rows"), fix.norm_phrase(escaped))

    def test_the_issues_symptom_lines_are_offered_verbatim(self):
        issue = ("value is 1\n\n**Signal:** `TypeError: x of null` in addEL_hook\n\n## Repro\n\n- **Expected:** value() "
                 "returns 2\n- **Actual:** value() returns 1, want 2\n\nSome other prose that is not a symptom.\n")
        got = fix.symptom_phrases(issue)
        self.assertEqual(got, ["Signal: `TypeError: x of null` in addEL_hook", "Expected: value() returns 2",
                               "Actual: value() returns 1, want 2"])
        for p in got:   # the check must accept every phrase it offers
            self.assertIn(fix.norm_phrase(p), fix.norm_phrase(issue))

    def test_repo_frames_and_imports_on_real_vitest_shapes(self):
        # stacks as vitest's JSON reporter printed them on orrery, 2026-11-18 (the advisor's design, checked locally)
        wt = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, wt)
        for p in ("src/lib/mission-event-merge.ts", "src/lib/x.repro.test.ts", "node_modules/@sentry/core/build/dom.js"):
            os.makedirs(os.path.dirname(os.path.join(wt, p)), exist_ok=True)
            open(os.path.join(wt, p), "w").close()
        raised = (f"TypeError: Cannot read properties of null (reading 'type')\n"
                  f"    at mergeFlightEvents ({wt}/src/lib/mission-event-merge.ts:92:33)\n"
                  f"    at {wt}/src/lib/x.repro.test.ts:6:3\n"
                  f"    at file://{wt}/node_modules/vitest/dist/chunks/run.js:1632:35")
        labelled = (f"AssertionError: TypeError: Cannot read properties of null (reading 'tagName'): expected \"vi.fn()\"\n"
                    f"    at Proxy.methodWrapper (file://{wt}/node_modules/chai/index.js:1737:25)\n"
                    f"    at {wt}/src/lib/x.repro.test.ts:61:11")
        in_a_library = (f"TypeError: Cannot read properties of null (reading 'tagName')\n"
                        f"    at htmlTreeAsString (file://{wt}/node_modules/@sentry/core/build/dom.js:20:5)\n"
                        f"    at {wt}/src/lib/x.repro.test.ts:9:3")
        self.assertEqual(fix.repo_frames([raised], wt), ["src/lib/mission-event-merge.ts"])
        self.assertEqual(fix.repo_frames([labelled], wt), [])
        self.assertEqual(fix.repo_frames([in_a_library], wt), [], "an error raised inside a package is not the repo's")
        self.assertEqual(fix.repo_frames(["    at x (/elsewhere/src/lib/mission-event-merge.ts:1:1)"], wt), [])
        self.assertEqual(fix.repo_imports("import { mergeFlightEvents } from './mission-event-merge';", wt),
                         ["./mission-event-merge"])
        self.assertEqual(fix.repo_imports("import { it } from 'vitest';\nimport { htmlTreeAsString } from "
                                          "'@sentry/browser-utils';\n// import { x } from './mission-event-merge'", wt), [])
        self.assertEqual(fix.repo_imports("import { thing } from '$lib/x.repro.test';", wt), [], "a test is not the repo")

    def test_an_error_raised_by_the_repositorys_code_is_a_proof(self):
        f = self.rfixture(["actionable"], ["srconly"], ["raisederror"])
        f.body = "value() throws TypeError: value is null"
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("repro: src/a.test.js fails on the base with the issue's symptom", plan["params"]["body"])

    def test_wrong_place_is_a_note_when_the_frozen_proof_passes(self):
        # #335 on 2026-11-18: a review turned down, as wrong-place, a fix the reference fix's tests pass
        f = self.rfixture(["actionable"], ["srconly"], ["good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes-wrongplace\n")
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertNotIn("unresolved", plan["params"]["body"].split("\n")[0], plan["params"]["body"][:300])
        self.assertIn("review note: the review says the fix is in the wrong place", plan["params"]["body"])
        self.assertEqual([c for c in f.calls() if c.startswith("fix")], ["fix"], "no kick-back")
        with open(os.path.join(f.state, "review_prompt")) as fh:
            prompt = fh.read()
        self.assertIn("THE FROZEN PROOF: src/a.test.js", prompt)
        self.assertNotIn("test adequacy", prompt)
        self.assertNotIn("wrong-place", prompt.split("name its class:")[1].split(".")[0])

    def test_wrong_place_still_kicks_back_without_a_frozen_proof(self):
        f = self.fixture(["actionable"], ["good", "good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes-wrongplace\napprove\n")
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(len([c for c in f.calls() if c.startswith("fix")]), 2, f.calls())

    def test_the_reproducer_stops_at_its_own_turn_cap(self):
        # #553 on 2026-11-18: a reproducer explored 90 turns and wrote nothing
        f = self.rfixture(["actionable"], [], ["chatty"])
        t0 = time.time()
        plan, _ = f.run("--repro-max-turns", "3")
        self.assertEqual(plan["outcome"], "escalate", plan)
        self.assertIn("the reproducer gave no answer (exit 125)", plan["detail"])
        self.assertLess(time.time() - t0, 30, "the fake sleeps 60 s after its turns: it was stopped")

    def test_a_fix_that_changes_the_frozen_test_fails_its_round(self):
        # the "good" fake fix also rewrites src/a.test.js; round 1's fix leaves it alone
        f = self.rfixture(["actionable", "actionable"], ["good", "srconly"], ["good"])
        plan, _ = f.run("--no-resume")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("round 0: the fix changed the frozen reproducing test: src/a.test.js", plan["params"]["body"])

    def test_the_repro_gate_reads_only_the_frozen_test(self):
        # the fixer may add tests (src/b.test.js passes on the base too); they are not the proof
        f = self.rfixture(["actionable"], ["srcplus"], ["good"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("+++ b/src/b.test.js", plan["params"]["diff"])

    def test_a_wrong_fix_fails_the_frozen_test(self):
        f = self.rfixture(["actionable", "actionable"], ["wrongsrc", "srconly"], ["good"])
        plan, _ = f.run("--no-resume")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("round 0: the fix's own tests fail with the fix: src/a.test.js", plan["params"]["body"])

    def test_a_round_that_inverts_an_assertion_on_main_fails_its_gates(self):
        # #553 (2026-11-16/17): every filter fix flipped `.not.toBeNull()` to `.toBeNull()` in a test on main, which
        # alone made it pass the repro gate; the advisor's second opinion: a gate, not advice to the review
        f = self.fixture(["actionable", "actionable"], ["flip", "good"])
        plan, _ = f.run("--no-resume")   # the fake fixer cannot undo the flip a resumed round starts from; pi is told why
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("round 0: out of scope: a fix may not remove or rewrite an assertion of a test on main (add a test "
                      "instead): guard.test.js: // expect(value()).not.toBeNull()", plan["params"]["body"])
        self.assertNotIn("guard.test.js", plan["params"]["diff"], "round 1's clean fix is the plan")

    def test_an_actionable_verdict_that_lists_what_is_missing_is_needs_info(self):
        # agents/triage.md: "missing" is only for needs-info; #553's wrong fixes followed 2 such first passes of 3
        f = self.fixture(["actionable-missing"], [])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate", plan)
        self.assertEqual(plan["detail"], "needs info from the reporter: the expected value")
        self.assertEqual(f.calls(), ["triage"], "no fix was attempted")

    def test_a_kick_back_verdict_that_prescribes_a_test_change_escalates(self):
        # #553: "the test ... is changed to assert toBeNull()" in 5 of 6 kick-back verdicts, never in the issue
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        with open(os.path.join(f.state, "acceptance"), "w") as fh:
            json.dump({"acceptance": [{"criterion": "the test in sentry.test.ts expects beforeSend(e).toBeNull()",
                                       "intent_source": "repo-data"}]}, fh)
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate", plan)
        self.assertIn("after round 0 the verdict's acceptance criteria prescribe changing a test, which the issue does "
                      "not: the test in sentry.test.ts expects", plan["detail"])
        self.assertEqual(f.calls(), ["triage", "fix", "advisor", "triage"], "no second fix")
        # a first pass with the same criteria is not judged by this rule (its own shape check is triage_run.sh's)
        g = self.fixture(["actionable"], ["good"])
        with open(os.path.join(g.state, "acceptance"), "w") as fh:
            json.dump({"acceptance": [{"criterion": "verified by a new test: assert zero pageerrors"}]}, fh)
        self.assertEqual(g.run()[0]["outcome"], "plan")

    def test_a_test_that_reads_source_text_fails_its_round(self):
        # #553 on 2026-11-17: a regex over AudioOverlay.svelte's text fails on the base and passes with the fix by
        # construction; the review approved it as "source-text guards"
        f = self.fixture(["actionable", "actionable"], ["srctext", "good"])
        plan, _ = f.run("--no-resume")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("round 0: test-proves-nothing: a test reads a source file's text instead of running its code: "
                      "src/a.test.js: const t = readFileSync(p, 'utf-8');", plan["params"]["body"])
        self.assertNotIn("readFileSync", plan["params"]["diff"], "round 1's clean fix is the plan")

    def test_reads_source_text_cases(self):
        r = fix.reads_source_text
        self.assertTrue(r(["const overlayPath = 'src/lib/components/AudioOverlay.svelte';",
                           "const overlay = readFileSync(join(process.cwd(), overlayPath), 'utf-8');"]))
        self.assertTrue(r(["src = open('src/podcast_scraper/pkg/mod.py').read()"]))
        self.assertTrue(r(["text = Path('src/app.ts').read_text()"]))
        # a fixture read, with the code under test imported, is a real test
        self.assertEqual(r(["import { merge } from '../lib/mission-event-merge.ts';",
                            "const data = JSON.parse(readFileSync('fixtures/cassini.json', 'utf-8'));"]), "")
        self.assertEqual(r(["from podcast_scraper.pkg import mod", "with open('tests/data/feed.yaml') as f:"]), "")
        self.assertEqual(r(["const m = await import('./a.js');", "expect(m.value()).toBe(2);"]), "")

    def test_change_criteria_reads_code_not_prose(self):
        v = lambda *c: {"problem": {"acceptance": list(c)}}   # noqa: E731
        self.assertEqual(fix.test_change_criteria(v("Navigating away never throws; verified by a Playwright test: "
                                                    "assert zero pageerrors")), [])
        # #517 (2026-11-17): a new test described in prose, "assert (a) ...", wrongly escalated the case
        self.assertEqual(fix.test_change_criteria(v("Test: force the bundle promise to reject, then call get('i18n/x') "
                                                    "and assert (a) the returned promise rejects")), [])
        self.assertEqual(fix.test_change_criteria(v({"criterion": "mergeFlightEvents() keeps both"})), [])
        self.assertEqual(len(fix.test_change_criteria(v("expect(beforeSend(e)).toBeNull() for the hook",
                                                        {"criterion": "tests/unit/test_mod.py checks it"},
                                                        "src/x.spec.ts is updated"))), 3)
        self.assertEqual(fix.test_change_criteria({"verdict": "actionable"}), [])
        self.assertEqual(fix.test_change_criteria(["not", "a", "dict"]), [])

    def test_an_advisor_that_cannot_locate_says_so_on_the_chain(self):
        import contextlib
        import io
        f = self.fixture(["actionable", "needs-info"], ["notest"])
        with open(os.path.join(f.state, "advisor.json"), "w") as fh:
            json.dump({"file": "", "not_locatable": "the throw is in code this repo does not have"}, fh)
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate", plan)
        self.assertIn("BUGFIX FIX #12 advisor: not locatable: the throw is in code this repo does not have", buf.getvalue())
        # and the chain escalates there: no second verdict, no second round (#553: $0.52 and $0.88 to reach needs-info)
        self.assertEqual(plan["detail"], "needs info from the reporter after round 0: the advisor could not locate the "
                                         "fault in this repository: the throw is in code this repo does not have")
        self.assertEqual(f.calls(), ["triage", "fix", "advisor"])

    def test_a_fix_episode_stops_at_its_turn_cap(self):
        # #553's round 0s took 105-194 turns and never gave a right fix; every good round took <= 67 (2026-11-16/17)
        import contextlib
        import io
        f = self.fixture(["actionable", "needs-info"], ["chatty"])
        buf, t0 = io.StringIO(), time.time()
        with contextlib.redirect_stdout(buf):
            plan, _ = f.run("--fix-max-turns", "3", "--no-advisor")
        self.assertEqual(plan["outcome"], "escalate", plan)
        self.assertIn("BUGFIX FIX #12 round 0: the fix episode reached its turn cap (3) with no change", buf.getvalue())
        self.assertLess(time.time() - t0, 30, "the fake fixer sleeps 60 s after its turns: it was stopped, not waited for")

    def test_sh_kills_the_group_at_the_turn_cap_and_reads_like_before_without_it(self):
        turn = json.dumps({"type": "message_end", "message": {"role": "assistant", "usage": {"output": 1}}})
        script = f"for i in 1 2 3 4 5; do echo '{turn}'; done; sleep 30; echo late"
        t0 = time.time()
        code, out = fix.sh(["sh", "-c", script], ".", dict(os.environ), 60, max_turns=3)
        self.assertEqual(code, fix.TURN_CAPPED)
        self.assertLess(time.time() - t0, 15, "the group was killed, not waited for")
        self.assertNotIn("late", out)
        self.assertGreaterEqual(fix.usage(out)[0], 3, "turns written before the kill landed are kept")
        code, out = fix.sh(["sh", "-c", f"echo '{turn}'; echo done"], ".", dict(os.environ), 60, max_turns=3)
        self.assertEqual((code, fix.usage(out)[0], out.endswith("done\n")), (0, 1, True))
        code, out = fix.sh(["sh", "-c", "echo a; sleep 30"], ".", dict(os.environ), 1, max_turns=3)
        self.assertEqual((code, out), (124, "a\n"))

    def test_a_review_that_did_not_run_is_said_on_the_plan(self):
        # it used to read as an approval: "(no review: ...)" has no VERDICT line
        f = self.fixture(["actionable"], ["good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("fail\n")
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("unresolved: the review did not run", plan["detail"])
        self.assertIn("the review did not run: (no review", plan["params"]["body"])
        self.assertEqual(f.calls(), ["triage", "fix", "review"])

    def test_only_a_fault_the_review_can_show_sends_a_fix_back(self):
        # C4b: "unverified" and the like cost a whole round under C4a (#335: $0.11 -> $0.31) with nothing better at
        # the end; they go on the plan, and the fix is not sent back
        f = self.fixture(["actionable"], ["good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes-unverified\n")
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(f.calls(), ["triage", "fix", "review"])
        self.assertIn("unresolved: the review raised points it cannot show from the diff", plan["detail"])
        self.assertIn("(class unverified)", plan["params"]["body"])

    def test_b11_assertions_a_fix_removes_from_tests_on_main_are_found_and_shown(self):
        # #553 (2026-11-16): `.not.toBeNull()` became `.toBeNull()` in a test on main, and the review approved it
        import types
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        git("init", "-q", d, cwd=d)
        os.makedirs(os.path.join(d, "src"))
        with open(os.path.join(d, "src", "sentry.test.ts"), "w") as fh:
            fh.write("it('still reports', () => {\n  expect(beforeSend(ev)).not.toBeNull();\n});\n")
        with open(os.path.join(d, "src", "sentry.ts"), "w") as fh:
            fh.write("export const beforeSend = (e) => e;\n")
        git("add", "-A", cwd=d)
        git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base", cwd=d)
        base = subprocess.run(["git", "rev-parse", "HEAD"], cwd=d, capture_output=True, text=True).stdout.strip()
        with open(os.path.join(d, "src", "sentry.test.ts"), "w") as fh:
            fh.write("it('drops it', () => {\n  expect(beforeSend(ev)).toBeNull();\n});\n")
        with open(os.path.join(d, "src", "new.test.ts"), "w") as fh:   # a new test file is not "on main"
            fh.write("it('x', () => { expect(1).toBe(1); });\n")
        git("add", "-A", cwd=d)
        got = fix.changed_assertions(d, base, d, dict(os.environ))
        self.assertEqual(got, [("src/sentry.test.ts", ["expect(beforeSend(ev)).not.toBeNull();"])])
        chain = types.SimpleNamespace(repo="example/shop", n=553, issue={"title": "t", "body": "b"}, checked="",
                                      a=types.SimpleNamespace(review_tools="none"), wt=d, base=base, run_dir=d, env=dict(os.environ))
        p = fix.Chain.review_prompt(chain, "diff")
        self.assertIn("THE FIX REMOVES OR REWRITES ASSERTIONS OF TESTS THAT ARE ON MAIN (found by the chain): "
                      "src/sentry.test.ts: expect(beforeSend(ev)).not.toBeNull();", p)
        self.assertIn("weakens or inverts an assertion of a test that is on main is class test-proves-nothing", p)

    def test_the_review_is_given_the_gates_and_the_task(self):
        f = self.fixture(["actionable"], ["good"])
        f.run()
        p = self.review_prompt(f)
        self.assertIn("THE GATES (run by the chain on this diff): 1 test files fail on the base and pass with the fix", p)
        # #553 (2026-11-16): the review took the verdict's task ("mandates the filter") for the acceptance criteria
        self.assertIn("THE TASK THE FIX WAS GIVEN (the verdict step's analysis; it can be wrong: judge the fix against "
                      "the issue as filed above, not against this task):\nmake value() return 2", p)
        self.assertIn("the issue's acceptance criteria are the ones in the issue as filed, not in the task", p)
        # the reviewer reads the issue first and the task last (the advisor's second opinion, 2026-11-17)
        at = [p.index(k) for k in ("THE ISSUE AS FILED:", "THE GATES", "DIFF:", "THE CHANGED FILES", "THE FIX AGENT'S",
                                   "THE TASK THE FIX WAS GIVEN", "You have no tools")]
        self.assertEqual(at, sorted(at), p[:400])
        for c in fix.KICK_CLASSES:
            self.assertIn(c, p)
        # #553 r2 (2026-11-16): the review saw "a skip is not an attach" against the issue's acceptance criterion and
        # approved, its rule being "only a fault you can show"; a missed acceptance criterion is one it can show
        self.assertIn("does not meet an acceptance criterion the issue states", p)
        self.assertIn("silently dropping behaviour", p)

    def test_review_class_reads_the_last_verdict_line(self):
        self.assertEqual(fix.review_class("x\nVERDICT: request-changes CLASS: over-broad"), "over-broad")
        self.assertEqual(fix.review_class("VERDICT: request-changes CLASS: hides-symptom\nlater\n**VERDICT: approve**"), "")
        self.assertEqual(fix.review_class("VERDICT: request-changes"), "")

    def resumes(self, f):
        path = os.path.join(f.state, "resumes")
        if not os.path.exists(path):
            return []
        with open(path) as fh:
            return fh.read().splitlines()

    def test_a_review_kick_back_resumes_the_fix_session_on_its_own_tree(self):
        # C10: the next round goes on with the previous session (no new exploration), the tree as it left it
        f = self.fixture(["actionable"], ["good", "good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\napprove\n")
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        r = self.resumes(f)
        self.assertEqual(len(r), 1, r)
        self.assertRegex(r[0], r"^resume s-good-\d+\.jsonl changed=2 desc=A REVIEW OF YOUR FIX ASKED FOR CHA")

    def test_a_gate_kick_back_resumes_with_the_gates_reason(self):
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        r = self.resumes(f)
        self.assertEqual(len(r), 1, r)
        self.assertRegex(r[0], r"^resume s-notest-\d+\.jsonl changed=1 desc=YOUR FIX DID NOT PASS ITS GATES: r")

    def test_a_diff_that_does_not_apply_falls_back_to_a_new_session(self):
        # the ops repo's review: the fall-back had no test. The resumed round is handed a diff git cannot apply
        f = self.fixture(["actionable"], ["good", "good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\napprove\n")
        real = fix.Chain.attempt

        def attempt(chain, manifest, resume=None):
            if resume:
                resume = (resume[0], "diff --git a/nope b/nope\n@@ not a patch @@\n", resume[2])
            return real(chain, manifest, resume)
        fix.Chain.attempt = attempt
        try:
            plan, _ = f.run()
        finally:
            fix.Chain.attempt = real
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(self.resumes(f), [], "no session resumed")
        self.assertIn("resume: the previous round's diff does not apply", plan["params"]["body"])
        with open(os.path.join(f.state, "fix_desc")) as fh:
            self.assertIn("make value() return 2", fh.read(), "the new session gets the task again")

    def test_no_resume_starts_a_new_session(self):
        f = self.fixture(["actionable"], ["good", "good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\napprove\n")
        f.run("--no-resume")
        self.assertEqual(self.resumes(f), [])

    def test_a_review_without_a_verdict_is_not_an_approval(self):
        # the ops repo's review (2026-11-16): any text read as approval, e.g. the partial output of a timed-out call
        f = self.fixture(["actionable"], ["good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("noverdict\n")
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("unresolved: the review gave no verdict", plan["detail"])
        self.assertTrue(plan["params"]["body"].startswith("**unresolved: the review gave no verdict"))

    def test_the_review_never_reads_a_symlink_or_a_file_outside_the_worktree(self):
        # a fix that adds a link to the routine's key file must not put the key in the reviewer's prompt
        import types
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        secret = os.path.join(d, "model-fix.key")
        with open(secret, "w") as fh:
            fh.write("sk-SECRET-KEY")
        wt = os.path.join(d, "wt")
        git("init", "-q", wt, cwd=d)
        os.makedirs(os.path.join(wt, "src"))
        with open(os.path.join(wt, "src", "a.js"), "w") as fh:
            fh.write("module.exports = () => 1;\n")
        git("add", "-A", cwd=wt)
        git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base", cwd=wt)
        base = subprocess.run(["git", "rev-parse", "HEAD"], cwd=wt, capture_output=True, text=True).stdout.strip()
        os.symlink(secret, os.path.join(wt, "src", "k.js"))
        with open(os.path.join(wt, "src", "a.js"), "w") as fh:
            fh.write("module.exports = () => 2;\n")
        git("add", "-A", cwd=wt)
        chain = types.SimpleNamespace(repo="example/shop", n=12, issue={"title": "t", "body": "b"}, checked="",
                                      a=types.SimpleNamespace(review_tools="none"), wt=wt, base=base, run_dir=d, env=dict(os.environ))
        p = fix.Chain.review_prompt(chain, "diff --git a/src/a.js b/src/a.js\n")
        self.assertNotIn("sk-SECRET-KEY", p)
        self.assertIn("=== src/k.js === [not included: not a regular file]", p)
        self.assertIn("=== src/a.js ===\nmodule.exports = () => 2;", p)

    def test_the_unresolved_flag_survives_a_long_plan_body(self):
        import types
        chain = types.SimpleNamespace(repo="example/shop", n=12, issue={"title": "t"}, base="a" * 40, checked="gates ok",
                                      unloaded_on_base=[], resumed_from="", log=["step " + "x" * 3000] * 5)
        plan = fix.Chain.plan(chain, 1, "diff", "VERDICT: request-changes", unresolved="the review asked for changes")
        body = plan["params"]["body"]
        self.assertLessEqual(len(body), fix.BODY_MAX)
        self.assertTrue(body.startswith("**unresolved: the review asked for changes; round 1's clean fix kept**"))

    def test_cap_bytes_counts_bytes(self):
        s = "é" * 100000   # 200,000 bytes in UTF-8
        out = fix.cap_bytes(s, fix.REVIEW_PROMPT_MAX)
        self.assertLessEqual(len(out.encode()), fix.REVIEW_PROMPT_MAX)
        self.assertIn("[... cut:", out)

    def test_step_one_is_advisory_it_notes_and_never_closes(self):
        # RFC-0013 §1, advisory (operator, 2026-11-17): a name nothing at the base has is noted, and the verdict runs
        f = self.fixture(["actionable"], ["good"])
        f.body = "TypeError: m.gone_from_main is not a function at src/a.js:3 (dev branch)"
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(f.calls(), ["triage", "fix", "review"], "the verdict, the fix and the review ran")
        self.assertIn("- step one: gone_from_main appears nowhere", plan["params"]["body"])
        # --no-step-one: no note
        g = self.fixture(["needs-info"], [])
        g.body = f.body
        plan, _ = g.run("--no-step-one")
        self.assertEqual(g.calls(), ["triage"])
        self.assertNotIn("appears nowhere", json.dumps(plan))
        # a fault inside step one never ends the case (the ops repo's review of 62b2523)
        h = self.fixture(["actionable"], ["good"])
        h.body = f.body
        real = fix.stepone.check
        fix.stepone.check = lambda *a: (_ for _ in ()).throw(UnicodeDecodeError("utf-8", b"\xe9", 0, 1, "x"))
        try:
            plan, _ = h.run()
        finally:
            fix.stepone.check = real
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("- step one: not checked (UnicodeDecodeError)", plan["params"]["body"])

    def test_the_kick_back_limits_are_two_and_one(self):
        a = fix.parse(["--repo", "r", "--worker-model", "w", "--advisor-model", "a", "--reviewer-model", "v"])
        self.assertEqual((a.kickback_max, a.review_kickback_max), (2, 1))

    def test_the_review_can_send_a_fix_back(self):
        """Phase B: the review asked for changes on the 3 wrong or partial plans of 4 and could not stop
        any. request-changes now kicks the fix back (the review is the evidence, no advisor), at most
        --review-kickback-max times; after that the plan goes to the operator with the review in it."""
        f = self.fixture(["actionable", "actionable"], ["good", "good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\napprove\n")
        plan, _ = f.run("--no-resume")   # the new session's task (review and diff); a resumed one: its own tests
        self.assertEqual(plan["outcome"], "plan", plan)
        # no new verdict after a review's kick-back (it did not dispute actionability): the fix step again, with the
        # whole review and the diff it judged
        self.assertEqual(f.calls(), ["triage", "fix", "review", "fix", "review"])
        self.assertIn("round 0: the review requested changes: The test only checks a constant.", plan["params"]["body"])
        self.assertIn("VERDICT: approve", plan["params"]["body"])
        with open(os.path.join(f.state, "fix_desc")) as fh:
            desc = fh.read()
        self.assertIn("make value() return 2", desc)
        self.assertIn("THE REVIEW:\nThe test only checks a constant.\nVERDICT: request-changes", desc)
        self.assertIn("THE DIFF IT REVIEWED (the worktree is back at the base):\ndiff --git", desc)
        # always request-changes: two kick-backs, then the plan carries the review's objection
        g = self.fixture(["actionable"] * 3, ["good"] * 3)
        with open(os.path.join(g.state, "reviews"), "w") as fh:
            fh.write("request-changes\n" * 3)
        # fresh sessions: resumed, the same fake fix again is "changed nothing since its last round" (its own test)
        plan, _ = g.run("--review-kickback-max", "2", "--no-resume")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(g.calls().count("review"), 3)
        self.assertIn("VERDICT: request-changes", plan["params"]["body"])
        self.assertIn("unresolved: the review asked for changes", plan["detail"])
        self.assertIn("unresolved: the review asked for changes", plan["params"]["body"].splitlines()[0])
        # no kick-backs left in the round budget: the plan as before
        h = self.fixture(["actionable"], ["good"])
        with open(os.path.join(h.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        plan, _ = h.run("--kickback-max", "0")
        self.assertEqual((plan["outcome"], h.calls()), ("plan", ["triage", "fix", "review"]), plan)
        self.assertIn("unresolved: the review asked for changes", plan["detail"])

    def test_a_sent_back_round_that_fails_keeps_the_last_clean_fix(self):
        """Round 0 is gate-clean and the review asks for changes; the round that follows fails a gate or its
        verdict is not actionable. The run returns round 0's plan with the review attached, not an escalation."""
        f = self.fixture(["actionable", "actionable", "needs-info"], ["good", "notest"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        # fresh sessions: the fake fixes assume a tree back at the base; resuming has its own tests
        plan, _ = f.run("--no-resume")
        self.assertEqual((plan["outcome"], plan["action"]), ("plan", "github.open_pr"), plan)
        self.assertIn("+module.exports = () => 2;", plan["params"]["diff"])
        self.assertIn("VERDICT: request-changes", plan["params"]["body"])
        self.assertIn("round 0: the review requested changes", plan["params"]["body"])
        self.assertIn("round 1: repro-first violated", plan["params"]["body"])
        self.assertIn("a clean fix in round 0", plan["detail"])
        # the verdict after the review's kick-back is not actionable: the same
        e = self.fixture(["actionable", "needs-info"], ["good"])
        with open(os.path.join(e.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        plan, _ = e.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertNotIn("Traceback", plan["params"]["body"])      # a Stop is a reason, not a bug
        # the limit reached by a failing round keeps it too
        g = self.fixture(["actionable"], ["good", "notest"])
        with open(os.path.join(g.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        plan, _ = g.run("--kickback-max", "1")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("a clean fix in round 0", plan["detail"])
        # no round was ever gate-clean: still an escalation with no plan
        h = self.fixture(["actionable", "needs-info"], ["notest"])
        plan, _ = h.run()
        self.assertEqual(plan["outcome"], "escalate", plan)

    def _later_round_raises(self, exc):
        """Round 0 is clean and the review sends it back; round 1's attempt raises `exc`."""
        f = self.fixture(["actionable", "actionable"], ["good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\n")
        real, calls = fix.Chain.attempt, []

        def attempt(chain, manifest, resume=None):
            calls.append(manifest)
            if len(calls) > 1:
                raise exc
            return real(chain, manifest)
        fix.Chain.attempt = attempt
        try:
            return f.run()[0]
        finally:
            fix.Chain.attempt = real

    def test_a_later_round_that_raises_keeps_the_last_clean_fix(self):
        for exc in (OSError("disk full"), subprocess.SubprocessError("boom"), ValueError("bad json")):
            plan = self._later_round_raises(exc)
            self.assertEqual((plan["outcome"], plan["action"]), ("plan", "github.open_pr"), (exc, plan))
            self.assertIn("+module.exports = () => 2;", plan["params"]["diff"])
            self.assertIn(type(exc).__name__, plan["params"]["body"])
            self.assertIn("Traceback", plan["params"]["body"], exc)    # a bug's traceback is not hidden
            self.assertIn("in attempt", plan["params"]["body"], exc)
        # not a failure a round can have: it still ends the run
        with self.assertRaises(KeyboardInterrupt):
            self._later_round_raises(KeyboardInterrupt())

    def test_a_kept_plan_says_the_review_s_changes_are_unresolved(self):
        plan = self._later_round_raises(OSError("disk full"))
        self.assertIn("unresolved: the review asked for changes", plan["detail"])
        self.assertIn("a clean fix in round 0", plan["detail"])
        first = plan["params"]["body"].splitlines()[0]
        self.assertIn("unresolved: the review asked for changes", first)
        self.assertIn("round 0", first)
        # a plan whose review approved says no such thing
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run()
        self.assertNotIn("unresolved", plan["detail"])
        self.assertNotIn("unresolved", plan["params"]["body"])

    def test_a_quoted_instruction_is_not_a_request_for_changes(self):
        asks = fix.review_requests_changes
        for quoted in ('I was told to end with "VERDICT: request-changes" or approve.',
                       "Reply `VERDICT: request-changes` only if it is broken.",
                       "> the prompt says: VERDICT: request-changes",
                       "It reads fine.\n```\nend with VERDICT: request-changes\n```\nNothing to change.",
                       "I was told to end with “VERDICT: request-changes” or approve."):
            self.assertFalse(asks(quoted), quoted)
        self.assertTrue(asks("It is broken, so VERDICT: request-changes"))
        self.assertTrue(asks('Quoting "fine" aside, VERDICT: request-changes'))

    def test_the_review_decides_on_its_last_verdict_line(self):
        f = self.fixture(["actionable"], ["good"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write(r'approve (I rejected \"VERDICT: request-changes\")' + "\n")
        plan, _ = f.run()
        self.assertEqual((plan["outcome"], f.calls()), ("plan", ["triage", "fix", "review"]), plan)
        asks = fix.review_requests_changes
        self.assertFalse(asks("End with VERDICT: request-changes or approve.\nVERDICT: approve"))
        self.assertTrue(asks("VERDICT: approve\nOn reflection:\n**VERDICT: request-changes**"))
        self.assertFalse(asks("VERDICT: request-changes\nVERDICT: approve"))
        self.assertTrue(asks("no verdict line, but VERDICT: request-changes mid-line"))
        self.assertFalse(asks("looks fine"))

    def test_only_the_fixer_may_change_the_tree(self):
        """SR-0001 H8: the verdict, the advisor and the review read; only the fix step gets pi's default
        tools (the advisor once started a dev server). Read-only is a tool allowlist, not a prompt."""
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        f.run()
        with open(os.path.join(f.state, "calls")) as fh:
            raw = [ln for ln in fh.read().splitlines() if ln.strip()]
        lines = [ln.split() for ln in raw]
        tools = {ln[0]: [w for w in ln if w.startswith("tools=")] for ln in lines}
        for step in ("triage", "advisor"):
            self.assertEqual(tools[step], ["tools=read,grep,find,ls"], step)
        self.assertEqual(tools["review"], ["tools=none"], "C4: the review is given what it judges, and gets no tools")
        self.assertEqual(tools["fix"], ["tools=all"])
        for leaf in ("triage_run.sh", "advisor_run.sh"):
            with open(os.path.join(fix.BAKEOFF, leaf)) as fh:
                self.assertIn('${PI_TOOLS:+--tools "$PI_TOOLS"}', fh.read(), leaf)

    def test_a_new_failure_kicks_back_and_needs_info_escalates(self):
        """The fix breaks src/b.js, which keep.test.js (related, passing on the base) checks."""
        f = self.fixture(["actionable", "needs-info"], ["breaks"])
        plan, out = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("after round 0", plan["detail"])
        with open(os.path.join(f.state, "calls")) as fh:
            self.assertNotIn("review", fh.read())

    def test_the_acceptance_transition(self):
        """A fix at the advisor's pin that still fails is an acceptance gap: no second advisor, and a verdict
        that comes back actionable is forced to needs-info (chain.go, measured 2026-07-26)."""
        f = self.fixture(["actionable", "actionable", "actionable"], ["notest", "notest"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("needs info", plan["detail"])
        self.assertEqual(f.calls(), ["triage", "fix", "advisor", "triage", "fix", "triage"])
        with open(os.path.join(f.state, "calls")) as fh:
            self.assertIn("gap=1", fh.read().splitlines()[-1])

    def test_scope_of_the_tests(self):
        """RFC-0010 §9: a test that passes on the base does not reproduce; a new failure in a related test
        stops the fix; a lockfile is out of scope. "A repro test must load and fail on the base": a test that
        cannot load on the base (it imports what the fix adds) shows no behaviour, so it is noted, and the
        fix needs at least one other test that loads and fails there (phase B: 2 of the first 4 plans
        passed the gates and were wrong)."""
        cases = {"norepro": "repro: src/a.test.js passes on the base without the fix",
                 "breaks": "new failures in the related tests: keep.test.js",
                 "lockfile": "out of scope: the diff touches package-lock.json",
                 "halffix": "the fix's own tests fail with the fix: src/a.test.js",
                 "newmod": "repro: no test loads and fails on the base (src/c.test.js does not load there)"}
        for fix_, want in cases.items():
            f = self.fixture(["actionable"], [fix_])
            plan, _ = f.run("--kickback-max", "0")
            self.assertEqual(plan["outcome"], "escalate", fix_)
            self.assertIn(want, plan["detail"], fix_)
        f = self.fixture(["actionable"], ["goodnewmod"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("Not loadable on the base (they import what the fix adds): src/c.test.js", plan["params"]["body"])

    def test_base_and_harness_seams(self):
        """Phase B's seams: --base fixes a given commit; --harness picks harnesses/<name>.sh."""
        f = self.fixture(["actionable"], ["good"])
        shutil.copy(os.path.join(f.bakeoff, "harnesses", "pi.sh"), os.path.join(f.bakeoff, "harnesses", "other.sh"))
        plan, _ = f.run("--base", f.base, "--harness", "other")
        self.assertEqual((plan["outcome"], plan["params"]["base"]), ("plan", f.base), plan)
        g = self.fixture(["actionable"], ["good"])
        plan, _ = g.run("--harness", "missing")
        self.assertEqual(plan["outcome"], "escalate", "a harness that is not there")

    def test_the_kickback_limit(self):
        f = self.fixture(["actionable", "actionable"], ["notest", "nothing"])
        plan, _ = f.run("--kickback-max", "1", "--no-advisor", "--no-resume")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("stuck: kick-back limit (1) reached; last: the fix changed nothing", plan["detail"])
        # resumed, the tree still holds round 0's change: a session that adds nothing is caught as such
        g = self.fixture(["actionable", "actionable"], ["notest", "nothing"])
        plan, _ = g.run("--kickback-max", "1", "--no-advisor")
        self.assertIn("stuck: kick-back limit (1) reached; last: the fix changed nothing since its last round", plan["detail"])
        self.assertEqual(f.calls(), ["triage", "fix", "triage", "fix"])

    def test_a_rename_out_of_a_forbidden_path_is_seen_on_the_vitest_path_too(self):
        f = self.fixture(["actionable"], ["jsrename"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("out of scope: the diff touches package.json", plan["detail"])

    def test_a_fix_may_not_delete_a_test(self):
        """vitest related cannot see a deleted *.test.js: a round that removes the test its change breaks
        would pass. A rename out of a test path is a delete too."""
        for fix_, name in (("jsdeltest", "keep.test.js"), ("jsmovetest", "keep.test.js")):
            f = self.fixture(["actionable"], [fix_])
            plan, _ = f.run("--kickback-max", "0")
            self.assertEqual(plan["outcome"], "escalate", fix_)
            self.assertIn(f"a fix may not delete a test: {name}", plan["detail"], fix_)

    def test_out_of_scope(self):
        f = self.fixture(["actionable"], ["workflow"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("last: out of scope: the diff touches .github/workflows/x.yml", plan["detail"])

    def test_a_case_without_an_issue(self):
        f = self.fixture([], [])
        old = os.environ.get("FLOTILLA_CASE")
        plan_path = os.path.join(f.d, "p.json")
        out = fix.main(["--repo", "example/shop", "--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer",
                        "--reviewer-model", "bugfix-reviewer", "--scratch", f.scratch],
                       environ={"FLOTILLA_CASE": json.dumps({"id": "c", "data": {}}), "FLOTILLA_PLAN_FILE": plan_path,
                                "FLOTILLA_MODELS": json.dumps(MODELS), "PATH": os.environ["PATH"]})
        self.assertEqual(out["outcome"], "escalate")
        self.assertIn("no issue", out["detail"])
        self.assertEqual(old, os.environ.get("FLOTILLA_CASE"))

    def rules(self, f):
        with open(os.path.join(f.state, "rules")) as fh:
            return fh.read().splitlines()

    def test_every_pi_step_gets_the_fleet_rules_not_the_repos_agents_md(self):
        # C2 (the research repo's projects/bugfix-cost): pi loads the repo's AGENTS.md into every call by default,
        # ~30k tokens on shop; the verdict, the fix and the review each get the fleet's rule file instead
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        path = os.path.join(os.path.dirname(os.path.abspath(fix.__file__)), "fleet_rules.md")
        self.assertTrue(os.path.isfile(path))
        # the review has no tools (C4a), so it gets no rules about using them: only --no-context-files
        self.assertEqual(self.rules(f), [f"verdict {path}", f"fix {path}", "review none no-context-files=yes"])

    def review_prompt(self, f):
        with open(os.path.join(f.state, "review_prompt")) as fh:
            return fh.read()

    def test_the_review_is_given_the_issue_the_diff_and_the_changed_files(self):
        # C4: one call on what it judges, no tools (it explored for 5 to 38 calls on glm-5.2 before)
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        p = self.review_prompt(f)
        self.assertIn("THE ISSUE AS FILED:\nvalue() returns 1, want 2", p)
        self.assertIn("DIFF:\ndiff --git", p)
        self.assertIn("=== src/a.js ===\nmodule.exports = () => 2;", p)
        self.assertIn("=== src/a.test.js ===\nexpect src/a.js 2", p)
        self.assertIn("You have no tools", p)
        self.assertTrue(p.rstrip().endswith("VERDICT: request-changes CLASS: <one of the classes>."))

    def test_review_tools_read_keeps_the_exploring_review(self):
        f = self.fixture(["actionable"], ["good"])
        f.run("--review-tools", "read")
        with open(os.path.join(f.state, "calls")) as fh:
            review = [ln for ln in fh.read().splitlines() if ln.startswith("review ")]
        self.assertEqual([w for w in review[0].split() if w.startswith("tools=")], ["tools=read,grep,find,ls"])
        self.assertNotIn("You have no tools", self.review_prompt(f))

    def test_a_large_changed_file_is_cut_to_fit_one_argument(self):
        # pi takes the prompt as one argv string, which Linux caps at 128 KiB: a one-line fix in a 300 KB file
        # (a plan's diff is small, the file is not) must still give a prompt pi can be started with
        import types
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        git("init", "-q", d, cwd=d)
        os.makedirs(os.path.join(d, "src"))
        with open(os.path.join(d, "src", "big.js"), "w") as fh:
            fh.write("module.exports = () => 1;\n" + "x" * 300000 + "\n")
        git("add", "-A", cwd=d)
        git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base", cwd=d)
        base = subprocess.run(["git", "rev-parse", "HEAD"], cwd=d, capture_output=True, text=True).stdout.strip()
        with open(os.path.join(d, "src", "big.js"), "r+") as fh:
            fh.write("module.exports = () => 2;")
        git("add", "-A", cwd=d)
        chain = types.SimpleNamespace(repo="example/shop", n=12, issue={"title": "t", "body": "value() returns 1, want 2"},
                                      a=types.SimpleNamespace(review_tools="none"), wt=d, base=base, run_dir=d, env=dict(os.environ))
        p = fix.Chain.review_prompt(chain, "diff --git a/src/big.js b/src/big.js\n-1\n+2\n")
        self.assertLessEqual(len(p.encode()), fix.REVIEW_PROMPT_MAX)
        self.assertIn("=== src/big.js ===\nmodule.exports = () => 2;", p)
        self.assertIn("[... cut:", p)
        self.assertIn("THE ISSUE AS FILED:\nvalue() returns 1, want 2", p)
        self.assertTrue(p.rstrip().endswith("VERDICT: request-changes CLASS: <one of the classes>."))

    def declared(self, f, data=None, sha=None):
        # a code root whose fleets/bugfix/playbooks/rules.md is the declared prompt (ADR-0018)
        root = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, root, True)
        os.makedirs(os.path.join(root, "fleets", "bugfix", "playbooks"))
        data = data if data is not None else b"declared rules\n"
        with open(os.path.join(root, "fleets", "bugfix", "playbooks", "rules.md"), "wb") as fh:
            fh.write(data)
        import hashlib
        f.extra_env = {"FLOTILLA_CODE_ROOT": root, "FLOTILLA_ROUTINE": "bugfix/fix",
                       "FLOTILLA_PROMPTS": json.dumps({"rules": {"path": "playbooks/rules.md",
                                                                 "sha256": sha or hashlib.sha256(data).hexdigest()}})}
        return os.path.join(root, "fleets", "bugfix", "playbooks", "rules.md")

    def test_the_declared_rules_prompt_is_what_pi_gets(self):
        f = self.fixture(["actionable"], ["good"])
        path = self.declared(f)
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(self.rules(f), [f"verdict {path}", f"fix {path}", "review none no-context-files=yes"])

    def test_a_rules_file_that_is_not_the_declared_one_is_refused_before_any_step(self):
        f = self.fixture(["actionable"], ["good"])
        self.declared(f, sha="0" * 64)
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("differs from the one flotillad checked", plan["detail"])
        self.assertFalse(os.path.exists(os.path.join(f.state, "rules")))

    def test_an_explicit_pi_rules_wins_over_the_declared_prompt(self):
        f = self.fixture(["actionable"], ["good"])
        self.declared(f)
        f.run("--pi-rules", "")
        self.assertEqual(self.rules(f), ["verdict none", "fix none", "review none no-context-files=no"])

    def test_the_fleets_rules_prompt_is_the_default_rules_file(self):
        # the example fleet declares fleets/bugfix/fix/v1/rules.md; without flotillad the default is
        # bugfix/fleet_rules.md: one text, so a run behaves alike either way
        root = os.path.dirname(os.path.dirname(os.path.abspath(fix.__file__)))
        with open(os.path.join(root, "bugfix", "fleet_rules.md"), "rb") as a, \
                open(os.path.join(root, "fleets", "bugfix", "fix", "v1", "rules.md"), "rb") as b:
            self.assertEqual(a.read(), b.read())

    def test_an_empty_pi_rules_keeps_pis_default(self):
        f = self.fixture(["actionable"], ["good"])
        plan, _ = f.run("--pi-rules", "")
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(self.rules(f), ["verdict none", "fix none", "review none no-context-files=no"])

    def test_the_bakeoff_scripts_pass_the_rules_to_pi(self):
        # the image ships these three scripts (deploy/docker/worker-bugfix.Dockerfile); each pi call carries the rules
        bakeoff = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(fix.__file__))), "bugfix-fleet", "bakeoff")
        for name in ("harnesses/pi.sh", "triage_run.sh", "advisor_run.sh"):
            with open(os.path.join(bakeoff, name)) as fh:
                calls = [x for x in fh.read().splitlines() if re.search(r"\bpi -p\b", x) and not x.lstrip().startswith("#")]
            self.assertTrue(calls, name)
            for x in calls:
                self.assertIn('${BUGFIX_PI_RULES:+--no-context-files --append-system-prompt "$BUGFIX_PI_RULES"}', x, name)

    def test_a_run_stopped_at_its_timeout_still_reports_its_spend(self):
        # the worker SIGTERMs the run's process group at the routine's timeout and SIGKILLs it 10 s later
        # (worker/flotilla_worker.py KILL_GRACE_S); the spend must be written inside those 10 s
        import signal
        import time
        import test_spend
        f = self.fixture(["actionable"], ["hang"])
        g = test_spend.Gateway([2.0, 2.25, 2.25])
        self.addCleanup(g.close)
        kf = os.path.join(f.d, "key")
        with open(kf, "w") as fh:
            fh.write("sk-test")
        models = {"endpoints": {"gw": {"base_url": g.base, "kind": "litellm", "key_file": kf}},
                  "aliases": {"bugfix-flash": {"endpoint": "gw", "model": "fleet-bugfix-flash"},
                              "bugfix-reviewer": {"endpoint": "gw", "model": "fleet-bugfix-reviewer"}}}
        home = os.path.join(f.d, "home")
        os.makedirs(home)
        env = dict(os.environ, FLOTILLA_PLAN_FILE=os.path.join(f.d, "plan.json"), FLOTILLA_MODELS=json.dumps(models),
                   FAKE_STATE=f.state, GIT_ALLOW_PROTOCOL="file", GIT_REMOTE_BASE=f.remotes, HOME=home, BUGFIX_BAKEOFF_DIR=f.bakeoff,
                   PATH=os.path.join(f.d, "bin") + os.pathsep + os.environ["PATH"],
                   GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_CASE=json.dumps({"id": "case-20261115-abc", "kind": "code_bug", "target": "repo:example/shop/issues/12",
                                             "data": {"issue": {"repo": "example/shop", "number": 12, "title": "value is 1",
                                                                "body": "value() returns 1, want 2", "url": "u"}}}))
        p = subprocess.Popen([sys.executable, os.path.abspath(fix.__file__), "--repo", "example/shop", "--worker-model", "bugfix-flash",
                              "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer", "--setup", "true",
                              "--test-cmd", f"python3 {f.suite} run %s", "--related-cmd", f"python3 {f.suite} related %s",
                              "--scratch", f.scratch, "--spend-file", ".spend", "--spend-poll", "0.1", "--no-repro"],
                             env=env, start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: p.poll() is None and os.killpg(p.pid, signal.SIGKILL))
        calls = os.path.join(f.state, "calls")
        for _ in range(300):   # until the fix step hangs
            if os.path.exists(calls) and "fix hang" in open(calls).read():
                break
            time.sleep(0.1)
        with open(os.path.join(f.state, "harness.pid")) as fh:
            harness = int(fh.read())
        os.killpg(p.pid, signal.SIGTERM)
        p.wait(timeout=10)
        with open(os.path.join(home, ".spend")) as fh:
            self.assertEqual(float(fh.read()), 0.25)
        # the fix step runs in its own session (sh), out of the worker's reach: fix.py must stop it, or the
        # coding agent goes on calling the model after the run has ended
        for _ in range(50):
            try:
                os.kill(harness, 0)
            except ProcessLookupError:
                break
            time.sleep(0.1)
        else:
            os.kill(harness, signal.SIGKILL)
            self.fail("the fix step outlived the run")

    def test_pi_sh_passes_the_rules_only_when_set(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        write(os.path.join(d, "bin", "pi"), 'printf "%s\\n" "$@" > "$OUT"\n')
        harness = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(fix.__file__))), "bugfix-fleet", "bakeoff",
                               "harnesses", "pi.sh")
        for rules, want in (("/r.md", ["--no-context-files", "--append-system-prompt", "/r.md"]), ("", [])):
            out = os.path.join(d, "args")
            env = dict(os.environ, PATH=os.path.join(d, "bin") + os.pathsep + os.environ["PATH"], OUT=out, BUGFIX_PI_RULES=rules)
            subprocess.run(["bash", harness, d, "the bug"], env=env, check=True, capture_output=True)
            with open(out) as fh:
                args = fh.read().splitlines()
            got = [a for a in args if a in ("--no-context-files", "--append-system-prompt", "/r.md")]
            self.assertEqual(got, want, rules)


class PythonRepo(unittest.TestCase):
    """The chain on a Python-shaped repo: --test-format pytest, --related pytest-dirs."""

    def fixture(self, verdicts, fixes):
        f = Fixture(verdicts, fixes, py=True)
        self.addCleanup(f.close)
        return f

    def test_a_clean_fix_goes_through_repro_the_base_check_related_and_the_gates(self):
        f = self.fixture(["actionable"], ["pygood"])
        plan, _ = f.run()
        self.assertEqual((plan["outcome"], plan["action"]), ("plan", "github.open_pr"), plan)
        p = plan["params"]
        self.assertEqual(p["repo"], "example/api")
        self.assertIn("1 test files fail on the base and pass with the fix", p["body"])
        self.assertIn("4 related files, none newly failing", p["body"], "test_keep, test_red, sub/test_s and the fix's test_mod under pkg/")
        self.assertIn("related: tests/unit/api/pkg", p["body"])
        self.assertNotIn("flotilla-suite", p["diff"])

    def test_a_new_failure_in_the_mirrored_directory_stops_the_fix(self):
        f = self.fixture(["actionable"], ["pybreaks"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("new failures in the related tests: tests/unit/api/pkg/test_keep.py", plan["detail"])

    def test_no_changed_source_file_maps_to_a_test_directory_fails_the_gate(self):
        """Nothing related would run: the gate fails closed (a kick-back), never widens to the whole suite."""
        for fix_, what in (("pylone", "src/api/lone/z.py"), ("pyinit", None)):
            f = self.fixture(["actionable"], [fix_])
            plan, _ = f.run("--kickback-max", "0")
            self.assertEqual(plan["outcome"], "escalate", fix_)
            self.assertIn("related: no changed source file under src/api maps to a test directory", plan["detail"], fix_)
            if what:
                self.assertIn(what, plan["detail"])
                self.assertIn("its tests belong in tests/unit/api/lone/", plan["detail"])
            else:
                self.assertIn("its tests belong in tests/unit/api/test___init__.py", plan["detail"])

    def test_a_source_file_with_no_mirror_is_in_the_checked_line(self):
        f = self.fixture(["actionable"], ["pypartial"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("no mirrored test directory for src/api/lone/z.py", plan["params"]["body"].split("The chain:")[0])

    def test_a_fixture_that_exists_only_with_the_fix_does_not_reproduce(self):
        f = self.fixture(["actionable"], ["pyfixture"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("repro: no test loads and fails on the base", plan["detail"])

    def test_a_nested_mirrored_directory_is_run_once_under_its_parent(self):
        f = self.fixture(["actionable"], ["pynested"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        body = plan["params"]["body"]
        self.assertIn("- related: tests/unit/api/pkg\n", body)
        self.assertIn("4 related files", body)

    def test_a_fix_whose_only_test_is_skipped_does_not_reproduce(self):
        f = self.fixture(["actionable"], ["pyskip"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("repro: no test loads and fails on the base", plan["detail"])

    def test_the_pytest_config_and_collection_files_are_out_of_scope_without_a_flag(self):
        for fix_, want in (("pyconf", "pyproject.toml"), ("pyconftest", "tests/unit/api/pkg/conftest.py")):
            f = self.fixture(["actionable"], [fix_])
            plan, _ = f.run("--kickback-max", "0")
            self.assertIn(f"out of scope: the diff touches {want}", plan["detail"], fix_)

    def test_the_patterns(self):
        for p in ("pyproject.toml", "Makefile", "setup.py", "setup.cfg", "requirements.txt", "requirements-dev.txt", "requirements/dev.txt",
                  "conftest.py", "tests/conftest.py", "tests/unit/api/conftest.py", "pytest.ini", "a/tox.ini", ".coveragerc",
                  "uv.lock", "poetry.lock", "constraints.txt", "constraints-ci.txt", ".pre-commit-config.yaml",
                  "sitecustomize.py", "x/usercustomize.py", "a.pth"):
            self.assertTrue(fix.PYTEST_OUT_OF_SCOPE.search(p), p)
        for p in ("src/api/a.py", "tests/unit/api/test_a.py", "docs/guide.md", "src/api/requirements_check.py"):
            self.assertFalse(fix.PYTEST_OUT_OF_SCOPE.search(p), p)

    def test_a_conftest_alone_is_out_of_scope_and_named(self):
        f = self.fixture(["actionable"], ["pyconftestonly"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertIn("out of scope: the diff touches tests/unit/api/pkg/conftest.py (pytest config, collection or install file)",
                      plan["detail"])

    def test_python_that_pytest_imports_outside_a_test_file_is_out_of_scope(self):
        """cwd and PYTHONPATH put the repo root first on sys.path: a top-level json.py shadows the installed
        module; a tests/ __init__.py runs at package collection and a helper module when it is imported."""
        cases = {"pyshadow": ("json.py", "a .py outside src/api and tests/unit/api"),
                 "pyinitfile": ("tests/unit/api/pkg/__init__.py", "only test files (test_*.py, *_test.py) may change"),
                 "pyhelper": ("tests/unit/api/pkg/helpers.py", "only test files (test_*.py, *_test.py) may change")}
        for fix_, (path, rule) in cases.items():
            f = self.fixture(["actionable"], [fix_])
            plan, _ = f.run("--kickback-max", "0")
            self.assertEqual(plan["outcome"], "escalate", fix_)
            self.assertIn(f"out of scope: the diff touches {path} (", plan["detail"], fix_)
            self.assertIn(rule, plan["detail"], fix_)

    def test_test_files_may_be_named_either_way(self):
        f = self.fixture(["actionable"], ["pysuffix"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)

    def test_pytest_needs_both_roots(self):
        with self.assertRaises(SystemExit):
            fix.parse(["--repo", "o/r", "--worker-model", "a", "--advisor-model", "b", "--reviewer-model", "c", "--test-format", "pytest"])

    def test_the_allowlist_names_every_other_changed_file(self):
        """Under pytest a changed path is allowed only if it is a .py under the source root or a test_*.py /
        *_test.py under the test root: config, data, docs and non-.py files anywhere are rejected."""
        for fix_, files in (("pyjson", ["tests/unit/api/pkg/data/x.json"]),
                            ("pydata", ["src/api/pkg/data.json", "README.md"]),
                            ("pyconfigs", [".pytest.ini", "pytest.toml", ".pytest.toml"])):
            f = self.fixture(["actionable"], [fix_])
            plan, _ = f.run("--kickback-max", "0")
            self.assertEqual(plan["outcome"], "escalate", fix_)
            for x in files:
                self.assertIn(f"{x} (", plan["detail"], (fix_, x))

    def test_the_blocklist_names_the_usual_config_files(self):
        f = self.fixture(["actionable"], ["pyconfigs"])
        plan, _ = f.run("--kickback-max", "0")
        for x in (".pytest.ini", "pytest.toml", ".pytest.toml"):
            self.assertIn(f"{x} (pytest config, collection or install file)", plan["detail"])

    def test_a_name_that_starts_with_a_quote_fails_closed(self):
        f = self.fixture(["actionable"], ["pyquote"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertIn('"q".py (a quoted path name', plan["detail"])

    def test_a_renamed_source_file_is_a_delete_and_an_add_in_the_plan(self):
        f = self.fixture(["actionable"], ["pyrenamesrc"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan", plan)
        d = plan["params"]["diff"]
        self.assertNotIn("rename from", d)
        self.assertIn("deleted file mode", d)
        self.assertIn("+++ b/src/api/pkg/z.py", d)

    def test_a_fix_may_not_delete_a_test(self):
        f = self.fixture(["actionable"], ["pydeltest"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("a fix may not delete a test: tests/unit/api/pkg/test_keep.py", plan["detail"])

    def test_a_rename_out_of_a_forbidden_path_is_seen(self):
        f = self.fixture(["actionable"], ["pyrename"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("conftest.py (pytest config, collection or install file)", plan["detail"])

    def test_a_non_ascii_path_is_not_quoted_past_the_scope(self):
        f = self.fixture(["actionable"], ["pyunicode"])
        plan, _ = f.run("--kickback-max", "0")
        self.assertEqual(plan["outcome"], "escalate")
        self.assertIn("tests/unit/api/é/conftest.py (pytest config", plan["detail"])
        g = self.fixture(["actionable"], ["pyunicodesrc"])
        plan, _ = g.run("--kickback-max", "0")
        self.assertIn("src/api/pkg/é.txt (", plan["detail"])
        self.assertNotIn('"', plan["detail"].split("touches")[1][:5], "the name is not git's quoted form")

    def test_the_pip_cache_is_the_run_s_own(self):
        f = self.fixture(["actionable"], ["pygood"])
        seen = os.path.join(f.d, "pip.txt")
        f.run("--setup", f'echo "$PIP_CACHE_DIR" > {seen}')
        with open(seen) as fh:
            cache = fh.read().strip()
        self.assertTrue(cache.startswith(os.path.join(f.scratch, "runs") + os.sep), cache)
        self.assertFalse(os.path.exists(cache), "removed with the run")
        self.assertFalse(os.path.exists(os.path.join(f.scratch, "pip-cache")))


RESUME = {"run_id": "run-1", "status": "interrupted"}


class Durable(unittest.TestCase):
    """ADR-0019: a run cut short leaves runs/<case id>/ and its checkpoint; the case's next run, when flotillad lets it
    (FLOTILLA_RESUME), goes on from the last boundary. A kill is a Terminated raised where the worker's SIGTERM or
    SIGKILL would have ended the run: at the boundary named, before it or after the step in flight."""

    CASE = "case-20261115-abc"

    def fixture(self, verdicts, fixes):
        f = Fixture(verdicts, fixes)
        self.addCleanup(f.close)
        return f

    def run_dir(self, f):
        return os.path.join(f.scratch, "runs", self.CASE)

    def checkpoint(self, f):
        with open(os.path.join(self.run_dir(f), fix.CHECKPOINT)) as fh:
            return json.load(fh)

    def kill(self, f, name, *extra, nth=1):
        """Run f's chain with Chain.<name> raising Terminated on its nth call, before it does anything; "pi" is the fix
        episode, killed after it ran (the session grown, the tree changed, nothing read of it)."""
        real_sh, real, n = fix.sh, getattr(fix.Chain, name, None), [0]

        def hit():
            n[0] += 1
            return n[0] == nth

        def sh(cmd, *a, **kw):
            out = real_sh(cmd, *a, **kw)
            if isinstance(cmd, list) and cmd[0].endswith("pi.sh") and hit():
                raise fix.Terminated()
            return out

        def method(chain, *a, **kw):
            if hit():
                raise fix.Terminated()
            return real(chain, *a, **kw)
        if name == "pi":
            fix.sh = sh
        else:
            setattr(fix.Chain, name, method)
        try:
            with self.assertRaises(fix.Terminated):
                f.run(*extra)
        finally:
            fix.sh = real_sh
            if name != "pi":
                setattr(fix.Chain, name, real)

    def resumes(self, f):
        path = os.path.join(f.state, "resumes")
        if not os.path.exists(path):
            return []
        with open(path) as fh:
            return fh.read().splitlines()

    def test_a_kill_at_each_boundary_resumes_where_it_stopped(self):
        for name, phase, fixes, calls in (
                ("verdict", "checkout", ["good"], ["triage", "fix", "review"]),
                ("attempt", "attempt", ["good"], ["triage", "fix", "review"]),
                ("pi", "attempt", ["good", "nothing"], ["triage", "fix", "fix", "review"]),
                ("gates", "fixed", ["good"], ["triage", "fix", "review"]),
                ("review", "gated", ["good"], ["triage", "fix", "review"]),
                ("plan", "reviewed", ["good"], ["triage", "fix", "review"])):
            with self.subTest(killed=name):
                f = self.fixture(["actionable"], fixes)
                self.kill(f, name)
                self.assertEqual(self.checkpoint(f)["phase"], phase)
                plan, _ = f.run(resume=RESUME)
                self.assertEqual(plan["outcome"], "plan", plan)
                self.assertIn("+module.exports = () => 2;", plan["params"]["diff"])
                self.assertIn(f"(resumed from a checkpoint: phase {phase}, round 0, saved ", plan["detail"])
                self.assertIn("resumed from checkpoint: phase " + phase, plan["params"]["body"])
                self.assertEqual(f.calls(), calls, "each leaf ran as often as an uninterrupted run needs, bar the continued episode")
                self.assertFalse(os.path.exists(self.run_dir(f)), "the normal end removes the directory and the checkpoint")
                if name == "pi":
                    r = self.resumes(f)
                    self.assertEqual(len(r), 1, r)
                    self.assertRegex(r[0], r"^resume s-good-\d+\.jsonl changed=2 desc=The run was interrupted")

    def test_a_kill_after_the_proof_resumes_held_to_the_same_frozen_test(self):
        # the reproducer's test is in every checkpoint after it: a resumed run does not write another proof, its rounds
        # are held to the frozen one, and the review is told it is frozen
        for name, phase in (("attempt", "attempt"), ("pi", "attempt"), ("gates", "fixed"), ("review", "gated"),
                            ("plan", "reviewed")):
            with self.subTest(killed=name):
                f = self.fixture(["actionable"], ["srconly", "srconly"])
                with open(os.path.join(f.state, "repros"), "w") as fh:
                    fh.write("good\n")
                f.repro = True
                self.kill(f, name)
                ck = self.checkpoint(f)
                self.assertEqual(ck["phase"], phase)
                self.assertEqual(list(ck["state"]["repro"]["files"]), ["src/a.test.js"])
                plan, _ = f.run(resume=RESUME)
                self.assertEqual(plan["outcome"], "plan", plan)
                self.assertEqual([c for c in f.calls() if c.startswith("repro")], ["repro"], "one proof, not two")
                self.assertIn("+++ b/src/a.test.js", plan["params"]["diff"], "the frozen test ships with the fix")
                if name != "plan":   # killed before its review: the resumed review is told the proof is frozen
                    with open(os.path.join(f.state, "review_prompt")) as fh:
                        self.assertIn("THE FROZEN PROOF: src/a.test.js", fh.read())

    def test_a_kill_in_a_kick_back_redoes_the_kick_back_and_resumes_the_session(self):
        f = self.fixture(["actionable", "actionable"], ["notest", "good"])
        self.kill(f, "verdict", nth=2)
        ck = self.checkpoint(f)
        self.assertEqual((ck["phase"], ck["state"]["round"]), ("gated", 0))
        self.assertIn("repro-first violated", ck["gated"]["reason"])
        plan, _ = f.run(resume=RESUME)
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("resumed from a checkpoint: phase gated, round 0", plan["detail"])
        self.assertEqual(f.calls(), ["triage", "fix", "advisor", "advisor", "triage", "fix", "review"])
        r = self.resumes(f)
        self.assertEqual(len(r), 1, r)
        self.assertRegex(r[0], r"^resume s-notest-\d+\.jsonl changed=1 desc=YOUR FIX DID NOT PASS ITS GATES")

    def test_a_review_kick_back_is_kept_across_a_kill(self):
        f = self.fixture(["actionable"], ["good", "goodnewmod"])
        with open(os.path.join(f.state, "reviews"), "w") as fh:
            fh.write("request-changes\napprove\n")
        self.kill(f, "attempt", nth=2)   # killed as round 1 begins, the review's request in the checkpoint
        ck = self.checkpoint(f)
        self.assertEqual((ck["phase"], ck["state"]["round"], ck["state"]["reviewed_back"]), ("attempt", 1, 1))
        self.assertEqual(ck["state"]["kept"]["round"], 0)
        plan, _ = f.run(resume=RESUME)
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(f.calls(), ["triage", "fix", "review", "fix", "review"], "the review of round 0 is not asked again")
        r = self.resumes(f)
        self.assertEqual(len(r), 1, r)
        self.assertRegex(r[0], r"^resume s-good-\d+\.jsonl changed=2 desc=A REVIEW OF YOUR FIX ASKED FOR CHA")

    def test_a_run_flotillad_did_not_resume_starts_fresh(self):
        f = self.fixture(["actionable", "actionable"], ["good"])
        self.kill(f, "attempt")
        self.assertTrue(os.path.exists(os.path.join(self.run_dir(f), fix.CHECKPOINT)))
        plan, _ = f.run()   # no FLOTILLA_RESUME: a reassign, an escalation, a first run
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertNotIn("resumed", plan["detail"])
        self.assertEqual(f.calls(), ["triage", "triage", "fix", "review"])

    def test_a_checkpoint_that_does_not_hold_is_ignored_and_the_case_starts_fresh(self):
        def edit(**kw):
            def go(f, ck):
                ck.update(kw)
            return go

        def state(**kw):
            def go(f, ck):
                ck["state"].update(kw)
            return go

        def outside(f, ck):   # a path that leaves the run directory, to a file that exists
            with open(os.path.join(f.scratch, "runs", "outside.json"), "w") as fh:
                fh.write("{}")
            ck["state"]["manifest"] = "../outside.json"

        def symlink(f, ck):
            target = os.path.join(f.scratch, "runs", "outside2.json")
            with open(target, "w") as fh:
                fh.write("{}")
            link = os.path.join(self.run_dir(f), "link.json")
            os.symlink(target, link)
            ck["state"]["prior"] = "link.json"

        def absolute(f, ck):
            ck["state"]["manifest"] = os.path.join(f.d, "bakeoff", "triage_run.sh")

        def old(f, ck):
            ck["saved"] = time.time() - fix.CHECKPOINT_MAX_AGE - 60

        for name, change in {"another case": edit(case="case-other"), "another repo": edit(repo="example/api"),
                             "another ref": edit(ref="release"), "an unknown version": edit(version=2),
                             "an unknown phase": edit(phase="done"), "a base that is not the worktree's": edit(base="0" * 40),
                             "older than the limit": old, "a path outside the run directory": outside,
                             "a symlink out of it": symlink, "an absolute path": absolute,
                             "a round that is not a number": state(round="1"), "pins that are not names": state(pins=[1]),
                             "a log that is not text": edit(log=[1])}.items():
            with self.subTest(name):
                f = self.fixture(["actionable", "actionable"], ["good"])
                self.kill(f, "attempt")
                ck = self.checkpoint(f)
                change(f, ck)
                with open(os.path.join(self.run_dir(f), fix.CHECKPOINT), "w") as fh:
                    json.dump(ck, fh)
                plan, _ = f.run(resume=RESUME)
                self.assertEqual(plan["outcome"], "plan", plan)
                self.assertNotIn("resumed", plan["detail"])
                self.assertEqual(f.calls(), ["triage", "triage", "fix", "review"], "started fresh")
        for name, damage in {"not JSON": lambda p: Path(p).write_text("{"), "gone": os.remove}.items():
            with self.subTest(name):
                f = self.fixture(["actionable", "actionable"], ["good"])
                self.kill(f, "attempt")
                damage(os.path.join(self.run_dir(f), fix.CHECKPOINT))
                plan, _ = f.run(resume=RESUME)
                self.assertEqual((plan["outcome"], f.calls()), ("plan", ["triage", "triage", "fix", "review"]))

    def test_stale_run_directories_are_deleted_at_start_and_recent_ones_are_not(self):
        f = self.fixture(["actionable"], ["good"])
        for name, age in (("case-old", fix.CHECKPOINT_MAX_AGE + 3600), ("case-young", 3600)):
            d = os.path.join(f.scratch, "runs", name)
            os.makedirs(os.path.join(d, "wt"))
            os.makedirs(os.path.join(f.scratch, "venvs", name))
            with open(os.path.join(d, fix.CHECKPOINT), "w") as fh:
                fh.write("{}")
            for p in (os.path.join(d, fix.CHECKPOINT), d):
                os.utime(p, (time.time() - age, time.time() - age))
        f.run()
        self.assertEqual(sorted(os.listdir(os.path.join(f.scratch, "runs"))), ["case-young"])
        self.assertEqual(sorted(os.listdir(os.path.join(f.scratch, "venvs"))), ["case-young"])

    def test_an_escalation_is_a_normal_end_and_an_error_is_not(self):
        f = self.fixture(["needs-info"], [])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertFalse(os.path.exists(self.run_dir(f)), "an escalation leaves nothing to resume")
        g = self.fixture(["actionable"], ["good"])
        real = fix.Chain.attempt

        def boom(chain, *a, **kw):
            raise RuntimeError("a bug")
        fix.Chain.attempt = boom
        try:
            with self.assertRaises(RuntimeError):
                g.run()
        finally:
            fix.Chain.attempt = real
        self.assertEqual(self.checkpoint(g)["phase"], "attempt", "an error keeps the directory: status error may be resumed")
        plan, _ = g.run(resume={"run_id": "run-1", "status": "error"})
        self.assertEqual(plan["outcome"], "plan", plan)

    def test_a_resumed_run_records_only_its_own_calls_and_says_it_resumed(self):
        f = self.fixture(["actionable"], ["good", "nothing"])
        first, second = os.path.join(f.d, "calls1.jsonl"), os.path.join(f.d, "calls2.jsonl")
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1", "FLOTILLA_CALLS_FILE": first}):
            self.kill(f, "pi")
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1", "FLOTILLA_CALLS_FILE": second}):
            plan, _ = f.run(resume=RESUME)
        self.assertEqual(plan["outcome"], "plan", plan)
        with open(first) as fh:
            before = [json.loads(x) for x in fh]
        with open(second) as fh:
            rows = [json.loads(x) for x in fh]
        self.assertEqual([c["role"] for c in before if c.get("kind") != "step"], ["verdict"], "the killed run recorded its verdict only")
        self.assertEqual([c["role"] for c in rows if c.get("kind") != "step"], ["coder", "coder", "reviewer"],
                         "the verdict is not recorded again; the episode the kill cut is recorded whole, by the run that continued it")
        steps = [c["detail"] for c in rows if c.get("kind") == "step"]
        self.assertTrue(any(s.startswith("resumed from checkpoint: phase attempt, round 0, saved ") for s in steps), steps)

    def test_a_resumed_run_does_not_report_its_earlier_run_s_spend(self):
        # no gateway key here: the run reports pi's figure, each session 0.5. The killed run's verdict and fix episode
        # (1.0) are its own, reported by it; the resumed run's fix episode and review (1.0) are its.
        f = self.fixture(["actionable"], ["good", "nothing"])
        home = os.path.join(f.d, "home")
        os.makedirs(home)
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1", "HOME": home}):
            self.kill(f, "pi")
            self.assertEqual(fix.spend.pi_cost(os.path.join(self.run_dir(f), "pi")), 1.0)
            f.run("--spend-file", ".spend", resume=dict(RESUME, spend_usd=1.0))   # flotillad has the killed run's 1.0 on record
        with open(os.path.join(home, ".spend")) as fh:
            self.assertEqual(float(fh.read()), 1.0)

    def test_a_real_sigterm_leaves_a_checkpoint_the_next_run_continues(self):
        import signal
        f = self.fixture(["actionable"], ["hang", "good"])
        env = dict(os.environ, FLOTILLA_PLAN_FILE=os.path.join(f.d, "plan.json"), FLOTILLA_MODELS=json.dumps(MODELS), FAKE_STATE=f.state,
                   GIT_ALLOW_PROTOCOL="file", GIT_REMOTE_BASE=f.remotes, BUGFIX_BAKEOFF_DIR=f.bakeoff,
                   PATH=os.path.join(f.d, "bin") + os.pathsep + os.environ["PATH"],
                   GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_CASE=json.dumps({"id": self.CASE, "kind": "code_bug", "target": "repo:example/shop/issues/12",
                                             "data": {"issue": {"repo": "example/shop", "number": 12, "title": "value is 1",
                                                                "body": "value() returns 1, want 2", "url": "u"}}}))
        p = subprocess.Popen([sys.executable, os.path.abspath(fix.__file__), "--repo", "example/shop", "--worker-model", "bugfix-flash",
                              "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer", "--setup", "true",
                              "--test-cmd", f"python3 {f.suite} run %s", "--related-cmd", f"python3 {f.suite} related %s",
                              "--scratch", f.scratch, "--no-repro"],
                             env=env, start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: p.poll() is None and os.killpg(p.pid, signal.SIGKILL))
        calls = os.path.join(f.state, "calls")
        for _ in range(300):   # until the fix step hangs
            if os.path.exists(calls) and "fix hang" in Path(calls).read_text():
                break
            time.sleep(0.1)
        os.killpg(p.pid, signal.SIGTERM)
        p.wait(timeout=10)
        self.assertEqual(self.checkpoint(f)["phase"], "attempt")
        plan, _ = f.run(resume=RESUME)
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertEqual(f.calls(), ["triage", "fix", "fix", "review"])
        r = self.resumes(f)
        self.assertEqual(len(r), 1, r)
        self.assertRegex(r[0], r"^resume s-hang-\d+\.jsonl changed=0 desc=The run was interrupted")


class Locking(unittest.TestCase):
    """The case's directory is held by one run at a time (flock on runs/<case id>/.lock), and what an interrupted run
    leaves is cleaned up, charged and scrubbed when the case's next run continues it."""

    CASE = Durable.CASE

    def fixture(self, verdicts, fixes):
        f = Fixture(verdicts, fixes)
        self.addCleanup(f.close)
        return f

    def run_dir(self, f):
        return os.path.join(f.scratch, "runs", self.CASE)

    def kill(self, f, name="attempt"):
        d = Durable()
        d.CASE = self.CASE
        d.kill(f, name)

    def hold(self, f):
        import fcntl
        os.makedirs(self.run_dir(f), exist_ok=True)
        fd = os.open(os.path.join(self.run_dir(f), fix.LOCK), os.O_RDWR | os.O_CREAT, 0o600)
        fcntl.flock(fd, fcntl.LOCK_EX)
        self.addCleanup(lambda: (os.close(fd) if self.open_fd(fd) else None))
        return fd

    @staticmethod
    def open_fd(fd):
        try:
            os.fstat(fd)
            return True
        except OSError:
            return False

    def snapshot(self, f):
        out = {}
        for root, dirs, files in os.walk(self.run_dir(f)):
            for n in files:
                if n != fix.LOCK:
                    p = os.path.join(root, n)
                    out[os.path.relpath(p, self.run_dir(f))] = (os.path.getmtime(p), os.path.getsize(p))
        return out

    def can_lock(self, f):
        try:
            fd = fix.lock_run_dir(self.run_dir(f), 0)
        except fix.RunBusy:
            return False
        os.close(fd)
        return True

    def test_a_run_that_cannot_get_the_lock_waits_then_ends_in_error_without_touching_the_checkpoint(self):
        f = self.fixture(["actionable"], ["good"])
        self.kill(f)
        before = self.snapshot(f)
        ck = Path(self.run_dir(f), fix.CHECKPOINT).read_bytes()
        fd = self.hold(f)
        t0 = time.monotonic()
        with self.assertRaises(fix.RunBusy) as cm:
            f.run("--lock-wait", "1", resume=RESUME)
        self.assertGreaterEqual(time.monotonic() - t0, 0.9, "it waited")
        self.assertIn("another run of this case is still running", str(cm.exception))
        self.assertEqual(Path(self.run_dir(f), fix.CHECKPOINT).read_bytes(), ck)
        self.assertEqual(self.snapshot(f), before, "no file of the directory touched")
        self.assertEqual(f.calls(), ["triage"], "no leaf ran")
        os.close(fd)
        plan, _ = f.run(resume=RESUME)   # the lock free: the same checkpoint is resumed, so the failed wait used nothing up
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("resumed from a checkpoint", plan["detail"])

    def test_a_run_waits_for_the_lock_and_goes_on_when_it_is_released(self):
        import threading
        f = self.fixture(["actionable"], ["good"])
        self.kill(f)
        fd = self.hold(f)
        threading.Timer(1.0, os.close, (fd,)).start()
        plan, _ = f.run("--lock-wait", "20", resume=RESUME)
        self.assertEqual(plan["outcome"], "plan", plan)
        self.assertIn("resumed from a checkpoint", plan["detail"])

    def test_the_lock_is_released_on_every_way_a_run_ends(self):
        f = self.fixture(["actionable", "needs-info", "actionable", "actionable"], ["good", "good"])
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "plan")
        self.assertTrue(self.can_lock(f), "a normal end")
        shutil.rmtree(self.run_dir(f))
        plan, _ = f.run()
        self.assertEqual(plan["outcome"], "escalate")
        self.assertTrue(self.can_lock(f), "an escalation")
        shutil.rmtree(self.run_dir(f))
        real = fix.Chain.attempt

        def boom(chain, *a, **kw):
            raise RuntimeError("a bug")
        fix.Chain.attempt = boom
        try:
            with self.assertRaises(RuntimeError):
                f.run()
        finally:
            fix.Chain.attempt = real
        self.assertTrue(self.can_lock(f), "an error")
        shutil.rmtree(self.run_dir(f))
        self.kill(f)
        self.assertTrue(self.can_lock(f), "a kill")

    def test_two_waiting_runs_do_not_both_get_it_when_the_holder_deleted_the_directory(self):
        d = os.path.join(tempfile.mkdtemp(), "runs", "c")
        self.addCleanup(shutil.rmtree, os.path.dirname(os.path.dirname(d)), True)
        a = fix.lock_run_dir(d, 0)
        import threading
        got = []
        t = threading.Thread(target=lambda: got.append(fix.lock_run_dir(d, 10, 0.05)))
        t.start()
        time.sleep(0.3)
        shutil.rmtree(d)   # the holder ends normally and deletes its directory with the lock file in it
        os.close(a)
        t.join(5)
        self.assertEqual(len(got), 1)
        with self.assertRaises(fix.RunBusy):
            fix.lock_run_dir(d, 0)   # the waiter holds the file now at the path, not the deleted one
        os.close(got[0])

    def test_the_command_exits_75_when_the_case_is_busy(self):
        f = self.fixture(["actionable"], ["good"])
        fd = self.hold(f)
        env = dict(os.environ, FLOTILLA_PLAN_FILE=os.path.join(f.d, "plan.json"), FLOTILLA_MODELS=json.dumps(MODELS), FAKE_STATE=f.state,
                   FLOTILLA_CASE=json.dumps({"id": self.CASE, "kind": "code_bug", "target": "t",
                                             "data": {"issue": {"repo": "example/shop", "number": 12, "title": "t", "body": "b", "url": "u"}}}))
        p = subprocess.run([sys.executable, os.path.abspath(fix.__file__), "--repo", "example/shop", "--worker-model", "bugfix-flash",
                            "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer", "--scratch", f.scratch, "--no-repro",
                            "--lock-wait", "0.3"], env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(p.returncode, fix.EXIT_BUSY, p.stdout + p.stderr)
        self.assertIn("another run of this case is still running", p.stdout)
        os.close(fd)

    def test_a_directory_with_read_only_trees_is_deleted_and_an_undeletable_one_is_said(self):
        f = self.fixture(["actionable", "actionable"], ["good"])
        self.kill(f)
        ro = os.path.join(self.run_dir(f), "go", "pkg", "mod")   # a Go module cache is read-only on purpose
        os.makedirs(ro)
        Path(ro, "x.go").write_text("package x\n")
        os.chmod(ro, 0o555)
        os.chmod(os.path.dirname(ro), 0o555)
        plan, _ = f.run()   # no resume: a fresh run clears the directory first
        self.assertEqual(plan["outcome"], "plan", plan)
        # nothing deletes it: a clear error, not a FileExistsError on every retry
        os.makedirs(os.path.join(self.run_dir(f), "stuck"), exist_ok=True)
        with unittest.mock.patch.object(fix, "rmtree_force", lambda p: None):
            with self.assertRaises(fix.RunDirStuck) as cm:
                f.run()
        self.assertIn("stuck", str(cm.exception))
        self.assertTrue(self.can_lock(f))

    def test_the_sweep_deletes_read_only_trees_and_leaves_a_directory_whose_lock_is_held(self):
        scratch = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, scratch, True)
        old = time.time() - fix.CHECKPOINT_MAX_AGE - 3600
        for name in ("case-ro", "case-live"):
            d = os.path.join(scratch, "runs", name, "mod")
            os.makedirs(d)
            Path(d, "f").write_text("x")
            os.chmod(d, 0o555)
            for p in (os.path.join(scratch, "runs", name),):
                os.utime(p, (old, old))
        fd = fix.lock_run_dir(os.path.join(scratch, "runs", "case-live"), 0)
        os.utime(os.path.join(scratch, "runs", "case-live"), (old, old))
        fix.sweep_stale(scratch)
        os.close(fd)
        self.assertEqual(os.listdir(os.path.join(scratch, "runs")), ["case-live"])
        _open = fix._open_up
        _open(os.path.join(scratch, "runs"))

    def test_a_resume_scrubs_the_shared_cache_before_it_pins_its_config(self):
        f = self.fixture(["actionable"], ["good"])
        self.kill(f)
        cache = os.path.join(f.scratch, "repos", "example__shop.git")
        os.makedirs(os.path.join(cache, ".git", "hooks"), exist_ok=True)
        Path(cache, ".git", "hooks", "post-checkout").write_text("#!/bin/sh\ntouch /tmp/never\n")
        subprocess.run(["git", "-C", cache, "remote", "set-url", "origin", "https://evil.example/x.git"], check=True)
        plan, _ = f.run(resume=RESUME)
        self.assertEqual(plan["outcome"], "plan", plan)
        url = subprocess.run(["git", "-C", cache, "config", "remote.origin.url"], capture_output=True, text=True).stdout.strip()
        self.assertEqual(url, f.remotes + "/example/shop", "the origin is the run's own again, not what an earlier run's code set")
        self.assertFalse(os.path.exists(os.path.join(cache, ".git", "hooks", "post-checkout")))

    def spend_of(self, f, home, **resume):
        with open(os.path.join(home, ".spend")) as fh:
            return float(fh.read())

    def test_a_resumed_run_reports_what_the_interrupted_one_never_did(self):
        f = self.fixture(["actionable"], ["good", "nothing"])
        home = os.path.join(f.d, "home")
        os.makedirs(home)
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1", "HOME": home}):
            self.kill(f, "pi")   # its verdict and fix episode, 1.0 on pi's figure, reported by no one
            f.run("--spend-file", ".spend", resume=dict(RESUME, spend_usd=0.0))
        self.assertEqual(self.spend_of(f, home), 2.0, "its own 1.0 and the interrupted run's 1.0")

    def test_a_resumed_run_that_cannot_start_does_not_count_the_earlier_sessions_again(self):
        f = self.fixture(["actionable"], ["good", "nothing"])
        home = os.path.join(f.d, "home")
        os.makedirs(home)
        with unittest.mock.patch.dict(os.environ, {"FAKE_SESS": "1", "HOME": home}):
            self.kill(f, "pi")

            class Broken:
                def __init__(self, *a, **kw):
                    raise RuntimeError("cannot start")
            with unittest.mock.patch.object(fix, "Chain", Broken):
                with self.assertRaises(RuntimeError):
                    f.run("--spend-file", ".spend", resume=dict(RESUME, spend_usd=1.0))
        self.assertEqual(self.spend_of(f, home), 0.0, "the sessions the earlier run reported are not this run's")

    def spawn(self, f, deadline):
        import signal
        env = dict(os.environ, FLOTILLA_PLAN_FILE=os.path.join(f.d, "plan.json"), FLOTILLA_MODELS=json.dumps(MODELS), FAKE_STATE=f.state,
                   GIT_ALLOW_PROTOCOL="file", GIT_REMOTE_BASE=f.remotes, BUGFIX_BAKEOFF_DIR=f.bakeoff,
                   PATH=os.path.join(f.d, "bin") + os.pathsep + os.environ["PATH"],
                   GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_RUN_DEADLINE=repr(deadline),
                   FLOTILLA_CASE=json.dumps({"id": self.CASE, "kind": "code_bug", "target": "repo:example/shop/issues/12",
                                             "data": {"issue": {"repo": "example/shop", "number": 12, "title": "value is 1",
                                                                "body": "value() returns 1, want 2", "url": "u"}}}))
        p = subprocess.Popen([sys.executable, os.path.abspath(fix.__file__), "--repo", "example/shop", "--worker-model", "bugfix-flash",
                              "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer", "--setup", "true",
                              "--test-cmd", f"python3 {f.suite} run %s", "--related-cmd", f"python3 {f.suite} related %s",
                              "--scratch", f.scratch, "--no-repro"],
                             env=env, start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: p.poll() is None and os.killpg(p.pid, signal.SIGKILL))
        calls = os.path.join(f.state, "calls")
        for _ in range(300):   # until the fix step hangs
            if os.path.exists(calls) and "fix hang" in Path(calls).read_text():
                break
            time.sleep(0.1)
        return p

    def test_a_run_ended_by_its_own_timeout_leaves_nothing_but_a_restart_s_or_a_cancel_s_term_keeps_the_checkpoint(self):
        import signal
        f = self.fixture(["actionable"], ["hang"])
        p = self.spawn(f, time.time() + 1)   # the worker's SIGTERM at the deadline
        time.sleep(1.2)
        os.killpg(p.pid, signal.SIGTERM)
        p.wait(timeout=20)
        self.assertFalse(os.path.exists(self.run_dir(f)), "status timeout is not resumed: the worktree and venv go")
        g = self.fixture(["actionable"], ["hang"])
        p = self.spawn(g, time.time() + 3600)   # a restart's or a cancel's SIGTERM, long before the deadline
        os.killpg(p.pid, signal.SIGTERM)
        p.wait(timeout=20)
        self.assertTrue(os.path.exists(os.path.join(self.run_dir(g), fix.CHECKPOINT)))

    def test_an_error_with_no_checkpoint_leaves_no_directory(self):
        f = self.fixture(["actionable"], ["good"])
        real = fix.Chain.checkout

        def boom(chain, *a, **kw):
            raise RuntimeError("the checkout failed")
        fix.Chain.checkout = boom
        try:
            with self.assertRaises(RuntimeError):
                f.run()
        finally:
            fix.Chain.checkout = real
        self.assertFalse(os.path.exists(self.run_dir(f)), "nothing to resume from: no worktree and venv left to the sweep")


class FullIssueBody(unittest.TestCase):
    """B8: intake cuts long bodies to fit flotillad's 64 KiB report (body_cut); the fix step must reason about the
    issue as filed, so it fetches the full body from GitHub's public API, and falls back to the cut one."""

    def serve(self, status, body):
        import http.server
        import threading

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(h):
                self.paths.append(h.path)
                data = json.dumps({"number": 7, "body": body}).encode()
                h.send_response(status)
                h.send_header("Content-Type", "application/json")
                h.end_headers()
                h.wfile.write(data)

            def log_message(h, *a):
                pass

        self.paths = []
        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        return f"http://127.0.0.1:{srv.server_address[1]}"

    def test_a_cut_body_is_fetched_whole(self):
        api = self.serve(200, "the whole report, " * 500)
        issue = {"repo": "example/api", "number": 7, "body": "the whole rep", "body_cut": True}
        got, note = fix.issue_for_case(issue, issue["repo"], {"GITHUB_API": api})
        self.assertEqual(got["body"], "the whole report, " * 500)
        self.assertFalse(got["body_cut"])
        self.assertEqual(self.paths, ["/repos/example/api/issues/7"])
        self.assertIn("full body", note)

    def test_a_body_that_was_not_cut_is_not_fetched(self):
        api = self.serve(200, "other")
        issue = {"repo": "example/shop", "number": 7, "body": "short", "body_cut": False}
        got, note = fix.issue_for_case(issue, issue["repo"], {"GITHUB_API": api})
        self.assertEqual((got["body"], self.paths, note), ("short", [], ""))

    def test_a_failed_fetch_is_retried_once_then_stops_the_case(self):
        # intake's fixed share cuts most bodies: the fix must not work from the first few hundred bytes, silently
        api = self.serve(404, "x")
        issue = {"repo": "example/api", "number": 7, "body": "cut", "body_cut": True}
        with self.assertRaises(fix.Stop) as e:
            fix.issue_for_case(issue, issue["repo"], {"GITHUB_API": api, "BUGFIX_BODY_RETRY_S": "0"})
        self.assertIn("could not be read whole from GitHub (HTTPError", str(e.exception))
        self.assertEqual(len(self.paths), 2, "one retry")

    def test_an_issue_without_its_repo_is_read_from_the_chains_repo(self):
        # Chain falls back to --repo when the case's issue names none; the read must use the same repo
        api = self.serve(200, "whole")
        issue = {"number": 7, "body": "cut", "body_cut": True}
        got, note = fix.issue_for_case(issue, "example/api", {"GITHUB_API": api})
        self.assertEqual((got["body"], self.paths), ("whole", ["/repos/example/api/issues/7"]))


if __name__ == "__main__":
    unittest.main()
