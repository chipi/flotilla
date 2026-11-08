# 0001 — Genesis

**Date:** 2026-11-08
**Operator:** Marko Dragoljevic
**Agent:** Claude Code (Opus 5.5), the homelab session
**Outcome:** flotilla exists as a design: [RFC-0001](../rfc/RFC-0001-fleet-platform.md) accepted
as [ADR-0001](../adr/ADR-0001-platform-foundations.md). There is no code yet.

## How it started

The operator brought an architecture brief for a "local-first SEO intelligence" system built on
Hermes Agent, and asked for three evaluations: the brief and its use cases, Hermes on its own (and
how to run it safely on the homelab), and a catalogue of use cases with outside-in website reach
first.

The evaluation, in agentic-ai-homelab's
[`docs/wip/agent-platform-evaluation.md`](https://github.com/chipi/agentic-ai-homelab/blob/main/docs/wip/agent-platform-evaluation.md),
kept three things apart: the problem, the constraints the homelab already lives by, and the
candidate solutions. It found:
- **Hermes:** unsupported on the Intel Mac mini, cloud web search by default, self-editing skills,
  and several 2026 CVEs.
- **OpenClaw:** a better fit for scheduled, gated, local work, with a much heavier CVE stream.
- **The homelab itself:** it already runs the plain version of the pattern, in the triage fleet and
  the daily brief.

## What the operator decided

- **Plain first.** Build our own platform and learn the machinery from the inside; Hermes and
  OpenClaw are inspiration.
- **Converge what exists:**
  - the triage fleet, the bugfix fleet and the daily brief, plus the new SEO work, become fleets
    of one platform;
  - an `ops` fleet takes the brief and the operational reviews done by hand today.
- **Spec- and config-driven,** with a small control plane: its own UI, a CLI, and MCP from the
  start.
- **A ledger of every decision.**
- **Integrated into the homelab's observability stack** like every other service.
- **The specifics:**
  - name `flotilla`;
  - a public repo under MIT, with a MkDocs docs site like the other projects;
  - Go control plane, Python job runtime;
  - runs on the mini (64 GB once fitted) as `claude`;
  - the daily brief goes local-only;
  - the production corpora move to the private eval repo.

## Open threads

- **Phase 0** (RFC-0001 §9): give agentic-ai-homelab its own mini deploy script, out of
  `signal-fleet/deploy/`, before any fleet code moves.
- **Phase 1:** move fleetd, signal-fleet and bugfix-fleet here with history. Turn the hard-coded
  specifics into config. Wire the Makefile gates and CI for Go and Python (they are placeholders
  that pass with no code until then).
- **Not decided:** whether to scrub the production corpora from agentic-ai-homelab's public
  history.
- **RFC-0002:** the SEO fleet (Orrery first; Google Search Console access to be set up by the
  operator).
