# 0008 — RFC-0014 steps 1 and 2: the hook and plumbing.call (2026-11-22)

**Date:** 2026-11-22
**Operator:** Marko Dragoljevic
**Agent:** Claude Code (Opus 5.5), the flotilla session
**Outcome:** RFC-0014 was written, reviewed twice and decided in one day. Its first two build steps
landed: the hook (live on the installation since sha-52bfbb9) and `plumbing.call` with its example
fleet and worker (on `main`, not deployed). Follows [0007](0007-folder-move-rerecord.md).

## What the operator pointed at

The visual plane and mashups: where flotilla stops, how work comes in from and goes out to the
installation's other tools, and how a person reads it live. The answer is
[RFC-0014](../rfc/RFC-0014-visual-plane-and-mashups.md): records in their own tools, a plumbing tool
(n8n on the installation; flotilla takes no dependency on it) carrying events in and posts out, and
flotilla deciding in between. Its build order is §8.

## What landed

- **The RFC, drafts 1 to 4 and two reviews.** Decided by the operator on 2026-11-22: n8n as the
  installation's plumbing; people from a file in host config where no directory service exists (the
  installation has none); a chat post is reversible because the executor can take it back; `risk:
  high` caps at propose; the visual plane's look, spend and history (§6).
- **Step 1, the hook** (`internal/hook`, `hooks:` in `flotillad.yaml`, `POST /hooks/{name}` on
  `hooks_listen`, a listener of its own): each hook opens one case of its declared kind, signed with
  HMAC-SHA256, replay-safe. flotillad alone joins the plumbing tool's network. A smoke hook closes its
  own case for the plumbing's smoke-test flow. Live since sha-52bfbb9; the installation's n8n ran its
  smoke flow against it (202, then 200 on the replay, the case closed `resolved_itself`).
- **Step 2, `plumbing.call`:**
  - five registry actions (`plumbing.create_tasks`, `post_decisions`, `reply`, `post`, `retract`),
    each `executor: routine`, checked by `plumbing_confirmed`;
  - `plumbing/call.py`, the shared sender: plan hash, payload shape, the chat text's gates (a label,
    no crowd call), a signed post with delivery id `plan-<hash16>`, no retry; its check asks the
    callback for the delivery's status. One signing vector is shared with `internal/hook`;
  - `fleets/plumbing/`: five send routines and a check on worker `plumbing` (the ops image, compose
    profile `plumbing`), whose proxy `egress-plumbing` is the only other flotilla service on the
    plumbing network;
  - the e2e's `hooks` and `plumbing` sections, against a stub plumbing that signs and verifies.
- **ADR-0017 amended** twice on 2026-11-22: what `reversible` means, `risk: high`, a check that asks
  the system it wrote to; then one executing routine per action, a load error otherwise (the
  dispatcher took the first match in load order).

## Fixed on the way

- Schema 35, an index on `ledger_entries(run_id, kind, seq)`: the pairs page's live side scanned the
  whole ledger per input.
- A rotated hook secret now recreates flotillad on deploy (its config hash takes the hook secrets).
- The e2e's `upgrade_start` no longer leaks a container; the no-effect matrix waits for the live side
  to settle before its "before".
- Deploy: `compose up` is retried when the engine drops mid-up, and a pass is the new tag's flotillad
  healthy, not the log's last line.

## Open threads

- **Step 3, case 3** (a meeting to its follow-ups): the example fleet's acting routine, its n8n flows
  on the installation, its acceptance (§7). `e2e/meeting-tasks` is the stand-in it replaces.
- Running `plumbing.call` on the installation needs the operator's setup: the n8n flows per callback,
  the worker's token and the callbacks' secrets, `worker-plumbing.env`, the proxy's ACL and
  `smokescreen-plumbing.yaml`, `plumbing` in `COMPOSE_PROFILES`.
- `plumbing.reply` cannot earn auto until steps 4 and 5 and case 1 add its remaining gates.
- flotillad in its container spends 5 to 10 times the CPU it needs natively; unexplained, and a CPU
  profile needs the operator's go for an opt-in pprof listener.
- The live state of this build: [`docs/wip/rfc-0014-build-state.md`](../wip/rfc-0014-build-state.md).
