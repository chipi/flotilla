# 0002 — From a design to a running platform (2026-11-08 to 2026-11-17)

**Dates:** 2026-11-08 to 2026-11-17 (about 370 commits)
**Operator:** Marko Dragoljevic
**Agent:** Claude Code (Opus 5.5), the installation session and then the flotilla session
**Outcome:** flotilla runs the installation's fleets. The triage fleet files real issues on flotilla
since
2026-11-11, the ops, SEO and bugfix fleets run in shadow, the fleet code has left
the installation's ops repo, and flotilla's own fleets are now examples: the operator's run from a private
repo. The initial vision has a plan to close it, the MVP ([vision plan](../wip/vision-plan.md)).

The genesis entry ([0001](0001-genesis.md)) ends with flotilla as a design. This entry is the record
of
the ten days that built it, by arc. Each arc names its decision records; the commits carry the
detail.

## Phases 1 and 2: the code moves in, and triage switches (11-08 to 11-11)

- **Phase 1** (11-08): fleetd, signal-fleet and bugfix-fleet imported from the installation's ops
  repo with their history; the replays read private corpora; CI ran the imported code.
- **Phase 2** (11-08 to 11-11): flotillad, the worker, the images, compose and an e2e stack
  ([ADR-0002](../adr/ADR-0002-run-in-docker.md)); state and secrets in volumes; a state mirror for
  the
  fleet session's readers; flotillad's own errors to GlitchTip; shadow comparison of a candidate
  worker.
  **The switch, 2026-11-11** (092a73f): triage runs on flotilla in propose.
- The words settled on the way: routine, not job ([ADR-0007](../adr/ADR-0007-routine.md)); one
  autonomy
  scale, shadow → propose → auto ([ADR-0008](../adr/ADR-0008-autonomy.md)); host agents act, workers
  think ([ADR-0009](../adr/ADR-0009-host-agent-names.md)); memory and knowledge
  ([ADR-0010](../adr/ADR-0010-memory-and-knowledge.md)); tools and actions
  ([ADR-0011](../adr/ADR-0011-tools-and-actions.md)).

## Phase 3: the runtime, and triage on it (11-08 to 11-13)

- **The runtime** ([RFC-0002](../rfc/RFC-0002-job-runtime.md),
  [ADR-0004](../adr/ADR-0004-job-runtime.md)):
  one model client, exact recording of every model call and gate lookup, the playbook and class
  hints
  as files and skills ([ADR-0012](../adr/ADR-0012-skills.md)), the probe loop on the shared harness,
  the
  fleets' ledger in flotillad's store.
- **Fleet packs** ([RFC-0006](../rfc/RFC-0006-fleet-packs.md),
  [ADR-0013](../adr/ADR-0013-fleet-packs.md),
  accepted 11-12 after six independent reviews): a fleet as a folder of text; evaluation lives
  outside
  flotilla, which provides a replay entry point (`replay_api`, agreed with the evaluation project).
- **Triage's own fixes** replayed before shipping: re-triage of a recurring dismissed error, linked
  ledger rows, unmapped projects filed in the ops repo; the **promotion** of the live fleet onto
  main
  and the API ledger, 2026-11-14.

## The pipeline, cases and host agents (11-09 to 11-14)

- **The agent pipeline** ([RFC-0004](../rfc/RFC-0004-agent-pipeline.md), eight revisions,
  [ADR-0005](../adr/ADR-0005-agent-pipeline.md)): signal → case → plan → policy → execution → check;
  ops is one fleet with item kinds, SEO the digital marketing fleet's first job.
- **Pipeline v1** (11-13): cases with a transition table and stuck-state timers, the action
  registry,
  policy (autonomy, locks, holds, the deploy stand-down, automatic demotion), monitors opening
  cases,
  the backup monitor and its acting and check routines on two ops workers.
- **The host agent** ([ADR-0006](../adr/ADR-0006-node-agent.md), plan H1 to H10): signed execution
  envelopes, a host's own allowlist and sudoers, a hash-chained audit log, an offline CA, enrolment
  and
  revocation, the connection to flotillad. Live on the host 2026-11-14 16:13Z, a week early.

## The operator's surfaces (11-12 to 11-17)

- **The UI** ([RFC-0007](../rfc/RFC-0007-operator-ui.md),
  [ADR-0014](../adr/ADR-0014-operator-ui.md)):
  users and roles with one `authorize()`, sessions, the freeze; rebuilt to mockup v3 as the visual
  contract; the gap batch G1 to G15 (each model call of a run, traces, logs, host load, knowledge,
  evidence toward the next autonomy level, the morning brief, Slack approvals through the host
  agent's
  connector, daily model contract checks); then the routine page drawn from its spec, the run page
  as
  one timeline, the decision mix, and the instruments and model panel of 11-17.
- **The MCP server** ([RFC-0008](../rfc/RFC-0008-mcp-server.md),
  [ADR-0015](../adr/ADR-0015-mcp-server.md)):
  other agents read and steer flotilla at `/mcp`, and can never approve; first real caller 11-16.
- **Post-incident review** ([RFC-0009](../rfc/RFC-0009-post-incident-review.md),
  [ADR-0016](../adr/ADR-0016-post-incident-review.md)): `ops/case-review` proposes what to learn;
  `process_change` is a plan, a PR the operator approves.

## Bugfix and SEO on the platform (11-15 to 11-17)

- **Bugfix phase A** ([RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md)): intake, fix, open-pr
  and
  pr-check as routines; a fix checks only what it touches; `github.open_pr` with a token narrowed to
  the
  plan's repo. Its workers run on a second machine as **remote workers**
  ([RFC-0011](../rfc/RFC-0011-remote-workers.md)),
  each holding only its own model key. A Python profile for a second repo (B8); the cost of a run is
  the
  gateway's bill; the clarifier's step one ([RFC-0013](../rfc/RFC-0013-bugfix-clarifier.md)). The
  coding harness had a security review
  ([SR-0001](../security-reviews/SR-0001-bugfix-coding-harness.md)).
  Phase B, whether its fixes are good, is an evaluation the operator runs with the fleet session;
  the
  fleet is STOPped meanwhile.
- **The SEO fleet** ([RFC-0012](../rfc/RFC-0012-seo-fleet.md)): site-check, traffic and
  search-console
  in shadow on the marketing worker; its first run opened two real cases.

## Deploying it safely (11-14 to 11-17)

- A deploy snapshots the store before every `up` and restores a fitting snapshot on a rollback past
  a
  schema step; it needs the commit's ci-fast and e2e green; the triage fleet's filing gate replays
  the
  new code on the newest frozen week; a STOP cancels runs in flight. The schema reached step 25.
- Telemetry per model call: role, episode, tools and cost (`run_calls`, schema steps 24 and 25).

## The MVP and the private fleets (11-17)

- The operator named the rest of the vision **the MVP** and added two steps: example fleets in
  flotilla
  with the operator's own in a private repo (step 7), and docs, a README and a public site (step 8).
- **Three repos, one direction:** the installation's research repo is research; what a fleet needs
  is crafted from it
  into the private fleets repo; flotilla runs only that ([history 0000](0000-origin-evaluation.md)
  is the
  evaluation that started it all, kept here).
- **Step 7, route B, done the same day:** the host runs the private repo's fleets mounted over the
  image's (`deploy/compose/fleets.yml`, `deploy/server/deploy_fleets.sh`), with every spec hash
  unchanged;
  the triage facts, the SEO sites and the repos `github.open_pr` may act on are the installation's
  config; the 18 replay gates moved to the private repo; `make lint` refuses installation facts in
  the
  code. Route A, the approved snapshots of ADR-0013, is parked.

## Open threads

- Slack approvals: built, not wired (the operator is creating the app).
- Step 8: these docs complete and current, the README, the public site.
- Bugfix phase B and turning the bugfix fleet on: the operator's.
- Route A (installation packs with approval), parked.
