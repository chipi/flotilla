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

## Earned trust (2026-11-23, the operator's second ask)

Asked how to see that an action earned its next level, the Autonomy table alone had four kinds of
friction: nothing said an action was ready; every row offered the same Promote; a recorded plan
earned nothing until it was judged on its case page; and a routine's `max_autonomy` or the
executor's cap could make a promotion do nothing, silently. One reading of each action's record
(`ui/trust.go`) now serves three places:

| Where | What it adds | What it replaces |
| --- | --- | --- |
| Now, Needs you | one line per action that is ready, linking to its row; a card of up to 3 recorded plans nobody judged, with Right and Wrong that post back to Now | the "Nothing needs you" card, while either waits |
| Autonomy | ready rows first, then the closest; the ready row's reason written from its record and its Promote the primary button; a line on the row when a cap or a routine below the next level holds the promotion | the same blue Promote on every row and an empty reason |
| The brief | a Trust section: each ready action, and how many recorded plans wait for a verdict | — |

`Store.ActionPlanners` names the live routines that planned an action (their caps apply);
`Store.UnjudgedRecorded` the recorded plans with no verdict from anyone.

## Page weight (2026-11-23, the operator's review of the screenshots)

- **Now:** "Wrong right now" took the top half of the page with 17 lines, above what needs a
  decision. It is now a card at the head of the side column: its count, the first 3 items, the rest
  behind "Show all N", and the one-line normal summary. Needs you leads the page; on a phone the
  card follows it.
- **Autonomy:** both tables sort, filter and page like every other table (the operator's rule since
  2026-11-14, missed here). Actions: level pills with Ready, a search on action and kind, sortable
  Action, Risk, Reversible, Level and Evidence (the default: ready first). Routines: level pills,
  search, every column sortable. 10 rows a page, since each row carries its forms. The two tables
  keep their own keys (`tableQ.Prefix`, "r" for Routines) and their links return to their table
  (`tableQ.Anchor`). A row that is not ready folds its reason box behind "Promote to X…".
- **Phone tables:** the first row of a stacked table never stacked its `td.stack` cells (the
  first-row rule outranked it); fixed for every table.

## Not covered

- The fleet-wide deploy (the mini and every remote worker's machine in one step): an open design
  question, not built.
- A refusal is in memory only (the link's state now); after flotillad restarts, it is known again
  on the worker's next retry, seconds later.
- The e2e's workers have a writable HOME, so the read-only HOME fault that broke meetings/notes on
  the installation (fixed in `worker/flotilla_worker.py`, unit-tested) is not exercised there.
- Earned trust: Now asks about recorded plans only. A proposed plan's evidence (approved as it
  stood, then verified) needs no extra answer. Nothing tells an operator who never opens Now or the
  brief, and a cap is named, not raised: raising a spec's `max_autonomy` is a change to the fleets
  repo and a deploy.
