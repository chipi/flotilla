"""The harness (ADR-0004, RFC-0002 §4): one model-driven step, the same code for every fleet.

  run_json_step(messages, call, tool, cfg, errors, on_quirk=None, log=print) -> Result

The JSON protocol (the first fleet's): each turn the model returns ONE object, either a tool request
{"action": "probe", "probe": <name>, "probe_args": {...}} or a decision {"action": "decide",
"disposition": <str>, ...}. The harness sends the request to `tool`, feeds the result back, and stops at
a decision or at the limits. Gates and outputs are the caller's: the harness only returns the decision.

`call(messages, step)` makes one model call and returns (reply text, usage); `tool(name, args)` returns
the tool's result (never raises). Both are the caller's functions, looked up by the caller at call time,
so a fleet's evaluation can patch them where it always has.

`cfg` is the routine's harness configuration (spec `harness:`, FLOTILLA_HARNESS):
  limits: max_tool_calls (tool turns), max_steps (= max_tool_calls + 1 model turns)
  quirks: the first fleet's loop as it was, kept so the move to the harness is like for like and removed
          one at a time later, each as a behaviour change proven by model replay (RFC-0002 §9 step 6):
    bad_json_uses_turn      a reply that is not JSON costs a turn
    empty_retry             an empty reply retries the turn with no new message; empty on the last
                            turn defers
    final_decision_call     one extra call when the turns run out on a decision that is not flat
    reply_echo_chars        earlier replies go back cut to this many characters (decision_echo_chars
                            for a decision that is not flat)
    result_chars            a tool result goes back cut to this many characters
  errors: {"fatal": (exception classes re-raised as they are), "defer": exception class, "defer_prefix": str}

`on_quirk(name)` is told when a turn hits a quirk, so the caller can mark that turn's record.
"""

import json
import re


class Result:
    def __init__(self, decision, trace, usage, in_loop):
        self.decision, self.trace, self.usage = decision, trace, usage
        self.in_loop = in_loop   # decided inside the turns (False: the turns ran out)


NO_DECISION = {"disposition": "escalate", "reason": "no terminal decision",
               "question": "triager did not reach a disposition"}


def parse(raw):
    m = re.search(r"\{.*\}", raw, re.S)
    if not m:
        raise ValueError("no JSON object")
    return json.loads(m.group(0))


def trim(v, n):
    s = v if isinstance(v, str) else json.dumps(v)
    return s if len(s) <= n else s[:n] + f"\n…[truncated {len(s)-n} of {len(s)} chars]"


def run_json_step(messages, call, tool, cfg, errors, on_quirk=None, log=print):
    lim, q = cfg["limits"], cfg["quirks"]
    n, steps = lim["max_tool_calls"], lim["max_steps"]
    if steps != n + 1:
        raise ValueError(f"harness limits: max_steps {steps} must be max_tool_calls {n} + 1 (the deciding turn)")
    for k in ("bad_json_uses_turn", "empty_retry", "final_decision_call"):
        if q.get(k) is not True:
            raise ValueError(f"harness quirk {k}: only true is implemented until RFC-0002 §9 step 6")
    echo, decho, rchars = q["reply_echo_chars"], q["decision_echo_chars"], q["result_chars"]
    fatal, defer, prefix = errors["fatal"], errors["defer"], errors["defer_prefix"]
    mark = on_quirk or (lambda name: None)
    trace, usage = [], {"prompt_tokens": 0, "completion_tokens": 0}
    last_empty = False
    for step in range(steps):
        try:
            raw, turn_usage = call(messages, step)
            if isinstance(turn_usage, dict):
                for k in ("prompt_tokens", "completion_tokens"):
                    usage[k] += turn_usage.get(k) or 0
        except fatal:
            raise
        except Exception as e:  # transport (one-off flake): retry next cycle, never escalate
            raise defer(f"{prefix}{e}") from e
        if not isinstance(raw, str) or not raw.strip():
            # a contentless completion is a provider flake: retry the same turn, feed nothing back
            log(f"    [triage] empty completion at step {step+1} — retrying turn")
            mark("empty_retry")
            last_empty = True
            continue
        last_empty = False
        try:
            out = parse(raw)
        except Exception as e:
            mark("bad_json_uses_turn")
            messages.append({"role": "assistant", "content": raw[:echo]})
            messages.append({"role": "user", "content": f"Invalid JSON ({e}). Return ONE valid JSON object only."})
            continue
        if out.get("action") == "probe" and step < n:
            name, args = out.get("probe"), out.get("probe_args") or {}
            result = tool(name, args)
            trace.append({"probe": name, "args": args, "result": result})
            log(f"    probe[{step+1}] {name} -> {trim(result, 90)}")
            messages.append({"role": "assistant", "content": raw[:echo]})
            messages.append({"role": "user", "content": f"PROBE {name} result:\n{trim(result, rchars)}"})
            continue
        # decide: the disposition object IS `out` (flat; "disposition" is a string)
        disp = {k: v for k, v in out.items() if k != "action"}
        if not isinstance(disp.get("disposition"), str):
            messages.append({"role": "assistant", "content": raw[:decho]})
            messages.append({"role": "user", "content":
                             ("Probe budget exhausted. " if step >= n else "")
                             + 'Return a FLAT decision: {"action":"decide","disposition":'
                               '"dismiss|cleanup|file|escalate", ...}.'})
            if step < n:
                continue
            try:   # its usage is not counted and any failure is a missing decision, as before
                out2 = parse(call(messages, step + 1)[0])
                disp = {k: v for k, v in out2.items() if k != "action"}
            except Exception:
                disp = {}
            finally:
                mark("final_decision_call")   # after the call: the mark goes on its own turn
        if not isinstance(disp.get("disposition"), str):
            disp = dict(NO_DECISION)
        return Result(disp, trace, usage, in_loop=True)
    if last_empty:
        raise defer("empty completion on the last turn")
    return Result(dict(NO_DECISION), trace, usage, in_loop=False)
