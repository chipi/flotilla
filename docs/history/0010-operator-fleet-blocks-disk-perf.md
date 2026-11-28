# 0010 — The operator fleet: step 1, blocks and flows, disk and the performance watch (2026-11-26 to 28)

**Date:** 2026-11-26 to 2026-11-28
**Operator:** Marko Dragoljevic
**Agent:** Claude Code (Opus 5.5), the flotilla session
**Outcome:** the operator fleet is live on the installation with three flows built from one block
library: backup (re-expressed, byte-equal), disk on five hosts, and a performance watch of the CI
runners, all at record. Follows [0009](0009-rfc-0014-case3-to-live-reads.md).

## What the operator pointed at

- An operator fleet backed by what actually broke ([the plan](../wip/operator-fleet-plan.md)):
  - a knowledge base of SOPs that grows after every fix;
  - a bridge from triage;
  - routing as an answer;
  - answers in the core, with chat apps as connectors.
- Then: "make other chains that reuse the elements", as "a library of blocks", where flows bring
  their own blocks and reuse the library's.
- Disk first, on every host, the laptop included.
- Through the homelab session: a performance watch, first on the CI runners.
- Last: what in GlitchTip and Grafana could be operationalized next.

## What landed

**Operator step 1** (C1 to C7, [its build plan](../wip/operator-step1-build.md)):

- [ADR-0023](../adr/ADR-0023-never-silent-and-answers-in-the-core.md), never silent:
  - the `watching` status;
  - one answers API behind the UI, the CLI and Slack;
  - Slack's notices.
- The SOP loop: match, follow or investigate, and a drafted SOP as a proposal.
- A fleet brings its own knowledge, shown on a Knowledge tab.
- The `routed:operator` label.
- The fleet renamed from `ops` to `operator`.
- Deployed as `sha-acb6742` with the fleets at `6d35b36`.

**[RFC-0016](../rfc/RFC-0016-blocks-and-flows.md), blocks and flows:**

- Blocks are immutable numbered folders. A flow pins each block (`use: metric-monitor@1`), and
  `flotilla flow expand` writes ordinary routine folders that `-check` keeps equal to it.
- The advisor's review replaced expansion at load with this committed generation.
- The backup chain re-expressed as a flow proved byte-equal to its live routines, so no version
  moved.
- Deployed as `46888d8`.

**Disk step 1** (RFC-0016 §8):

- New block versions: `metric-monitor@2` (groups, exclusions, hysteresis), `sop-act@2`,
  `sop-investigate@2` and `metric-verify@2`.
- Two host preconditions: free ratio below a value, and a path exists.
- A record-only action class.
- On the installation, `/` on five hosts:
  - the threshold is 10% free (the operator, from 30 days of data);
  - a case clears after 2 clear readings;
  - the threshold would have opened about 4 cases in 30 days.

**[RFC-0017](../rfc/RFC-0017-performance-watch.md), performance watch:**

- `ranked-watch@1` reads a ranked cost list and service levels by source name, and keeps its own
  snapshot.
- Known fixes are knowledge; the homelab session checked the mapping.
- Weekly and daily steps.
- The Hub added stable ids and windows for it.
- Review fixes: a stale source carries the open cases instead of closing them, and fetches are
  hardened.

**Deployed:**

- `9905dbe` with the fleets at `c59af92`, plus host config:
  - the two Hub sources, at its container on a shared Docker network;
  - the Hub's host on the egress allowlist.
- The first runs:
  - the weekly watch set its baseline and opened 3 `perf_regression` cases for the three breached
    CI service levels;
  - the disk monitor read 5 hosts, none below 10%.
- `DEPLOY-VERIFY: PASS` after each deploy.

## Fixed on the way

- Gates that did not stop a push (C2, C5), fixed by checking every exit code before pushing.
- `llms.txt` pushed against its 30000-byte cap:
  - short `purposes` for new docs;
  - the generated flow routines left out of the listing.
- The full e2e broken by C6 (the case list read 20 rows; the platform hour budget), fixed in
  `fbff9a0` and `acb6742`.
- A rebase that committed conflict markers: caught before the push and folded into one clean
  commit.

## Mistakes recorded

- The C7 builder left the shared fleets checkout on `operator/rename`, so another session pushed
  it to the fleets repo's main (kept, the operator 2026-11-27).
- Builders now use their own worktrees.
- One private branch was pushed with a plain `-f` instead of `--force-with-lease`; nothing was
  lost.

## Found, not flotilla's

- `status` failing its output schema in MCP clients is a client that keeps a tool list fetched
  before a deploy; `/mcp` reconnect does not refetch it.
  - A new process works.
  - The server's output validated strictly against its schema.

## Open threads

- **The next case: the DGX not serving prod.** From 30 days of GlitchTip and 60 of Grafana:
  - about 1100 errors on 5 days;
  - no alert fired on 11-26;
  - it needs a monitor for "prod cannot reach the DGX" and a host agent there.
- **The bugfix remote workers** (on the operator's laptop, not the X3) stayed on `sha-7ee5e53`
  through the three mini deploys of 11-27 and were refused, the GlitchTip flood FLOTILLA-5.
  - Updated to `sha-9905dbe` on 11-28: all four are accepted.
  - The cause: the laptop step was not run after each mini deploy. It belongs in the deploy
    itself, or it recurs on every deploy.
- **Disk:** a host that stops reporting reads as clear and closes its case.
- **Performance watch:**
  - `perf_regression` cases have no acting routine, so they wait at `handed_over`, and the stuck
    timer escalates them after 24 hours as stuck, not as a breached service level. The flow
    needs a small acting step that keeps them open as watching.
  - Its first report text reaches the morning brief of 11-28.
  - Player #64 (the public checkout) is in the fleets repo's `fixes.yaml` (`186b571`), not yet
    deployed.
- **Waiting on the operator:**
  - the Slack connector;
  - the `routed:operator` labels;
  - the operator intake's image.
- **The shared fleets checkout** is still on `operator/rename`.
