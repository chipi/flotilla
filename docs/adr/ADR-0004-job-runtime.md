# ADR-0004 — The job runtime: five core concepts

**Status:** Accepted. *Amended by [ADR-0005](ADR-0005-agent-pipeline.md) (2026-11-09): recall
replaces memory and external knowledge (live reads allowed as recorded calls, with trusted and
untrusted sources and per-run taint, amending "read from a snapshot, never live"; semantic search);
one capability registry replaces tools and outputs; cases replace the ledger's decisions and
proposals; model-written queries are decided later.* *Amended by [ADR-0012](ADR-0012-skills.md)
(2026-11-11): the context `fragments` and RFC-0002 §9 step 3's `hints/*.md` become skills
(`skills.rule`, `fleets/triage/skills/hint-*/SKILL.md`).*
**Date:** 2026-11-08
**Decided by:** the operator, on [RFC-0002](../rfc/RFC-0002-job-runtime.md) (revision 3, after
three passes by an independent reviewer)

## Context

Phase 3 builds the job runtime and moves the triage fleet onto it. The operator wants the runtime's
core concepts established from the start, so that each fleet configures them its own way and they
grow as fleets are added: triage first, then SEO, ops and bugfix. The runtime must work with any
OpenAI-compatible endpoint, with LiteLLM as one deployment of it, not a requirement. RFC-0002 has
the design, with what triage does today read from the code.

## Decision

1. **One model client** for every call, to any OpenAI-compatible Chat Completions endpoint.
   - Specs name aliases.
   - Capabilities (tool protocol, JSON mode, cost source) are set per alias.
   - Cost precedence: reported, table, zero, unknown. An unknown cost is never counted as zero.
   - Typed errors (`AuthOrBilling`, `Transient`, `EmptyCompletion`) with triage's fail-closed and
     defer behaviour, as platform behaviour.
   - The deployment that answered is recorded.
2. **Five core concepts.** Each has a definition, rules that do not change, a spec section and
   extension points (RFC-0002 §4–§8):
   - **Harness:** one fixed loop for every fleet.
     - Tools are named and typed, and read-only is enforced by the credential.
     - No writes inside the harness.
     - Gates read only recorded evidence, with a frozen `now`.
     - Every request and reply is recorded.
     - Steps are `code`, `harness` or `external`, where `external` is a coding harness with its own
       contract.
   - **Context:** assembled in code from declared layers (playbook, case, memory, knowledge, tools),
     each with a budget.
     - Trimming is visible.
     - The exact text is stored with the run.
     - Memory and knowledge are delimited as data.
   - **Memory:** the fleets' own: decisions with `decision_keys`, feedback as events, notes.
     - No model-written prose reaches a model.
     - Agent feedback is proposed until the operator confirms it.
     - A note reaches a model only after the operator activates it, with a model replay.
   - **External knowledge:** reference material written by people or third parties.
     - Declared in the spec.
     - Read-only and versioned.
     - Read from a snapshot (a volume filled by the deploy), never live.
     - Separate from fleet data.
   - **Evaluation:** four modes (code replay, exact replay, model replay, shadow comparison), with
     a table of which change needs which.
3. **The order of phase 3:**
   - step 1, the recording exact replay needs, now in agentic-ai-homelab;
   - steps 2 to 6 after the phase 2 switch, each proven in its mode and live by a second shadow job
     on a candidate image.
4. **Non-goals:**
   - a memory shared by every agent;
   - semantic search, until a case needs it;
   - model-written queries;
   - streaming;
   - a third-party agent framework.

## Consequences

**Positive:**

- A new fleet configures the same five concepts instead of writing its own loop, client or memory.
- Every behaviour change has a named way to be proven.
- No path lets model- or agent-written text steer later decisions without the operator.
- Running without LiteLLM is possible.

**Negative:**

- More recording per run: messages, replies, lookups, time.
- A second shadow worker is needed after the switch.
- Temporary `quirks` settings exist until triage's like-for-like move is done.
- Notes need a model replay to activate, which is slower than switching them on.

**Neutral:**

- AGENTS.md rule 5 and ADR-0001 §9 now say "an OpenAI-compatible endpoint; LiteLLM is the reference
  deployment".
- The SEO fleet's proposal is RFC-0003.

## Alternatives considered

RFC-0002 §13:

- requiring LiteLLM;
- a framework's loop and memory;
- memory the model writes itself;
- replay as one mode;
- vector search from the start;
- reading knowledge live.
