# 0004 — v2 step 2: shadows with no effect (2026-11-19)

**Date:** 2026-11-19
**Operator:** Marko Dragoljevic
**Agent:** Claude Code (Opus 5.5 planned; Sonnet 5.5 built), the flotilla session
**Outcome:** A routine version that is not live now runs beside the live one on its own worker,
from the live run's frozen input, and changes nothing. What it decides is paired with what the live
version decided, per subject, and shown to the operator. Built in order C1 to C12 on `main`, not
pushed. Design: [v2 design](../wip/v2-design.md) section 4, decided in
[ADR-0020](../adr/ADR-0020-routine-versions-stations-harnesses.md) section 8.

## What was built

| Step | What |
| --- | --- |
| C1 | schema 30 and 31: `runs.shadow` and `input_key`, `run_inputs`, `shadow_runs`, `shadow_records`; every reader of live rows leaves shadow rows out (a guard test over the SQL); the ledger as a shadow sees it; real pair counts |
| C2 | a per-run token for the workers' listener: the namespace and run id a request names are ignored |
| C3 | every signal a shadow emits is marked and separable (`shadow=1`, `shadow=true`, `flotilla.shadow`, a GlitchTip tag and fingerprint), and a shadow emits no freshness or last success |
| C4, C5 | the worker's read and write env files, shadow mode, state snapshots; the frozen input, its schema and its export |
| C6 | the shadow path never calls a live write: a plan is recorded as "would run X (blocked: shadow)" |
| C7 | the shadow runtime in the scheduler (queue, supersede, skip rules, retire, STOP); a shadow worker's token is no caller; workers may not share a token |
| C8 | pairs: store, API, CLI, export, the routine page's list and the side-by-side page |
| C9 | compose: the shadow worker with its own proxy, secrets, state and limits |
| C10 | the e2e: a hostile shadow, signal marks, wall time, mutation checks |
| C11 | docs |
| C12 | remote workers' routines cannot be shadowed yet: refused at start when shadows are on |
| later | the operator's shadow-run on a given input (`flotilla versions shadow-run`), an exact before and after |

## Decisions worth knowing

- **A shadow runs on a worker declared `shadow: true`,** never on the live one, with the read
  environment only. Its token is flotillad's to call it with and no caller of flotillad's workers'
  listener, so a shadow that reads the token file it is given can do nothing with it.
- **Levels stay truthful.** A shadow's failure is logged at ERROR like any; it is separated by its
  marks, not softened.
- **Shadows are off by default** (`shadows.enabled`), and a routine on a remote worker is refused
  rather than skipped silently.
- **The wall-time tolerance was measured before it was chosen:** the live run's median with a
  shadow beside it against without, three runs each, spread about 0.06 s; the bound is 1.5 times.

## Found by the e2e on the way

- The run-token required-mode e2e I wrote for the ledger sync fix never ran: its second flotillad
  exits at once when no timed routine is enabled.
- A pair page scrolled sideways at 360 px for a long subject.
- The compose README's new sections were over the lint length; `make docs-validate` is green.

## Open

- Remote shadows (the link has no snapshot or commit read).
- Per-station texts of a pair (step 3).
- The Slack and brief marks rest on code and unit tests, not an e2e connector stand-in.
- The cause of one api `-race` flake the advisor saw was never found; `-count=5` passes.
- The real-store copy check is the coordinator's.
