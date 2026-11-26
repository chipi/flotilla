# ADR-0022 — The no-effect check runs only for the code it guards

**Status:** Accepted (2026-11-26, the operator). Changes when the no-effect workflow of
[docs/wip/v2-design.md](../wip/v2-design.md) section 4 runs, not what it proves.

## Context

The no-effect check proves one property: a shadow version changes nothing outside its own record.
It runs the e2e's no-effect sections (about 12 minutes on the hosted runner), then five mutants,
each of which puts one fault in and must turn the run red. Each mutant builds the stack again
(about 8 minutes each), so one run takes about 50 minutes.

It ran on every push to main and every PR that was not docs only, and a deploy waited for it. A
change to a test file, an example fleet, a fleet's own code or the UI cannot touch the shadow's
isolation, but it still cost 50 minutes before the mini would deploy it. On 2026-11-26 a commit
that only replaced example paper names in one test file waited on it. The operator ruled that this
is wasted time, and chose to run the check only for the code it guards.

## Decision

1. **The workflow runs only for the guarded paths** (`paths:` on push and pull_request, the same
   list on both):
   - the scheduler, store, spec, versions, worker client, run-token, dispatch, API, cases and chat
     connector packages;
   - `cmd/flotillad`, `worker/`, `runtime/` and `plumbing/`;
   - compose, the images and the e2e stack;
   - the workflow itself.

   Test files (`*_test.go`, `test_*.py`) never run it. Fleets, fleet code, the CLI, the host agent,
   the UI and docs do not run it.
2. **The mutants run only when what they break changed:**
   - the files the mutants alter, and the scripts that put them in;
   - the list is `deploy/e2e/mutant-paths.txt`;
   - a manual run, or a push with no diff base, always runs them.

   Any other guarded change runs the sections alone, one build.
3. **A deploy of a commit with no run is judged by the nearest ancestor that has one.**
   `deploy/server/checks_green.sh` walks back up to 29 commits for an optional check (`e2e?`,
   `no-effect?`) that the commit itself has no run of, and requires that run green (or finished).
   Before, a missing run was accepted as is. With a path filter, a later unrelated commit would then
   have hidden an earlier red one. The same rule now covers e2e's docs-only skips.

`deploy/server/test_checks_green.py` keeps the push and PR lists equal. It also checks that every
file a no-effect mutant alters (`internal/sched/shadow.go`, `runtime/harness.py`) runs the mutants,
and that the nearest-ancestor rule refuses an earlier red run.

## Consequences

- **The 16 commits on main up to fbf0d16:**
  - 7 run nothing;
  - 6 run the sections alone (about 12 minutes);
  - 3 run everything (about 50 minutes).

  That is about 222 minutes of runner time in place of about 500.
- **A mistake in the path lists lets a change through unchecked:** code that matters to a shadow's
  isolation, but sits outside the lists, does not run the check. The lists name packages rather
  than files, so a new file in a guarded package is covered. A new package is not covered, and
  must be added when it takes part in shadow runs.
- **A change to `deploy/e2e/run.sh` runs the sections but not the mutants.** The mutants' own
  plumbing in that script is tested only when the shadow code, the workflow or the mutant list
  changes, or on a manual run.
- **A deploy reads up to 29 more commits' check runs from GitHub** when the commit has no run of an
  optional check: a few seconds.

## Rollback

Revert this commit: the workflow's `paths-ignore` (docs only) and the old `checks_green.sh` come
back together. To run everything on a commit without reverting, start the workflow by hand
(`workflow_dispatch`).
