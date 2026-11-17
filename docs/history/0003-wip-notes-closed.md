# 0003 — Six finished wip plans closed (2026-11-17)

**Date:** 2026-11-17
**Operator:** Marko Dragoljevic
**Agent:** Claude Code (Opus 5.5), the flotilla session
**Outcome:** `docs/wip/` keeps only the plans still open. Six plans whose work had shipped were
closed: what they were and how they ended is below; what someone needs again is in the living docs
named in the last column; the open items left in them moved to the
[vision plan](../wip/vision-plan.md) ("Also open"). Their full text stays in git history
(`git log --all -- docs/wip/<name>.md`). Found by step 8's docs audit (MVP: docs complete and
current).

| Note | What it was | How it ended | Lasting parts now in |
| --- | --- | --- | --- |
| `phase-2-plan.md` | flotillad runs the triage fleet: the images, compose, the state mirror, shadow comparison, the switch checklist | the switch, 2026-11-11 (092a73f): triage at propose on flotilla | [deploy/compose/README.md](https://github.com/chipi/flotilla/blob/main/deploy/compose/README.md), [history 0002](0002-design-to-running-platform.md) |
| `phase-3-plan.md` | the runtime and triage onto it, RFC-0002 §9 steps 1 to 6 | steps 1 to 5 built, proved by replays, live since the promotion; step 6 (the routine as declared steps) not started | [ADR-0004](../adr/ADR-0004-job-runtime.md), [RFC-0002](../rfc/RFC-0002-job-runtime.md) §9; its open items in the vision plan |
| `promotion-plan.md` | the live triage fleet onto main and the API ledger | done 2026-11-14 11:12Z | [history 0002](0002-design-to-running-platform.md); ending `triage/cycle-shadow` stays the operator's (vision plan) |
| `host-agent-plan.md` | `flotilla-host` v1, H1 to H10 (ADR-0006 §12) | built; live on the mini 2026-11-14 16:13Z | [ADR-0006](../adr/ADR-0006-node-agent.md), [Host keys](../operations/host-keys.md), [security](../security.md); the key procedures not yet run on a real host (vision plan) |
| `replay-entry-point.md` | the versioned module an evaluation project calls instead of patching internals (ADR-0013) | built: `signal-fleet/mvp/replay_api.py` with `test_replay_api.py` | the module's docstring, [ADR-0013](../adr/ADR-0013-fleet-packs.md), [RFC-0002](../rfc/RFC-0002-job-runtime.md) §8 |
| `bugfix-phase-a.md` | the build order A1 to A7 for the bugfix fleet's form and flow | done 2026-11-15: the routines in shadow, the workers remote | [RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md), [Bugfix operations](../operations/bugfix.md) |

Still open in `docs/wip/`: the vision plan (the MVP), pipeline v1 (its gate), the UI plans (mockup
parity, the next UI work) and the bugfix phase B brief.
