# Closing the initial vision

→ becomes a history entry when its last step is done.

The vision is the operator's of 2026-11-08 ([RFC-0001](../rfc/RFC-0001-fleet-platform.md),
[ADR-0001](../adr/ADR-0001-platform-foundations.md)): one plain fleet platform with triage, bugfix,
ops and a digital-marketing fleet (SEO first); a ledger of every decision; specs and config; a small
control plane with a CLI, an API, its own UI and an MCP server; the homelab's observability from the
first deploy. The order: redesign what exists into the platform, add SEO, then move every fleet
out of agentic-ai-homelab. The order of the steps below is the operator's (2026-11-14). The operator
calls closing them **the MVP** (2026-11-17), and added steps 7 and 8.

## Where it stands (2026-11-17, sha-69a8c58 on the mini and the X3)

How it got here: [history 0002](../history/0002-design-to-running-platform.md).

| Area | State |
| --- | --- |
| Platform | flotillad, workers, egress proxies, the UI, ledger, cases, policy, the host agent on the mini, the MCP server; schema 25 |
| Fleets | flotilla ships example fleets; the mini runs the operator's from the private fleets repo (step 7, route B) |
| Triage | live at propose on flotilla since 2026-11-11, on its own images since 2026-11-14; the candidate `triage/cycle-shadow` still runs |
| Ops | pipeline v1 in shadow: the backup monitor, rerun and verify ([pipeline v1](pipeline-v1-plan.md)); post-incident review; the morning brief; the daily model contract check |
| Slack approvals | built and tested against a stand-in; not wired (the operator is making the app) |
| MCP server | live since 2026-11-16, first caller a Claude session (step 2) |
| Bugfix | phase A in shadow, its workers remote on a second machine; `github.open_pr` at propose; the fleet STOPped while its evaluation (phase B) runs |
| SEO | site-check, traffic and search-console in shadow on the marketing worker (step 5) |
| agentic-ai-homelab | holds no fleet code since 2026-11-16 (step 6) |

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
account on the 5 properties) and Umami (a view-only user) are reachable. The traffic and
search-console routines followed the same day.

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

### 7. Example fleets in flotilla, the operator's fleets in a private repo

The operator, 2026-11-17: flotilla must not be drowned in the operator's exact fleets. The public repo
holds the platform and **example fleets** only; the operator's own fleets (their routines, playbooks,
prompts, repos, sites and thresholds) live in a **private repo** and are installed into the operator's
flotilla from there.

- The mechanism is [ADR-0013](../adr/ADR-0013-fleet-packs.md)'s fleet packs: a pack is a folder of
  text with named slots, and an installation fills the slots in its own config. Packs are designed,
  not built (route A, parked); route B, below, does the separation today. (When this step was
  written, `fleets/` was in the old layout and real values were in code.)
- The example fleets (triage, bugfix, ops, SEO) are generic, carry no real repo, site or host, and
  run in a demo installation against stand-in data.
- **Refactor to config only** (the operator, 2026-11-17): every installation fact moves out of code
  into config files (slots); code and example packs hold placeholders only.
- **A test that keeps it so:** a CI check that fails when code or an example pack holds an
  installation fact. It allows only placeholders (`example.com`, `example/repo`, documentation IP
  ranges) and refuses real-looking repo slugs, domains, tailnet names and host paths. Plus a replay
  that the operator's fleets, installed from the private repo, decide exactly as they do today.
- To decide per fleet: is its example a cut-down copy of the operator's fleet, or written on its own?
- **The repos** (the operator, 2026-11-17): `fleet-eval-data` is research only; what a fleet needs is
  crafted from it into the private fleets repo, and flotilla runs only that, with no link back. Done
  for the triage gates on 2026-11-17 (4a82e1d): the replay corpora, the filing gate and the weekly
  freeze come from the private repo.
- **The route** (the operator, 2026-11-17): B now, then A as the next MVP step.
  - **B, now:** the private repo holds the operator's `fleets/`, and the installation mounts its
    checkout read-only over `/opt/flotilla/fleets` in flotillad and in every worker that reads fleet
    files (they load specs, playbooks and skills from there), named in config. A deploy script pins
    the checkout to a commit, runs `flotillad -check` and the replays against it, then recreates the
    containers. Proof: every routine's spec hash is unchanged by the switch. Then flotilla's own
    `fleets/` becomes the example fleets.
  - **B is live since 2026-11-17 14:42Z:** the mini runs the private repo's `fleets/` (cdd472a,
    `deploy_fleets.sh`, `FLEETS DEPLOY PASS`, all 18 spec hashes unchanged; the first triage cycle on
    it, 14:45Z, ended ok).
  - **B done 2026-11-17:** `fleets/` are example fleets (`fleets/README.md`); the triage facts are the
    fleet folder's `triage/installation.json`, the sites `seo/sites.json`, the repos `github.open_pr`
    may act on `action_enums` in flotillad's config; the 18 replay gates moved to the private repo
    (`REPLAYS PASS (18)` with its facts, 5 fail with the example's); `make lint` refuses installation
    facts in the code (`scripts/installation_facts.py`). The retired deploy files and the research
    datasets left the repo.
  - **A, parked** (the operator, 2026-11-17: B, then back to the rest of the MVP):
    [ADR-0013](../adr/ADR-0013-fleet-packs.md)'s installation packs (RFC-0006 §10 step
    3): the mount becomes a pack folder; a change is shown, approved by its set hash, recorded and
    starts at shadow; runs read the approved snapshot, not the folder; rollback approves the previous
    set. Triage becomes a pack once RFC-0006 §9 is done; until then its private facts (the repo maps)
    move to config files the private repo supplies.

Done when flotilla's repo holds no fleet of the operator's, the mini runs its fleets from the private
repo with the same decisions (replayed), and the example fleets run in the demo installation.

### 8. Docs, README and a public site

The operator, 2026-11-17: the MVP is also something to show people.

- **Complete docs:** every page current. `docs/history/` gets the sessions since 2026-11-08, finished
  wip plans are closed, and AGENTS.md describes what runs today.
- **A README worthy of a world-class project.**
- **A public site** that presents the work, as eval-harness has (a landing page built from the repo,
  with the docs under it), showing the example fleets. At `flotilla.humancopilot.eu` (the operator,
  2026-11-17), built the way eval-harness's `evals.humancopilot.eu` is: the landing page with the
  docs under `/docs/`. The same level of detail and kind of page as evals', with flotilla's own
  visual style, graphics and messaging, not a copy of evals' look (the operator).

Done when the site is live, and the README and docs describe flotilla as it is.

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
- From the closed phase 3 plan ([history 0003](../history/0003-wip-notes-closed.md)): comparing live
  triage and a candidate at the filing level (live's filing records against the candidate's queued
  proposals; `shadow_compare.py` compares dispositions only); a contract check of the DGX's vLLM
  directly (the candidate runs on it since 2026-11-16; the daily check covers the aliases); the
  deployment-id-to-model map and an alert when an alias's deployment changes.
- From the closed host agent plan: the [key procedures](../operations/host-keys.md) (execution key
  rotation, the intermediate CA, a host's certificate) have not been run on a real host.
