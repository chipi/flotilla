# Positioning: flotilla in the industry's words

Decided with the operator on 2026-11-18, after comparing flotilla with an AI Engineer talk on standard
agents (the harness, the trust stack, specialist agents under an orchestrator, efficacy, standards).
This is the basis for the site's message and the README's opening.

## Vocabulary

- **On the site:** "agents". It is the word the industry uses, and the site leads with it.
- **In the product and the docs:** "routine" stays. Where the docs go deeper, they map it once: a
  routine is what the industry calls an agent (a spec, a harness, its tools, a model policy, limits).
- **Fleet:** a team of routines (agents) that runs one business process.
- **flotilla:** where a business runs its agents and governs them. It is not a framework for
  building one agent.
- Routines with no model today (deps, the business scans): reviewed with the examples; most could
  use a model somewhere.

## Our place

Agents are hard to build, and that is well covered by others. They are also hard to trust in a
business: flotilla is that layer. A trust stack inside one agent (model, system prompt, tools, hooks,
rules) is the base; flotilla adds the layers outside it: policy decided by code, autonomy earned per
action, people's approvals, the ledger, replay against real past cases.

Routines must still be trustworthy agents on their own: the outer layers stand on that base.

## Stances

- **Code coordinates agents.** Cases and recorded handoffs, not a model directing other agents.
- **Autonomy is earned, never configured.**
- **Our own spec and our own harness**, vendor agnostic (any OpenAI-compatible endpoint), portable
  (fleets are folders in a repo).
- **Business processes, not interactive chat.** The chat-agent half of the harness list (streaming,
  WebSockets, image processing, file chunking) is out of scope.

## Backlog

1. **The system prompt as a first-class part of a routine.** Today it is each fleet's code (triage's
   playbook); skills are in the spec, the system prompt is not. Declared in the spec, versioned, hashed
   and replay-gated, like the skills.
2. **Unknown-tool rejection in the shared harness.** Today only triage answers an unknown probe
   (`probes.py`); every routine's harness should.
3. **Hooks in the harness loop:** checks before and after each tool call (what an agent's tools may
   read or touch), in addition to the gates after its decision.
4. **Durable runs:** a long run (a bugfix fix) that dies mid-way resumes instead of starting
   over.
5. **Efficacy for the people who own a fleet:** what each routine achieved against its business goal,
   in their terms (cases handled, judged right or wrong, time and cost), not only the operator's view.
   Not solid today. The bridge to eval-harness.
6. **Composability:** routines that call each other within a process, first where triage and bugfix
   meet.
7. **To evaluate: a model directing agents** (an orchestrator agent), as an idea to explore
   against our stance that code coordinates.

Not planned: parallel tool execution, multi-agent turn-taking, and the chat-agent plumbing above.
