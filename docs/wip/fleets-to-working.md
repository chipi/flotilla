# Fleets to working: the next steps (2026-11-26)

**Decision (the operator, 2026-11-26):** keep every fleet on flotilla, and make each one work as
the next steps. Triage is the only fleet that does real, steady work today. RFC-0014's case 1 (ask
the second brain) moved to n8n alone (homelab RFC-0010 A4); its flotilla routine stays deployed and
idle.

## Where each fleet stands

Read from the installation on 2026-11-26 (`flotilla status`, `case list --all`, `action list`) and
GitHub's labels.

| Fleet | Runs | What it produced | Working? |
| --- | --- | --- | --- |
| triage | every 10 min | 117 issues filed in 30 days (three repos), escalations, stale candidates | yes |
| bugfix | stopped since 2026-11-16 | 9 bug cases, 4 PR plans, no PR ever opened | no |
| ops | backup monitor every 10 min, case review every 30 min | 1 failed-backup case, 2 rerun plans, none carried out | runs, acts on nothing |
| seo | weekly Search Console and traffic, a daily site check | 6 cases, all reports, no action | runs, acts on nothing |
| meetings (case 3) | on a Drive note | 2 cases, both test notes; the only 2 actions ever carried out | proven once, unused |
| knowledge (case 1) | on a question | 1 case, recorded, not answered | moved to n8n |
| platform | heartbeat, model contract | health of flotilla itself | yes |

## What "working" means for each

A fleet works when it does its job on real input, without someone watching, and the operator acts
on what it produces. For each fleet below: the job in one line, then what to settle first.

- **bugfix:** a bug issue becomes a reviewed fix PR.
  - Why no PR was ever opened: read the 4 plans and the stop on 2026-11-16.
  - Bring the remote workers on the X3 up to flotillad's version. Since sha-07c2272 they are
    refused: `its version sha-7ee5e53, not flotillad's`.
  - Then one real issue to one merged PR, end to end.
- **ops:** a failed backup is noticed and rerun.
  - Why `backup.rerun_observability` is still at record, with 2 plans never carried out.
  - Whether the case review produces anything the operator reads.
- **seo:** a regression on a site becomes something the operator acts on.
  - Today the cases are reports, and one (a traffic drop) is escalated with no owner.
  - Decide the action each kind should lead to (an issue, a fix PR, a note), and its autonomy.
- **meetings:** a meeting's notes become tasks and a decisions page, from real meetings rather
  than the test note.
  - Point it at the real notes folder.
  - Settle the approval: auto, or a Slack button.
- **knowledge:** retire the routine and the `ask` hook from the installation's config, or keep them
  as the reads example. The reads worker, `egress-reads` and their config can then go too.
  - The operator decides; nothing calls them now.
- **A process case for flotilla:** the next RFC-0014 case should be a business process (several
  steps, a decision, an action with consequences), so that cases, approvals, the ledger and replay
  earn their place. Triage and bugfix already have that shape.

## Order

Not set yet. Proposed:

1. bugfix: it has the business-process shape, and it is the closest to triage.
2. ops: smallest; an action exists and is only held at record.
3. meetings.
4. seo.

Agree the order with the operator before starting any of them.
