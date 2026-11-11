# ADR-0011 — Tools and actions, not "capabilities"

**Status:** Accepted (operator, 2026-11-11)
**Date:** 2026-11-11

## Context

"Capability" meant two things. In built code (ADR-0004), a model alias's **capabilities** are what its
endpoint can do: tool calls, JSON output, how cost is reported. In the design
([ADR-0005](ADR-0005-agent-pipeline.md)), the **capability registry** held every named operation
flotilla may perform, split into read capabilities (an agent may call them) and write capabilities
(an agent may only plan them). "Write capability" was long, it was the main noun on the Autonomy
page, and since [ADR-0009](ADR-0009-host-agent-names.md) the host agent's role that runs writes is
`actions`. Nothing of the registry is built.

## Decision

| Before | After |
| --- | --- |
| read capability | **tool**: what an agent may call during its step (the word models use) |
| write capability | **action**: what an agent may only plan; policy decides it, the host agent's `actions` role runs it |
| capability registry | the **registry**: one list of tools and actions, each named, typed and scoped |
| the capability's autonomy ([ADR-0008](ADR-0008-autonomy.md)) | **the action's autonomy**, earned per action |
| a model alias's capabilities | **unchanged**: the only meaning "capability" keeps |

In one line: an agent calls tools and plans actions; policy decides each action; a host agent runs it.

Accepted RFCs and ADRs keep "capability" as records; this ADR is the mapping. Linux capabilities (the
containers' hardening) are another thing and keep their name.

## Consequences

**Positive:** with ADR-0007 to ADR-0010 the words fit together: fleets of routines; monitors open
cases; agents call tools and plan actions; autonomy goes shadow, propose, auto; memory and knowledge;
workers think, host agents act.

**Neutral:** no code or deployment changes; the model aliases' `capabilities` stay as built.

## Alternatives considered

- **Keep "capability" for the registry:** two meanings in one codebase, one of them built.
- **"operation" for both:** accurate, but it hides the read and write split the whole safety design
  rests on.
