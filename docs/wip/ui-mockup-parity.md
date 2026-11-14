# UI mockup parity

→ becomes a history entry once every page matches mockup v3 and is deployed.

Mockup v3 (the "flotilla UI mockup v3" design, 2026-11-12) is the visual contract for the operator
UI. The first deploy (2026-11-14, sha-f80c0c0) built a reduced version of it from the RFC-0007
table. That was wrong. This note lists every element of every mockup screen, what is built, what
the rebuild does, and where the data is missing.

Rules for the rebuild:

- Each element is built as drawn: same structure, same words, same tokens.
- An element whose data flotilla does not have yet is a **gap** (G-numbers below). The page names
  it in place; nothing is dropped silently.
- The operator's feedback of 2026-11-14 wins over the mockup where they disagree. Each such
  place is listed under "Overrides".

## Overrides (the operator's feedback over the mockup)

| # | Mockup | Feedback | The rebuild |
| --- | --- | --- | --- |
| O1 | Sub-tabs and filter pills: radius 22px (pills 20px), 44px (40px) tall | "2nd row of buttons is ugly, too rounded and too big" | radius 6px, 36px tall on a desktop; 44px on a phone, the touch-target floor |
| O2 | Tables with ↕ arrows, no paging (History: "Load 50 more") | "all tables have to have headers, filters and paging from the start" | every table: real sortable headers, a filter row and pages ("1–50 of 312", Previous / Next), on the server |
| O3 | Runs: the routine is plain text | "routine should take us to the routine's page" | the routine links to its fleet's Workflow with that routine's block selected (the mockup's routine detail) |
| O4 | Fleets cards have no sparkline | "fleets look very basic, get some more data in, sparklines" | each card gets a 14-day runs sparkline under its numbers |
| O5 | Now has no failed-runs list | "failed runs need paging, filtering, sorting; too long" | Now groups failures by routine in "Wrong right now" (one line per routine: "triage/cycle failed 3 times in 24h"), linking to Runs filtered to Problems |

## Gaps (data flotilla does not have yet)

| # | What | Where it shows | Status | Still open |
| --- | --- | --- | --- | --- |
| G1 | The model and tokens of each run | Runs "Model" column, Trace meta, Now "Models, last 24h", Models and spend | closed 2026-11-14 (840ab01): the model client logs each call; runs keep them (run_calls) | — |
| G2 | A run's spans | Trace | closed 2026-11-14 (11bb74c): the run page reads the trace from VictoriaTraces | routines without spans of their own show flotillad's run span only |
| G3 | A case's problem sentence | Cases "Problem", Case H1, Decision title | closed 2026-11-14 (d429933): detections carry a title, kept on the case | monitors other than the backup's send none yet |
| G4 | Host load (CPU, memory, disk), roles, containers, logs | Hosts, Host | load closed 2026-11-14 (35f7629): VictoriaMetrics by host_metrics; containers where cAdvisor runs | roles; the mini's containers (no cAdvisor); the host agent's log |
| G5 | Fleet logs | Fleet "Logs" tab, the workflow block's "Log, last lines" | closed 2026-11-14 (8a0b33b): VictoriaLogs by run, routine and fleet (vlogs_url) | the host agent's log, a macOS file the containerised Alloy cannot read |
| G6 | Fleet packs and their approval | Fleets "pack" line, Fleet pack line | closed 2026-11-14 (946581c): `fleets/<fleet>/fleet.yaml` and a pack hash | approving a pack: a spec change goes through review and deploy |
| G7 | What the agent read (memory, sources, trust) | Case aside | partly closed 2026-11-14 (10724d1): the case's runs and their model calls | for a model run, the playbook, skills and notes it loaded, and their trust |
| G8 | Diagnosis feedback (Right / Wrong / note) | Case aside | closed 2026-11-14 (82395d2): verdicts, notes, Knowledge | agents reading active notes (G7) |
| G9 | Slack approvals and their daily cap | Approvals subtitle, Decision "Approve from" | Slack is not wired | the Slack connector |
| G10 | A plan's host command | Decision "What would run" second line | closed 2026-11-14 (999ddc4): the agent reports its allowlist in hello | needs the agent upgraded to this build |
| G11 | Models and spend, Knowledge, the morning brief | their pages | Models and spend and Knowledge built 2026-11-14 | the morning brief |
| G12 | Platform and per-host execution caps as numbers | Autonomy guardrails, Now status line | closed in the rebuild: read from `policy.Limits` | — |
| G13 | The evidence a level needs | Autonomy "Evidence toward the next level" bar | policy has no threshold: only a person promotes | a promotion rule in policy; the page shows the record without a bar until then |
| G14 | A recurrence rule per kind | Autonomy | policy has none; a case that comes back is a new case | a recurrence rule in policy |
| G15 | Contract checks on a schedule | Models "Check now", the selected model's checks | runtime/contract_check.py exists, no routine runs it | a platform routine that runs it daily |

## Other deviations, named

- **The plan-changed box** (the Decision box's "The plan changed after this page opened") needs
  the page to learn of a new plan. The rebuild binds each answer to the plan hash shown instead: flotillad
  refuses an approval whose hash is not the case's plan now (`ApprovePlan`), and the page says so.
- **Reject's words.** The mockup says the action "counts one rejection on its record"; flotilla puts
  the action back to shadow on a rejection (RFC-0004 §5.2), and the page says that.
- **Undo on a demotion** is not offered: raising a level back needs a reason through the API.
- **STOP all routines** is filled red at rest, as drawn, although the Red Means Stop Rule keeps the
  brakes quiet; the mockup is the contract.
- **Run ids** are long (`triage-cycle-…`), so Runs and the run page show them in the Exact face and
  shorten them in tables.

## Pages

The mockup's Nav is the frame for every page; then the pages in mockup order.

### Nav (every page)

| Element | Built now | Rebuild |
| --- | --- | --- |
| Brand mark (the boat icon) + "flotilla" | word only | icon + word |
| Four groups Decide, Watch, Govern, Record | yes, with today's pages | groups as drawn: Decide (Now, Approvals), Watch (Cases, Runs, Fleets, Hosts), Govern (Autonomy, Models and spend, Knowledge), Record (Brief, History) |
| One orange badge on Decide: the count that waits for you | no | yes: cases awaiting approval + escalated, same count as Now's headline |
| "Autonomy: normal" pill with a green lamp; red "Autonomy: frozen" | no | yes, links to Autonomy |
| Version text (mockup "sample data · v3") | user · role | flotillad's version |
| Avatar circle "M" | no | yes; it links to the account page (Sign out, Users for an admin), which the mockup does not draw |
| Sub-tabs of the active group | pills | O1 |
| Phone bottom bar: icon + word, badge, active `#2A2D35` | words only | icon + word + badge; `#2A2D35` becomes a DESIGN.md token |
| Pages not built (Models and spend, Knowledge, Brief) | absent | in the nav as drawn; each opens a page that says it is not built and names G11 |

### Now

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| Date line "Tuesday 10 November, 07:40 · since you looked at 22:10" | "Signed in as" | as drawn | "since you looked" needs the previous visit: a per-user `now_seen_at`, written when Now renders |
| H1 "7 cases handled overnight. 5 need you." | "Something needs a look." | as drawn | closed cases since the last look; waiting count |
| "Read the morning brief" button | no | not drawn until the brief exists (G11) | — |
| "Wrong right now, not a decision" box: red-lamp lines with a link | "Wrong right now" card | as drawn; stale, paused and STOPped routines, unhealthy workers, disconnected hosts, failed runs grouped by routine (O5) | yes |
| Its footer line "Everything else is normal: the circuit breaker…, N of 20 executions…, models cost…" | no | executions today from host_executions; the breaker and the cap (G12); spend from runs | partial |
| "Needs you" + "All N plans in Approvals" | list of cases | the first waiting plan as a Decision box, then the rest as case cards (chip, id, kind · fleet, age, title, why) | yes, G3 for titles |
| "Handled overnight · showing 5 of N" + "Show all handled cases in History" | no | as drawn: closed cases since the last look | yes |
| Aside: Hosts topology (flotillad box, a line to each host, dashed when down) | no | as drawn from the hosts table; no hosts enrolled shows the box and "No host agent is enrolled yet" | yes |
| Aside: Models, last 24h | no | G1, named in the panel | no |
| Aside: Fleets (lamp, name, note) | no | as drawn | yes |
| The freeze card | on Now | moves to Autonomy (Govern owns autonomy changes); the nav pill shows it | — |
| "Sign out everywhere" | on Now | to the account page | — |

### Cases

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| H1, subtitle "One case per problem, from detection to a verified result.", "All past cases in History" | different words | as drawn | — |
| Six ScoreCards with 14-day sparklines and a dashed 7-day average: Open cases, Waiting for you, Opened, Closed, Time to close, Closed without you | no | as drawn | cases and case_events |
| Status pills with counts: All, Waiting for you, Open, Closed; search "id, problem, kind, target" | open / all link | as drawn (O1, O2) | yes |
| Pivot bar: status, kind and target tags filter; "← Back", "Clear all" | no | as drawn, in the URL (`?status=…&kind=…&target=…`) | yes |
| Table: Case (fleet icon box, id, fleet), Status chip, Problem, Kind chip, Target chip, Last event; sortable | 6 plain columns | as drawn; on a phone the row becomes the mockup's grid | fleet from the routine that acts on the kind; G3 |
| Footer "N of M cases. Click a status, kind or target tag…" | no | as drawn, with pages (O2) | — |
| Empty state | "No open cases." | says why: which routines can open cases, and that the ops routines are disabled today | yes |

### Case

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| Breadcrumb "Cases / id" | yes | as drawn | — |
| H1 sentence, meta line "id · opened · fleet, routine … · recurrence" | kind on target + chip | as drawn | G3 for the sentence |
| Decision box (shared with Now and Approvals) when a plan waits | no | as drawn: band, title, what would run, risk / if it goes wrong / approve from / proposed by, why it asks you, evidence, after you approve, plan hash + copy, note, Approve plan HASH (5 s hold with Undo), Reject… with a reason; CLI-only plans show the command | registry, plans; G9, G10 |
| Timeline with a legend (grey code, blue agent, orange you), each event: what, time · actor, run link, detail, code block | plain list | as drawn | case_events; run links from plans |
| Aside "Why policy asked": the levels, the result, the action's record link | Plans card | as drawn, from the plan's reasons | yes |
| Aside "What the agent read" | no | G7, named in the panel | no |
| Aside "After the check: was the diagnosis right?" | no | G8, named in the panel | no |
| Close and Reassign (the CLI's `case close / reassign`) | no | not drawn in the mockup; kept as two quiet buttons under the timeline for an operator | yes |

### Approvals (not built)

As drawn: summary line, "Jump to a plan" list, one Decision box per waiting plan, "Nothing needs
you." empty state, "Decided since you last looked" list, "All decisions in History". G9 for the
Slack line.

### Runs

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| H1, subtitle as drawn | H1 only | as drawn | — |
| Filter pills: All, Problems, With model spend | none | as drawn (O1), plus a routine and status filter and search (O2) | yes |
| Table: Run, Routine, Trigger, Started, Took, Status chip, Cases, Model, Cost; sortable | 7 columns, no headers on phones | as drawn; Routine links to the routine (O3); Cases from plans; Model is G1 (named in the header's note) | partial |
| Paging | first 100 | O2 | — |

### Trace (the run page)

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| Breadcrumb, H1 "run ID · routine", meta "time · took · status · case · worker · model calls · spans" | facts card | as drawn | G1 |
| "Open in VictoriaTraces" | no | when the run has a trace id and a traces URL is configured | trace_id |
| Span waterfall + selected span details | no | G2, named in place; the run's report is shown under it | no |

### Fleets

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| H1, "N fleets · M routines. Open a fleet…" | H1 | as drawn | — |
| "STOP all routines" (red) / "Release STOP" | per fleet only | as drawn: STOPs every fleet | yes |
| Status pills with counts (All, Needs you, Degraded, Healthy, Paused) + Find | no | as drawn | derived from routines and cases |
| Cards: lamp, name, status chip, purpose, routines / cases 24h / spend 24h, pack, last run | name + routine list | as drawn + O4; pack is G6 | purpose from the fleet's routines' descriptions until fleets have their own |

### Fleet

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| Breadcrumb, lamp + H1 + status chip, description, pack line, "Pause fleet" | name + STOP card | as drawn; pack line G6; "Pause fleet" is STOP | partial |
| Tabs Workflow, Routines, Costs and stats, Logs (underline tabs) | none | as drawn | — |
| Workflow: the dark board, five stages (Sources, Monitors, Remediation, Decide and act, Verify and report), a block per routine with lamp and 24h counts, legend | no | as drawn, the stages from each routine's trigger (every + detects: Monitors; case: Remediation; check: Verify; every without detects: report) | yes |
| Selected block aside: facts, last runs, log lines, Run now / Dry run / Pause / Spec in git | per-routine cards | as drawn; logs G5; "Dry run" runs at shadow if the API has it, else not drawn | partial |
| Routines tab: the table (Routine, Worker, Trigger, Autonomy, Last run, 7 days, Spend 24h) | no | as drawn (O2) | yes |
| Costs and stats: eight ScoreCards | no | as drawn; Tokens is G1 | partial |
| Logs | no | G5, named | no |

### Hosts (not built)

As drawn from the hosts table: status pills, role pills, Find, table Host, Status, Roles, Autonomy
cap, CPU · mem · disk (G4), Executions 24h, Host agent, Certificate. No host is enrolled today, so
the empty state says how to enrol one.

### Host (not built)

As drawn: breadcrumb, lamp + name, tabs Observability (G4), Host agent and allowlist (version,
clock offset, certificate, audit chain from the hosts table), Executions (host_executions), Logs (G5).

### Models and spend, Knowledge, Brief, Earlier briefs

G11: in the nav, each page says it is not built yet and what it needs.

### Autonomy

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| H1 "Autonomy and guardrails", subtitle | "Autonomy" | as drawn | — |
| Brakes card: line, "Freeze all autonomy" (outlined, pause icon), frozen banner, "Unfreeze…" with a reason, Recent changes with Undo | freeze card on Now | as drawn here (Govern owns it); Recent changes from the events table | yes |
| Actions table: Action, For kinds, Risk, Reversible, Level chip, Evidence bar toward the next level, Promote / Demote buttons with a reason | select + reason per row | as drawn | ActionStatus record |
| Guardrails card | no | as drawn; caps are G12 | partial |
| Host autonomy caps; Recurrence rule per kind | no | as drawn from config | partial |
| Routine autonomy (today's table) | yes | not in the mockup; kept as a compact table under Actions, since routines have their own autonomy | yes |

### History

| Element | Built now | Rebuild | Data |
| --- | --- | --- | --- |
| H1, subtitle "Everything that already happened…" | ledger decisions | as drawn | — |
| Period pills, type pills (Everything, Closed cases, Your decisions, Executions), Fleet select, Filter | ledger buttons | as drawn | case_events, events, host_executions |
| Table: When, Type chip, Case, What happened, Who, Outcome | ledger columns | as drawn (O2) | yes |
| "N shown · T in all" + more | Older link | pages (O2) | — |
| The triage ledger's decisions | the whole page | a "Ledger" type in the type pills, so the triage fleet's decisions stay readable | yes |

### Users and sign-in

Not in the mockup. They move under the avatar (an account page) and use the same components.

## The critique's findings (2026-11-14) folded in

- Sign out's hover contrast (1.4:1): Sign out moves to the account page as a normal button.
- `#2A2D35` outside the tokens: becomes `night-active` in DESIGN.md.
- Freeze as a filled red button: outlined with the pause icon, as drawn.
- The last admin can be demoted or revoked from Users: refused, with the reason on the page.

## Order of work

Nav and layout, then Now, Cases, Case (with the Decision box), Approvals, Runs, Trace, Fleets,
Fleet, Hosts, Host, Autonomy, History, the account page, the not-built pages. Each page is checked
against its mockup at 390 and 1440 wide before the next. One deploy at the end, on the operator's go.
