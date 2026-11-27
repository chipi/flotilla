"""SOPs: a fleet's standard operating procedures, as skill folders (operator step 1 build plan, decisions 4 and 5).

An SOP is `<fleet>/sops/<id>/SKILL.md`: the skills layer's file (name equal to the folder, a 16 KiB cap, a sha256, a `loaded`
record), with a flat `key: value` frontmatter whose values are JSON except `name` and `description`:

  name: backup-stale
  description: Use when a backup's age passes its limit on a host with a backup job.
  applies_to: {"kinds":["failed_backup"],"hosts":["*"],"alerts":[]}
  diagnosis: [{"read":"metric","name":"backup_age"}]
  fix: [{"action":"backup.rerun"}]
  verify: {"check":"backup_newer_than_execution","within":"60m"}
  rollback: []
  rung: 2
  evidence: ["case-..."]

An SOP is data a routine reads, never code and never a grant:
  - `fix` names registry actions only. fix_actions() refuses a name the registry lacks, so an SOP can never add a command.
  - `rung` is a request. Policy still takes the lowest of the routine's, the action's, the host's and the installation's caps.
  - `diagnosis` reads are metrics only in step 1: no host read exists yet.
  - match() is deterministic and never guesses: the most specific SOP wins, an exact tie matches none and says so.
Nothing here writes an SOP: a new or changed one is a proposal a person commits (rule 3: nothing rewrites its own playbooks).
Stdlib only.
"""
import fnmatch
import json
import os
import re
import sys
from dataclasses import dataclass, field

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import skills  # noqa: E402

LAYER = "fleet sop"
KEYS = ("name", "description", "applies_to", "diagnosis", "fix", "verify", "rollback", "rung", "evidence")
REQUIRED = ("name", "description", "applies_to", "fix", "verify", "rung")
APPLIES = ("kinds", "hosts", "alerts")
WITHIN = re.compile(r"^[1-9][0-9]*[smh]$")
ACTION = re.compile(r"^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$")
METRIC = re.compile(r"^[a-zA-Z_:][a-zA-Z0-9_:]{0,127}$")
GLOB = re.compile(r"[*?\[]")
MAX_RUNG = 3
CITE = "SOP "          # how a plan's detail begins when it follows an SOP: "SOP <name> sha256:<hash>: ..."


class SopError(skills.SkillError):
    """An SOP that is malformed, or one that asks for what the registry does not have: the installation's mistake, never skipped."""


@dataclass
class Sop:
    name: str
    description: str
    applies_to: dict
    fix: list
    verify: dict
    rung: int
    body: str
    diagnosis: list = field(default_factory=list)
    rollback: list = field(default_factory=list)
    evidence: list = field(default_factory=list)
    sha256: str = ""
    path: str = ""

    def cite(self):
        return f"{CITE}{self.name} sha256:{self.sha256}"


def _strs(name, key, v, nonempty=False):
    if not isinstance(v, list) or not all(isinstance(x, str) and x.strip() for x in v) or (nonempty and not v):
        raise SopError(f"SOP {name}: {key} is a list of {'one or more ' if nonempty else ''}non-empty texts")
    return v


def _steps(name, key, v, nonempty):
    if not isinstance(v, list) or (nonempty and not v):
        raise SopError(f"SOP {name}: {key} is a list{' of one or more actions' if nonempty else ''}")
    for s in v:
        if not isinstance(s, dict) or set(s) != {"action"} or not isinstance(s["action"], str) or not ACTION.match(s["action"]):
            raise SopError(f"SOP {name}: {key} step {s!r} is not {{\"action\": \"<registry action>\"}}")
    return v


def parse(text, name):
    """The Sop in a SKILL.md (its sha256 is the loader's to set), or SopError. Refuses bad JSON, a key it does not know, a key twice,
    a name that is not the folder's and a missing required key."""
    try:
        head, body = skills.split(text, name)
    except skills.SkillError as e:
        raise SopError(str(e)) from e
    raw = {}
    for line in head:
        if not line.strip():
            continue
        k, sep, v = line.partition(":")
        k, v = k.strip(), v.strip()
        if not sep or k not in KEYS:
            raise SopError(f"SOP {name}: frontmatter line {line[:60]!r} is not `key: value` with a key of {', '.join(KEYS)}")
        if k in raw:
            raise SopError(f"SOP {name}: {k} is given twice")
        if k in ("name", "description"):
            raw[k] = v[1:-1] if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'" else v
            continue
        try:
            raw[k] = json.loads(v)
        except ValueError as e:
            raise SopError(f"SOP {name}: {k} is not JSON ({e})") from e
    missing = [k for k in REQUIRED if k not in raw]
    if missing:
        raise SopError(f"SOP {name}: missing {', '.join(missing)}")
    if raw["name"] != name:
        raise SopError(f"SOP {name}: frontmatter name is {raw['name']!r}, not the folder's name")
    if not 1 <= len(raw["description"]) <= skills.MAX_DESCRIPTION:
        raise SopError(f"SOP {name}: description must be 1 to {skills.MAX_DESCRIPTION} characters")
    if not body:
        raise SopError(f"SOP {name}: no instructions after the frontmatter")
    at = raw["applies_to"]
    if not isinstance(at, dict) or set(at) - set(APPLIES) or "kinds" not in at:
        raise SopError(f"SOP {name}: applies_to is {{\"kinds\": [...], \"hosts\": [...], \"alerts\": [...]}}, kinds at least")
    applies = {k: _strs(name, f"applies_to.{k}", at.get(k, []), nonempty=(k == "kinds")) for k in APPLIES}
    diagnosis = raw.get("diagnosis", [])
    if not isinstance(diagnosis, list) or any(
            not isinstance(d, dict) or set(d) != {"read", "name"} or d["read"] != "metric"
            or not isinstance(d["name"], str) or not METRIC.match(d["name"]) for d in diagnosis):
        raise SopError(f"SOP {name}: diagnosis is a list of {{\"read\": \"metric\", \"name\": <metric>}} (no host read exists yet)")
    fix = _steps(name, "fix", raw["fix"], True)
    verify = raw["verify"]
    if not isinstance(verify, dict) or set(verify) != {"check", "within"} or not isinstance(verify["check"], str) \
            or not verify["check"] or not isinstance(verify["within"], str) or not WITHIN.match(verify["within"]):
        raise SopError(f"SOP {name}: verify is {{\"check\": <name>, \"within\": \"60m\"}}")
    rung = raw["rung"]
    if isinstance(rung, bool) or not isinstance(rung, int) or not 0 <= rung <= MAX_RUNG:
        raise SopError(f"SOP {name}: rung is a whole number from 0 to {MAX_RUNG} (a request, never a grant)")
    return Sop(name=name, description=raw["description"], applies_to=applies, fix=fix, verify=verify, rung=rung, body=body,
               diagnosis=diagnosis, rollback=_steps(name, "rollback", raw.get("rollback", []), False),
               evidence=_strs(name, "evidence", raw.get("evidence", [])))


def sops_dir(fleet, root=None):
    return os.path.join(root or skills.code_root(), "fleets", fleet, "sops")


def load_all(directory, version=None):
    """Every SOP under `directory` (a fleet's sops folder), by name: each read through the skills layer, so each has its sha256, the
    size cap and a `loaded` record. A folder that is not an SOP is the installation's mistake and refuses the lot (never skipped);
    no folder is no SOPs."""
    if not os.path.isdir(directory):
        return []
    out = []
    for entry in sorted(os.listdir(directory)):
        d = os.path.join(directory, entry)
        if not os.path.isdir(d) and not os.path.islink(d):
            continue
        if not skills.NAME_RE.match(entry):
            raise SopError(f"SOP folder {entry!r} does not match {skills.NAME_RE.pattern}")
        path = os.path.join(d, "SKILL.md")
        if not os.path.isfile(path):
            raise SopError(f"SOP {entry}: no SKILL.md")
        rec, sop = skills.read(path, entry, LAYER, version, parse_fn=parse)
        sop.sha256, sop.path = rec["sha256"], path
        out.append(sop)
    return out


def case_data(case):
    """The case's opening data as a dict: FLOTILLA_CASE has it as an object, a case event has it as JSON text."""
    d = (case or {}).get("data")
    if isinstance(d, str):
        try:
            d = json.loads(d)
        except ValueError:
            return {}
    return d if isinstance(d, dict) else {}


def case_host(case):
    t = (case or {}).get("target") or ""
    return t.split(":", 1)[1] if t.startswith("host:") else ""


def _rank(patterns, value):
    """How specifically the best of `patterns` matches `value`: 2 an exact name, 1 a glob with something fixed in it, 0 anything
    (`*`, or no pattern at all); -1 when none matches."""
    if not patterns:
        return 0
    best = -1
    for p in patterns:
        if not fnmatch.fnmatchcase(value, p):
            continue
        best = max(best, 2 if not GLOB.search(p) else 0 if set(p) == {"*"} else 1)
    return best


def specificity(sop, case):
    """The SOP's specificity for this case as (alert, host) ranks, or None when it does not apply: the kind must be listed, the
    host (the target's `host:<h>`; none is "") and the alert (data.alert) must match what the SOP names."""
    at = sop.applies_to
    if (case or {}).get("kind") not in at["kinds"]:
        return None
    host = _rank(at["hosts"], case_host(case))
    alert = 0
    if at["alerts"]:
        named = str(case_data(case).get("alert") or "")
        alert = _rank(at["alerts"], named) if named else -1
    return None if host < 0 or alert < 0 else (alert, host)


def match(sops, case):
    """(the SOP that applies, why not): the most specific SOP wins. Two SOPs equally specific match none and the reason names them
    (never a guess); none applying is (None, "no SOP applies")."""
    scored = [(s, specificity(s, case)) for s in sops]
    scored = [(s, k) for s, k in scored if k is not None]
    if not scored:
        return None, "no SOP applies"
    top = max(k for _, k in scored)
    best = [s for s, k in scored if k == top]
    if len(best) > 1:
        return None, f"ambiguous: {', '.join(sorted(s.name for s in best))} apply equally (alert {top[0]}, host {top[1]})"
    return best[0], ""


def registry_paths(environ=None):
    environ = os.environ if environ is None else environ
    raw = environ.get("FLOTILLA_REGISTRY", "").strip()
    return [p for p in raw.split(os.pathsep) if p] or [os.path.join(skills.code_root(), "host", "registry.json")]


def load_registry(paths=None):
    """{action name: its declaration} from the registry files in effect (the image's, plus the installation's own: FLOTILLA_REGISTRY
    is a list separated like PATH). An action declared twice is refused, as flotillad refuses it."""
    out = {}
    for p in paths or registry_paths():
        with open(p) as f:
            for a in json.load(f)["actions"]:
                if a["name"] in out:
                    raise SopError(f"registry: {a['name']} is declared twice ({p})")
                out[a["name"]] = a
    return out


def fix_actions(sop, registry, kind=None):
    """The registry's declarations of the SOP's fix actions, in order, or SopError: an action the registry lacks is refused (an SOP
    never adds a command), so is one that is not for the case's kind, a rollback the registry lacks, and a verify check that is not the
    one the first action's registry check runs (the registry decides what verifies a fix, the SOP only says which)."""
    lack = [s["action"] for s in sop.fix + sop.rollback if s["action"] not in registry]
    if lack:
        raise SopError(f"SOP {sop.name}: {', '.join(lack)} is not in the registry (an SOP never adds a command)")
    acts = [registry[s["action"]] for s in sop.fix]
    if kind is not None:
        wrong = [a["name"] for a in acts if kind not in (a.get("for_kinds") or [])]
        if wrong:
            raise SopError(f"SOP {sop.name}: {', '.join(wrong)} is not an action for {kind} cases")
    want = (acts[0].get("check") or {}).get("verify") or ""
    if want and sop.verify["check"] != want:
        raise SopError(f"SOP {sop.name}: verify.check is {sop.verify['check']!r}, the registry's check for {acts[0]['name']} is {want!r}")
    return acts


def render(sop):
    """The SKILL.md text of an SOP (a draft's body): parse(render(x)) is x."""
    def js(v):
        return json.dumps(v, separators=(",", ":"))
    lines = ["---", f"name: {sop.name}", f"description: {' '.join(sop.description.split())}", f"applies_to: {js(sop.applies_to)}",
             f"diagnosis: {js(sop.diagnosis)}", f"fix: {js(sop.fix)}", f"verify: {js(sop.verify)}", f"rollback: {js(sop.rollback)}",
             f"rung: {sop.rung}", f"evidence: {js(sop.evidence)}", "---", sop.body.strip(), ""]
    return "\n".join(lines)
