# The UI after case 3 and the fleet metrics (2026-11-23)

What the last arcs taught: the bugfix fleet was dead for hours and nothing in flotilla said so;
hooks, plumbing and staged cases were added with no place on screen; and the Hub and the UI were
about to count fleets two ways. Six changes follow from it.

**The rule for every change:** no new page or nav item where a surface the operator already looks
at answers the question; a change replaces what is weaker there instead of adding beside it; and a
number shown twice (the UI, the Hub) is computed once.

| # | The question it answers | Where it goes | What it replaces | Left out, and why |
| --- | --- | --- | --- | --- |
| 1 | Is any part of the fleet refused or unreachable? | Now, "Wrong right now": a remote worker refused (its version against flotillad's) or not connected. The fleet board's source node and the Hosts row of a worker link show the same state | the board's "not configured" for every remote worker (it never read their links), the Hosts row's idle "worker link" | a history of refusals: the link retries every ~20 s, so the state now is the signal, and a log line per refusal already exists |
| 2 | Has this action earned the next autonomy level? | Autonomy, the action's evidence line: approved and rejected beside planned, verified and failed. The fleet's stats: "Fixed and verified" becomes "Closed" with how they closed | the narrower card (verified only) | a new chart: the evidence line and the card's breakdown carry it |
| 3 | Is the outside world reaching flotilla, and did a send land? | Now, "Wrong right now": a hook that only refused in the last hour. The fleet board's Sources: a node per hook that opens the fleet's cases, its calls in 24 h. The case timeline: a send's ref (the Google Task, the Doc line) as its own line | the ref buried in a move's detail text | a Connections page: hooks are a fleet's sources and sends are its actions, both already have a place |
| 4 | Where is a staged case, and what comes next? | The case page's path line becomes a strip: the stages it went through, where it is, and the stage its plan hands it to | the text-only "At stage X" line; the timeline's bare stage name for a plan's then | a static list of a kind's stages: the chain is decided by plans at run time, so only the travelled path and the planned next one are facts |
| 5 | Which fleet does a case belong to? | A `fleet` column on cases, set once when the case opens (the opening routine's fleet, a hook's case the hook's project) and back-filled; the UI and the metrics both read it | the UI's per-read derivation and the metrics' own SQL rule, which disagreed for a hook's case | — |
| 6 | What did triage do? | Triage files to the ledger, not cases: its fleet stats show decisions and filings in place of the case cards; the metrics gain `flotilla_ledger_decisions_total{fleet,disposition}` for the Hub | four case cards that are always 0 for it | — |

Order of work: 5 (the metrics ship on it), 1, 3, 4, 2, 6.

## Built (2026-11-23)

- 5: schema 36, `cases.fleet`, set by `caseFleetOf` on open and back-filled; `caseFleet` in the
  UI reads it first.
- 1: `api.Server.Remote` (flotillad's host endpoint's view of each configured remote worker) joins
  `Status()`'s workers, so Now, the fleet board and Hosts all read one state.
- 3: `Store.HookActivities` from the `hook.*` events (schema 36 adds an index on events by kind
  and time); a send's ref is an `action.ref` case event, set in the exact face, never folded.
- 4: `caseStages` from `case.staged` and `plan.then`; the path line keeps the hand-on count and
  the spend.
- 2: `Store.AnswersByAction`, an answer attributed to the plan the case was asking about; the
  "Closed" card.
- 6: `ledgerCards` for a fleet with no cases; `flotilla_ledger_decisions_total{fleet,disposition}`
  for the Hub.

## Not covered

- The fleet-wide deploy (the mini and every remote worker's machine in one step): an open design
  question, not built.
- A refusal is in memory only (the link's state now); after flotillad restarts, it is known again
  on the worker's next retry, seconds later.
- The e2e's workers have a writable HOME, so the read-only HOME fault that broke meetings/notes on
  the installation (fixed in `worker/flotilla_worker.py`, unit-tested) is not exercised there.
