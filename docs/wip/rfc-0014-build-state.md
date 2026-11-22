# RFC-0014 build: where it stands (2026-11-22)

The hand-off for the next session on RFC-0014 (the visual plane and mashups). The order of work is
§8; this note says which steps are done, what is open, and what needs the operator.

## Done

- **Step 1, the hook** (`hooks_listen`, `POST /hooks/{name}`, signed, `internal/hook`): live on the
  installation since sha-52bfbb9. The installation's plumbing (n8n) ran its smoke flow against it:
  202 on the first send, 200 on the replay, the smoke case closed `resolved_itself`.
- **Step 2, `plumbing.call`** (main at 6c59125; not deployed, and nothing on the installation runs it):
  - `risk: high` caps at propose (policy and `MaxAutonomy`); ADR-0017 amended (reversible means the
    executor can take it back; a check may ask the system it wrote to).
  - five registry actions (`plumbing.create_tasks`, `post_decisions`, `reply`, `post`, `retract`),
    each `executor: routine`, checked by `plumbing_confirmed`.
  - `plumbing/call.py`, the shared sender: plan hash, payload shape, chat text gates (label, no
    crowd call), signed post (delivery `plan-<hash16>`), no retry; `check` asks the callback for the
    delivery's status. A signing vector shared with `internal/hook` keeps both sides together.
  - `fleets/plumbing/` (five send routines and a check) on worker `plumbing` (the ops image, compose
    profile `plumbing`), its proxy `egress-plumbing` the only other service of flotilla's on the
    plumbing network. Setup: `deploy/compose/README.md`, "The plumbing worker".
  - the e2e's `plumbing` section: the hooks section's meeting case planned, approved, posted to a
    stub plumbing that verifies each signature, closed by the check; the proxy refuses other hosts.
- **Fixed on the way:** the pairs page's live side used a whole-ledger scan per input (schema 35,
  `ledger_entries_run`); a rotated hook secret now recreates flotillad on deploy; the e2e's
  `upgrade_start` no longer leaks a container; the no-effect matrix waits for the live side to settle
  before its "before" (a live plan landing in the window read as a shadow's).

## Next, in §8's order

1. **Step 3, case 3 (a meeting to its follow-ups):** the example fleet (the meeting's acting routine
   that plans `plumbing.create_tasks` and `post_decisions` from the notes, as one proposed task list
   in the meeting's own case, B3 option c), its n8n flows on the installation, and its acceptance
   (§7). The e2e's `e2e/meeting-tasks` is the stand-in to replace.
2. Step 4, the directory reader (`file` source first, the people file in host config).
3. Step 5, live reads (read-only MCP tools), then case 1, case 2, the visual plane, the wall screen.

## Open in the code

- ~~Nothing enforces one execute routine per action~~: a second one is a load error now, a disabled
  one included (`spec.Load`, `TestOneExecutorPerAction`, ADR-0017's 2026-11-22 amendment).
- `plumbing.reply` cannot earn auto until its remaining gates exist: the asker in the directory
  (step 4), a citation matching a recorded read (step 5), the thread from the case (case 1), and a
  retracted answer counted as a failed execution.
- ~~No `docs/history` entry for this arc yet~~: [0008](../history/0008-rfc-0014-hook-and-plumbing-call.md).
- flotillad in its container spends 5 to 10 times the CPU the same code needs natively (the pairs
  page: 4.5 s live against 0.15 s natively on the same store), and 12 to 14 % CPU idle; not
  explained. Finding it needs a CPU profile of the running process (an opt-in loopback pprof
  listener: the operator's call).

## Needs the operator

- To run plumbing.call on the installation: its n8n flows for each callback (each answers `send`
  with a `ref` and `status` with done or not, finding the delivery id it wrote into what it made),
  then the worker's token, the callbacks' secrets, `worker-plumbing.env`, the proxy's ACL and its
  `smokescreen-plumbing.yaml` with the plumbing network's own range (smokescreen refuses every
  private address outside `allow_ranges`), and `plumbing` in `COMPOSE_PROFILES`.
- Whether n8n may read the hooks' and callbacks' secrets from a read-only mounted folder (option b)
  instead of a credential pasted by hand for each.
- The opt-in pprof listener above.
