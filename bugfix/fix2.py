"""bugfix/fix2: the bugfix chain of a version that declares stations (v2-design.md sections 2 and 3, step 6 phase C).

bugfix/fix.py is the chain of fleets/bugfix/fix/v1 and is never edited under it (the spec hash does not cover code: new code ships
beside the old). This is the same chain for a version whose spec declares the stations fix, repro and review; a run is started with
FLOTILLA_STATIONS and no model, text or limit in its flags:

  python3 bugfix/fix2.py --repo example/shop --spend-file .bugfix-fix-spend

What a station gives the chain: its model alias (`fix` the verdict's too, `review` the advisor's: the two aliases v1 named by
flags), its `rules` text (the system text of its pi call), the `agent` text of `repro` (agents/repro.md), its limits (timeout,
max_turns, max_output_tokens: the floor of the model's reply limit in pi's models.json) and, for `review`, its tools (none or read).
The version's own limits (kickback_max, review_kickback_max, test_timeout, setup_timeout) are read from FLOTILLA_STATIONS too.
Verdict and advisor are v1's leaves still (bash, harnesses of bugfix-fleet/bakeoff); everything else is fix.py's Chain.

The pi calls of the chain's three stations go through runtime/adapter.py and runtime/harness_pi.py; see Chain below.
"""
import contextlib
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
sys.path.append(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "runtime"))
import adapter  # noqa: E402
import callrec_pi  # noqa: E402
import declared_prompts  # noqa: E402
import fix  # noqa: E402
import harness_pi  # noqa: E402
import harness_prompts  # noqa: E402
import prompts  # noqa: E402
import stations  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
FIX, REPRO, REVIEW = "fix", "repro", "review"
# What a v1 flag said, now said by a station or the version's limits: a run that still passes one is refused, so a model, a text or
# a limit is never in two places (v2-design.md section 2).
REFUSED_FLAGS = ("--worker-model", "--advisor-model", "--reviewer-model", "--harness", "--pi-rules", "--review-tools", "--min-output",
                 "--episode-timeout", "--review-timeout", "--fix-max-turns", "--repro-max-turns", "--kickback-max",
                 "--review-kickback-max", "--test-timeout", "--setup-timeout")
VERSION_LIMITS = {"kickback_max": "--kickback-max", "review_kickback_max": "--review-kickback-max",
                  "test_timeout": "--test-timeout", "setup_timeout": "--setup-timeout"}
# The checkpoint keys of the repro and review roles name the pi adapter, not the bash leaf: a v1 checkpoint is not continued by
# this chain, nor this chain's by v1 (fix.load_checkpoint compares them). The fix role's key stays "pi": fix.Chain.attempt names
# the leaf it runs from it.
ADAPTER_KEY = "pi-adapter"
NODE_OPTIONS = "--max-old-space-size=4096"   # pi's heap, as harnesses/pi.sh and repro.sh set it
FIX_PARSE = fix.parse   # fix.main is run with parse swapped for this module's


def seconds(v):
    """A station's timeout (`20m`, `90s`, `1h30m`; a bare number is seconds) in seconds."""
    if isinstance(v, bool):
        raise stations.StationsError(f"duration {v!r}")
    if isinstance(v, (int, float)):
        return int(v)
    m = re.fullmatch(r"(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?", str(v or "").strip())
    if not m or not any(m.groups()):
        raise stations.StationsError(f"duration {v!r} is not like 20m, 90s or 1h30m")
    h, mi, s = (int(x or 0) for x in m.groups())
    return h * 3600 + mi * 60 + s


def _need(st, names):
    missing = [n for n in names if n not in st.doc["stations"]]
    if missing:
        raise stations.StationsError(f"the version declares no station {', '.join(missing)} (it has {st.names()}): "
                                     f"bugfix/fix2.py runs the stations {', '.join(names)}")


def flags_of(st, argv):
    """The v1 flags the stations and the version's limits stand for, as fix.parse takes them."""
    argv = list(argv)
    bad = [x for x in argv if x.split("=", 1)[0] in REFUSED_FLAGS]
    if bad:
        raise stations.StationsError(f"{', '.join(bad)}: the stations and the version's limits say that; no model, text or limit in a flag")
    repro = "--no-repro" not in argv
    _need(st, (FIX, REPRO, REVIEW) if repro else (FIX, REVIEW))
    for name in (FIX, REPRO, REVIEW) if repro else (FIX, REVIEW):
        s = st.station(name)
        if s.get("harness") != "pi":
            raise stations.StationsError(f"station {name}: harness {s.get('harness')!r}; bugfix/fix2.py serves pi")
        if not s.get("model"):
            raise stations.StationsError(f"station {name} names no model")
        want = ("none", "read") if name == REVIEW else ("all",)
        if s.get("tools") not in want:
            raise stations.StationsError(f"station {name}: tools {s.get('tools')!r}; the {name} step takes {' or '.join(want)}")
    fx, rv = st.station(FIX), st.station(REVIEW)
    flags = ["--worker-model", fx["model"], "--advisor-model", rv["model"], "--reviewer-model", rv["model"],
             "--review-tools", rv["tools"]]
    lim = lambda s, k: (s.get("limits") or {}).get(k) or 0  # noqa: E731
    if lim(fx, "timeout"):
        flags += ["--episode-timeout", str(seconds(lim(fx, "timeout")))]
    if lim(rv, "timeout"):
        flags += ["--review-timeout", str(seconds(lim(rv, "timeout")))]
    if lim(fx, "max_turns"):
        flags += ["--fix-max-turns", str(lim(fx, "max_turns"))]
    if repro and lim(st.station(REPRO), "max_turns"):
        flags += ["--repro-max-turns", str(lim(st.station(REPRO), "max_turns"))]
    floor = max((int(lim(st.station(n), "max_output_tokens")) for n in ((FIX, REPRO, REVIEW) if repro else (FIX, REVIEW))), default=0)
    flags += ["--min-output", str(floor)]
    for name, flag in VERSION_LIMITS.items():
        if name in st.limits():
            flags += [flag, str(int(st.limits()[name]))]
    return flags


def parse(argv, st):
    """fix.parse over the flags the run was given and the ones its stations stand for."""
    a = FIX_PARSE(list(argv) + flags_of(st, argv))
    a.stations = st
    a.repro_harness = a.review_harness = ADAPTER_KEY
    return a


def fleet_root(environ):
    """(code root, fleet) a station's text is resolved under: the texts are paths inside the fleet's folder."""
    fleet = (environ.get("FLOTILLA_ROUTINE") or "bugfix/fix").split("/")[0]
    return environ.get("FLOTILLA_CODE_ROOT") or os.path.dirname(HERE), fleet


def text_path(st, station, name, environ):
    """The verified file of a station's text (declared_prompts: hash, no symlink, inside the fleet), None when the station declares
    none; declared_prompts.PromptError when the file is not what flotillad hashed."""
    ref = (st.station(station).get("texts") or {}).get(name)
    if not ref:
        return None
    root, fleet = fleet_root(environ)
    return declared_prompts.resolve(name, {"FLOTILLA_PROMPTS": json.dumps({name: ref})}, root, fleet)


def pi_rules(a, environ):
    """fix.pi_rules for stations: the fix station's `rules` text, the rules the verdict and the advisor leaves get too ('' when
    it declares none: pi then loads no rules, and still never the repository's)."""
    return text_path(a.stations, FIX, "rules", environ) or ""


def pi_entry(models, provider, model):
    """pi's catalog entry of `model` in a models.json (without its id): what pi_config wrote for it, its context window and the
    reply-limit floor."""
    for m in ((models.get("providers") or {}).get(provider) or {}).get("models") or []:
        if m.get("id") == model:
            return {k: v for k, v in m.items() if k != "id"}
    return {}


class Chain(fix.Chain):
    def __init__(self, a, case, run_dir, environ, meter=None, restored=None):
        super().__init__(a, case, run_dir, environ, meter, restored)
        self.st = a.stations
        # pi_config wrote one models.json with every alias of the run, as v1's leaves need it; the adapter writes the one route of
        # its call over it, so this is the file to put back after each (restore_models)
        self.models_path = os.path.join(run_dir, "pi", "models.json")
        with open(self.models_path) as f:
            self.models_text = f.read()
        self.models = json.loads(self.models_text)
        self.rules = {}
        for name in (FIX, REPRO, REVIEW):
            if name in self.st.doc["stations"]:
                try:
                    self.rules[name] = text_path(self.st, name, "rules", environ)
                except declared_prompts.PromptError as e:
                    raise fix.Stop(f"prompt: {e}") from None
        # the reproducer's text, read (and its hash checked) before any model call; repro.sh read agents/repro.md for it
        self.repro_agent = ""
        if a.repro:
            root, fleet = fleet_root(environ)
            try:
                self.repro_agent = harness_prompts.agent_def_body(
                    self.st.render(REPRO, "agent", {}, fleet, root=root, environ=environ).text)
            except (stations.StationsError, prompts.PromptError) as e:
                raise fix.Stop(f"prompt: {e}") from None
        self.environ0 = dict(environ)

    def route(self, alias):
        """The model route of an alias with pi's catalog entry for it (context window, prices, the reply-limit floor) as pi_config
        wrote it, so the models.json the adapter writes for the route is the entry pi_config's was."""
        r = adapter.route(self.cfg, alias)
        r.meta["pi_model"] = pi_entry(self.models, "flotilla-" + r.endpoint, r.model)
        return r

    def restore_models(self):
        with open(self.models_path, "w") as f:
            f.write(self.models_text)
        os.chmod(self.models_path, 0o600)

    def execute(self, req, env=None):
        """One station's pi call through the adapter (adapter.execute on harness_pi.Pi, run by fix.sh as every call of the chain is).
        The adapter writes pi/models.json for the call's one route, so the run's own file, which the verdict and advisor leaves read,
        is put back when it returns, however it returns."""
        try:
            got = adapter.execute(harness_pi.PI, req, self.env if env is None else env, run=fix.sh)
        except adapter.HarnessError as e:
            raise fix.Stop(f"{req.station}: {e}") from None
        finally:
            self.restore_models()
        self.keep(got[0], req, got[1])
        return got

    def keep(self, ep, req, code):
        """The episode as <run>/episodes/<n>-<station>.json, a version 2 document (adapter.episode_doc: `station`, not v1's `role`),
        for evaluation. pi's calls and spend have one source: its session files (calls.record, spend.pi_cost), never this file as
        well: the file is marked recorded before it exists, so a run that resumes does not write its calls again, and
        spend.episode_cost counts the episodes of the harnesses that are not pi. A file that cannot be written is said and the run goes on."""
        self.episode_n += 1
        stem = f"{self.episode_n:03d}-{req.station}"
        try:
            os.makedirs(os.path.join(self.run_dir, adapter.EPISODES), exist_ok=True)
            open(os.path.join(self.run_dir, adapter.EPISODES, stem + ".json" + callrec_pi.RECORDED), "w").close()
            doc = adapter.episode_doc(ep, req.model.alias, self.episode_n, code, harness_pi.PI.resume_ref(ep))
            doc["calls_role"] = fix.CALLS_ROLE.get(req.station, req.station)
            adapter.write_episode(self.run_dir, stem, doc)
        except OSError as e:
            print(f"BUGFIX FIX #{self.n} no episode file for {req.station}: {e}", flush=True)

    def adopt(self, ck):
        super().adopt(ck)
        self.forget_pi_episodes()

    def forget_pi_episodes(self):
        """A resumed run numbers its sessions and episode files by first appearance (callrec_pi.Recorder.skip_existing); the pi episode
        files are a second record of sessions that are numbered already, and would shift every later session's number."""
        for f in list(self.calls.seq):
            if f.endswith(".json") and os.path.dirname(f) == self.calls.episodes:
                try:
                    with open(f) as fh:
                        if json.load(fh).get("harness") == "pi":
                            del self.calls.seq[f]
                except (OSError, ValueError, AttributeError):
                    pass

    # fix.Chain runs the fix and the reproducer as bash leaves (harnesses/pi.sh, repro.sh) through the module's `sh`. These two run
    # the same steps with the leaf's call replaced: while one of them runs, a command that is one of those leaves is the station's
    # call through the adapter, whatever else the step runs (git, the tests) is `sh` as it was.
    @contextlib.contextmanager
    def leaves(self):
        outer = fix.sh
        scripts = {os.path.join(fix.BAKEOFF, "harnesses", self.a.harness + ".sh"): FIX,
                   os.path.join(fix.BAKEOFF, "harnesses", "repro.sh"): REPRO}

        def dispatch(cmd, cwd, env=None, timeout=None, check=False, max_turns=None, turn_ended=None, tee_path=None, stdin=None):
            role = scripts.get(cmd[0]) if isinstance(cmd, list) and len(cmd) == 3 else None
            if role is None:
                return outer(cmd, cwd, env, timeout, check=check, max_turns=max_turns, turn_ended=turn_ended, tee_path=tee_path,
                             stdin=stdin)
            return self.station(role, cmd[2], env, timeout, max_turns)
        fix.sh = dispatch
        try:
            yield
        finally:
            fix.sh = outer

    def station(self, role, desc, env, timeout, max_turns):
        """The fix's or the reproducer's call: (exit code, pi's stream), as the leaf gave them. A fix is the template around the task
        (harness_prompts.fix_prompt; a resumed one is sent `desc` as it is, the next message of its session), the reproducer's the
        agent text then the issue. harnesses/pi.sh and repro.sh export NODE_OPTIONS for pi's heap; so does this."""
        s = self.st.station(role)
        resume = (env or {}).get("PI_RESUME_SESSION") or None if role == FIX else None
        prompt = (desc if resume else harness_prompts.fix_prompt(desc)) if role == FIX else harness_prompts.repro_prompt(self.repro_agent, desc)
        limit = seconds((s.get("limits") or {}).get("timeout") or timeout)
        req = adapter.Request(station=role, prompt=prompt, system_path=self.rules.get(role) or None, model=self.route(s["model"]),
                              tools="all", resume=resume, max_turns=max_turns or 0, workdir=self.wt, run_dir=self.run_dir, timeout=limit)
        _, code, stream = self.execute(req, dict(env if env is not None else self.env, NODE_OPTIONS=NODE_OPTIONS))
        return code, stream

    def attempt(self, manifest, resume=None, cont=None):
        with self.leaves():
            return super().attempt(manifest, resume, cont)

    def repro(self, manifest):
        with self.leaves():
            return super().repro(manifest)

    def review(self, diff, task="", summary=""):
        prompt = self.review_prompt(diff, task, summary)
        tools = "none" if self.a.review_tools == "none" else "read"
        # the rules are about using tools: a review that has none is not given them (and still never the repository's own)
        req = adapter.Request(station=REVIEW, prompt=prompt, system_path=(self.rules.get(REVIEW) or None) if tools == "read" else None,
                              model=self.route(self.a.reviewer_model), tools=tools, resume=None, max_turns=0, workdir=self.wt,
                              run_dir=self.run_dir, timeout=self.a.review_timeout)
        ep, code, _ = self.execute(req)
        self.calls.record("reviewer", self.a.reviewer_model)
        if ep.answer:
            return ep.answer
        return f"(no review: {fix.tail(ep.error, 300)})" if ep.error else f"(no review: the reviewer episode ended {code} with no text)"


@contextlib.contextmanager
def _swapped(**attrs):
    old = {k: getattr(fix, k) for k in attrs}
    for k, v in attrs.items():
        setattr(fix, k, v)
    try:
        yield
    finally:
        for k, v in old.items():
            setattr(fix, k, v)


def main(argv=None, environ=None):
    """fix.main with this module's parse, Chain and rules: the run's lock, checkpoint, spend and plan file are fix.py's."""
    environ = dict(os.environ if environ is None else environ)
    argv = sys.argv[1:] if argv is None else argv
    try:
        st = stations.load(environ)
        if st is None:
            raise stations.StationsError("FLOTILLA_STATIONS is not set: bugfix/fix2.py runs a version that declares stations")
        flags_of(st, argv)
    except stations.StationsError as e:
        out = {"outcome": "escalate", "detail": f"stations: {e}"}
        with open(environ["FLOTILLA_PLAN_FILE"], "w") as f:
            json.dump(out, f)
        print(f"BUGFIX FIX2 outcome=escalate {out['detail']}")
        return out
    with _swapped(parse=lambda av: parse(av, st), Chain=Chain, pi_rules=pi_rules):
        return fix.main(argv, environ)


if __name__ == "__main__":
    try:
        main()
    except fix.RunBusy as e:
        print(f"BUGFIX FIX {e}", flush=True)
        sys.exit(fix.EXIT_BUSY)
