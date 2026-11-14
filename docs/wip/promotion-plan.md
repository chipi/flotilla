# Promotion plan: the live triage fleet onto main and the API ledger

→ becomes part of the phase 2 history entry once it is done.

**Status:** planned for the morning of 2026-11-14, re-evaluated then (operator, 2026-11-13). Nothing
here runs without the operator's go on the day.

## What moves

The live routine `triage/cycle` goes from `sha-9048e6f` (the deploy branch `deploy/9048e6f`) to main
`35b04d2`. Main holds:

- RFC-0002 §9 steps 2 to 5: the model client, the playbook and skills, the shared harness, and the
  ledger client;
- the dismissal re-triage (`ea6102a`);
- the filing fixes `88ccea5`, `81a460f` and `35b04d2`. `filed.tsv` gains a 9th column,
  `linked_via`; the fleet session was told on 2026-11-13.

The ledger backend goes from `tsv` to `api`. No store schema changes between `9048e6f` and
`35b04d2` (no store or migration files in the diff), so a rollback to `9048e6f` stays possible.

## State on 2026-11-13

- **The candidate.** `sha-b180293` runs as `triage/cycle-shadow` on `api` since 09:07Z. Its
  namespace was imported and checked (576 filings, 1,746 decisions, 11,962 recurrences). By 11:03Z it
  had made 12 runs, all ok, and written 1 decision to the store; `LEDGER CHECK: PASS`.
- **No filing path in shadow.** A `file` decision there only queues a proposal. The filing path
  through the store first runs live, at this promotion. Until then it is proven only by
  `gate_ledger_backend_replay.py` against a real flotillad.
- **Candidate against live.** Since its seed, 55 decisions each; 50 alike and 5 different. In all 5
  the candidate's first request differed because a probe read another value at its moment, not
  because of the code (the fleet session's exact replay: 5 of 5, and 128 of 128).
- **The switch review.** 231 live cycles since 2026-11-11 18:45Z, 1 error (during the
  `sha-9048e6f` deploy). `DEPLOY-VERIFY: PASS`. The fleet session's filing gate shows no failure.
  Its weekly freeze and weekly check first run on Saturday 2026-11-14.

## The morning of 2026-11-14: re-evaluate, then decide

1. CI on `35b04d2` is `VERDICT GREEN`, and its images are on ghcr (the `prod-deploy` preflight).
2. The candidate has more decisions on the store. `ledger_sync.py check` for `triage/cycle-shadow`
   passes, and its log has 0 ledger errors.
3. Candidate against live, counts only, with main's `deploy/mini/shadow_compare.py`. Not the deploy
   checkout's copy, which prints test-side items (SPLIT.md rule 7; 2026-11-13).
4. The fleet session's weekly freeze (LaunchDaemon `com.homelab.fleet-weekly-freeze`, Saturdays
   10:17Z; a week's window takes longer than the 7 min 43 s of its first one-day run). Start the
   window only after `~/signal-fleet/frozen/reports/weekly-freeze.log` ends with
   `WEEKLY FREEZE: PASS`.
5. The fleet session reruns its filing gate preview (9048e6f → 35b04d2, counts only) on the new
   week and sends the verdict. On the week of 11-06 it was `FILING GATE: PASS`, so no
   `SF_FILING_GATE_ACCEPT` was needed. Its counts-only gate (fleet-eval-data ba11f6d) has to be in the
   mini's `~/fleet-eval-data` before the deploy, because the new week is test side. That needs the
   operator's go for the fleet session.
6. **The `filed.tsv` header** (found 2026-11-13): main's `FILED_COLS` has a 9th column, `linked_via`.
   `deploy/compose/README.md` says an older file keeps its 8-column header until a filing rewrites it,
   and `import` refuses it until `ledger_sync.py pad` has run. **Decided and rehearsed
   2026-11-14** (homelab session; reviewed by the fleet session): the live worker's image
   `sha-9048e6f` has no `ledger_sync.py` at all (not in commit `9048e6f`'s tree either), so
   "import from the live worker" could not run. The window's step 2 now runs pad, import and check
   with a one-off `sha-35b04d2` worker container on the live volume while flotillad is still
   `sha-9048e6f`, and checks again after the deploy. Rehearsed on the mini's dev engine, on
   `docker cp` copies of the candidate's state (576 filings) and then of the live worker's state
   (590 filings, 13,753 disposition rows): `import` before `pad` refused (`IMPORT REFUSED: a
   ledger's header is not the current one`); `PAD: 590 rows, 9 columns (header updated): OK`;
   `imported into triage/cycle: 590 filings, 1777 decisions, 11976 recurrences`; `LEDGER CHECK
   (triage/cycle): PASS` against flotillad `sha-9048e6f` and again against `sha-35b04d2`, both TSVs
   byte-identical. Rollback after `pad` still works: `9048e6f`'s filing reads the file by position
   (`zip` over 8 columns) and writes 8 columns, dropping `linked_via`, which is empty until the first
   linked filing. Not rehearsed: the live volume itself, the `.env` switch and a live cycle.

## The window (operator's go; about 15 minutes)

1. `flotilla pause triage/cycle`, with the operator's token. Wait until no run is in flight.
2. Pad, import and check the live namespace with the new worker image, one-off, on the live
   volume (the live image has no `ledger_sync.py`; step 6 above), in this order, each with:
   `docker run --rm --memory 512m --network flotilla_flotilla -v flotilla-triage-state:/home/worker/signal-fleet
   -v flotilla-worker-triage-secrets:/run/flotilla:ro -w /opt/flotilla/signal-fleet/mvp
   -e FLOTILLA_API_URL=http://flotillad:8485 -e FLOTILLA_API_TOKEN_FILE=/run/flotilla/worker.token
   -e NO_PROXY=flotillad -e FLOTILLA_ROUTINE=triage/cycle ghcr.io/chipi/flotilla-worker-triage:sha-35b04d2
   python3 ledger_sync.py pad` (then `import`, then `check`). Expect `PAD: N rows, 9 columns (header
   updated): OK`, then `imported into triage/cycle: …`. **Stop on anything but `LEDGER CHECK
   (triage/cycle): PASS`.**
3. In `.env`, set `TRIAGE_LEDGER_BACKEND=api`; the old `.env` is kept beside it.
4. `deploy.sh sha-35b04d2`. If the fleet session's preview on the new week flags a change, set
   `SF_FILING_GATE_ACCEPT` with the reason: the filing replays pass, and the fleet session's
   `test_exact_link` passes 4/4. Tell the fleet session before this step.
5. Before resuming, `check` again, now from the live worker (it runs `sha-35b04d2`): `docker exec`
   with the same `-w` and `-e` flags as step 2, `flotilla-worker-triage python3 ledger_sync.py check`.
   Then `flotilla resume triage/cycle`. Watch the first cycles. Then run `check` for `triage/cycle`,
   and
   `deploy-verify compare --project flotilla --release sha-35b04d2`.
6. End the candidate: put `triage/cycle-shadow` back in `disabled_routines`, drop
   `COMPOSE_PROFILES=candidate`, and deploy again. Its volume stays until it is removed by hand.
7. Tell the fleet session: the live fleet runs on `api`, and its first API-mode filing records follow
   from its next filings.

## Rollback (about 5 minutes)

Set `TRIAGE_LEDGER_BACKEND=tsv`, then `deploy.sh --rollback` (back to `sha-9048e6f`). Both TSVs are
written on `api` as well, so the files are complete. The store's namespace stays, and nothing reads
it on `tsv`.

## After

- Delete the `deploy/9048e6f` branch.
- Saturday's freeze and weekly check run on the new code.
- Update phase-2-plan 2.8 and phase-3-plan step 5, and write the history entry.
