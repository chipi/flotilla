"""Hooks (ADR-0018): code the harness runs around every tool call, declared by name in the spec.

  harness:
    hooks:
      before_tool: [{name: ..., mode: enforce}]
      after_tool:  [{name: redact_credentials, mode: observe}]

A hook is a pure function with no I/O of its own: the spec names it, the code implements it (like a gate), and a
name no registry knows fails when the run starts. `call` is {"name": tool, "args": {...}}.

  before_tool  fn(call) -> None | Refuse(reason)
               enforce: the tool is not called; the model gets "<refused by hook NAME: reason>" as the tool's
               result, through the same path as an unknown tool, and the turn is spent. A hook that raises refuses.
  after_tool   fn(call, result) -> None | Replace(text, reason)
               enforce: the replacement is the result, in the trace and in the model's context; the event carries
               the original's length and sha256. A hook that raises withholds the result.

  observe      the hook runs and what it would have done is recorded; nothing changes.

Every effect (and every error) is reported through `event("hook", ...)`; the caller records it (hooks.step_detail
for the calls file, hooks.span for tracing). Shared hooks live here; a fleet passes its own as `extra`. Stdlib only.
"""
import hashlib
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import toolrec  # noqa: E402

MODES = ("enforce", "observe")
NAME_RE = re.compile(r"^[a-z][a-z0-9_]{0,39}$")
REASON_MAX = 200


class HookError(Exception):
    """A hook declaration that cannot run: refused when the run starts, never skipped."""


class Refuse:
    def __init__(self, reason):
        self.reason = reason


class Replace:
    def __init__(self, text, reason):
        self.text, self.reason = str(text), reason


def _text(result):
    return result if isinstance(result, str) else json.dumps(result)


def _redact_credentials(call, result):
    text = _text(result)
    out = toolrec.redact_credentials(text)
    return Replace(out, "credentials masked") if out != text else None


# name -> (the phase it runs in, its function)
SHARED = {"redact_credentials": ("after_tool", _redact_credentials)}


class Hooks:
    def __init__(self, before=(), after=()):
        self.before, self.after = list(before), list(after)   # [(name, mode, fn)]

    def __bool__(self):
        return bool(self.before or self.after)

    def run_before(self, call, event):
        """The refusal text the model gets in place of the tool's result, or None to call the tool."""
        for name, mode, fn in self.before:
            try:
                out = fn(call)
            except Exception as e:  # noqa: BLE001 — a hook that cannot decide refuses
                reason = f"hook raised {type(e).__name__}"
                event("hook", phase="before_tool", name=name, mode=mode, effect="error", reason=reason, tool=call["name"])
                if mode == "enforce":
                    return _refused(name, reason)
                continue
            if isinstance(out, Refuse):
                reason = _line(out.reason)
                event("hook", phase="before_tool", name=name, mode=mode, effect="refuse", reason=reason, tool=call["name"])
                if mode == "enforce":
                    return _refused(name, reason)
        return None

    def run_after(self, call, result, event):
        """The result the model gets: the tool's own, or what an enforcing hook replaced it with."""
        for name, mode, fn in self.after:
            try:
                out = fn(call, result)
            except Exception as e:  # noqa: BLE001 — a hook that cannot decide withholds
                reason = f"hook raised {type(e).__name__}"
                event("hook", phase="after_tool", name=name, mode=mode, effect="error", reason=reason, tool=call["name"])
                if mode == "enforce":
                    return f"<withheld by hook {name}: {reason}>"
                continue
            if isinstance(out, Replace):
                original = _text(result)
                event("hook", phase="after_tool", name=name, mode=mode, effect="replace", reason=_line(out.reason),
                      tool=call["name"], original_chars=len(original),
                      original_sha256=hashlib.sha256(original.encode()).hexdigest())
                if mode == "enforce":
                    result = out.text
        return result


def _line(reason):
    return " ".join(str(reason).split())[:REASON_MAX]


def _refused(name, reason):
    return f"<refused by hook {name}: {reason}>"


def build(declared, extra=None):
    """Hooks from the spec's `harness.hooks` (FLOTILLA_HARNESS) with the shared registry plus the fleet's `extra`.
    HookError for an unknown name, a hook declared in a phase it does not run in, a bad mode or a duplicate."""
    declared = declared or {}
    registry = dict(SHARED, **(extra or {}))
    unknown = set(declared) - {"before_tool", "after_tool"}
    if unknown:
        raise HookError(f"harness.hooks: {sorted(unknown)} is not a phase (before_tool, after_tool)")
    lists = {}
    for phase in ("before_tool", "after_tool"):
        seen, out = set(), []
        for ref in declared.get(phase) or []:
            name, mode = (ref or {}).get("name"), (ref or {}).get("mode") or "enforce"
            if not isinstance(name, str) or not NAME_RE.match(name):
                raise HookError(f"harness.hooks.{phase}: hook name {name!r}")
            if name in seen:
                raise HookError(f"harness.hooks.{phase}: hook {name} is declared twice")
            seen.add(name)
            if mode not in MODES:
                raise HookError(f"harness.hooks.{phase}.{name}: mode {mode!r} is not enforce or observe")
            if name not in registry:
                raise HookError(f"harness.hooks.{phase}: hook {name} is not registered")
            if registry[name][0] != phase:
                raise HookError(f"harness.hooks.{phase}: hook {name} runs {registry[name][0]}, not {phase}")
            out.append((name, mode, registry[name][1]))
        lists[phase] = out
    return Hooks(lists["before_tool"], lists["after_tool"])


def step_detail(f):
    """The text of an event's calls-file step row (the line's `detail`)."""
    what = {"refuse": "refused", "replace": "replaced", "error": "failed"}[f["effect"]]
    head = f"{f['phase']} {f['name']} ({f['mode']}): " + (f"would have {what}" if f["mode"] == "observe" else what)
    if f["effect"] == "replace":
        head += f" the result of {f['tool']} ({f['original_chars']} chars, sha256 {f['original_sha256'][:12]})"
    else:
        head += f" the call to {f['tool']}"
    return f"{head}: {f['reason']}"


def span(f):
    """A harness.hook span when the OpenTelemetry SDK is present; nothing otherwise."""
    try:
        from opentelemetry import trace
    except ImportError:
        return
    attrs = {"flotilla.hook.phase": f["phase"], "flotilla.hook.name": f["name"], "flotilla.hook.mode": f["mode"],
             "flotilla.hook.effect": f["effect"], "flotilla.hook.tool": f["tool"]}
    if "original_sha256" in f:
        attrs["flotilla.hook.original_chars"], attrs["flotilla.hook.original_sha256"] = f["original_chars"], f["original_sha256"]
    with trace.get_tracer("flotilla.harness").start_as_current_span("harness.hook", attributes=attrs):
        pass
