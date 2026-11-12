# ADR-0012 — Skills in the harness

**Status:** Accepted

**Date:** 2026-11-11

**Decided by:** the operator, on [RFC-0005](../rfc/RFC-0005-skills.md) (revision 3, after two
passes by an independent reviewer)

## Context

Skills (a `SKILL.md` folder of know-how, the format Claude Code and opencode read) let an agent pull
in instructions only when needed. flotilla's harness had one fixed text per routine, the playbook,
and the triage fleet's class hints, fragments its code picks by the signal's class. The operator
asked for skills in flotilla's own harness from the start, rather than switching to a harness that
has them.

## Decision

As RFC-0005 describes:

1. **The format** is the shared `SKILL.md`; flotilla requires `name` and `description` and ignores
   every other field, which grants nothing.
2. **Sources:** trusted, versioned git only, in three layers pinned per deploy: flotilla's default
   skills and each fleet's skills inside the images, the installation's skills repo pinned by commit
   in config. Names are unique across layers. (Amended by [ADR-0013](ADR-0013-fleet-packs.md):
   an installation's fleet pack is a skill layer of its own, its hash and the operator's recorded
   approval in place of "pinned by commit".)
3. **Loading:** by rule (code matches the case's structured fields, in spec order) or on demand (the
   model calls `load_skill` from an index of names and descriptions), each listed in the routine's
   spec; the default is none. On-demand loads count against `max_skill_loads`, not the step or tool
   limits.
4. **Skills never grant anything:** scripts are never run or read; a script becomes a tool or an
   action only through a reviewed registry change; a routine is offered only its `harness.tools`.
5. **Check, context, recording:** flotillad's `-check` validates every skill before a deploy; a
   skill is never trimmed; rule skills render in the fleet's `case` template (for triage, exactly
   where the hint is today); skills enter as instructions, outside the data delimiters, and do not
   taint; every load is recorded with layer, commit and hash, as a `skill.load` span.
6. **v1 in two parts:** with RFC-0002 §9 step 3, triage's four class hints become rule-loaded skills,
   replaying byte-identical; with step 4, on-demand loading. The installation layer's deploy mechanics,
   reading `references/`, and tool declarations in a skill come with the first skill that needs them.

Decided by the operator: playbooks stay the routine's own text; skills are pinned per deploy; agents
do not write skills in v1.

## Consequences

**Positive:** reusable know-how across routines and fleets, a smaller context, the same skills
readable in Claude Code and opencode, and no change to policy, taint, recording or budgets.

**Negative:** another layer in the context and the spec to keep correct; the worker image gains the
`fleets/` tree.

**Neutral:** amends [ADR-0004](ADR-0004-job-runtime.md): its context `fragments` and RFC-0002 §9 step
3's `hints/*.md` become `skills.rule` and `fleets/triage/skills/hint-*/SKILL.md`.

## Alternatives considered

RFC-0005 §12: switching harness, everything in the playbook, MCP prompts, letting skills run their
scripts, counting skill loads against the existing limits.
