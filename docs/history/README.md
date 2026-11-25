# History — session continuity log

Append-only log of working sessions on this project, plus (optionally) a
parallel decision log.

## Files

- `0000-origin-evaluation.md` — the 2026-11-08 evaluation (problem, constraints, options) that
  led to flotilla, written in the installation's ops repo and kept here.
- `0001-genesis.md` — the founding session that produced v0.1. The most
  important doc here; future sessions should read it first.
- `0002-design-to-running-platform.md` — 2026-11-08 to 11-17: the ten days that built flotilla, by
  arc (phases 1 to 3, the pipeline, host agents, the UI, MCP, bugfix and SEO, the MVP and the private
  fleets).
- `0003-wip-notes-closed.md` — six finished wip plans closed on 2026-11-17, and where their lasting
  parts went.
- `0004-v2-step-2-shadows.md` — 2026-11-19: v2 step 2, shadows with no effect: what was built,
  decided and found on the way.
- `0005-v2-steps-3-4.md` — 2026-11-19: v2 steps 3 and 4, stations and the harness layer in
  `runtime/`: what was built, decided and found on the way.
- `0006-v2-step-5.md` — 2026-11-19: v2 step 5, the example fleets in the v2 format, the examples
  catalogue, `run_tokens: required`, and the no-effect matrix as a required check.
- `0007-folder-move-rerecord.md` — 2026-11-20: a live flat routine moves to the folder layout in one
  deploy, re-recorded at the start when every recorded key and file is equal.
- `0008-rfc-0014-hook-and-plumbing-call.md` — 2026-11-22: RFC-0014 written and decided, its step 1
  (the hook, live) and step 2 (`plumbing.call`, its example fleet and worker).
- `0009-rfc-0014-case3-to-live-reads.md` — 2026-11-22 to 25: case 3 live, the directory reader, live
  reads, the UI pass over real data, the mini VM's clock fixed and the read pool reverted, and four
  GlitchTip noise fixes.
- `decisions.md` — *(optional, not started)* append-only decision log (D-NNNN
  entries). Newer decisions at the top. Use this if you make operational
  decisions worth grep-finding later (port picks, image bumps, tooling
  swaps) that don't rise to ADR-level.

## Convention

- One file per session named `NNNN-<short-slug>.md`.
- If keeping a decision log: sessions append a section for each
  non-trivial decision made.
- Decisions get IDs (`D-NNNN`) so they can be referenced from code, ADRs,
  or other sessions.
- Never edit a past session doc — write a new one referencing it.
- Don't conflate this with `docs/adr/`. ADRs are *architectural* decisions
  that govern the project's design. The decision log here captures
  *operational* decisions (port choices, image picks, defer-vs-do calls).
  When a decision is structural enough to need an ADR, write the ADR and
  link to it from the decision-log entry.

## When to write a new session entry

If your session:

- Adds a new pillar or restructures the project
- Makes 3+ decisions worth logging
- Changes scope or direction
- Resolves an open thread from a prior session

then write a new `NNNN-<slug>.md` summarizing the session.

If your session is just "small fix" / "typo" / "rename one file" — no
history entry needed, the commit message is enough.
