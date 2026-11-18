# ADR-0019 — Durable runs: a checkpoint, and a resume flotillad allows

**Status:** Accepted (2026-11-18). Builds on [RFC-0013](../rfc/RFC-0013-bugfix-clarifier.md) §2 (the
case's shared limit), [ADR-0004](ADR-0004-job-runtime.md) (the runtime's recording) and the run
lifecycle in [A case, end to end](../architecture/run-lifecycle.md).

## Context

A bugfix run takes minutes to an hour and spends money in model calls. Until now every way a run
could be cut short threw all of it away: `bugfix/fix.py` deleted its directory on the scratch volume
whatever happened, and the case's next run started from the verdict again.

What a cut-short run gets as a status today (read in the code, 2026-11-18):

| What happened | The run's status | Where |
| --- | --- | --- |
| flotillad restarts while the run is in flight | `interrupted`, detail "flotillad restarted while it ran" | `store.MarkInterrupted`, at flotillad's start |
| the worker (local) restarts or crashes mid-run | `error`: the HTTP call fails with a reset or EOF, which is not a refused dial | `worker.Client.Run`, then `sched.RunWithin` |
| the link to a remote worker drops mid-run | nothing yet: `RunRemote` waits across reconnects, and the host agent's link keeps an unsent result and delivers it on the next connection; a run that finished in the gap ends with its own status | `hostlink.RunRemote`, `hostagent.Link` |
| the remote worker's process restarts, so its result never comes | `error`, after the run's timeout and `RunGrace` ("no result from remote worker ... in time"), or at once when the link reports the failed run | `hostlink.RunRemote` |
| the worker cannot be reached before the run starts | `skipped` (a deploy recreating the worker) | `sched.RunWithin`, `ErrUnreachable` |
| the run passes its timeout | `timeout` | the worker |
| a STOP or a cancel | `cancelled` | the worker |

So both of the cases the operator named end as `error` (or, for flotillad's own restart,
`interrupted`); none is a new status, and none had a way back.

## Decision

1. **A checkpoint per case, on the scratch volume.** `bugfix/fix.py` works in `runs/<case id>/` (one
   directory per case, not per run). After each boundary it writes `checkpoint.json` atomically
   (written beside, flushed, renamed over, the directory flushed):

   | Phase | Written | A resume from it |
   | --- | --- | --- |
   | `checkout` | after the checkout and step one | asks for the verdict |
   | `attempt` | before each fix episode, with the loop's state | continues pi's session if it grew, with the tree as left; else a fresh episode |
   | `fixed` | pi's episode ended, the gates have not run | puts the tree back from the saved patch and runs the gates: no new episode |
   | `gated` | the gates ended | goes on to the review (or, for a failed gate, the kick-back, which asks the advisor and the verdict again) |
   | `reviewed` | the review ended | goes on from the review's text: no new review call |

   It holds the schema version, the case id, the repo and ref, the base sha, the phase and round,
   the pins, the verdict and manifest paths (relative to the directory), the review kick-backs
   spent, the last clean fix the review sent back, the notes so far, and the resume tuple of the
   next episode (session path, patch path, feedback). The kick-back evidence and the advisor's pin
   are not a boundary of their own: a kill there redoes them from `gated`.

2. **flotillad decides whether a run may resume; the checkpoint decides whether it can.** A spec
   declares `resume: {on: [interrupted, error], max: 1}` on a routine that acts on a case. When
   flotillad dispatches the case, it hands the run `FLOTILLA_RESUME={"run_id": ..., "status": ...}`
   only if the case's latest run of that routine ended with one of the listed statuses and fewer
   than `max` resumes were handed out since the case's last state change. An escalation, a reassign
   or a stage change ends that chain: the next run is fresh. flotillad records the run on the case's
   timeline (`run.started`, with the run id) and a resume as `run.resumed` (naming the prior run and
   its status): the store has no run-to-case link, and ADR-0019 adds no schema step, so the events
   are the record.

3. **`fix.py` restores only with the word and a checkpoint that holds.** It resumes when
   `FLOTILLA_RESUME` is set and the checkpoint's case, repo and ref match, its worktree is at the
   base it names, it is younger than 24 h, its fields have the types the loader expects, and every
   path in it stays inside the run directory once symlinks are resolved. Anything else starts the
   case fresh: the directory and its venv are deleted first. A resumed run does not check out or
   install again; it takes the worktree, the venv and pi's sessions where they are.

4. **The directory is deleted unless a resume could use it.** It goes when the run ends with a plan
   or an escalation (a case-limit escalation included), when its own timeout ended it (status
   `timeout`, which no routine resumes: the worker tells the run its deadline in
   `FLOTILLA_RUN_DEADLINE`, and a SIGTERM within 5 s of it is the timeout's), and when no checkpoint
   was written yet. A run cut short by a restart, a crash or an error, with a checkpoint, leaves it.
   A cancel's SIGTERM cannot be told from a restart's, so a cancelled run leaves its directory too.
   Anything left is swept when older than 24 h (any run, at its start; a directory whose lock is held
   is a live run's and is skipped). A directory that cannot be deleted whole (a read-only module
   cache) is made writable and deleted again; if something still stays, a fresh run ends with a plain
   error naming it, not a `FileExistsError` on every retry.

   **One run of a case at a time.** `fix.py` holds an exclusive `flock` on `runs/<case id>/.lock` for
   the whole run, taken before anything else and released on every way out (the fd closes with the
   process). A run that cannot get it within 120 s (`--lock-wait`) ends as an error, exit code 75,
   with "another run of this case is still running", and has touched neither the checkpoint nor the
   directory (only the empty lock file may be new). The waiter checks the file it locked is still the
   one at the path: the holder deletes the directory when it ends normally. flotillad treats a run
   that ended with exit 75 as it treats a skipped one: it never started its work, so it is not the
   run the next one resumes from and its resume is not spent (`max` counts a chain's resumes, not
   attempts). After the backoff, dispatch tries again and resumes the same checkpoint.

5. **A resumed run reports its own work, and what the earlier run never reported.** Calls the earlier
   run recorded are not recorded again; the session the earlier run was killed in is recorded whole,
   since the killed run never wrote it. Its spend is the gateway's bill from its own start, plus a
   carry: pi's figure for the sessions already in the directory less the spend flotillad has on
   record for the earlier run (`spend_usd` in `FLOTILLA_RESUME`; 0 for an interrupted run, which
   reported none), never below 0. The baseline is read before the chain is built, so a run whose
   chain cannot start does not count the earlier sessions again. The calls file has a step row
   "resumed from checkpoint: phase X, round n, saved T", and the plan's detail says so.

6. **The case's limit still bounds everything it can see** (RFC-0013 §2). A resumed run is a run: its
   spend and time add to the case, a case past its limit is escalated and not resumed, and the run's
   timeout is what the case has left. What the limit sees of a cut-short run:

   | | Time | Money |
   | --- | --- | --- |
   | run ended with a result (`error`, `timeout`, `cancelled`) | the worker's duration | the spend the run reported |
   | run lost with its call (a reset, a SIGKILL at the worker) | wall clock from its start to the lost call | nothing: no spend file was read |
   | run interrupted by a flotillad restart | charged at flotillad's start: start to now, on the case of its `run.started` event | not at that point: the resumed run reports it (the carry above), so the limit sees it when that run ends |

   A run that ended busy (exit 75) is charged no time. Not counted: a run of a routine without
   `resume` that a restart interrupts (no `run.started` event links it to a case), and the money of
   an interrupted run until its successor has ended.

7. **A flotillad restart stops what it orphaned and hands the cases back at once.** At start, before
   `MarkInterrupted`, flotillad reads the runs still `running`. After the workers and the host links
   are set up and before anything dispatches, it asks each run's worker to cancel it, as an
   operator's cancel does (SIGTERM, SIGKILL after the grace), records a `run.cancel` event with actor
   `flotillad` for each (delivered, queued or not delivered, with why), and frees the leases those
   routines hold on cases in `handed_over`, so dispatch starts the next run, a resume where the
   routine declares one, in its next pass. `cases.holder` is the routine's id (`fleet/routine`), set
   by `LeaseCase` for a lease of `lease_until`. A case in `executing` is never freed (the action must
   not run twice, RFC-0004 §3.5; `ExpireLeases` keeps the same exclusion), and a case in `checking`
   or any other state keeps its lease until `ExpireLeases`. A local worker is asked directly (`POST
   /v1/runs/<id>/cancel`; a worker that no longer has the run answers "no such run", which is
   fine). A remote worker is asked through its link, with the same `cancel` frame with `stop` that
   an operator's cancel uses, but not through the old path: that one needs the run in the link's
   in-memory table, which died with the old flotillad. The link's `CancelOrphan` sends the frame for
   a run it never saw, and when the worker's link is not connected yet it remembers the run and
   sends the frame when the link connects. That memory is in flotillad's process only: a second
   restart before the link connects loses it, and the remote run then ends by its own timeout and
   grace, as before. The cancel is best effort: a refused or failed one is logged and in the event,
   and the case's next run waits for the lock (decision 4), so a cancel that arrives late costs a
   wait, not a shared worktree. This applies to every routine, `resume` or not (see the
   consequences).

## Consequences

- Positive: a flotillad restart or a worker crash in the middle of a fix costs the work since the
  last boundary, not the whole run; a kill during the gates (minutes of tests) costs the gates only.
- Positive: the decision to resume is flotillad's, in the spec and on the case's timeline, not a
  guess by the routine from what it finds on disk.
- Negative: a case's directory, with its worktree and venv, stays on the scratch volume for up to 24
  h after a run cut short by a restart, a crash, an error or a cancel. The sweep bounds it; a timeout,
  an escalation and a run with no checkpoint delete theirs at once.
- Positive: after a flotillad restart the case is handed over at once (decision 7), not after the
  lease's run-start-plus-limit-plus-2-minutes (up to 3 hours for bugfix/fix).
- Behaviour change for every routine: a restart now cancels its orphaned runs, `resume` or not.
  Before, such a run went on at its worker to its end or timeout, with its result discarded, because
  flotillad's `RunWithin` that waited for it was gone. For triage/cycle that means a cycle caught by
  a restart is stopped, where it used to finish unseen: its ledger writes and issue filings up to
  the kill stand, the rest do not happen, the run is `interrupted` and the next heartbeat's run
  starts a fresh cycle (it has no case and no resume); the freshness clock is the same, since an
  interrupted run was never an ok one. Leases of such routines on handed-over cases are freed too:
  their next run starts at once, where it waited out the lease. Not freed, as before: executing
  cases.
- Negative: the money of an interrupted run is only known to the limit after its successor ends,
  and a routine without `resume` that a restart interrupts adds nothing to its case (decision 6).
- Negative: a remote worker's cancel that is queued for a link that has not connected is lost on a
  second restart (decision 7).
- Not done: a sweep at worker start. The sweep is `fix.py`'s, bugfix knowledge the generic worker
  does not have; the next fix run's sweep and the delete-unless-resumable rule bound the volume.
- Neutral: a case that is resumed fixes the base it began with, not main's newest commit. The plan
  records the base, and the PR's CI is the whole suite.
- Security: the repo's test code runs in the worktree with the run directory as `HOME`, so it can
  write the checkpoint. That is no new reach: it already controls everything in the directory for
  the run's length. A resume only continues the same case's chain; the loader checks types and
  paths, the base must equal the worktree's HEAD, and the bytes that reach pi are the case's own
  session, patch and feedback. Directories are per case, so one case's code cannot plant what
  another case installs.
- Not verified: the continued pi session ("the run was interrupted; the worktree is as you left it;
  continue") is the same `--session` mechanism a kick-back already uses, tested here with a fake pi;
  a real pi session cut mid-write has not been resumed. `fix.py` drops a last line a kill cut short.

## Alternatives considered

- **Resume by what is on disk, with no word from flotillad.** Rejected: a reassigned or escalated
  case would pick up a stale checkpoint, and the decision would be invisible on the case.
- **A `runs.resumed_from` column, or a run-to-case table.** Rejected: a schema step for what the
  case's events already record.
- **Checkpoint by committing to a branch.** Rejected: nothing may leave the worker before a person
  approves it.
- **Resume on `timeout` too.** Not now: a run that ran out of time would run out again from a
  checkpoint of its own pace; `on` lists only what is not the run's own fault.

## References

- [A case, end to end](../architecture/run-lifecycle.md), the failure table.
- [RFC-0013](../rfc/RFC-0013-bugfix-clarifier.md) §2, the shared case limit.
- `bugfix/fix.py` (`save`, `load_checkpoint`, `rounds`, `lock_run_dir`), `internal/dispatch/dispatch.go`
  (`resumption`), `internal/sched/sched.go` (`recordCaseRun`), `internal/sched/recover.go`
  (`RecoverInterrupted`), `internal/hostlink/workers.go` (`CancelOrphan`), `internal/store/cases.go`
  (`FreeHandedOverLeases`), `internal/spec/spec.go` (`Resume`).
