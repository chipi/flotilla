# ADR-0008 — One autonomy scale: shadow, propose, auto

**Status:** Accepted (operator, 2026-11-11)
**Date:** 2026-11-11

## Context

Three settings limit how much happens without the operator, and they used two ladders and three
names:

| Where | Name | Values |
| --- | --- | --- |
| a routine (built) | stage | `shadow`, `propose`, `live` |
| a write capability ([ADR-0005](ADR-0005-agent-pipeline.md), design) | autonomy | `shadow`, `propose`, `auto` |
| a host ([ADR-0006](ADR-0006-node-agent.md), design) | ceiling | `shadow`, `propose`, `auto` |

`shadow` and `propose` meant the same in both ladders, but the top step differed, and `live` reads
as "acts on its own", which is what `auto` means. The UI mockup needed a sentence to explain
"stage: live" next to "capability: propose".

## Decision

**Autonomy is one scale, and it is a lifecycle:** first a thing runs in `shadow` (its plans are
recorded, nothing is done), then it may `propose` (it asks the operator), then it may act on its own
(`auto`). Each step up is earned on evidence and promoted only by the operator.

Three places cap it, each by its own name:

| Where | Name | Set by |
| --- | --- | --- |
| a routine | **the routine's autonomy** (was "stage"; `max_stage` becomes `max_autonomy`) | the spec's default and cap; the operator |
| a write capability | **the capability's autonomy** | earned, promoted by the operator |
| a host | **the host's autonomy cap** (was "ceiling") | the host's reviewed config |

What happens is the lowest of the three; taint and the freeze can only lower it further.

Amended by [ADR-0013](ADR-0013-fleet-packs.md) (2026-11-12): an installation's config can cap
every routine (`max_autonomy`, non-impact mode), a fourth cap in the same "lowest wins".

Amended by [ADR-0014](ADR-0014-operator-ui.md) (2026-11-13): the freeze lowers every routine to
`shadow`, so nothing outward happens while it holds; it never overwrites a routine's own control.

Amended by [ADR-0017](ADR-0017-routine-actions.md) (2026-11-19): the host's autonomy cap applies to
actions the host agent runs only. A routine action is capped by the installation's
`routine_actions.max_autonomy` (default `propose`; `auto` lets it earn auto), a repo change never goes
above `propose`. Per-host caps are still one cap, propose, for every host.

Amended by [ADR-0020](ADR-0020-routine-versions-stations-harnesses.md) (2026-11-18): the lowest level
is now named `record` (it records the plan and acts on nothing), because `shadow` became the state of
a routine version that runs beside the live one. A spec may still say `autonomy: shadow`, read as
`record`. `FLEETD_STAGE` is still `shadow` for `record` (below), pinned by a test.

Renamed where flotilla owns the word: the docs and UI; the spec (`autonomy`, `max_autonomy`); the
store (the runs column and the control key, by a schema step that also turns `live` into `auto`);
the API and CLI (`flotilla autonomy <routine> shadow|propose|auto`); the metric label `autonomy`.

**Not renamed:** `FLEETD_STAGE`, which the triage fleet's code reads (and records in every capture)
with the values `shadow`, `propose`, `live`. flotillad keeps passing it, mapping `auto` to `live`,
until that code is ported (AGENTS.md: the fleet code is frozen here). Accepted RFCs and ADRs keep
their wording as records; this ADR is the mapping.

## Consequences

**Positive:** one word and one scale for "how far may this go without asking", the same in the
routine, the capability, the host, the UI and the alerts; "the lowest of three" reads naturally.

**Negative:** a second coordinated rename in a week (code, stored controls, metrics, the host's
operator habits: `flotilla stage` becomes `flotilla autonomy`).

**Neutral:** the stored history keeps every run; old `stage` metric series age out.

## Alternatives considered

- **Keep two ladders, rename one** (e.g. the routine's to "mode"): still two scales to explain.
- **`ask` instead of `propose`:** plainer in the UI, but every ADR and the Slack approval flow say
  "propose", and it names what the routine does.
- **Keep "live" for routines:** the confusion this ADR exists to remove.
