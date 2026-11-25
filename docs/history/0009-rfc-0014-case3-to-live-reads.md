# 0009 — RFC-0014 case 3 to live reads, the UI pass and the mini's clock (2026-11-22 to 25)

**Date:** 2026-11-22 to 2026-11-25
**Operator:** Marko Dragoljevic
**Agent:** Claude Code (Opus 5.5), the flotilla session
**Outcome:** RFC-0014 steps 3 to 5 landed: case 3 ran live on the installation, the directory reader
and live reads are built. The UI had a consistency pass over real data. flotillad's slowness was the
mini VM's clock, fixed there; the read pool built for it was reverted. Four GlitchTip noise sources
were fixed. Follows [0008](0008-rfc-0014-hook-and-plumbing-call.md).

## What the operator pointed at

RFC-0014's build order (§8) after steps 1 and 2: case 3 (a meeting into its follow-ups), the
directory, live reads; then the UI with real data ("consistency above everything"), flotillad's
slowness in its container, and the GlitchTip issues the homelab session relayed as too noisy.

## What landed

- **Case 3** (a meeting to its tasks and decisions): a plan's `then` (RFC-0013 §2 amended), so a
  verified check hands a case on instead of closing it; the example fleet `meetings`; the e2e's
  plumbing section as its acceptance. Live on 2026-11-23: `case-20261123-083d19eb2f` opened 12:36Z,
  both plans approved, n8n made 3 tasks and appended the decisions once, closed verified 12:39Z.
- **Step 4, the directory reader** (`internal/directory`, the `file` source; `people` and
  `person_ids` tables, a `person.*` event per change). Nothing reads a person yet; case 1's asker is
  the first lookup.
- **Step 5, live reads** (RFC-0014 §4.8, as built there; the order of work and decisions in
  [`rfc-0014-step5-plan.md`](../wip/rfc-0014-step5-plan.md)): a station's `reads:` (MCP servers,
  `tools: all`, caps per run and per call); the runtime's MCP client; every read recorded with the
  run (`input_reads`) and replayed to its shadows, which wait for the live run and fail on a read it
  never made; offline runs never go live; the `reads` worker and proxy (compose profile `reads`, off
  by default); the e2e's `reads` section against a stub MCP server. The protocol check against n8n's
  MCP Server Trigger passed (the homelab session). Not on the installation yet: case 1 is its first
  user (Google Docs and Google Tasks, the operator's choice).
- **Earned autonomy counts per plan** (RFC-0013's 2026-11-23 amendment): in a two-plan case, each
  plan's action is credited or debited from its own outcome, not the case's.
- **The UI pass** ([`ui-unify-2026-11-23.md`](../wip/ui-unify-2026-11-23.md),
  [`ui-evolution-2026-11-23.md`](../wip/ui-evolution-2026-11-23.md)): every page leads with what the
  operator acts on; tables sort, filter and page alike; the ledger explorer (Record → Ledger); the
  approve buttons say "Approve this plan"; every page rendered from a copy of the installation's store
  and screenshotted at desktop and phone width, then checked live through a viewer login. Hosts says
  "not collected" for a host without load metrics.
- **GlitchTip noise** (the operator, 2026-11-24):
  - 761: the contract's unknown-model check reads the key's `/models` list instead of calling a model
    that does not exist (the gateway reported every refusal);
  - 944: a remote worker refused for its version logs at ERROR once per version pair;
  - 943: a remote worker marked `intermittent` (the laptop's) raises no host alert for
    `not_connected`;
  - 941: on shutdown the host endpoint closes its links and waits for its handlers before the store
    closes ("sql: database is closed" at every restart).

## flotillad's slowness, and the read pool

The pairs call took 9 to 13 s live against 83 ms natively on a copy of the same store. The first
suspect, every read queued behind the writer's one connection, was wrong: a read pool (3904abb) was
deployed and the call still took 11 to 13 s, with the store's waits under 0.2 s. The wait metric it
added ruled the store out. The cause was the mini's VM: its kernel had marked the tsc clock unstable
on 11-21 and fallen back to the emulated hpet, so every clock read exited to QEMU. The homelab
session set `tsc=reliable` and restarted the VM on 2026-11-24: VM load 42 to 2, the pairs call 1.2 s.

The read pool then had nothing left to serve and exposed check-then-act races that the one
connection had serialised. CI failed on two of them in a row: a snapshot deleted twice (a real race,
fixed with a mutex, 3c06e93) and a case escalated in a dispatch test that never reproduced locally.
The operator chose to revert the pool (7ee5e53, 2026-11-25); `Store.EndRun` (a run's end and its
spend in one transaction), the 404 on cancelling an ended run and the wait metric stay. The pool can
return once these races are found and fixed and the metric shows reads queueing.

## Fixed on the way

- The MCP client restarts a half-started session on the next call.
- The e2e's browser check finds the backup chain's case through the cases filter, since tables page.
- A case card wraps a long unbroken word (the live Now page scrolled sideways on a phone).
- The e2e's revoked-host check waits as long as the agent's backoff can make its next try (100 s).

## Deploy

Live: sha-1d76a58 on 2026-11-23 (the UI passes, the directory, live reads off, the read pool), then
sha-7ee5e53 on 2026-11-25 at 13:58Z, CI `VERDICT GREEN — success 7`: the GlitchTip fixes, the snapshot
race fix, the read pool reverted. Its second deploy put `intermittent: true` on the laptop's four
remote workers in flotillad.yaml (the old flotillad refused the field); `-check` lists them as
intermittent. The laptop's workers followed (a first pull timed out on ghcr.io, a retry passed), and
all four were accepted again at 14:00:41Z.

GlitchTip across the two restarts, against the counts before: 941 (closed store) and 943 (host
alert) had no new events. 944 had 8 (one per worker per restart, while the laptop still ran the
old version), where it used to log one on every reconnect: 3,592 in all. 761 waits for the contract
check's next daily run. The pairs call takes 1.8 to 2.1 s with the pool gone, against 1.2 s with it.

## Open threads

- **Case 1, the second brain** (a question answered from the operator's notes, RFC-0014): the first
  user of live reads and of the directory. Needs the reads worker on the installation (its MCP
  servers in n8n, the worker's secrets, `reads` in `COMPOSE_PROFILES`).
- **Then** case 2, the visual plane, the wall screen (RFC-0014 §8).
- `plumbing.reply` cannot earn auto until its remaining gates exist (the asker in the directory, a
  citation matching a recorded read, the thread from the case, a retracted answer counted as failed).
- The dispatch test failure on 3c06e93 (`TestExecuteRunsOnceAndMovesToChecking`, a case escalated
  60 ms after it opened) is not explained; if it recurs with the pool gone, it is a race of its own.
- The live state of this build: [`docs/wip/rfc-0014-build-state.md`](../wip/rfc-0014-build-state.md).
