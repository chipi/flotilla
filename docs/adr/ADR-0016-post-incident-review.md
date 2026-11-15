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

## Consequences

- **Positive:** every closed case is looked at; a wrong check is found by flotilla, not only by a
  person reading.
- **Negative:** a new case kind and a routine to keep; proposals a person must answer.
- **Neutral:** the review writes nothing but proposals; every change still goes through a person and,
  for the repo, review and CI.

## Alternatives considered

In RFC-0009: notes only; a model review from the start; monitors that tune themselves.
