# Fleets: every note, per fleet

Where each fleet's designs, guides, plans, results and idea lists live, across the three repos that
hold them, so
none is lost in a reorganization. Built 2026-11-17 by the fleet session from a read of all three, on
the operator's
ask; the homelab fleet docs moved here the same day. Each fleet's material is also a candidate for
the future private
fleets repo.

- **flotilla** (this repo): the platform and the fleets' code; links below.
- **agentic-ai-homelab** (public): homelab's own operations; its fleet docs moved here on 2026-11-17
  ([history: from homelab](#history-from-homelab)).
- **fleet-eval-data** (private, `chipi/fleet-eval-data`): the evaluation of the fleets: plans, runs,
  results,
  datasets. Paths are given as text; the repo is private.

Status is what the note itself says. "Historical" means kept as the record.

## Triage (signal-fleet, Fleet 2)

| Note | Where | What it is | Status |
| --- | --- | --- | --- |
| [Triage fleet guide](../operations/triage.md) | flotilla (from homelab) | how it works and why: parts, a signal's path, the ledgers, the rules | text as of 2026-11-05; to update |
| [Triage runbook](../operations/triage-runbook.md) | flotilla (from homelab) | install, run, review, improve; the replay-proven loop | text as of 2026-11-05; to update |
| `signal-fleet/README.md`, `signal-fleet/mvp/README.md` | flotilla | the fleet's code overview | fleetd-era text |
| `signal-fleet/SIGNALS.md` | flotilla | the living design doc | pre-RFC brainstorm |
| `signal-fleet/EVAL.md`, `signal-fleet/EVAL-LESSONS-0.6.0.md` | flotilla | the eval design; what the 233-decision replay taught (§7: 2026-11-04) | design history; living |
| `signal-fleet/PHASE-0-infra.md`, `REVIEW-2026-07-24-*.md`, `REVIEW-ASK-round7.md` | flotilla | July readiness check and architectural review | historical |
| `fleets/triage/` (playbook, hint skills, routine specs) | flotilla | the triager's prompt, hints, routines | live |
| [RFC-0004: agent pipeline](../rfc/RFC-0004-agent-pipeline.md) | flotilla | triage on the platform | accepted |
| [RFC-0003 (homelab): signal to action](../history/homelab/RFC-0003-signal-to-action-fleet.md) | flotilla (from homelab) | the fleet's first design | historical |
| `docs/wip/phase-2-plan.md`, `phase-3-plan.md`, `promotion-plan.md`, `replay-entry-point.md` | flotilla | the move onto flotilla, the promotion, the replay contract | closed 2026-11-17 ([history 0003](../history/0003-wip-notes-closed.md)); was in progress / agreed |
| `ROADMAP.md` | fleet-eval-data | the plan of record: triage v2, then bug fixing | live since 2026-11-16 |
| `projects/triage-ops/OPPORTUNITIES.md` | fleet-eval-data | every improvement idea with its status | living |
| `projects/orrery-triage/` (README, GATES.md) | fleet-eval-data | decisions replayed and scored; the gates at $0 | results |
| `projects/signal-fleet-{filing,dedup,operational,rollup,routing,stale,retriage}/` | fleet-eval-data | the fleet's deterministic code against outcomes (NOTES.md each; filing audits, SILENT-FILINGS) | results, 11-05..13 |
| `projects/signal-fleet-retriage/CONFIRMATION-PLAN.md` | fleet-eval-data | held-out confirmation of re-triage | opens on or after 2026-12-19 |
| `projects/signal-fleet-replays/` | fleet-eval-data | the corpora behind flotilla's replay gates | live |
| `projects/triage-heldout/SPLIT.md` | fleet-eval-data | the held-out split (D = 2026-11-07) and its rules | live |
| `projects/stale-client-rule/README.md` | fleet-eval-data | a filing rule for stale clients | parked |
| `docs/wip/fleet-items.md` | homelab | the fleet session's open items (also the X3 assistant's gaps) | open |
| `docs/wip/eval-harness-feedback-fleet.md` | homelab | the eval-harness friction log, entries 1-55 | open |
| `workstation/claude/skills/triage-fleet-review/` | homelab | the weekly fleet review skill | live (not checked against flotilla) |

## Bug fixing (Fleet 1)

| Note | Where | What it is | Status |
| --- | --- | --- | --- |
| [The bugfix fleet](../operations/bugfix.md) | flotilla | operating it on the host | live |
| [RFC-0010: bugfix on the platform](../rfc/RFC-0010-bugfix-on-the-platform.md), [RFC-0013: the clarifier](../rfc/RFC-0013-bugfix-clarifier.md) | flotilla | the platform's design | proposed; accepted |
| [SR-0001: the coding harness](../security-reviews/SR-0001-bugfix-coding-harness.md) | flotilla | security review | 2026-11-15 |
| `docs/wip/bugfix-phase-a.md` (closed 2026-11-17, history 0003), `bugfix-phase-b-brief.md` | flotilla | build order; phase B brief | B active |
| `bugfix/fleet_rules.md` | flotilla | rules given to each headless run | live |
| `bugfix-fleet/` (README, BAKEOFF.md, agents/, bakeoff/) | flotilla | the July prototype and its bake-off corpus | historical |
| [RFC-0002 (homelab): autonomous bug-fix fleet](../history/homelab/RFC-0002-autonomous-bug-fix-fleet.md), [history 0005: bake-off matrix](../history/homelab/0005-bakeoff-matrix-arc.md) | flotilla (from homelab) | the first design and the bake-off | historical |
| `projects/bugfix-eval/` (PLAN, NOTES, NOTES-PODCAST, PODCAST-*, RESULTS-K0) | fleet-eval-data | the offline eval of `bugfix/fix` | results |
| `projects/bugfix-cost/PLAN.md` | fleet-eval-data | where the money goes, every cost change measured | live |
| `projects/bugfix-phase-b/ASSESSMENT.md` | fleet-eval-data | reply to the phase B brief | proposal |

## Remediation (Fleet 3)

| Note | Where | What it is | Status |
| --- | --- | --- | --- |
| [RFC-0005 (homelab): post-reboot recovery](../history/homelab/RFC-0005-remediation-fleet-post-reboot-recovery.md) | flotilla (from homelab) | Fleet 3's proposal | proposed 2026-09-30; whether the ops fleet replaces it is not decided |
| [RFC-0004: agent pipeline](../rfc/RFC-0004-agent-pipeline.md), [RFC-0009: post-incident review](../rfc/RFC-0009-post-incident-review.md) | flotilla | ops remediation routines on the platform | accepted |

## SEO

| Note | Where | What it is | Status |
| --- | --- | --- | --- |
| [RFC-0012: the seo fleet](../rfc/RFC-0012-seo-fleet.md), [The seo fleet](../operations/seo.md) | flotilla | design; operating it | accepted; live in shadow |

## Ops and platform fleets

| Note | Where | What it is | Status |
| --- | --- | --- | --- |
| `fleets/ops/`, `fleets/platform/` | flotilla | routine specs (backups, case review, heartbeat, model contract) | live |
| [Slack approvals](../operations/slack.md), [Host keys](../operations/host-keys.md), `docs/wip/host-agent-plan.md` (closed, history 0003), `pipeline-v1-plan.md` | flotilla | host agent, approvals, pipeline | live / planned |
| `docs/wip/flotilla-items.md` | homelab | the flotilla session's open items | open |
| `docs/recipes/slack-ops.md`, `docs/rfc/RFC-0009-service-slos.md` | homelab | homelab's own Slack and service levels (cite flotilla metrics) | live |

## Common: the platform and the evaluation

| Note | Where | What it is | Status |
| --- | --- | --- | --- |
| [Architecture](../architecture/index.md), RFC-0001..0013, ADR-0001..0016 | flotilla | the platform | accepted |
| [fleetd supervisor RFC](../history/homelab/RFC-0004-fleetd-supervisor.md), [its ADR](../history/homelab/ADR-0008-fleet-daemon-tech-and-framework-non-adoption.md), [its deploy recipe](../history/homelab/fleetd-deploy.md), [fleet architecture](../history/homelab/fleet-architecture.md) | flotilla (from homelab) | the fleets before flotilla | historical |
| `README.md`, `BETA-080.md`, `BOUNDARY-PROPOSAL.md` | fleet-eval-data | the eval home; the eval-harness 0.8 beta; where evaluation lives | live; proposal |
| `projects/issue-quality/` (PLAN, RUBRIC, baseline) | fleet-eval-data | the quality of filed issues (triage writes, bug fixing reads) | in progress |
| `projects/knowledge/` (STAGE1-PLAN, -SUMMARY, K1, K2) | fleet-eval-data | docs and skills for every fleet | in progress |
| `docs/wip/NEXT_STEPS.md` "Fleets" | homelab | points to the two items lists | live |

## History: from homelab

Moved 2026-11-17, each with a banner saying what replaced it: the [fleet
architecture](../history/homelab/fleet-architecture.md),
the [fleetd deploy recipe](../history/homelab/fleetd-deploy.md),
RFC-[0002](../history/homelab/RFC-0002-autonomous-bug-fix-fleet.md),
[0003](../history/homelab/RFC-0003-signal-to-action-fleet.md), [0004](../history/homelab/RFC-0004-fleetd-supervisor.md),
[0005](../history/homelab/RFC-0005-remediation-fleet-post-reboot-recovery.md), [ADR-0008](../history/homelab/ADR-0008-fleet-daemon-tech-and-framework-non-adoption.md),
[history 0005](../history/homelab/0005-bakeoff-matrix-arc.md).

## Known gaps (found in the inventory, not fixed here)

- Headers that drifted: flotilla's `signal-fleet/README.md` ("current state 2026-08-19"),
  `bugfix-fleet/README.md`
  ("not production"), `docs/wip/vision-plan.md` (says homelab still holds the fleet code, which it
  no longer does).
- Fleet to-dos sit in four places: homelab `NEXT_STEPS.md`, `fleet-items.md`, `flotilla-items.md`,
  and
  fleet-eval-data's `ROADMAP.md` with `OPPORTUNITIES.md`.
- Homelab Grafana dashboards `signal-fleet-*.json` and `bugfix-fleet*.json` read metrics of retired
  jobs (not checked).
