# ADR-0021 — The examples catalogue, and run tokens required by default

**Status:** Accepted (2026-11-19). Two decisions of v2 build step 5, one record because they ship in
the same release and one rollback lever covers the part that can hurt. Builds on
[ADR-0020](ADR-0020-routine-versions-stations-harnesses.md) (routine versions; the per-run token of
step 2) and [ADR-0001](ADR-0001-platform-foundations.md) (the example fleets are examples, and an
installation's own fleets are private).

## Context

Step 5 moved the example fleets to the v2 format (a folder per routine, `routine.yaml` plus
`v1/`), put triage's cycle on a station, and gave the e2e a no-effect matrix over them. Two
questions came out of it that are about the platform, not about one fleet.

1. **The examples are in the image, and an installation's fleets mount replaces them.** The image
   carries `fleets/` at `/opt/flotilla/fleets`; an installation bind-mounts its own folder over that
   path, and nothing of the examples survives. An installation could never see what flotilla ships
   (a routine to copy from, a trigger to read, the flow it draws) without reading the repository,
   and could not list them in its UI. The first idea, listing them in `disabled_routines`, loads
   them as routines.
2. **A worker's own token still opened the workers' listener.** Since step 2 every run has its own
   token (`FLOTILLA_API_TOKEN_FILE`, the run's id and routine behind it), but `run_tokens` defaulted
   to `accept`: the worker's token worked too, and a routine can read it from its own filesystem.
   The hostile shadow of the e2e proved the run token is confined; the default left the wider door
   open.

## Decision

1. **The examples are a read-only catalogue.** The flotillad image copies `fleets/` a second time to
   `/opt/flotilla/examples`, out of reach of the fleets mount (two `COPY` lines of one folder,
   pinned by a test, so the copies cannot drift). `examples_dir` in `flotillad.yaml` (unset by
   default: an installation loads none until it asks) names it. `spec.LoadExamples` loads it on its
   own, with its own validation and uniqueness, under the ids `example-<fleet>/<routine>`, and
   returns a `Catalogue`, not routines. The scheduler, the version reconciler, shadows, dispatch for
   cases, actions and checks, budgets, the brief, the metrics loop, and the worker and model-alias
   checks are never handed it: the type makes it a compile-time fact, and a test lists the only
   files that may read it. An example needs none of the installation's workers or model aliases, so
   an installation without `fix-flash` starts. The status API lists the examples (`flotilla status`
   prints them with the state `example`), the UI has an Examples page (description, trigger, worker,
   versions, the flow), and every API entry that names a routine refuses an example's id with 409.
   To run one, the operator copies its folder into the installation's own fleets repository under a
   name of its choosing and deploys with `deploy_fleets.sh`: the first version goes live through the
   existing flow. There is no in-place enable, because the files must stay what the image holds.
2. **`run_tokens: required` is the default.** A blank value means required: only a run's own token
   opens the workers' listener, and a worker's own token opens nothing. So that this cannot fail
   silently, capabilities are added fields: a worker's `/healthz` lists `caps: ["run_token"]`, an
   agent's worker hello lists `run_token` (`hostwire.CapRunToken`, an older hello still decodes).
   Under required, flotillad refuses to start, and `-check` refuses, while a reachable worker lacks
   the capability (naming it), and refuses at link a remote agent that lacks it; under accept
   nothing is refused and `-check` warns. The exact-version gate stays and refuses an older agent
   first when flotillad's version is set. `-check` prints one line per worker (`run-token capable`,
   `NOT run-token capable (update it)`, `not reachable`, `remote`).
3. **The callers that held the worker's token follow.** `ledger_sync.py import` and `check` and
   `gate_ledger_backend_replay.py` take `--token-file` and say so when they have none; `pad` is
   local and needs none. The compose files no longer give the workers the worker token as the
   routine ledger fallback. `gate_ledger_backend_replay.py` also writes rows, which no token outside
   a run may do under required (the operator's is import and read only, a run's is confined to its
   routine's namespace), so it is run against a flotillad in `accept`; the e2e does.

## Rollback lever

`run_tokens: accept` in `flotillad.yaml` restores the old behaviour for one release (a worker's own
token opens the listener again; no capability is checked; `-check` warns). It needs a flotillad
restart and nothing else: no store change, no file move. The catalogue has no lever to pull: unset
`examples_dir` and it is gone. Neither is in the schema (still 32).

## Consequences

Positive:

- An installation sees the examples in its own UI without loading, scheduling or paying for any.
- A routine that reads the worker's token file gets nothing from it.
- An old worker or agent is refused where it can be fixed, at the deploy, not discovered as a 401
  in a routine's log.

Negative:

- By hand inside a worker, `ledger_sync` and the replay no longer work with the worker's own
  token: an operator token file is needed (`--token-file`), and the replay needs `accept`.
- Every remote host agent and remote worker must be updated before flotillad, or the link is
  refused.
- The flotillad image holds the examples twice (the worker images do not carry the second copy).
- The default flips for an installation that set nothing; its `-check` shows it.

## Alternatives considered

- **List the examples in `disabled_routines`.** Rejected: about 40 ids in the installation's config
  that drift as the examples do; the model-alias check still runs for them, so an installation
  without the example aliases fails to start; their uniqueness and chain checks run across the
  examples and the installation's routines (both want `code_bug` acting); an unknown id is fatal.
- **A fleet-level switch.** Rejected: none exists, and adding one still loads the routines into the
  scheduler's map; "disabled" would remain a flag every consumer must remember to read.
- **Mount the examples read-only at a second path of the installation's choosing.** Rejected: every
  installation would have to do it; the image already knows where the examples are.
- **Keep `accept` as the default and make `required` the opt-in.** Rejected: the door stays open
  for everyone who does not read the release notes; the capabilities make the flip safe to ship.
- **Let the operator's token write rows for `gate_ledger_backend_replay.py`.** Rejected here: it
  widens `ActLedgerSync` (import and read only, by decision) for a one-off gate. If the replay is
  needed under required, that is a design change to take on its own.
