"""bugfix/triage2: the verdict and the advisor of the chain on stations (bugfix/fix2.py), ported from the bake-off's bash leaves.

bugfix-fleet/bakeoff/triage_run.sh and advisor_run.sh do five things: build a prompt, run pi, read its answer out of the stream,
check the answer's shape and write what the chain reads (the verdict's triage.json and the triaged manifest, the advisor's
advisor.json), and keep ledgers. They are never edited (bugfix/fix.py runs them); this does the same steps with the pi call left to
a function the caller gives it, which fix2.py makes a call through runtime/adapter.py. What the bash did with jq, grep, cut, paste
and sed is still run by those tools, with the bash's own text, so their behaviour is theirs and not a second reading of it:
`_val` is `$(...)`, `_test` is `[ ... ]`, and a command that fails where the script had `set -e` ends the run with its exit code.

What the bash did that this reproduces on purpose: `read -r -d '' X <<EOF` drops the whitespace (space, tab, newline) at both
ends of the text, so the advisor's findings, which start with a blank line, are glued to the task before them; `$(...)` drops the
trailing newlines; and the touched files of an evidence folder with none are "\\nnone" (paste printed a newline for no input and
the `|| echo none` followed). bugfix/differential.py runs the real scripts against this port, scenario by scenario (a CRLF issue body
and raw U+2028 in an answer among them); bugfix/test_triage2.py checks the prompt and print texts against the scripts and the bytes
the scripts keep (a CR from jq, a stream split at newlines only, the pin line of an answer that is not an object).

What is not the same, and is said in ADR-0020's amendment: pi's stdout and stderr arrive as one stream, so harness.json holds the
stream's lines that are JSON objects and harness.err the rest; the wall cap is the station's `timeout` (TRIAGE_MAX_WALL and
ADVISOR_MAX_WALL are not read) and a cut is made by the runner, not a 5 s poll; what the script printed is the same lines, with its
own wall seconds.
"""
import json
import os
import re
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import harness_prompts  # noqa: E402

RULE = "──────────────────────────────"
FRAGMENT_HEAD = "set -euo pipefail\n"


class Abort(Exception):
    """The script's `set -e`: a command failed and the run ends with its exit code."""

    def __init__(self, code):
        super().__init__(code)
        self.code = code


class Run:
    """What the script printed, in order (stdout and stderr together, as fix.sh gives them)."""

    def __init__(self, env, cwd):
        self.out = []
        self.env = env
        self.cwd = cwd

    def say(self, line):
        self.out.append(line + "\n")

    def text(self):
        return "".join(self.out)


def _bash(r, script, *args, cwd=None, stdin=None):
    # bytes in and out, decoded with no newline translation: text mode would turn a CR (a web-written issue body's \r\n) into \n, and
    # bash's $(...) keeps it, so the prompts would not be the scripts' byte for byte
    data = stdin.encode("utf-8", "surrogateescape") if isinstance(stdin, str) else stdin
    p = subprocess.run(["bash", "-c", FRAGMENT_HEAD + script, "triage", *args], capture_output=True, env=r.env, cwd=cwd or r.cwd,
                       input=data)
    p.stdout = p.stdout.decode("utf-8", "surrogateescape")
    p.stderr = p.stderr.decode("utf-8", "surrogateescape")
    if p.stderr:
        r.out.append(p.stderr)
    return p


def _val(r, script, *args):
    """`X=$(script)`: its stdout less the trailing newlines; a failure ends the run."""
    p = _bash(r, script, *args)
    if p.returncode != 0:
        raise Abort(p.returncode)
    return p.stdout.rstrip("\n")


def _val_try(r, script, *args):
    """`$(script)` where a failure does not end the run (inside a heredoc or a test)."""
    return _bash(r, script, *args).stdout.rstrip("\n")


def _test(r, script, *args):
    """`[ ... ]` (or any status): whether it succeeded."""
    return _bash(r, script, *args).returncode == 0


def read_trim(text):
    """`read -r -d ''`: the text without the whitespace at either end."""
    return text.strip(" \t\n")


def touched(r, path):
    """The evidence folder's touched.txt as `paste -sd, | sed`: the files, comma-joined."""
    return _val(r, "paste -sd, \"$1\" 2>/dev/null | sed 's/^,*//;s/,*$//'", path)


def touched_or_patch(r, evidence, listed):
    """The verdict's fallback: with no touched file, those of harness.patch that are not tests, or `none`."""
    if listed:
        return listed
    return _val(r, "grep -E '^\\+\\+\\+ b/' \"$1/harness.patch\" 2>/dev/null | sed 's|^+++ b/||' | grep -vE '\\.(test|spec)\\.' "
                   "| paste -sd, - || echo none", evidence)


def cut(r, field, path, orelse=None):
    if orelse is None:
        return _val(r, f"cut -f{field} \"$1\"", path)
    return _val(r, f"cut -f{field} \"$1\" 2>/dev/null || echo \"{orelse}\"", path)


# ---- the verdict's prompt

FIRST_PASS = ("This is a FIRST PASS (kickback_round = 0) on the issue below. The repository\n"
              "you are in is the codebase the issue is about. Do not edit any file — you\n"
              "only read. End your response with the JSON verdict and nothing after it,\n"
              "in a ```json fenced block.\n\n"
              "## Issue to triage (verbatim)\n\n")

KICK_BACK = ("This is a KICK-BACK SECOND PASS (kickback_round = {kb}). A specialist\n"
             "attempted your normalized problem and FAILED. Judge for yourself whether the\n"
             "patch went to the right place (compare what it touched against where the\n"
             "symptom's owner lives), then route per your definition: wrong place →\n"
             "topology gap; right place but oracle still red → acceptance gap. Do not\n"
             "edit any file — you only read. End your response with the JSON verdict and\n"
             "nothing after it, in a ```json fenced block. Set kickback_round to\n"
             "{kb} in your output.\n\n"
             "## Original issue (verbatim)\n\n{desc}\n\n"
             "## Your prior normalized problem (pin removed — see evidence)\n\n{problem}\n\n"
             "## Failed attempt — evidence\n\n"
             "- verdict: {verdict}\n"
             "- files the patch touched (non-test): {touched}\n"
             "- burn: {wall}s wall, {turns} turns, {outtok} output tokens\n")

REFUTED_PIN = ("Your previous pin was `{pin}`. The specialist followed it — the\n"
               "patch touched that file — and the attempt STILL FAILED. That pin is REFUTED:\n"
               "do not pin it again unless genuinely new evidence names it. If your best\n"
               "remaining theory is the refuted file, the honest verdict is needs-info.")
UNFOLLOWED_PIN = ("Your previous pin was `{pin}`; the failed patch did not touch it\n"
                  "(the specialist did not follow the pin). Weigh that separately.")

ADVISOR_FINDINGS = ("\n## Advisor findings (a read-only consultation: one hypothesis about where the fix belongs)\n\n"
                    "{findings}\n\n"
                    "The acceptance criteria stay the issue's own: do not add criteria that name a\n"
                    "file to change, a test to rewrite, or a filter that drops the error.\n")
FINDINGS_JQ = ("if (.file // \"\") == \"\" then \"Not locatable in this repository: \" + (.not_locatable // \"-\") + \"\\nIf the report does not say enough "
               "to locate the fault, the verdict is needs-info.\"\n"
               "         else \"Possible owner: \" + .file + \" :: \" + (.function // \"\") + \"\\nDecoy to warn off: \" + (.decoy // \"-\") + "
               "\"\\nRationale: \" + (.rationale // \"\") end")

ACCEPTANCE_GAP = ("\n## ACCEPTANCE GAP — localization is SETTLED\n\n"
                  "The failed patch modified the exact file/function the advisor pinned, and the\n"
                  "hidden acceptance still fails. Do NOT propose a new location; do NOT re-pin.\n"
                  "The gap is in expected behavior, not topology. Verdict must be needs-info:\n"
                  "ask the reporter precise questions about expected values/behavior at the\n"
                  "pinned location (exact formulas, units, reference values, edge cases).\n")


def verdict_prompt(r, agent_def, desc, kb_round, evidence="", prior="", acceptance_gap=False):
    """triage_run.sh's PROMPT: the agent definition's body, a rule, and the task (a first pass, or the kick-back with the failed
    attempt's evidence). `evidence` is the folder fix.py wrote for the failed round, `prior` the verdict it answers."""
    if kb_round == 0:
        task = read_trim(FIRST_PASS + desc + "\n")
    else:
        wverdict = cut(r, 3, os.path.join(evidence, "result.tsv"))
        wwall = cut(r, 4, os.path.join(evidence, "result.tsv"))
        wturns = cut(r, 6, os.path.join(evidence, "result.tsv"))
        woutok = cut(r, 7, os.path.join(evidence, "result.tsv"))
        wtouched = touched_or_patch(r, evidence, touched(r, os.path.join(evidence, "touched.txt")))
        prior_pin = _val(r, "jq -r '.problem.pin.file // \"\"' \"$1\"", prior)
        problem = _val(r, "jq -c '.problem | .pin = {file:\"\",function:\"\",decoy:\"\"}' \"$1\"", prior)
        refuted = ""
        if prior_pin and _test(r, "echo \",$1,\" | grep -qF \",$2,\"", wtouched, prior_pin):
            refuted = REFUTED_PIN.format(pin=prior_pin)
        elif prior_pin:
            refuted = UNFOLLOWED_PIN.format(pin=prior_pin)
        body = KICK_BACK.format(kb=kb_round, desc=desc, problem=problem, verdict=wverdict, touched=wtouched or "none", wall=wwall,
                                turns=wturns, outtok=woutok)
        task = read_trim(body + (("\n" + refuted) if refuted else "") + "\n")
        advisor = os.path.join(evidence, "advisor.json")
        if os.path.isfile(advisor):
            findings = _val_try(r, "jq -r '" + FINDINGS_JQ + "' \"$1\"", advisor)
            task = task + read_trim(ADVISOR_FINDINGS.format(findings=findings))
        if acceptance_gap:
            task = task + read_trim(ACCEPTANCE_GAP)
    return read_trim(agent_def + "\n\n---\n\n" + task + "\n")


# ---- the advisor's prompt

ADVISOR_PROMPT = ("You are a senior engineer doing a READ-ONLY consultation. A cheaper agent\n"
                  "tried to fix the bug below and FAILED — its patch touched the wrong place.\n"
                  "Your ONLY job: locate the true owner of the symptom in this repository.\n"
                  "Do not fix anything. Do not edit any file.\n\n"
                  "## Bug report\n{desc}\n\n"
                  "## Failed attempt evidence\n"
                  "- verdict: {verdict}\n"
                  "- files the failed patch touched: {touched}\n\n"
                  "Investigate the code (trace the symptom's actual runtime path: which module\n"
                  "computes the value / owns the behavior the report describes — beware\n"
                  "look-alike functions in other layers). Then reply with ONLY a JSON object\n"
                  "in a ```json fenced block:\n"
                  "{{\"file\": \"path of the true owner\", \"function\": \"the function to change\",\n"
                  " \"decoy\": \"the wrong-but-tempting path the failed patch fell into\",\n"
                  " \"rationale\": \"2-3 sentences: why this owner, traced how\"}}\n"
                  "If you cannot trace the symptom to code in this repository from the report\n"
                  "and the code (the failing code is not here, or the report does not say enough\n"
                  "to find it), say so instead; do not pick a place to change, and never a test\n"
                  "or a filter that hides the error:\n"
                  "{{\"file\": \"\", \"not_locatable\": \"1-2 sentences: what is missing to locate it\"}}\n")


def advisor_prompt(desc, verdict, touched_files):
    return read_trim(ADVISOR_PROMPT.format(desc=desc, verdict=verdict, touched=touched_files or "unknown"))


# ---- what is read out of pi's stream (the scripts' own python, as it was)

def extract_verdict(src, dst, r):
    events = []
    with open(src) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    text = ""
    for e in events:
        m = e.get("message") or {}
        if m.get("role") == "assistant":
            for part in m.get("content") or []:
                if isinstance(part, dict) and part.get("type") == "text":
                    text += part.get("text") or ""
    blocks = re.findall(r"```json\s*(.*?)```", text, re.S)
    raw = blocks[-1] if blocks else None
    if raw is None:  # fall back: last balanced {...} in the text
        starts = [i for i, c in enumerate(text) if c == "{"]
        for s in reversed(starts):
            depth = 0
            for i in range(s, len(text)):
                if text[i] == "{":
                    depth += 1
                elif text[i] == "}":
                    depth -= 1
                    if depth == 0:
                        raw = text[s:i + 1]
                        break
            if raw:
                try:
                    json.loads(raw)
                    break
                except json.JSONDecodeError:
                    raw = None
    if raw is None:
        r.say("EXTRACT: no JSON found")
        return
    try:
        verdict = json.loads(raw)
    except json.JSONDecodeError as e:
        r.say(f"EXTRACT: invalid JSON ({e})")
        return
    with open(dst, "w") as f:
        json.dump(verdict, f, indent=2)
    r.say("EXTRACT: ok")


def extract_advisor(src, dst, r):
    text = ""
    for line in open(src):
        line = line.strip()
        if not line:
            continue
        try:
            e = json.loads(line)
        except json.JSONDecodeError:
            continue
        m = e.get("message") or {}
        if m.get("role") == "assistant":
            for p in m.get("content") or []:
                if isinstance(p, dict) and p.get("type") == "text":
                    text += p.get("text") or ""
    blocks = re.findall(r"```json\s*(.*?)```", text, re.S)
    if not blocks:
        r.say("ADVISOR: no JSON")
        return
    try:
        obj = json.loads(blocks[-1])
    except json.JSONDecodeError as ex:
        r.say(f"ADVISOR: bad JSON ({ex})")
        return
    json.dump(obj, open(dst, "w"), indent=2)
    r.say("ADVISOR: ok")


def pin_shown(r, pin):
    """advisor_run.sh's `echo "ADVISOR PIN: $(jq ...)"`: the jq is inside the echo, so when it fails (the answer is JSON but not an
    object) it prints nothing and the script goes on to exit 0."""
    return _val_try(r, "jq -r 'if (.file // \"\") == \"\" then \"not locatable: \" + (.not_locatable // \"-\") else .file + \" :: \" "
                       "+ (.function // \"\") end' \"$1\"", pin)


def split_stream(stream):
    """pi's stdout and stderr came as one stream: the lines that are JSON objects were its stdout (harness.json), the rest its stderr."""
    # lines end at \n only, as the scripts' files do: str.splitlines also splits at U+2028, U+2029 and U+0085, which JSON.stringify
    # leaves raw, and cut an event in two; and a line is stdout only when it is a JSON object (a stderr line can start with "{")
    out, err = [], []
    text = stream or ""
    lines = text.split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    for ln in lines:
        try:
            obj = json.loads(ln)
        except ValueError:
            obj = None
        (out if isinstance(obj, dict) else err).append(ln + "\n")
    return "".join(out), "".join(err)


# ---- the scripts

JQ_TDESC = (".problem | (.symptom|tostring) + \"\\n\\nExpected: \" + (.expected|tostring)\n"
            "    + \"\\n\\nAcceptance criteria:\\n\" + (.acceptance | map(if type == \"string\" then \"- \" + . else \"- \" + .criterion + \" [\" + "
            "(.intent_source // \"?\") + ((.source_ref // \"\") | if . == \"\" then \"\" else \": \" + . end) + \"]\" end) | join(\"\\n\"))\n"
            "    + (if (.domain_facts | length) > 0 then \"\\n\\nDomain facts:\\n\" + (.domain_facts | map(\"- \" + .) | join(\"\\n\")) else \"\" end)\n"
            "    + (if (.pin.file // \"\") != \"\" and $kb == \"1\" then \"\\n\\nTarget: \" + .pin.file + "
            "(if (.pin.function // \"\") != \"\" then \" — \" + .pin.function else \"\" end)\n"
            "       + (if (.pin.decoy // \"\") != \"\" then \"\\nDo NOT change \" + .pin.decoy + \" — it is not the owner of this symptom.\" "
            "else \"\" end) else \"\" end)")
SMUGGLE = ("jq -r '[.problem.symptom, .problem.expected, (.problem.acceptance|join(\" \"))] | join(\" \")' \"$1\" "
           "| grep -oE 'src/[A-Za-z0-9_./-]+|\\b[a-z][a-zA-Z0-9]*[A-Z][a-zA-Z0-9]*\\(?\\)?' | sort -u | paste -sd, - || true")
UNCITED = ("jq -r '[.problem.acceptance[] | select((type==\"string\") or ((.intent_source // \"\") == \"\"))] | length' \"$1\" 2>/dev/null "
           "|| echo 0")


def verdict(argv, env, cwd, agent_text, model, here, call):
    """triage_run.sh over `argv` (the ticket, the harness name, then the failed round's evidence folder and the verdict it answers)
    in `env`: (exit code, what it printed). `agent_text` is the triager's definition (agents/triage.md), `model` the model's name
    for the ledger, `here` the bake-off folder (langfuse_push.py), and call(prompt, limit) -> (exit code, pi's stream, whether it
    was cut at `limit`) runs pi."""
    r = Run(dict(env), cwd)
    try:
        _verdict(r, argv, agent_text, model, here, call)
        return 0, r.text()
    except Abort as a:
        return a.code, r.text()
    except OSError as e:
        r.say(f"triage2: {e}")
        return 1, r.text()


def _verdict(r, argv, agent_text, model, here, call):
    env = r.env
    if not argv:
        raise Abort(1)
    bug_json = argv[0]
    harness = (argv[1] if len(argv) > 1 else "") or "pi"
    kb_evidence = argv[2] if len(argv) > 2 else ""
    kb_prior = argv[3] if len(argv) > 3 else ""
    root = env.get("BAKEOFF_ROOT") or os.path.join(env.get("HOME", ""), ".bugfix-fleet", "bakeoff")
    wt = env.get("BAKEOFF_WT") or os.path.join(root, "shop")
    ident = _val(r, "jq -r \"$1\" \"$2\"", ".id", bug_json)
    fix = _val(r, "jq -r \"$1\" \"$2\"", ".fix_commit // empty", bug_json)
    desc = _val(r, "jq -r \"$1\" \"$2\"", ".description", bug_json)
    base = f"{fix}^" if fix else (env.get("TRIAGE_BASE") or "origin/main")
    ver = ""
    for line in agent_text.split("\n"):
        if line.startswith("version:"):
            parts = line.split(": ")
            ver = parts[1] if len(parts) > 1 else ""
            break
    ver = ver or "1"
    kb_round = 0
    if kb_evidence:
        if not os.path.isfile(kb_prior):
            r.say("ABORT: kick-back needs the prior triage.json")
            raise Abort(2)
        kb_round = int(_val(r, "echo $(( $(jq -r '.kickback_round // 0' \"$1\") + 1 ))", kb_prior))
    suffix = f"-kb{kb_round}" if kb_round > 0 else ""
    out = os.path.join(root, "results", f"{ident}-triage{suffix}", harness)
    os.makedirs(out, exist_ok=True)

    r.say(f"══ [{ident} → triage/{harness}/{model}] reset to base {base} ══")
    for cmd in (["git", "-C", wt, "reset", "--hard", base], ["git", "-C", wt, "clean", "-fd"]):
        p = subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True, errors="surrogateescape", env=env, cwd=r.cwd)
        if p.stderr:
            r.out.append(p.stderr)
        if p.returncode != 0:
            raise Abort(p.returncode)

    agent_def = harness_prompts.agent_def_body(agent_text)
    prompt = verdict_prompt(r, agent_def, desc, kb_round, kb_evidence, kb_prior, env.get("ACCEPTANCE_GAP", "0") == "1")

    r.say("══ run triager ══")
    started = time.monotonic()
    code, stream, cut_ = call(prompt)
    if cut_:
        r.say(f"   budget: wall {cut_}s exceeded — cutting")
    harness_json = os.path.join(out, "harness.json")
    jsons, errs = split_stream(stream)
    with open(harness_json, "w") as f:
        f.write(jsons)
    with open(os.path.join(out, "harness.err"), "w") as f:
        f.write(errs)
    wall = int(time.monotonic() - started)
    cost = _val(r, "jq -rs 'map(.message.usage.cost.total // empty) | add // 0' \"$1\" 2>/dev/null || echo 0", harness_json)
    outtok = _val(r, "jq -rs 'map(.message.usage.output // empty) | add // 0' \"$1\" 2>/dev/null || echo 0", harness_json)
    r.say(f"   triager: {wall}s  ${cost}  (out={outtok})")

    changed = _val(r, "git -C \"$1\" status --porcelain | wc -l | tr -d ' '", wt)
    if int(changed) > 0:
        r.say(f"   VIOLATION: triager touched {changed} file(s)")

    r.say("══ extract + shape-check verdict ══")
    triage_json = os.path.join(out, "triage.json")
    try:
        extract_verdict(harness_json, triage_json, r)
    except Exception as e:  # noqa: BLE001 -- the script's `|| true`: an extractor that fails is a verdict of none
        r.say(f"EXTRACT: {type(e).__name__}: {e}")

    shape, notes = "PASS", ""

    def note(msg):
        nonlocal shape, notes
        shape, notes = "FAIL", notes + msg + "; "

    if not os.path.isfile(triage_json):
        note("no parseable JSON verdict")
        verdict_ = "none"
    else:
        verdict_ = _val(r, "jq -r '.verdict // \"missing\"' \"$1\"", triage_json)
        level = _val(r, "jq -r '.level // \"missing\"' \"$1\"", triage_json)
        if verdict_ not in ("actionable", "needs-info", "reject"):
            note(f"bad verdict '{verdict_}'")
        if verdict_ == "actionable":
            if not _test(r, "[ \"$(jq -r '.problem.acceptance | length' \"$1\" 2>/dev/null || echo 0)\" -gt 0 ]", triage_json):
                note("empty acceptance")
            if not _test(r, "[ -n \"$(jq -r '.problem.symptom // \"\"' \"$1\")\" ]", triage_json):
                note("empty symptom")
            if not _test(r, "[ -n \"$(jq -r '.problem.expected // \"\"' \"$1\")\" ]", triage_json):
                note("empty expected")
            uncited = _val(r, UNCITED, triage_json)
            if not _test(r, "[ \"$1\" -eq 0 ]", uncited):
                note(f"{uncited} uncited acceptance criteria (intent gate)")
            if kb_round == 0:
                if level != "L1":
                    note(f"first pass must be L1 (got {level})")
                if not _test(r, "[ \"$(jq -r '.problem.pin.file // \"\"' \"$1\")\" = \"\" ]", triage_json):
                    note("pin set on first pass")
                smuggled = _val(r, SMUGGLE, triage_json)
                if smuggled:
                    note(f"localization smuggled: {smuggled}")
            elif level == "L2-pinned":
                if not _test(r, "[ -n \"$(jq -r '.problem.pin.file // \"\"' \"$1\")\" ]", triage_json):
                    note("L2-pinned without pin.file")
            elif _test(r, "[ \"$(cut -f8 \"$1/result.tsv\" 2>/dev/null)\" = \"no\" ]", kb_evidence):
                note(f"scope=no kick-back must pin L2 (got {level})")
    if int(changed) > 0:
        note(f"edited worktree ({changed} files)")

    if verdict_ == "actionable" and os.path.isfile(triage_json):
        triaged_dir = env.get("BAKEOFF_TRIAGED_DIR") or os.path.join(here, "bugs", "triaged")
        os.makedirs(triaged_dir, exist_ok=True)
        triaged = os.path.join(triaged_dir, f"{ident}-triaged{suffix}.json")
        tdesc = _val(r, "jq -r '" + JQ_TDESC + "' --arg kb \"$1\" \"$2\"", "1" if kb_round > 0 else "0", triage_json)
        level_ = "L1-triaged" if kb_round == 0 else f"L2-triaged-kb{kb_round}"
        p = subprocess.run(["jq", "--arg", "id", f"{ident}-triaged{suffix}", "--arg", "desc", tdesc, "--arg", "lvl", level_,
                            ".id=$id | .description=$desc | .level=$lvl", bug_json], capture_output=True, text=True,
                           errors="surrogateescape", env=env, cwd=r.cwd)
        if p.stderr:
            r.out.append(p.stderr)
        with open(triaged, "w") as f:
            f.write(p.stdout)
        if p.returncode != 0:
            raise Abort(p.returncode)
        r.say(f"   triaged manifest → {triaged}")

    r.say(RULE)
    r.say(f"TRIAGE: verdict={verdict_}  shape={shape}{('  [' + notes + ']') if notes else ''}  | {wall}s ${cost}")
    with open(os.path.join(out, "shape.txt"), "w") as f:
        f.write(f"{shape}{(' [' + notes + ']') if notes else ''}\n")
    ledger = os.path.join(root, "results", "triage_runs.tsv")
    if not os.path.isfile(ledger):
        with open(ledger, "w") as f:
            f.write("run_idx\tmodel\tid\tverdict\tshape\twall\tcost\touttok\tnotes\tprompt_ver\n")
    with open(ledger, "a") as f:
        f.write("\t".join([env.get("BAKEOFF_RUN_IDX") or "1", model, f"{ident}{suffix}", verdict_, shape, str(wall), cost, outtok,
                           notes or "—", f"v{ver}"]) + "\n")
    _bash(r, "[ -f \"$1/langfuse.env\" ] && { set -a; . \"$1/langfuse.env\"; set +a; }\n"
             "python3 \"$2/langfuse_push.py\" \"$3\" \"$4\" \"$5\" \"$6\" \"${7:-0}\" \"0\" \"${8:-0}\" \"$9\" 2>&1 | sed 's/^/   /' || true",
          root, here, f"{ident}-triage{suffix}", harness, model, f"{verdict_}/{shape}", cost or "0", str(wall) or "0", harness_json)
    r.say(f"artifacts → {out}")


def advisor(argv, env, cwd, model, call):
    """advisor_run.sh over `argv` (the ticket, the failed round's evidence folder) in `env`: (exit code, what it printed).
    call(prompt) is as verdict's."""
    r = Run(dict(env), cwd)
    try:
        _advisor(r, argv, model, call)
        return 0, r.text()
    except Abort as a:
        return a.code, r.text()
    except OSError as e:
        r.say(f"triage2: {e}")
        return 1, r.text()


def _advisor(r, argv, model, call):
    env = r.env
    if len(argv) < 2:
        raise Abort(1)
    bug_json, wout = argv[0], argv[1]
    desc = _val(r, "jq -r .description \"$1\"", bug_json)
    wtouched = touched(r, os.path.join(wout, "touched.txt"))
    wverdict = cut(r, 3, os.path.join(wout, "result.tsv"), orelse="FAIL")
    prompt = advisor_prompt(desc, wverdict, wtouched)
    r.say(f"══ advisor episode ({model}) ══")
    started = time.monotonic()
    code, stream, cut_ = call(prompt)
    if cut_:
        r.say(f"   budget: wall {cut_}s exceeded — cutting")
    raw = os.path.join(wout, "advisor-raw.json")
    jsons, errs = split_stream(stream)
    with open(raw, "w") as f:
        f.write(jsons)
    with open(os.path.join(wout, "advisor.err"), "w") as f:
        f.write(errs)
    wall = int(time.monotonic() - started)
    pin = os.path.join(wout, "advisor.json")
    try:
        extract_advisor(raw, pin, r)
    except Exception as e:  # noqa: BLE001 -- `|| true`
        r.say(f"ADVISOR: {type(e).__name__}: {e}")
    if os.path.isfile(pin):
        r.say(f"ADVISOR PIN: {pin_shown(r, pin)}  ({wall}s)")
        return
    r.say(f"ADVISOR: no usable output ({wall}s)")
    raise Abort(4)
