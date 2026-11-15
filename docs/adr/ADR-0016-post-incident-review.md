# ADR-0016 — Post-incident review

**Status:** Accepted

**Date:** 2026-11-15

**Decided by:** the operator, on [RFC-0009](../rfc/RFC-0009-post-incident-review.md), its four
questions as proposed

## Context

The first shadow case was the chain right within a wrong check (a 20 h threshold on a ~24 h backup),
and nothing in flotilla said so. After a case closes, a review should say what to learn.

## Decision

1. **`ops/case-review`**, a code-only routine on `ops-read`, runs when a case closes and daily over
   7 days, and only proposes (Q3).
2. **Code rules first**, each naming its evidence: below the cycle, self-clearing, a fix that does not
   hold, a wrong diagnosis, a refused plan (RFC-0009 §2). A model-written review later, as untrusted
   input.
3. **Monitors report a `measure`** (name, value, threshold) and, when they know it, the source's normal
   cycle, computed from the source (Q4); the backup monitor's is the median interval between successful
   backups over 14 days.
4. **Two outputs:** a note in Knowledge (a person activates it), or a `process_change` case, always
   at propose: a diff to the repo with its evidence.
5. **Applying:** until step 4 brings `github.open_pr`, an approved `process_change` shows its diff and
   the operator applies it by hand, the case recording the commit (Q1). Installation config outside
   the repo gets a note naming the key and the value, not a `process_change` (Q2).
6. **The record:** `process_change` plans count on Autonomy; a rejected one keeps its reason, and the
   same rule on the same kind and target proposes nothing for 30 days unless its evidence changes.
7. **A process change is a real plan** (operator, 2026-11-15, while building: "full plan now").
   - The registry gains `executor: repo` actions: no host preconditions or check, a `text` param kind
     (bounded, bound by the plan's hash), and never above propose (`Action.MaxAutonomy`, and policy
     caps it).
   - `repo.process_change` is the first. `ops/process-change` re-checks a proposal against the repo
     files of its image and plans it with the change as a diff, which Approvals and the case page
     show.
   - flotillad sends nothing to a host for a `repo:` target. Once approved, the operator applies the
     change and records its commit on the case (`RecordApplied`), which closes it as verified, the one
     close a person may make from `executing`.
   - `ops/case-review` reads cases on flotillad's worker listener with the ops-read worker's own token
     (`case-read`, `note-propose`); monitors put their measure on the case's timeline as structured
     data (schema step 20).
   - `repo.process_change` starts at shadow like every action: promote it to propose on Autonomy to
     have its plans wait for you.

## Consequences

- **Positive:** every closed case is looked at; a wrong check is found by flotilla, not only by a
  person reading.
- **Negative:** a new case kind and a routine to keep; proposals a person must answer.
- **Neutral:** the review writes nothing but proposals; every change still goes through a person and,
  for the repo, review and CI.

## Alternatives considered

In RFC-0009: notes only; a model review from the start; monitors that tune themselves.
