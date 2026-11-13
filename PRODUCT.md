# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Decided in [RFC-0001](docs/rfc/RFC-0001-fleet-platform.md) §10 (decision 3): the UI is served by the
flotillad Go binary, as server-rendered pages with a little JavaScript, with no separate frontend
build. Its pages call the server's own functions under the same authorization as the API, the CLI
and the MCP server ([ADR-0014](docs/adr/ADR-0014-operator-ui.md)): it adds no authority and takes
none away.

## Users

The primary user is the operator of one flotilla installation: the person who runs the fleets,
sets their limits and approves what agents may do. They use the UI on a phone (often away from a
desk, answering an approval or checking that all is well) and on a laptop (reading a case in
depth, reviewing runs). Today that is one person on one homelab.

flotilla is written for any installation, so operators of other installations (people running
their own flotilla, sharing fleet packs) are a later, secondary audience. The UI must not assume
the first operator's setup. Several people can work one installation, each with a role: `admin`,
`operator` or `viewer` (ADR-0014).

## Product Purpose

flotilla runs small fleets of AI agents (triage, operations, marketing, bug fixing) that may act,
within limits the operator sets and can see. The UI exists so the operator can **decide and
trust**: approve or refuse what an agent wants to do, with enough of the case in front of them
(why it was proposed, the evidence, what exactly would happen, what policy already allowed or
refused) to decide quickly and correctly, often from the phone.

Watching the fleets (runs, cases, health, spend) is the second job: it serves deciding, and it is
how trust in a routine is earned before its autonomy is raised.

Success: the operator never approves something they did not understand, never has to open
another tool to decide, and can see at a glance that nothing is waiting or wrong.

## Positioning

Agents in flotilla read through named tools and only plan actions; policy in the control plane
decides each one (run, ask the operator, or refuse), and how much may happen without asking is
earned per action and per routine (`shadow`, `propose`, `auto`) and raised only by the operator.
The UI is where that earned, recorded trust is visible and decided: every case keeps its whole
timeline, and every approval is bound to the exact plan the operator saw.

## Operating Context

- **Cases:** every problem is a case: detected, diagnosed, handed to the routine that can fix it,
  planned, decided by policy, executed, checked and closed, with its timeline kept.
- **Approvals** come from the UI, the CLI, or the operator's pinned Slack user within limits. An
  approval is bound to the plan's hash.
- **Routines** run on triggers (time or case events), each at an autonomy level; shadow runs
  record what they would do without doing it.
- **Fleet packs** (ADR-0013): flows as folders of text; a new or changed pack is shown, approved
  by its set hash and starts at `shadow`.
- The operator reaches the UI over a private network only (tailnet), from a phone or a laptop.
- Neighbouring tools the operator also uses: the CLI (`flotilla …`), Slack for approvals within
  limits, the homelab's dashboards for metrics and logs.

## Capabilities and Constraints

- Views planned (RFC-0001 phase 5): fleets and routines, run history, cases with their timelines,
  the ledger, and the approval queue (approve or reject with a reason). Pack approvals (ADR-0013)
  join them.
- **Works well on a phone:** every screen usable at 360 to 430 px wide, with no page wider than
  the screen; checked at those widths before any UI change ships.
- **Approvals are the operator's only:** outward actions are approved by the operator in the UI
  or the CLI (or the pinned Slack user within limits), never by an agent and never through MCP.
  The UI makes clear who is acting and binds each approval to the exact plan shown.
- Network: API, UI and MCP listen on the tailnet only.
- Terminology is fixed by accepted decisions and used as is: case, routine (not "job",
  ADR-0007), autonomy `shadow` / `propose` / `auto` (ADR-0008), tools (reads) and actions (writes,
  ADR-0011), worker and host agent (ADR-0009), memory and knowledge (ADR-0010), skill (ADR-0012),
  fleet pack, pack hash and set hash (ADR-0013).
- Not built yet: cases, policy, host agents, MCP and the UI itself.

## Evidence on Hand

- The architecture and decisions: `docs/architecture/`, `docs/adr/`, `docs/rfc/`.
- A UI mockup, version 2 (2026-11-11), published as a design canvas: the source of the term review
  behind ADR-0007 and ADR-0008. Its UX review round is planned for the end of phase 4, before the
  UI is built.
- Real run, case and ledger data exist only in installations; no customers, testimonials,
  benchmarks or usage numbers exist and none may be invented.

## Product Principles

1. **Decide with the case in view.** Every decision shows why, the evidence, and exactly what
   would happen; nothing important is one click away when deciding.
2. **Trust is earned and visible.** Autonomy levels, what ran in shadow, and what the operator
   approved are shown, never implied.
3. **The phone is a first-class place to work.** An approval or a check that nothing is wrong
   must be complete on a phone.
4. **Exactly what ran.** What the UI shows is the recorded truth (the plan's hash, the pack's
   hashes, the run's record), not a summary that could drift from it.
5. **The operator acts; agents propose.** The UI never lets an agent's identity stand in for the
   operator's.

## Accessibility & Inclusion

WCAG 2.2 AA for every screen, on phone and desktop.
