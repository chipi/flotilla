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

4. **The directory is deleted when the run ends normally** (a plan or an escalation), and swept when
   it is older than 24 h (any run, at its start). A run that is cut short (a signal, an error, a
   crash) leaves it.

5. **A resumed run reports only its own work.** Calls the earlier run recorded are not recorded
   again; the session the earlier run was killed in is recorded whole, since the killed run never
   wrote it. Its spend is the gateway's bill from its own start, and pi's fallback figure leaves out
   what was already in the directory. The calls file has a step row "resumed from checkpoint: phase
   X, round n, saved T", and the plan's detail says so.

6. **The case's limit still bounds everything** (RFC-0013 §2). A resumed run is a run: its spend and
   time add to the case, a case past its limit is escalated and not resumed, and the run's timeout
   is what the case has left.

## Consequences

- Positive: a flotillad restart or a worker crash in the middle of a fix costs the work since the
  last boundary, not the whole run; a kill during the gates (minutes of tests) costs the gates only.
- Positive: the decision to resume is flotillad's, in the spec and on the case's timeline, not a
  guess by the routine from what it finds on disk.
- Negative: a case's directory, with its worktree and venv, stays on the scratch volume for up to 24
  h after a cut-short run. The sweep bounds it.
- Negative: after a flotillad restart the interrupted run's case is still leased to its routine, and
  the lease ends at the run's start plus the routine's limit and 2 minutes (`store.ExpireLeases`),
  so the resume starts then, up to 3 hours later for bugfix/fix, and before the 24 h handed-over
  escalation. Releasing those leases at flotillad's start would make it immediate; this ADR does not
  change leases.
- Negative: the killed run's own spend is not always reported: after a SIGKILL the worker reads no
  spend file, so the case's limit sees none of it. The limit counts what runs report (RFC-0013 §2),
  as before.
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
- `bugfix/fix.py` (`save`, `load_checkpoint`, `rounds`), `internal/dispatch/dispatch.go`
  (`resumption`), `internal/sched/sched.go` (`recordCaseRun`), `internal/spec/spec.go` (`Resume`).
