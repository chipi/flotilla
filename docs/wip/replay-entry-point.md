# The replay entry point (proposal for agreement)

**Status:** draft for agreement with the triage fleet's evaluation project, before any code
([ADR-0013](../adr/ADR-0013-fleet-packs.md), [RFC-0006](../rfc/RFC-0006-fleet-packs.md) §8). Until it
is agreed and built, the seams agreed in RFC-0002 §9 steps 2 to 5 stay frozen.
→ becomes part of RFC-0002 §8's amendment (the entry point's contract) once agreed.

## Why

An evaluation project runs flotilla's own code over recorded history. Today it does that by
replacing module attributes, a different set for each decision:

| Decision | Seams replaced today |
| --- | --- |
| filing (`filing.file_or_update`) | the ledger path, `_gh`, `_now`, `_today`, `_glitchtip_note`, `_grafana_annotation`, and with recorded keys `normalized_key`, `group_key_for`, `alert_key`, `repo_for` |
| the stale sweep (`stale.run_pass`) | `stale._occurrences`, `sources.firing_alerts`, `stale.is_stale`'s quiet days, `SF_STALE_NUDGE` |
| the triage step (`triage.investigate`) | `triage.post_json`, `triage._call`, `triage._gate`, `observ.new_trace_id` |

Every rename or new seam on flotilla's side breaks a reader, so every change has needed an
agreement. The entry point replaces the patching with one versioned module that flotilla keeps
working.

## The interface (version 1)

One module in the triage fleet's code, `signal-fleet/mvp/replay_api.py`, with `API_VERSION = 1`.
An evaluation project imports it from a flotilla checkout at the commit it replays.

```python
import replay_api as ra

world = ra.World(
    now="2026-10-30T15:33:02Z",          # the decision's time: the clock every call reads
    ledger=[{...filed row...}, ...],      # the filed ledger as it stood (FILED_COLS; missing columns empty)
    github=reader,                        # reader(method, path, payload) -> the JSON GitHub answered then
    keys=None,                            # optional {"norm_key", "group_key", "alert_key", "repo"} as recorded
)

out = ra.file(world, signal, issue, kind)            # one filing decision
out = ra.sweep(world, occurrences, firing, quiet_days=None)   # one stale sweep
out = ra.triage(world, signal, model=..., tools=..., gates=...)  # one triage step (below)
```

Each call returns the same shape:

```python
{
  "api": 1,
  "result": "COMMENTED: recurrence on o/r#5 (via norm_key)",   # the decision's own result line
  "writes": [{"method": "POST", "path": "...", "payload": {...}}, ...],  # GitHub writes, never sent
  "ledger": [...],          # the ledger after the decision (rows written are in the record too)
  "record": {...},          # the decision's record, as callrec writes it (matched, keys, link_own, written, ...)
}
```

**The triage step's seams**, each recorded or live per call (RFC-0006 §8's table):

| Argument | Recorded | Live |
| --- | --- | --- |
| `model` | `{"replies": [...]}`: the recorded replies, in order | `{"alias": "triage-pro", "k": 3}` |
| `tools` | `{"results": {...}}`: the recorded tool results | `"live"`: the installation's, read-only (freezing) |
| `gates` | `{"lookups": {...}}`: the recorded gate lookups | `"live"` |

The clock is always the world's `now`. Whatever is live is recorded in the returned record, so a
model replay's output is itself a record under the input contract.

## Rules

- **Nothing is ever sent.** GitHub writes, GlitchTip notes and Grafana annotations are captured;
  the ledger lives in a temporary file owned by the world; no flotillad store is touched
  (`SF_LEDGER_BACKEND` is `tsv` inside the call, whatever the caller's environment says).
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

## Questions for the evaluation project

1. Is one decision per call right, or do you need a stepper over a sequence (your `Replayer`)? A
   stepper can live on your side over `ra.file`, since the world carries the ledger forward.
2. `github` as one reader function: enough, or do you need the timeline object itself passed in?
3. The recorded keys (`keys`): still needed once replays run on unscrubbed data only on the host?
4. Anything your adapters patch that is not in the table above?

## Order

1. Agreement on this page (the evaluation project's answers folded in).
2. `ra.file` and `ra.sweep`, with a flotilla gate per corpus, proven equal to the current
   adapters' results on the same corpus.
3. `ra.triage` with recorded seams (exact replay), then live seams (model replay, freezing).
4. The scorer moves out (ADR-0013), using `ra.triage`.
