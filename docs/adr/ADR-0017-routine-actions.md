# ADR-0017 — Routine actions

**Status:** Accepted

**Date:** 2026-11-17

**Decided by:** the operator, who asked for business-process examples (marketing, sales ops, outbound,
customer success) that act on the systems a business runs, not only on hosts and repos

## Context

The registry had two executors. A **host** action runs on a host, signed, through the host agent's
own allowlist. A **repo** action is a change to a repo, carried out by a routine (`bugfix/open-pr`)
or by a person, and always capped at propose (ADR-0016).

A business process acts on other systems: a CRM field is normalized, two duplicate contacts are
merged, a campaign is launched or held, an email is sent, an account is flagged for its owner. None
of these is a host command or a repo change, and some of them (a field normalized, a campaign held)
are reversible enough to earn auto, which a repo action never may.

## Decision

1. **A third executor, `routine`.** A routine with an `execute:` trigger carries the action out against
   the system it reaches, exactly as an approved repo action reaches `bugfix/open-pr`: once per plan,
   with the approved plan (`FLOTILLA_PLAN`), its result moving the case to checking or escalating it.
   No host agent is involved; the case's target is not a host.
2. **Its params** may be `enum` or `text` with a maximum, as a repo action's; a `target_host` param
   is refused. It needs no host precondition: its executing routine re-checks what the plan assumed
   before it acts (the field still has its old value, the campaign is still ready, the contact is not
   suppressed).
3. **Its autonomy is earned like a host action's.** It may reach auto, unless it is declared
   `reversible: no` (a merge, a launch, a sent email): policy keeps those at propose, a person approving
   each.
4. **It declares a check**, and a check routine verifies the action happened by reading the system
   afterwards. An action with no check is escalated as not verified, as before.
5. **The executing routine checks the plan's hash** (`policy.Plan.Hash`) against the approved one before
   it acts, as `bugfix/open_pr.py` does: a changed parameter is refused, not carried out.

## Consequences

- The business examples (`fleets/crm`, `campaigns`, `outbound`, `renewals`) declare six routine actions
  in the image's registry, carried out by `business/apply.py` on stand-in systems (JSON files). An
  installation replaces the stand-in adapter (`business/store.py`) with one for its own CRM, campaign
  tool or mail service, and gives those routines their own worker with that system's credentials.
- `registry.Action.ByRoutine()` is the test for "a routine carries it out" (a repo or a routine action);
  `Repo()` stays the test for "a change to a repo", which alone is capped at propose by kind.
- Not decided here: a routine action's executor reaching a remote system through its own egress
  allowlist is the installation's (its worker's proxy), as for every worker.
