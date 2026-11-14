# Closing the initial vision

→ becomes a history entry when its last step is done.

The vision is the operator's of 2026-11-08 ([RFC-0001](../rfc/RFC-0001-fleet-platform.md),
[ADR-0001](../adr/ADR-0001-platform-foundations.md)): one plain fleet platform with triage, bugfix,
ops and a digital-marketing fleet (SEO first); a ledger of every decision; specs and config; a small
control plane with a CLI, an API, its own UI and an MCP server; the homelab's observability from the
first deploy. The order: redesign what exists into the platform, add SEO, then move every fleet
out of agentic-ai-homelab. The order of the steps below is the operator's (2026-11-14).

## Where it stands (2026-11-14, sha-ccfef86 on the mini)

| Area | State |
| --- | --- |
| Platform | flotillad, workers, egress proxy, the UI (mockup v3, every page; [parity](ui-mockup-parity.md)), ledger, cases, policy, the host agent on the mini; schema 19 |
| Triage | live on flotilla since 2026-11-14 ([promotion plan](promotion-plan.md)); the candidate `triage/cycle-shadow` still runs |
| Ops | pipeline v1 in shadow: the backup monitor, rerun and verify ([pipeline v1](pipeline-v1-plan.md)); the morning brief, written by flotillad without a model; the daily model contract check |
| Slack approvals | built and tested against a stand-in; not wired (no app) |
| MCP server | not built (only a comment in `internal/api`) |
| Bugfix | its code is in `bugfix-fleet/`; it does not run as a flotilla routine |
| SEO / digital marketing | not started |
| agentic-ai-homelab | still holds `fleetd/`, `signal-fleet/`, `bugfix-fleet/`, frozen |

## The steps

### 1. Slack and the v1 gate (2026-11-15)

- The operator creates a separate flotilla Slack app ([runbook](../operations/slack.md), steps 1
  to 3; not the Homelab Ops app, which keeps only Grafana's webhook).
- Wire it: flotillad.yaml `slack` (workspace, member id, `ui_url`), the mini agent's `slack` section,
  the token files (mode 600, the agent's user), upgrade the agent; post one plan and answer it from
  Slack end to end.
- The v1 gate: the operator judges 5 shadow `failed_backup` cases (Right or Wrong on the case); the
  Autonomy bar counts them ([G13](ui-mockup-parity.md)). Then the operator decides on promoting
  `backup.rerun_observability` to propose, approved from Slack.
- Check what ran overnight: the 07:00 brief, model calls recording on runs (fixed 2026-11-14,
  0d7f642), the daily contract check.

Done when one real plan was approved in Slack and ran, and the gate's decision is recorded.

### 2. The MCP server

Promised from the start (RFC-0001): other agents (Claude sessions, the X3 assistant) see and steer
flotilla through the same API as the CLI and the UI.

- Read tools: status, runs, cases, the ledger, the approval queue, models and spend.
- Action tools: run or dry-run a routine, pause, propose.
- An agent's call is never the operator's approval: no approve tool (security T3); every call is
  recorded with its identity.
- An RFC first, short: transport (stdio over SSH or HTTP), auth (an agent token), the tool list.

Done when a Claude session lists open cases and runs a routine through it, and a refused approve is
tested.

### 3. Bugfix onto the platform

- Its routines as specs in `fleets/bugfix/`, on its own worker (repo write credentials, nothing else).
- Its decisions in the ledger; its PRs as cases with a plan, so autonomy, the evidence bar and Slack
  apply (`github.open_pr` in the registry).
- The bake-off results in `bugfix-fleet/` decide the harness; read them before designing.

Done when bugfix runs in shadow on the mini and its runs show on its fleet page.

### 4. SEO, the digital-marketing fleet

- Design first: Orrery as the first site; Google Search Console and Umami as sources (GA4 is out);
  what a finding is, how often, what it may do (propose only to start).
- Its own worker and credentials; findings into the brief's sections.

Done when the design is accepted and the first routine runs in shadow.

### 5. Move the fleet code out of agentic-ai-homelab

- Check that nothing on the mini or the DGX runs the homelab copies (launchd jobs, crontabs, deploy
  scripts reading them).
- Delete `fleetd/`, `signal-fleet/`, `bugfix-fleet/` there, with a pointer to flotilla.

Done when the homelab's CI and the mini's deploy pass without them.

Then: the robotics lab's asks (its FLOTILLA-ASKS.md), assessed only after the steps above.

## Also open, not in the order

- Ending the candidate `triage/cycle-shadow` needs the operator's go.
- The brief's model-written summary and the playbook changes proposed from the marks (a local-model
  brief routine).
- Agents reading active notes into a model (a prompt change: replay first).
- G4's host roles; container numbers on the mini (no cAdvisor).
- The recurrence count of the earlier design (3 in 7 days) and `caused_by` attaching
  ([run lifecycle](../architecture/run-lifecycle.md)).
- The contract check at a time of day (it runs every 24 h from start).
