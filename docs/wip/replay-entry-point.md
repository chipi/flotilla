# The replay entry point (agreed, not built)

**Status:** agreed with the triage fleet's evaluation project (2026-11-12), not built
([ADR-0013](../adr/ADR-0013-fleet-packs.md), [RFC-0006](../rfc/RFC-0006-fleet-packs.md) §8). Until it
is built, the seams agreed in RFC-0002 §9 steps 2 to 5 stay frozen.
→ becomes part of RFC-0002 §8's amendment (the entry point's contract) once built.

## Why

An evaluation project runs flotilla's own code over recorded history. Today it does that by
replacing module attributes, a different set for each decision:

| Decision | Seams replaced today |
| --- | --- |
| filing (`filing.file_or_update`, and `filing.file_escalation` for an escalate decision) | the ledger path, `_gh`, `_now`, `_today`, `_glitchtip_note`, `_grafana_annotation`, and with recorded keys `normalized_key`, `group_key_for`, `alert_key`, `repo_for` |
| the stale sweep (`stale.run_pass`) | `stale._occurrences`, `sources.firing_alerts`, `stale.is_stale`'s quiet days, `SF_STALE_NUDGE` (it gates the whole pass) |
| the triage step (`triage.investigate`) | `triage.post_json` (with an arm's body extras: `reasoning_effort`, `max_tokens`, `allowed_openai_params`; its timeout; metadata tags that mark eval traffic in Langfuse; a reply given as a tool call), `triage._call` (a stub model: a fixed decision, offline), `triage._gate`, `observ.new_trace_id`, `SF_OBSERV_DISABLED` |
| the gates alone (`triage._gate` over decisions rebuilt from recorded run outputs, no model call) | `triage._gate` |

Every rename or new seam on flotilla's side breaks a reader, so every change has needed an
agreement. The entry point replaces the patching with one versioned module that flotilla keeps
working.

## The interface (version 1)

One module in the triage fleet's code, `signal-fleet/mvp/replay_api.py`, with `API_VERSION = 1`.
An evaluation project imports it from a flotilla checkout at the commit it replays. One decision per
call; a stepper over a sequence (the evaluation project's `Replayer`) lives on the caller's side,
carrying the world between calls.

```python
import replay_api as ra

world = ra.World(
    now="2026-10-30T15:33:02Z",          # the decision's stamp: what _now and _today return
    ledger=[{...filed row...}, ...],      # the filed ledger as it stood (FILED_COLS; missing columns empty)
    github=gh,                            # gh(method, path, payload) -> JSON, for every call, writes included
    keys=None,                            # optional {"norm_key", "group_key", "alert_key", "repo"} as recorded
)

out = ra.file(world, signal, issue, kind)        # a file decision (file_or_update)
out = ra.escalate(world, signal, decision)       # an escalate decision (file_escalation)
out = ra.sweep(world, occurrences, firing, quiet_days=None)    # one stale sweep
out = ra.gates(world, signal, decision, trace=..., gates=...)  # the gates alone, no model call
out = ra.triage(world, signal, model=..., tools=..., gates=...) # one triage step (below)
```

**`github` is the caller's.** Every GitHub call goes through it, writes included, and its answer is
what the fleet's code reads back (a create answers with the number the caller chooses, such as the
number production gave that issue). The entry point also records every write in `writes`. The
caller decides what time its reads stand at: filing reads GitHub as it stood just before the
decision began, which is not the decision's stamp, so the entry point never assumes the reader's
time is `now`.

**Keys:** with `keys`, `ra.file` and `ra.escalate` use the recorded keys and repository instead of
computing them (recorded data is scrubbed, and scrubbing changes keys and routing labels).
`ra.sweep` always uses the code's own key functions, since a sweep derives keys for every issue.

**The sweep** forces `SF_STALE_NUDGE` on inside the call and takes `quiet_days` as given.

Each call returns the same shape:

```python
{
  "api": 1,
  "result": "COMMENTED: recurrence on o/r#5 (via norm_key)",   # the decision's own result line
  "writes": [{"method": "POST", "path": "...", "payload": {...}}, ...],  # every GitHub write, as sent to gh
  "ledger": [...],          # the ledger after the decision (rows written are in the record too)
  "record": {...},          # the decision's record, as callrec writes it (matched, keys, link_own, written, ...)
}
```

`ra.triage` and `ra.gates` add `"decision"` (after the gates) and `"model_decision"` (the model's
own, before them; `ra.gates` returns the one it was given).

**The gates alone:** `ra.gates` runs the registered gates in the routine's configured order over
the model's decision and the probe `trace` it was made on, with no model call; its `result` is the
gates string and its record the lookups the gates made. With recorded `gates` (`{"lookups": [...]}`),
a lookup the record does not hold raises `ra.LookupMissing` instead of being answered live (the
edge gate would otherwise turn the miss into a "lookup failed" escalation).

**The triage step's seams**, each recorded or live per call (RFC-0006 §8's table):

| Argument | Recorded | Live |
| --- | --- | --- |
| `model` | `{"replies": [...]}`: the recorded replies, in order; or `{"stub": decision}`, a fixed decision | `{"alias": "triage-pro", "k": 3, "body": {...}, "timeout": 120, "reply_in_tool_call": false}` |
| `tools` | `{"results": {...}}`: the recorded tool results | `"live"`: the installation's, read-only (freezing) |
| `gates` | `{"lookups": {...}}`: the recorded gate lookups | `"live"` |

A live model call carries metadata tags marking it as replay traffic (`flotilla_replay: true` and the
caller's tags), so Langfuse tells it from production. Observability is off inside every call
(`SF_OBSERV_DISABLED`, no trace ids). The clock is the world's `now`. Whatever is live is recorded in
the returned record, so a model replay's output is itself a record under the input contract.

## Rules

- **Nothing is sent by flotilla.** GitHub calls go only to the caller's `gh`; GlitchTip notes and
  Grafana annotations do nothing; the ledger lives in a temporary file owned by the world; no
  flotillad store is touched (`SF_LEDGER_BACKEND` is `tsv` inside the call, whatever the caller's
  environment says).
- **One world at a time in a process.** The implementation still swaps module attributes inside
  the call and restores them on return, including on an exception; callers that want parallel
  replays use processes.
- **Faithful by construction:** the code under replay is the module's own fleet code at that commit,
  unchanged. flotilla's gates run the entry point over each replay corpus, so a change that breaks
  it fails in flotilla's CI, not in the evaluation project.
- **Versioning:** `API_VERSION` rises only for a breaking change (a removed argument or field, a
  changed meaning); additive fields keep the version. A commit from before the entry point has no
  `replay_api.py`; for those the evaluation project keeps its adapters (the weekly faithfulness
  check replays a week with the code that ran).
- **No evaluation inside:** no labels, scores, splits or comparisons; the evaluation project
  compares outputs.

## Agreed with the evaluation project (2026-11-12)

1. One decision per call; the stepper stays on the caller's side.
2. One `gh` function for every call, writes included; its answer is what the code reads back; the
   entry point records writes as well. The reader's time is the caller's.
3. Recorded keys stay, optional, for filing and escalation; the sweep uses the code's key functions.
4. The table's missing seams were added: `file_escalation` (`ra.escalate`), the sweep's
   `SF_STALE_NUDGE`, the model call's options and stub, the model's own decision in the result,
   observability off, and the gates alone (`ra.gates`).

## Order

1. `ra.file`, `ra.escalate` and `ra.sweep`, with a flotilla gate per corpus, proven equal to the
   evaluation project's adapter on the same corpus: the 2026-10-02 window (253 decisions, 34
   sweeps; the adapter reproduces it faithfully, 324 of 324), with the expected per-step outputs
   from the evaluation project.
2. `ra.gates`, then `ra.triage` with recorded seams (exact replay) and a stub model, then live
   seams (model replay, freezing; refused with `NotImplementedError` until then).
3. The scorer moves out (ADR-0013), using `ra.triage`.
