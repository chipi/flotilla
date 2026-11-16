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
| MCP server | built 2026-11-15 (ADR-0015); no real caller yet |
| Bugfix | phase A live 2026-11-15 in shadow: intake, fix, open-pr and pr-check as routines, its workers on the X3 as remote workers (RFC-0011); phase B (the evaluation) is a separate activity |
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

**Built 2026-11-15** ([ADR-0015](../adr/ADR-0015-mcp-server.md), [runbook](../operations/mcp.md)):
17 tools at `/mcp` on the UI node, one token per caller, the official SDK, agents reading the ledger
and proposing notes, a dry run. **Done 2026-11-16:** a headless Claude session on the X3 (caller
`claude-x3`) listed the 13 open cases, ran `platform/heartbeat` (run trigger `manual/claude-x3`) and
found no tool that approves; every call is in the events under its identity.

Promised from the start (RFC-0001): other agents (Claude sessions, the X3 assistant) see and steer
flotilla through the same API as the CLI and the UI.

- Read tools: status, runs, cases, the ledger, the approval queue, models and spend.
- Action tools: run or dry-run a routine, pause, propose.
- An agent's call is never the operator's approval: no approve tool (security T3); every call is
  recorded with its identity.
- An RFC first, short: transport (stdio over SSH or HTTP), auth (an agent token), the tool list.

Done when a Claude session lists open cases and runs a routine through it, and a refused approve is
tested.

### 3. Post-incident review (Ops)

**Built 2026-11-15** ([ADR-0016](../adr/ADR-0016-post-incident-review.md)): `ops/case-review` with
the five rules, monitors' measures, `process_change` as a real plan (`repo.process_change`), applied
by hand with its commit recorded. The done check passes on the frozen first shadow case (25 h, with
its evidence). **Done 2026-11-16:** deployed, and `repo.process_change` promoted to propose (the
operator).

The operator, 2026-11-15: after a case closes, a review looks at what happened and what to learn, as
a post-mortem would. Found by the first shadow case (`case-20261114-6983646f65`): the chain was right
within its preconditions, but the check itself was wrong (a 20 h threshold on a backup that runs
about every 24 h), and nothing in flotilla said so.

- **A review routine on closed cases** (`ops/case-review`), starting with code rules, each citing the
  case's evidence:
  - a case that cleared itself inside the source's normal interval (a threshold below that interval);
  - the same fix coming back (G14's recurrence, T17);
  - a Wrong verdict; an escalation; a plan the operator rejected.
- **Two outputs:**
  - **Knowledge:** a note proposed in Knowledge (proposed, then active or dropped by a person), which
    agents read in later cases.
  - **A process change:** a new case kind, `process_change`, whose plan is a change to a check, a
    threshold, a spec or the registry, shown as a diff. Always at propose, never auto; approving it
    opens a PR on the repo, so the change goes through review, CI and deploy like any other. The
    system never edits its own config in place.
- **Later:** a model-written post-mortem from the case's timeline, as untrusted input that only
  proposes.
- **It feeds the record:** approved and rejected `process_change` plans count on Autonomy; rejections
  teach the review what not to propose.
- An RFC first, short: the rules, the `process_change` kind and its action (`github.open_pr` on the
  repo), who may approve.

Done when last night's pattern, replayed, produces a `process_change` proposing 25 h with its evidence,
and a rejected proposal is recorded.

### 4. Bugfix onto the platform

**Phase A done 2026-11-15** ([RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md) §7, §9;
[RFC-0011](../rfc/RFC-0011-remote-workers.md); [SR-0001](../security-reviews/SR-0001-bugfix-coding-harness.md)):
the routines run in shadow, the fix and push workers on the X3 through their links, a fix checks only
what it touches, and the fleet page shows the runs. Left: the operator makes the GitHub App, then
`github.open_pr` to propose. Phase B (making the fixes good) is the operator's, with another agent.

- Its routines as specs in `fleets/bugfix/`, on its own worker (repo write credentials, nothing else).
- Its decisions in the ledger; its PRs as cases with a plan, so autonomy, the evidence bar and Slack
  apply (`github.open_pr` in the registry).
- The bake-off results in `bugfix-fleet/` decide the harness; read them before designing.
- **In beta evaluation** (operator, 2026-11-15): the bake-off found every arm equal on 5 bug families,
  which cannot tell arms apart. Nothing it chose is assumed to work. The port is measured in
  eval-harness on a frozen `orrery-bugfix` dataset (orrery's closed bugs, their fix commits' tests as
  the oracle), one arm per change ([RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md) §8).

Done when bugfix runs in shadow on the mini and its runs show on its fleet page (phase A: the form
and the flow). Making its fixes good (the evaluation, the harness, the models) is phase B, a separate
activity over the following days and weeks (RFC-0010 §7, operator 2026-11-15).

### 5. SEO, the digital-marketing fleet

**Done 2026-11-15** ([RFC-0012](../rfc/RFC-0012-seo-fleet.md), accepted;
[operations](../operations/seo.md)): `seo/site-check` runs daily in shadow on the `marketing` worker
on the mini, over the operator's 5 sites. Its first run (19:37Z) opened 2 cases: Orrery's robots.txt
keeps Baiduspider out, airandspacehistory.com's home canonical is `/home`. Search Console (a service
account on the 5 properties) and Umami (a view-only user) are reachable; the traffic and
search-console routines are next.

- Design first: Orrery as the first site; Google Search Console and Umami as sources (GA4 is out);
  what a finding is, how often, what it may do (propose only to start).
- Its own worker and credentials; findings into the brief's sections.

Done when the design is accepted and the first routine runs in shadow.

### 6. Move the fleet code out of agentic-ai-homelab

**Done 2026-11-16** (agentic-ai-homelab 9e3ab73): `fleetd/`, `signal-fleet/` and `bugfix-fleet/` removed;
the mini's `com.homelab.fleetd` and `com.homelab.bugfix-metrics` jobs retired (archived in
`~/flotilla/retired/`); their three alert rules deleted; fleet-eval-data's scripts read flotilla's copy.

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
