# ADR-0010 — Memory and knowledge, not "recall"

**Status:** Accepted (operator, 2026-11-11)
**Date:** 2026-11-11

## Context

[ADR-0004](ADR-0004-job-runtime.md) named two of the runtime's core concepts **memory** (what flotilla
recorded) and **external knowledge** (what exists elsewhere), the operator's words.
[ADR-0005](ADR-0005-agent-pipeline.md) (RFC-0004 decision 30) merged them under one name, **recall**,
because one lookup client serves both. The word is unusual, and it hides the difference a reader most
needs: flotilla's own record is exact and trusted; plugged-in sources are searched and trusted or not
per path. Nothing of it is built.

## Decision

Two names, no umbrella:

- **memory:** what flotilla recorded itself: cases, outcomes, the operator's feedback, the notes the
  operator activated. Looked up exactly, by key; structured fields only.
- **knowledge:** what is plugged in from elsewhere: repos, docs, runbooks, MCP servers, each with its
  trust set per path. Searched by path maps, full text or meaning; each piece carries its source,
  version and trust.

The design keeps one **lookup client** for both, and the context layer that holds what was looked up.
Where one phrase is needed for both: "what the agent may look up".

Renamed in the living docs and the UI (the page "Recall" becomes **Knowledge**, with Memory and
Sources tabs). Accepted RFCs and ADRs keep "recall" as records; this ADR is the mapping. The worker
role `knowledge-refresh` keeps its name.

## Consequences

**Positive:** the two words say where a fact came from and how far to trust it.

**Neutral:** no code or deployment changes; nothing of it was built.

## Alternatives considered

- **Keep "recall":** one word, but an unusual one that hides the trust difference.
- **"knowledge" for both:** flotilla's own record of a case is memory, not knowledge.
