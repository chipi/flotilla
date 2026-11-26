# ADR-0023 — Never silent, and answers in the core with chat apps as connectors

**Status:** Accepted (2026-11-26, the operator). Amends ADR-0005 decision 1 for routines that opt
in. Built as step 1 of [the operator fleet plan](../wip/operator-fleet-plan.md); the build plan
is [operator-step1-build.md](../wip/operator-step1-build.md).

## Context

**A case went silent.** On 2026-11-21 the mini's backup was missed and reached 47.8 hours old.

- The ops fleet's monitor opened a `failed_backup` case.
- The rerun was held at `record`, so the case closed within 3 seconds as `plan_recorded`.
- It then absorbed 136 more readings over 23 hours.
- Nobody was told. The same happened on 2026-11-14.

**A recorded plan ends the case:** `applyDecision` maps policy `Record` to `closed/plan_recorded`,
and later detections are only `signal.absorbed` into the closed case.

**Answers are Slack-shaped.**

- Only plans can be answered, and only with Approve or Reject.
- The answer comes through the UI, the CLI, or Slack's connector, which knows nothing else.
- The operator also wants to answer an escalation by routing it (to operator, to bugfix, dismiss,
  mute), and wants this to work with any chat app an installation uses, not only Slack.

## Decision

1. **Never silent.**
   - A routine may set `case.on_record: watch` (the default stays `close`). A plan recorded under
     it moves its case to a new open status, `watching`, instead of closing it.
   - **While watching:** later detections attach to the open case and count as repeats.
   - **Notices:** the operator is told at once, then on a bounded schedule (4 hours, 24 hours,
     then daily, at most 7 notices), never once per signal. Acknowledging snoozes the schedule.
   - **Closing:** the case closes only when its monitor reports the condition clear (resolution
     `resolved_itself`), or when the operator acts on it.
   - **Evidence:** the promotion gate counts a watched case as a recorded plan, like a closed one.
2. **Answers belong to the core.**
   - A case offers answers by its kind:
     - a plan: approve, reject;
     - an escalation: operator, bugfix, dismiss, mute, comment, as its case kind declares;
     - a notice: acknowledge.
   - One API lists a case's answers and gives one (`/v1/cases/{id}/answers`).
   - ADR-0005's limits are applied there, the same for every client: risk, plan hash, the daily
     cap, freezes and holds.
   - The actor is the person, resolved through the directory. Only operators answer. MCP never
     does, and neither does a routine.
3. **Chat apps are connectors.**
   - A connector does two things: it shows a case's question with its answers, and it relays a
     person's choice.
   - flotillad checks every choice against the answers it offered. The UI and CLI are clients of
     the same API.
   - **Slack is the first connector,** in the host agent as ADR-0006 built it.
   - **Other apps** get connectors when an installation needs them. GitHub labels already serve
     triage as one.
   - **Without a connector,** an installation can wire any app through the plumbing pattern (a
     signed hook in, a signed callback out).

## Consequences

- **Old behaviour is unchanged.**
  - Routines that do not opt in behave exactly as before. The existing tests and e2e sections
    stay as they are.
  - Policy's decisions do not change, and the golden replay file stays byte-identical.
- **Old cases keep the old rule:** a closed `plan_recorded` case from before this ADR still
  absorbs its key until cleared. The operator fleet's rename therefore drains them first.
- **The connector wire changes,** so flotillad and the host agent are upgraded together.
- **The Slack connector stays inside the mini's host agent** for low and medium risk. ADR-0006's
  separate instance is due when an operator action is promoted past `propose`.

## Non-goals

- No approval through MCP, and no answer that only one chat app can give.
- No new chat app now.
- No change to which outcome policy decides.
- Host read actions for diagnosis: a separate ADR, with the disk case.
