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

## Amendment, 2026-11-19: the ceiling of a routine action

Decision 3 said a routine action may reach auto, but the host executor's ceiling (propose,
ADR-0006 §5) was put in every plan's policy state, so no routine action could. The host ceiling
now caps only an action the host agent runs. A routine action has its own ceiling, an option of
the installation:

```yaml
routine_actions:
  max_autonomy: propose   # or auto; unset is propose
```

- **Default `propose`**, so deploying this changes no decision until an installation opts in (replayed:
  `internal/policy/testdata/default_ceiling_replay.jsonl`, 1296 decisions of the registry's real
  actions, run through the code before the change). `flotillad -check` prints the value.
- **With `auto`** a routine action runs without approval only when all three allow it: its routine's
  autonomy, the action's earned autonomy (ADR-0008) and this option. The freeze, the circuit breaker,
  taint, `reversible: no`, the recurrence rule, locks, holds and limits still apply, and a plan
  from a rolled-back version is still escalated.
- **A repo change** has no ceiling of this kind and never goes above propose by kind.
- **The host executor's ceiling stays `propose` for every host.** Per-host ceilings from each host's
  reviewed config (ADR-0006 §5) are not built; the code and the UI name it as the host executor's
  ceiling, and it does not read this option.
- The recorded reason says which ceiling applied: `host ceiling` (unchanged wording) for a host action,
  `routine-action ceiling` for a routine action, none for a repo change.

## Amendment, 2026-11-22: what `reversible` means, and `risk: high` (RFC-0014 §4.6)

- **`reversible` says whether the executor can take the action back.** A chat post can: the bot edits
  or deletes its own message and posts a correction (`plumbing.retract`). So `plumbing.reply` and
  `plumbing.post` are `reversible: yes`, and a person may approve them in Slack. A sent email stays
  `no`: nothing recalls it. Before an installation declares a chat post `yes`, it checks that its bot
  token can edit and delete its own messages in every channel kind it posts to.
- **`risk: high` also caps at propose,** however reversible: a message to a customer or the public is
  declared high, and a person approves each. Policy records `high risk: at most propose`, and
  `MaxAutonomy` returns propose; Slack already refused high. No action was declared high, so no past
  decision changes (the ceiling replay passes unchanged).
- **A routine action's check may ask the system it wrote to**, when flotilla has no read of its own:
  plumbing.call's check asks the callback, signed, whether the plan's delivery (`plan-<hash16>`) is
  done there. The plumbing finds it from the delivery id it wrote into what it made.
- **One executing routine per action.** A second routine with the same `execute:` action is a load
  error (`flotillad -check` fails), a disabled one included, as a second check routine for one verify
  condition already was: the dispatcher took the first match in load order, so which one ran depended
  on the folder names.

## Consequences

- The business examples (`fleets/crm`, `campaigns`, `outbound`, `renewals`) declare six routine actions
  in the image's registry, carried out by `business/apply.py` on stand-in systems (JSON files). An
  installation replaces the stand-in adapter (`business/store.py`) with one for its own CRM, campaign
  tool or mail service, and gives those routines their own worker with that system's credentials.
- `registry.Action.ByRoutine()` is the test for "a routine carries it out" (a repo or a routine action);
  `Repo()` stays the test for "a change to a repo", which alone is capped at propose by kind.
- Not decided here: a routine action's executor reaching a remote system through its own egress
  allowlist is the installation's (its worker's proxy), as for every worker.
