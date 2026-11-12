# ADR-0013 — Fleet packs

**Status:** Accepted

**Date:** 2026-11-12

**Decided by:** the operator, on [RFC-0006](../rfc/RFC-0006-fleet-packs.md) (revision 9, after six
passes by independent reviewers; the last found no blocker)

## Context

A fleet's behaviour lives more and more in text: routine specs, playbooks, skills, harness settings.
The operator wanted three things from that: people sharing flows by copying files, an installation
trying a change without effect before the acting one takes it (development, test, production, or
any other tiers), and operations people knowing exactly what runs and trusting it before AI acts.
Separately, the operator decided that evaluation in any form lives outside flotilla, and flotilla
provides its input.

## Decision

As RFC-0006 describes:

1. **A fleet pack is a folder of text** (`FLEET.md`, `routines/`, `playbooks/`, `skills/`,
   `defaults/`) holding a fleet or a single routine. It never brings code: it names tools, gates,
   actions and step types from the installation's registry and cannot add, widen or raise anything.
2. **Installation facts stay out:** a pack declares slots (by name and kind) that the installation
   fills in its config, with overrides of the pack's defaults shown with a warning. The model alias
   travels with the pack; what an alias maps to stays in the installation.
3. **A pack is any folder; nothing assumes git.** Its identity is its content hash (the pack hash);
   what an installation approves is a set (the pack hash with its routines, slots and overrides: the
   set hash). Runs read the approved snapshot, never the folder; built-in packs are the images'.
4. **Trust is what the operator saw and approved:** a change is copied into a pending snapshot,
   shown, approved by its set hash, recorded as an audit event, and the changed routines start at
   `shadow`. Rollback and prune are commands with the same record.
5. **Installations and promotion:** an installation can be capped at `shadow` and at a daily budget
   (non-impact mode), carries an `installation` name beside its `environment` label, and promotes a
   pack by moving the same files with a promotion record. flotilla does not compare installations.
6. **Evaluation lives outside flotilla.** A development test (pass/fail on a code change) stays; an
   evaluation (quality, labels, variants, scores) belongs to the evaluation harness and each fleet's
   evaluation project. flotilla provides a versioned replay entry point and an input contract
   (complete, versioned, exactly labelled, never rewritten records, and an export).
7. **The order of work** is RFC-0006 §10: the built-in fleets to the pack layout with hashing and
   recording; installation packs with approval; ADR-0011's registry, then the `requires` check;
   the caps; the export and the promotion record; the ops fleet as the first pack. The replay entry
   point goes first, as an RFC-0002 amendment.

## Consequences

**Positive:** a flow can be shared, tried without effect and promoted unchanged; every run says
exactly which files and values decided it; trust needs no list of sources; evaluation has one home
and a stable input.

**Negative:** more state in flotillad (snapshots, sets, audit events), a registry to keep equal to
the workers' code, the built-in fleets' layout move, and a compose file to parametrise for a second
installation on one host.

**Neutral, amendments:**

- [ADR-0012](ADR-0012-skills.md) decision 2 and [RFC-0005](../rfc/RFC-0005-skills.md) §3, §6 and
  decision 2: an installation pack is a skill layer of its own; its hashes and the operator's
  recorded approval take the place of "pinned by commit".
- [ADR-0008](ADR-0008-autonomy.md) and [ADR-0005](ADR-0005-agent-pipeline.md) decision 4: the
  installation's cap is a fourth cap in "lowest wins".
- [ADR-0004](ADR-0004-job-runtime.md) and [RFC-0002](../rfc/RFC-0002-job-runtime.md) §8: amended on
  2026-11-12 already (where evaluation lives); RFC-0002 §4's "a push" among outputs is a git push.
- What describes today's system (the workers' listener serving the ledger only, in
  `deploy/compose/README.md` and the comments in `internal/api`) stays true until the snapshot API
  is built, and changes with §10 step 3.

## Alternatives considered

RFC-0006 §14: export and import with an archive format; packs that carry code; git sources only;
a fixed dev/test/acceptance/prod model; evaluation inside flotilla.
