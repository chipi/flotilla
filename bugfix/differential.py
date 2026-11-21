"""python3 bugfix/differential.py: the bugfix chain's old pi paths against its new ones, scenario by scenario (v2 step 6, phase C).

The goldens (test_pi_golden.py) pin what the chain does today. This runs the same scripted scenarios twice, each in a process of
its own with its own fixture, and prints every field that differs between the two sides:

  old   the chain as it was at a commit (OLD_SHA, the one before the switch), taken from git in a detached worktree: its
        fix.py, harnesses/pi.sh and repro.sh, never re-implemented here
  new   a tree (the working tree, or a copy of it) and the entry point that runs the chain (fix.py, then fix2.py)

Both sides run the same fake `pi` on PATH, which records what the real pi would have been given (argv, stdin, the
environment the call reads, its cwd, the models.json it finds, the system file's hash) and then does what the scenario's
script says (the fakes of test_fix.py). The model-visible text is the prompt: the old side sends it as the last argument, the
new side on stdin, and the table compares the two as one field.

A delta is allowed only when ALLOWED names it and its predicate holds; any other is UNEXPLAINED, and `main` exits 1.
"""
import getpass
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import textwrap

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
import test_fix as tf  # noqa: E402
import test_pi_golden as golden  # noqa: E402
import harness_prompts  # noqa: E402

# the commit before the switch: bugfix/fix.py, harnesses/pi.sh and repro.sh as the chain has run them
OLD_SHA = "8b9af6d98f6fc5acf7c72d43a00608caa8cd1533"

RECORDED_ENV = golden.ENV_KEYS + ("NODE_OPTIONS", "PI_OFFLINE", "PI_SKIP_VERSION_CHECK")

# The fake pi (d/bin/pi). It records the call, then execs the scenario's leaf for the role the call is: the fix and the
# reproducer are told apart by their prompt (a fix's starts with harness_prompts.FIX_HEAD, or resumes a --session), the review
# by its tools switch. The leaves are test_fix.py's own fakes, which take the worktree and the prompt as arguments.
FAKE_PI = textwrap.dedent('''\
    #!/usr/bin/env python3
    import hashlib, json, os, sys
    argv = sys.argv[1:]
    stdin = sys.stdin.read()
    prompt = stdin if stdin else (argv[-1] if argv else "")
    def after(flag):
        return argv[argv.index(flag) + 1] if flag in argv and argv.index(flag) + 1 < len(argv) else None
    tools = "none" if "--no-tools" in argv else ("read:" + after("--tools") if "--tools" in argv else "all")
    session = after("--session")
    if tools != "all":
        role = "review"
    elif prompt.startswith(%(fix_head)r) or session:
        role = "fix"
    else:
        role = "repro"
    system = after("--append-system-prompt")
    sha = None
    if system:
        try:
            sha = hashlib.sha256(open(system, "rb").read()).hexdigest()
        except OSError:
            sha = "unreadable"
    models = None
    try:
        models = json.load(open(os.path.join(os.environ["PI_CODING_AGENT_DIR"], "models.json")))
    except (KeyError, OSError, ValueError):
        pass
    rec = {"role": role, "argv": argv, "prompt": prompt, "prompt_channel": "stdin" if stdin else "argv", "cwd": os.getcwd(),
           "env": {k: os.environ[k] for k in %(keys)r if k in os.environ}, "system_sha": sha, "models_json": models}
    with open(os.path.join(os.environ["FAKE_STATE"], "differential.jsonl"), "a") as f:
        f.write(json.dumps(rec) + "\\n")
    leaf = os.path.join(os.environ["DIFF_LEAVES"], role + ".sh")
    env = dict(os.environ)
    if session:
        env["PI_RESUME_SESSION"] = session
    args = ["bash", leaf, os.getcwd(), prompt] if role != "review" else ["bash", leaf, *argv, prompt]
    os.execvpe("bash", args, env)
    ''') % {"fix_head": harness_prompts.FIX_HEAD, "keys": RECORDED_ENV}


def tests_base():
    return os.environ.get("FLOTILLA_TEST_TMP_BASE") or f"/var/tmp/flotilla-tests-{os.environ.get('USER') or getpass.getuser()}"


class OldTree:
    """The repository at `sha` in a detached worktree of its own under the tests' base folder, removed on exit."""

    def __init__(self, sha=OLD_SHA):
        self.sha = sha
        base = tests_base()
        os.makedirs(base, exist_ok=True)
        self.dir = tempfile.mkdtemp(prefix="differential-", dir=base)
        self.path = os.path.join(self.dir, "old")

    def __enter__(self):
        subprocess.run(["git", "-C", ROOT, "worktree", "add", "--detach", self.path, self.sha], check=True, capture_output=True)
        return self.path

    def __exit__(self, *exc):
        subprocess.run(["git", "-C", ROOT, "worktree", "remove", "--force", self.path], capture_output=True)
        shutil.rmtree(self.dir, ignore_errors=True)


def copy_tree(dst):
    """The parts of the working tree a chain run reads, copied to dst: a tree a test may edit to make a mutant."""
    for rel in ("bugfix", "runtime", os.path.join("bugfix-fleet", "bakeoff", "harnesses"), os.path.join("bugfix-fleet", "agents")):
        shutil.copytree(os.path.join(ROOT, rel), os.path.join(dst, rel),
                        ignore=shutil.ignore_patterns("__pycache__", "testdata", "*.orig"))
    return dst


def parse_argv(argv):
    """pi's argv as fields: what each switch says, and what is left over (the prompt, when it travels in argv)."""
    out = {"provider": None, "model": None, "mode": None, "tools": "all", "no_context_files": False, "system_file": None,
           "session": None, "print": False, "other": [], "positional": []}
    i = 0
    while i < len(argv):
        a = argv[i]
        val = argv[i + 1] if i + 1 < len(argv) else None
        if a == "-p":
            out["print"] = True
        elif a == "--mode":
            out["mode"], i = val, i + 1
        elif a == "--provider":
            out["provider"], i = val, i + 1
        elif a == "--model":
            out["model"], i = val, i + 1
        elif a == "--no-tools":
            out["tools"] = "none"
        elif a == "--tools":
            out["tools"], i = "read:" + str(val), i + 1
        elif a == "--no-context-files":
            out["no_context_files"] = True
        elif a == "--append-system-prompt":
            out["system_file"], i = val, i + 1
        elif a == "--session":
            out["session"], i = val, i + 1
        elif a.startswith("-"):
            out["other"].append(a)
        else:
            out["positional"].append(a)
        i += 1
    return out


def fields(inv):
    """One recorded call as the flat {field: value} the two sides are compared on."""
    a = parse_argv(inv["argv"])
    out = {"role": inv["role"], "prompt": inv["prompt"], "prompt_channel": inv["prompt_channel"], "cwd": inv["cwd"],
           "provider": a["provider"], "model": a["model"], "mode": a["mode"], "print": a["print"], "tools": a["tools"],
           "no_context_files": a["no_context_files"], "system_file": a["system_file"], "system_sha": inv["system_sha"],
           "session": a["session"], "other_args": a["other"], "models_json": inv["models_json"]}
    for k in RECORDED_ENV:
        out["env." + k] = inv["env"].get(k)
    return out


def _called_entry_only(models, provider, model):
    """pi's models.json as it matters to one call: the provider's block with the called model's entry alone."""
    block = ((models or {}).get("providers") or {}).get(provider)
    if not isinstance(block, dict):
        return None
    return dict(block, models=[m for m in block.get("models") or [] if m.get("id") == model])


# name: (field, predicate(old, new, old_fields, new_fields)). A delta is allowed only when a rule names its field and holds.
ALLOWED = {
    "the prompt moves from argv to stdin": ("prompt_channel", lambda o, n, of, nf: (o, n) == ("argv", "stdin")),
    "the provider's name is the route's endpoint": ("provider", lambda o, n, of, nf: bool(n) and n.startswith("flotilla-")),
    "pi's phone-home switch: PI_OFFLINE": ("env.PI_OFFLINE", lambda o, n, of, nf: (o, n) == (None, "1")),
    "pi's phone-home switch: PI_SKIP_VERSION_CHECK": ("env.PI_SKIP_VERSION_CHECK", lambda o, n, of, nf: (o, n) == (None, "1")),
    "models.json is rewritten for the one route (its entry is the old file's)":
        ("models_json", lambda o, n, of, nf: _called_entry_only(n, nf["provider"], nf["model"])
         == _called_entry_only(o, of["provider"], of["model"]) and _called_entry_only(n, nf["provider"], nf["model"]) is not None),
}


def classify(field, old, new, old_f, new_f):
    """The name of the rule that allows this delta, or None: unexplained."""
    for name, (f, pred) in ALLOWED.items():
        if f == field and pred(old, new, old_f, new_f):
            return name
    return None


def compare(scenario, old, new):
    """[(scenario, call, role, field, old value, new value, allowed rule or None)] for every field that differs."""
    rows = []
    for k in ("outcome", "action", "detail", "title", "body"):
        if old["plan"].get(k) != new["plan"].get(k):
            rows.append((scenario, "plan", "-", k, old["plan"].get(k), new["plan"].get(k), None))
    if len(old["calls"]) != len(new["calls"]):
        rows.append((scenario, "-", "-", "pi calls", len(old["calls"]), len(new["calls"]), None))
    for i, (o, n) in enumerate(zip(old["calls"], new["calls"]), 1):
        of, nf = fields(o), fields(n)
        for k in of:
            if of[k] != nf[k]:
                rows.append((scenario, i, nf["role"], k, of[k], nf[k], classify(k, of[k], nf[k], of, nf)))
    return rows


def _norm(f, roots):
    subs = [(os.path.realpath(f.d), "<D>"), (f.d, "<D>"), (f.base, "<BASE>"), (f.base[:12], "<BASE12>")]
    subs += [(r, "<ROOT>") for r in roots]
    subs.sort(key=lambda s: -len(s[0]))

    def norm(x):
        if isinstance(x, str):
            for a, b in subs:
                x = x.replace(a, b)
            x = re.sub(r"(s-[a-z]+)-\d+\.jsonl", r"\1-N.jsonl", x)
            return re.sub(r"(sess-[a-z]+-)\d+", r"\1N", x)
        if isinstance(x, list):
            return [norm(v) for v in x]
        if isinstance(x, dict):
            return {k: norm(v) for k, v in x.items()}
        return x
    return norm


def install_leaves(f, root):
    """The real harnesses/pi.sh and repro.sh of `root` beside the scenario's fake verdict and advisor, a fake pi on PATH, and the
    scenario's own fakes (test_fix.py) as the leaves it execs."""
    h = os.path.join(f.bakeoff, "harnesses")
    for name in ("pi.sh", "repro.sh"):
        shutil.copy(os.path.join(root, "bugfix-fleet", "bakeoff", "harnesses", name), os.path.join(h, name))
    os.makedirs(os.path.join(f.d, "agents"), exist_ok=True)
    shutil.copy(os.path.join(root, "bugfix-fleet", "agents", "repro.md"), os.path.join(f.d, "agents", "repro.md"))
    leaves = os.path.join(f.d, "leaves")
    tf.write(os.path.join(leaves, "fix.sh"), tf.PI_HARNESS)
    tf.write(os.path.join(leaves, "repro.sh"), tf.REPRO_HARNESS)
    tf.write(os.path.join(leaves, "review.sh"), tf.PI_BIN)
    with open(os.path.join(f.d, "bin", "pi"), "w") as fh:
        fh.write(FAKE_PI)
    os.chmod(os.path.join(f.d, "bin", "pi"), 0o755)
    return leaves


FLEET_DIR = os.path.join("fleets", "bugfix", "fix", "v2")


def translate(args, rules):
    """A scenario's chain arguments (v1's flags) as the two sides take them: (flags both take, v1's whole flags, the stations' settings).
    `rules` is the path of the rules file both sides read, so a path is never a delta: v1 gets it as --pi-rules, unless the scenario
    says none (''). The stations stand for the model, limit and tools flags (bugfix/fix2.py refuses them)."""
    flags, set_ = [], {"rules": True, "review_tools": "none", "fix_turns": 0, "repro_turns": 0, "timeout": None, "version": {}}
    i = 0
    while i < len(args):
        a, v = args[i], args[i + 1] if i + 1 < len(args) else None
        if a == "--pi-rules":
            set_["rules"], i = bool(v), i + 1
        elif a == "--review-tools":
            set_["review_tools"], i = v, i + 1
        elif a == "--fix-max-turns":
            set_["fix_turns"], i = int(v), i + 1
        elif a == "--repro-max-turns":
            set_["repro_turns"], i = int(v), i + 1
        elif a == "--episode-timeout":
            set_["timeout"], i = int(v), i + 1
        elif a == "--kickback-max":
            set_["version"]["kickback_max"], i = int(v), i + 1
        elif a == "--review-kickback-max":
            set_["version"]["review_kickback_max"], i = int(v), i + 1
        else:
            flags.append(a)
        i += 1
    old = list(flags) + ["--pi-rules", rules if set_["rules"] else "", "--review-tools", set_["review_tools"]]
    for key, flag in (("fix_turns", "--fix-max-turns"), ("repro_turns", "--repro-max-turns"), ("timeout", "--episode-timeout")):
        if set_[key]:
            old += [flag, str(set_[key])]
    for k, v in set_["version"].items():
        old += ["--" + k.replace("_", "-"), str(v)]
    return flags, old, set_


def code_root(dst, root=ROOT):
    """A code root at dst with the version's two texts (copies of bugfix/fleet_rules.md and agents/repro.md) where stations_doc names them."""
    os.makedirs(os.path.join(dst, FLEET_DIR), exist_ok=True)
    shutil.copy(os.path.join(root, "bugfix", "fleet_rules.md"), os.path.join(dst, FLEET_DIR, "rules.md"))
    shutil.copy(os.path.join(root, "bugfix-fleet", "agents", "repro.md"), os.path.join(dst, FLEET_DIR, "agent.md"))
    return dst


def stations_doc(settings, code):
    """FLOTILLA_STATIONS for a scenario: the three stations on the fixture's aliases, v1's defaults where the scenario sets no limit,
    their texts files of the code root (copies of bugfix/fleet_rules.md and agents/repro.md)."""
    def text(rel):
        with open(os.path.join(code, "fleets", "bugfix", rel), "rb") as fh:
            return {"path": rel, "sha256": hashlib.sha256(fh.read()).hexdigest()}
    rules = {"rules": text("fix/v2/rules.md")} if settings["rules"] else {}
    limits = {"timeout": f"{settings['timeout']}s" if settings["timeout"] else "20m", "max_output_tokens": 32768}
    return {"schema_version": 1, "version_limits": settings["version"], "stations": {
        "fix": {"harness": "pi", "model": "bugfix-flash", "tools": "all", "texts": dict(rules),
                "limits": dict(limits, **({"max_turns": settings["fix_turns"]} if settings["fix_turns"] else {}))},
        "repro": {"harness": "pi", "model": "bugfix-flash", "tools": "all", "texts": dict(rules, agent=text("fix/v2/agent.md")),
                  "limits": dict(limits, **({"max_turns": settings["repro_turns"]} if settings["repro_turns"] else {}))},
        "review": {"harness": "pi", "model": "bugfix-reviewer", "tools": settings["review_tools"], "texts": dict(rules),
                   "limits": {"timeout": "10m", "max_output_tokens": 32768}}}}


def run_scenario(name, root, entry, roots=()):
    """One scenario on the chain at `root`, started as `python3 <root>/bugfix/<entry>`: {"plan", "calls"} as recorded. fix2.py takes
    the scenario as stations; fix.py as flags."""
    verdicts, fixes, state, args = golden.SCENARIOS[name]
    f = tf.Fixture(verdicts, fixes)
    try:
        for fname, items in state.items():
            with open(os.path.join(f.state, fname), "w") as fh:
                fh.write("".join(x + "\n" for x in items))
        leaves = install_leaves(f, root)
        code = code_root(os.path.join(f.d, "code"), root)
        flags, old, settings = translate(args, os.path.join(code, FLEET_DIR, "rules.md"))
        plan = os.path.join(f.d, "plan.json")
        env = dict(os.environ, FLOTILLA_PLAN_FILE=plan, FLOTILLA_MODELS=json.dumps(tf.MODELS), FAKE_STATE=f.state, DIFF_LEAVES=leaves,
                   GIT_ALLOW_PROTOCOL="file", GIT_REMOTE_BASE=f.remotes, BUGFIX_BAKEOFF_DIR=f.bakeoff,
                   PATH=os.path.join(f.d, "bin") + os.pathsep + os.environ["PATH"],
                   GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t",
                   FLOTILLA_CASE=json.dumps({"id": "case-20261115-abc", "kind": "code_bug", "target": f"repo:{f.repo}/issues/12",
                                             "data": {"issue": {"repo": f.repo, "number": 12, "title": "value is 1",
                                                                "body": "value() returns 1, want 2", "url": "u"}}}))
        for k in [k for k in env if k.startswith(("PI_", "BUGFIX_PI", "FLOTILLA_STATIONS"))]:
            del env[k]
        common = ["--repo", f.repo, "--setup", "true", "--test-cmd", f"python3 {f.suite} run %s",
                  "--related-cmd", f"python3 {f.suite} related %s", "--scratch", f.scratch, *([] if "repros" in state else ["--no-repro"])]
        if entry == "fix2.py":
            env.update(FLOTILLA_STATIONS=json.dumps(stations_doc(settings, code)), FLOTILLA_CODE_ROOT=code, FLOTILLA_ROUTINE="bugfix/fix")
            tail = [*common, *flags]
        else:
            tail = [*common, "--worker-model", "bugfix-flash", "--advisor-model", "bugfix-reviewer", "--reviewer-model", "bugfix-reviewer",
                    *old]
        p = subprocess.run([sys.executable, os.path.join(root, "bugfix", entry), *tail], env=env, cwd=root, stdin=subprocess.DEVNULL,
                           capture_output=True, text=True)
        if not os.path.exists(plan):
            raise RuntimeError(f"{name}: {entry} at {root} wrote no plan (exit {p.returncode}): {(p.stdout + p.stderr)[-800:]}")
        with open(plan) as fh:
            doc = json.load(fh)
        with open(os.path.join(f.state, "differential.jsonl")) as fh:
            calls = [json.loads(x) for x in fh]
        norm = _norm(f, [os.path.realpath(root), root, *roots])
        prm = doc.get("params") or {}
        return norm({"plan": {k: doc.get(k) for k in ("outcome", "action", "detail")} | {"title": prm.get("title"), "body": prm.get("body")},
                     "calls": calls})
    finally:
        f.close()


def differential(old_root, new_root, new_entry="fix.py", scenarios=None):
    """The delta rows of every scenario between the chain at old_root (fix.py) and the one at new_root (new_entry)."""
    rows = []
    for name in scenarios or list(golden.SCENARIOS):
        rows += compare(name, run_scenario(name, old_root, "fix.py"), run_scenario(name, new_root, new_entry))
    return rows


def short(v, n=70):
    s = v if isinstance(v, str) else json.dumps(v, sort_keys=True)
    s = s.replace("\n", "\\n")
    return s if len(s) <= n else s[:n - 1] + "…"


def table(rows):
    """The delta table as text, and the number of unexplained deltas."""
    lines = [f"{'scenario':<38} {'call':>4} {'role':<7} {'field':<24} {'old':<30} {'new':<30} allowed by"]
    bad = 0
    for sc, call, role, fld, o, n, rule in rows:
        bad += rule is None
        if isinstance(o, str) and isinstance(n, str) and len(o) > 30 and len(n) > 30:   # two long texts: show where they part
            at = next((i for i, (x, y) in enumerate(zip(o, n)) if x != y), min(len(o), len(n)))
            o, n = f"@{at}: " + o[max(at - 8, 0):at + 12], f"@{at}: " + n[max(at - 8, 0):at + 12]
        lines.append(f"{sc:<38} {call!s:>4} {role:<7} {fld:<24} {short(o, 30):<30} {short(n, 30):<30} {rule or 'UNEXPLAINED'}")
    lines.append(f"DIFFERENTIAL {len(rows)} deltas, {len(rows) - bad} allowed, {bad} unexplained: " + ("PASS" if not bad else "FAIL"))
    return "\n".join(lines), bad


def main(argv=None):
    import argparse
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--old", default=OLD_SHA, help="the commit the old side is taken from")
    ap.add_argument("--entry", default="fix.py", help="the new side's entry point under bugfix/")
    ap.add_argument("scenarios", nargs="*")
    a = ap.parse_args(argv)
    with OldTree(a.old) as old:
        text, bad = table(differential(old, ROOT, a.entry, a.scenarios or None))
    print(text)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
