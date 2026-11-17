# History — session continuity log

Append-only log of working sessions on this project, plus (optionally) a
parallel decision log.

## Files

- `0000-origin-evaluation.md` — the 2026-11-08 evaluation (problem, constraints, options) that
  led to flotilla, written in agentic-ai-homelab and kept here.
- `0001-genesis.md` — the founding session that produced v0.1. The most
  important doc here; future sessions should read it first.
- `0002-design-to-running-platform.md` — 2026-11-08 to 11-17: the ten days that built flotilla, by
  arc (phases 1 to 3, the pipeline, host agents, the UI, MCP, bugfix and SEO, the MVP and the private
  fleets).
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
