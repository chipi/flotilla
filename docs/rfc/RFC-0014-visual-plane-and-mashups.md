# RFC-0014 — The visual plane and mashups: flotilla seen live, and as the governed step in business processes

**Status:** Draft, for the operator's review. Nothing is built from it until the open questions are
decided. The boundaries in §2 were decided on 2026-11-22; a first review (an advisor's, the same
day) found where the first draft did not match the code, and this version follows it.
**Builds on:** [the two ideas](../wip/ideas-2026-11-19.md) (2026-11-19), [ADR-0001](../adr/ADR-0001-platform-foundations.md)
(one binary, server-rendered UI), [ADR-0005](../adr/ADR-0005-agent-pipeline.md) (approvals outside the
UI), [RFC-0002](RFC-0002-job-runtime.md) §5 and §7 (data between delimiters; external knowledge), the
case stages (RFC-0013 §2), [ADR-0020](../adr/ADR-0020-routine-versions-stations-harnesses.md) (versions,
shadows, pairs).

## 1. What this is for

Two ideas, one aim: that people trust agents with real work because they can see what the agents do
and how they are governed.

- **The visual plane.** A live picture of flotilla at work: fleets and routines moving, cases
  flowing, approvals waiting, shadows being compared. Every shape is a real run, case or approval, so
  the picture is also the proof. A page the operator and the organization's staff open, like the
  others; a big screen on a wall is a later, special case of it (§3.6).
- **Mashups.** Companies wire agents into how people work (notes in chat, a knowledge desk, small
  "if this, then that" flows). flotilla could be the step in those flows that a business can trust:
  every decision in a ledger, anything outward approved, autonomy earned per action, new versions
  shadowed and rolled back, results evaluated.

## 2. Decided (2026-11-22)

| Boundary | Decision |
| --- | --- |
| V1 audience | Staff only: people signed in to flotilla with the roles that exist today. |
| V2 what a person sees | What their role already lets them see on the other pages, no more. |
| V3 where | A normal page of flotillad's UI (`/live`), behind the session login, on the UI listener. |
| V4 how live | Pushed as it happens: a snapshot on load, then each event as flotillad records it. |
| V5 controls | A few safe ones, none that changes flotilla (§3.4). |
| V6 a wall screen | A real use, kept as a special case for later (§3.6). |
| M1 triggers | One signed inbound webhook that opens a case (§4.2). It never starts a routine on its own. |
| M2 a business process | A fleet; its steps are case stages (what bugfix and ops already do). No flow engine. |
| M3 knowledge | As the knowledge plan says: the platform's knowledge volume and its lookup tools, each step only after the one before it shows a measured effect (§4.4). |
| M4 the strategy | Three layers (records, plumbing, flotilla) meeting at three points: in, out, read (§4.0). |
| M5 the check | Four end-to-end cases carry the design (§4.5). |
| Docs | Updated last, in full, architecture and diagrams included (§8). |
| Open | Where flotilla stops and a plumbing tool starts (§4.1); chat events (§4.3); the rest of §6. |

## 3. The visual plane

### 3.1 What it shows

What flotillad already records, nothing new to collect:

- each fleet and its routines; a run starting, running, ending (ok, error, skipped);
- a case opened, moving stage to stage, closed, escalated;
- a plan waiting for approval, and the approval;
- a shadow run beside its live run, and whether their decisions agreed;
- spend against the day's budget;
- freshness: a routine that is late shows as late.

### 3.2 Who sees what

The page answers to the same rules as every page: a person signed in with a role sees on `/live`
what that role already reads elsewhere in the UI (routine names, case titles, the run pages behind a
shape). It adds no new view of anything, so it needs no new role and no new access. A shape links to
the page that already shows that thing, where the role's rules apply as they do today.

### 3.3 How it works

- **A hub, not polling the store.** The store has one connection (`internal/store/store.go:573`), and
  its events are plain inserts with no subscription (`store.go:648-652`). So the places that record
  a run, a case move, an approval and a pair also hand a small notice to an in-memory hub. The hub
  keeps the current state (refreshed at most every few seconds and shared by every client) and fans
  notices out. No client ever queries the store for the stream.
- **The stream:** server-sent events on the UI listener, behind the same session as every page: a
  snapshot on connect, then notices. A reconnect resumes from the hub's own sequence number (not the
  store's ids), and a gap longer than the hub keeps gets a fresh snapshot.
- **Limits:** a cap on clients; each client has a bounded buffer and is dropped when it falls behind,
  so a slow tab never slows the hub; write deadlines; a heartbeat every few seconds; after a restart,
  reconnecting clients share one snapshot.
- **Its own freshness first.** The page shows how old its last notice is before anything else, and
  turns plainly stale when the heartbeat stops. A frozen stream must never look like calm.

### 3.4 The safe controls

Pick which fleets to show; pause or slow the animation; one fleet or all; density. Nothing that
approves, runs or pauses. Acting stays on the pages where it happens today ("now", the case pages,
Slack).

### 3.5 The look

flotilla's own world, not a dashboard: the sea, fleets as convoys, routines as vessels, a run as a
wake, a case as cargo moving between stations, a waiting approval as a ship at anchor. A mockup
first, as the contract, at laptop and phone widths. It must be readable without colour alone (shape
and label too), honour reduced motion, and keep a legend on screen.

### 3.6 Later: a wall screen

A big screen in a shared room is the same page with two differences:

- **A signed-in session on the screen's browser,** with a role that sees only what anyone passing may
  see. The roles today are not that (`viewer` reads case contents, runs, events and the ledger:
  `internal/api/authorize.go:40-48`, `api.go:175-179`), so the wall needs one.
- **A projection, not the full data.** Today's records name people (an event's actor is
  `user:<name>`), hosts, repos and the problem (a case's target, project and title), and case ids
  carry the date (`internal/store/cases.go:45-56`, `83-88`). A wall therefore gets its own record type
  built from a declared list of fields, never a filter over the full one. Shapes can still tell a
  watcher a lot (a burst of cases is an outage, the time of a case matches a known incident), so
  times are coarse, ids are the hub's own, and counts stay low-resolution.

It is designed when a real wall is wanted, not now.

## 4. Mashups

### 4.0 The strategy: three layers

A small company runs on its tools: a productivity suite (Google Workspace or Microsoft 365), chat
(Slack or Teams), a wiki (Confluence or the like), code (GitHub), tasks (Jira or the like). It wants
agents to take on its recurring work and to be its "second brain", without leaving those tools.

| Layer | Who | Does | Does not |
| --- | --- | --- | --- |
| Systems of record | the company's tools | hold the data and the people; people keep working there | decide anything |
| Plumbing | n8n or a tool like it | move events and data: watch, copy, reformat, call an API | judge, draft, own consequences |
| flotilla | the company's fleets (agent = routine) | decide, draft and act, each step governed | connectors to every system; flows of plumbing |

**The line:** a step that is deterministic (route, copy, format, notify) is the plumbing's. A step
that needs judgment or has consequences (decide, answer, draft, file, change something) is a
flotilla routine, where it is in the ledger, approved until it has earned autonomy, versioned with
shadows, evaluated and budgeted.

**They meet at three points, and only there:**

1. **In:** the plumbing calls a signed hook, which opens a case (§4.2).
2. **Out:** a routine's outward effect is an action: done by flotilla itself where it has the
   effect (opening a pull request today), or asked of the plumbing through a signed callback, so
   flotilla never needs a Jira or a wiki connector of its own (§4.6).
3. **Read:** the plumbing exports what the company knows (wiki spaces, folders, task projects, chat
   digests) to files on a schedule; flotilla's knowledge volume serves them to routines, read-only,
   with their sources (§4.4).

People rarely open flotilla: they ask, approve and read results in their own tools. The operator and
staff use flotilla's pages for the ledger, the approvals and the live view (§3).

### 4.1 Where flotilla stops (open)

**A. flotilla is the governed agent step; plumbing tools keep the plumbing.**

- n8n and its kind keep triggers, connectors and data shuffling. They open a case through the webhook,
  and flotilla's actions can call them back.
- flotilla adds what they lack: the ledger, approvals for anything outward, autonomy earned per
  action, versions with shadows and rollback, and evaluation.
- For: no connector catalogue to build, secure and keep up; each side does what it is good at.
- Against: a company without a plumbing tool needs one; the full picture is split across two tools.

**B. flotilla grows its own connectors.**

- Triggers and connectors in flotilla itself (chat, email, documents, trackers), usable alone.
- For: one tool, one picture, governance on every step including the plumbing.
- Against: a large surface that every connector widens (credentials, rate limits, API changes); it
  competes where others are strong and flotilla adds nothing new.

**C. Between them: a routine is given MCP servers as tools.**

- flotilla takes in no connector code: a routine declares MCP servers (a tracker's, a chat's) as
  tools.
- What governance would take, since it is not free: the egress proxy sees hosts, not which tool is
  called inside a request (`deploy/compose/docker-compose.yml:51-53`). So the runtime needs an MCP
  client of its own (a new dependency), each call recorded as a tool of a model call, each tool that
  writes declared as an action in the registry (risk, reversible, at `propose`), its results entering
  as data between delimiters (RFC-0002 §5) and recorded for replay (they are live state a replay
  cannot fetch again), and a shadow refused every writing tool, with read-only credentials of its own.
- For: reach without a catalogue; governance on each call once that is built.
- Against: an MCP server's quality and permissions are its maker's; each one is a new credential, a
  new egress rule and a new source of injected text to review.

**Recommendation (for review):** A now. C later, as its own RFC, read-only tools first, when one
example fleet needs a tool A cannot give. B not at all.

### 4.2 The inbound webhook

A hook is declared, not a caller: callers (`callers.yaml`) are agents and operators presenting a
bearer token, of which flotilla keeps only a hash (`internal/api/api.go:91-137`). A hook is an
outside service, so it signs.

- **Declared in `flotillad.yaml`'s `hooks:`**, one entry per hook: its name; the secret, a mode-600
  file the config names; the case kind it opens, with its urgency and title wording; the payload
  fields it takes, each with a type and size, and which field is the case's target, checked against
  a pattern. Everything that decides what flotilla does is fixed here, never chosen by the payload.
- **`POST /hooks/<name>`**, signed with HMAC-SHA256 over the timestamp and the body. A timestamp
  outside a five-minute window is refused, and so is a delivery id already seen (a nonce table that
  expires). The body is capped in size and the hook in rate.
- **It opens one case and nothing else.** The case's kind starts its acting routine under the kind's
  case limits and canonical-key dedupe (`internal/spec/spec.go:210-226`, `internal/store/cases.go:108-126`),
  as every case does. There is no "start a routine": a bare run would skip both and leave no case.
- **The payload is data.** Only the declared fields are kept, scrubbed before the store, between
  delimiters wherever a model reads them, never as rules or instructions. The case title is the hook's
  wording, not the payload's, since titles reach Slack notices.
- **Its own key space.** The canonical key's kind is `hook:<name>`, so an outsider can never match a
  key another fleet uses. The delivery id makes a retry open no second case; the canonical key folds
  a storm into one open case.
- **Recorded.** Every call, accepted or refused, is in the ledger with its hook, and an accepted one
  shows on the visual plane as a case arriving.

### 4.3 Chat events (assessed later)

A message to the bot, a reaction, a thread reply that opens or answers a case, without a plumbing
tool between. To assess with §4.1: the chat platform's app model, what a message may open (the same
rule as the webhook: the kind fixed, the text data), how a person is identified (ADR-0005's pinned-user
rule), and the noise a busy channel makes.

### 4.4 Knowledge

As the knowledge plan sets it: the platform's knowledge volume (RFC-0002 §7, kind `files`: declared
paths of each project's repo at the deployed release, read-only, with maps from each repo's
`llms.txt`) and the lookup tools (`doc_index`, `doc_read`; full-text search only if replays show the
maps miss). Its order holds: bug fixing reads docs first and must show an effect, then the volume,
then triage. A knowledge desk (§4.5) is a consumer of that volume, not a second store.

**Beyond code repos.** The plan's sources are each project's repo. A second brain needs the
company's other knowledge too: the plumbing exports wiki spaces, folders, task projects and chat
digests to files (a folder or a git repo of its own) on a schedule, and the volume takes that export
as one more declared source, with its own VERSION (the export's time). The same rules hold:
declared, never discovered; read-only; data, never rules; and each new source only after a replay
shows it helps.

### 4.5 End-to-end cases

Four cases, each step marked with its layer (R records, P plumbing, F flotilla) and whether flotilla
has it today. They are the check that the three layers and three meeting points (§4.0) carry real
work. Each becomes a disabled example fleet once its missing parts exist.

**Case 1. Ask the second brain** (a knowledge desk).

| Step | Layer | Today |
| --- | --- | --- |
| Someone asks a question in the chat's `#ask` channel. | R | |
| The plumbing sees the message and calls the hook `ask` (the text, the asker's chat id, the thread). | P | hook: missing (§4.2) |
| The hook opens a case of kind `question`; its text is data. | F | cases: yes |
| `knowledge-desk/answer` reads the knowledge volume (wiki and folder exports, code docs) and drafts an answer with its sources. | F | the volume: planned (§4.4); exports: missing |
| Its action `reply` starts at `propose`: a person approves the first answers in chat. | F | approvals in chat: yes (ADR-0005); the action: missing (§4.6) |
| flotilla calls the plumbing back; the reply lands in the thread. | F to P to R | callback: missing (§4.6) |
| The question, the sources read, the answer and who approved it are in the ledger; the answers are scored (sources cited, approved as sent). | F | ledger, evaluation: yes |
| After enough approved answers, `reply` earns `auto` for this kind. | F | earned autonomy: yes |

**Case 2. Who did what yesterday** (a team brief).

| Step | Layer | Today |
| --- | --- | --- |
| A schedule in flotilla starts `team/brief` each morning. | F | yes |
| It reads the code host directly (merged changes, reviews) and last night's task and chat exports from the volume. | F (reads R through P's export) | code reads: yes; exports: missing |
| It writes the brief: what moved, what is stuck, who is waiting on whom. | F | a brief: yes (the operator's) |
| Its action `post` to the team's channel starts at `propose`, then earns `auto`. | F to P to R | the action: missing (§4.6) |

**Case 3. A meeting into decisions and follow-ups.**

| Step | Layer | Today |
| --- | --- | --- |
| A meeting's transcript lands in a shared folder. | R | |
| The plumbing sees the new file and calls the hook `meeting` (the file's text). | P | hook: missing |
| `meetings/notes` extracts the decisions and the follow-ups. | F | |
| Each follow-up becomes a case of its own at `propose` (who, what, by when). | F | case stages: yes |
| A person approves a follow-up in chat; its action asks the plumbing to create the task. | F to P to R | approvals: yes; the action: missing |
| The decisions go to the next export, so the second brain remembers them. | F to P | missing |

**Case 4. Code, as today** (triage and bugfix).

| Step | Layer | Today |
| --- | --- | --- |
| An alert or an error arrives (the error tracker, the monitoring). | R | |
| triage polls them on its schedule and decides: file, attach, dismiss, escalate. | F | yes |
| A filed issue becomes a case; bugfix's stages draft a fix, reproduce, review. | F | yes |
| The pull request is opened only on a person's approval. | F to R | yes (the platform's own effect) |

Case 4 is the proof that the core already works: cases, stages, approvals, earned autonomy, shadows,
the ledger. Cases 1-3 add the three meeting points around it.

**What the cases leave open:** whether nightly exports are fresh enough for a second brain, or some
reads must be live (§4.1 C, read-only tools); how a chat user maps to a person flotilla knows (for
approvals and for "who asked"); how large a company's documents get against the knowledge plan's
"cheapest lookup first".

### 4.6 What flotilla must add for them

- **The hook** (§4.2).
- **One generic outward action, `plumbing.call`:** a signed request to a callback the installation
  declares (an n8n webhook, say), with a fixed name and payload shape per declared callback, its risk
  and reversibility in the action registry, at `propose` until earned. It is how a routine creates a
  task, posts a reply or files a page without flotilla owning a connector.
- **Exports as a knowledge source** (§4.4).
- **People across tools:** a declared map from a chat user to a person flotilla knows, so approvals
  and "who asked" mean the same person everywhere.

### 4.7 The plumbing for the installation's own test

To run the cases on the installation itself: n8n, self-hosted beside flotilla (one container and a
store), as the plumbing. Its webhooks, connectors (chat, the productivity suite, the wiki, the task
tracker, the code host) and a code step for the HMAC signature cover every P step above. Its licence
is fair-code (the Sustainable Use License): fine for an organization's own use, restricted for
offering it to others as a service. Activepieces (MIT core) is the alternative if flotilla's public
story ever pairs with a plumbing tool. Running it is the installation's operations work.

**Decided (operator, 2026-11-22): n8n.** The installation's operations set it up; flotilla adds no
dependency on it, because the hook and `plumbing.call` speak plain signed HTTP that any plumbing tool
can send and receive.

## 5. Not in scope

- A flow engine or a visual flow editor (M2).
- A connector catalogue (unless §4.1 says otherwise).
- The visual plane as a control surface.
- A wall screen in a shared room, until it is wanted (§3.6).
- More than one installation on one flotilla.
- Reaching `/live` or a hook from outside the installation's network.
- Anything that changes how routines decide: the visual plane only watches, a hook only opens cases.

## 6. Ideas to bounce, and open questions

1. **§4.1:** A, B, C or a mix? Is the recommendation (A now, C later read-only) right?
2. **The look (§3.5):** the sea and fleets, or another world? Which moments should catch the eye: an
   approval waiting, a shadow disagreeing, a routine late?
3. **Spend:** shown as a fill against the budget, as money, or both (staff may see money today)?
4. **History:** only "now", or also a slow replay of the day?
5. **The first hook:** which outside service would call it, and what case kind it opens. A real first
   caller in the installation, so the hook has recorded calls to replay its decisions against before
   it ships (project rule 3).
6. **Which case first** (§4.5)? Case 1 (the second brain) exercises all three meeting points; case 3
   needs no knowledge volume.
7. **The plumbing** (§4.7): decided, n8n.
8. **Fresh enough?** Nightly exports for the second brain, or some reads live from the start (§4.1 C,
   read-only)?
9. **People across tools** (§4.6): where the map from a chat user to a person lives, and who keeps it.
10. **The order** (§8): the visual plane first, then the hook? Or the hook first?

## 7. How we would know it works

- **The visual plane:** a scripted check with three staff who have not seen it, three questions each
  (what is busy, what waits for a person, what is late), answered in a minute. An e2e that the page
  shows nothing a person's role does not already see elsewhere, and that a stopped stream shows as
  stale within one heartbeat.
- **The hook:** an e2e of a signed call opening one case; the same delivery id opening none; an
  unsigned, stale or oversized call refused; a payload naming another kind, title or target ignored;
  a payload carrying instructions kept as data.
- **Mashups:** one example fleet run end to end through a hook, its decisions in the ledger and every
  outward step approved.

## 8. Order of work (proposed)

1. The hub at the write sites, and `/live`: the snapshot and the stream, the freshness mark, the safe
   controls; the mockup pass first, as the contract.
2. The hook: `hooks:` config, the signature, window, nonce, caps and rate, the §7 e2e, with a first
   real caller.
3. `plumbing.call` (§4.6) and the map of people across tools.
4. The plumbing on the installation (§4.7), run by its operations, and the first case end to end
   (§6 Q6), then the others; exports as a knowledge source when a case needs them, after the
   knowledge plan's step before it has shown an effect.
5. §4.1's option C as its own RFC, if a case needs live reads.
6. The wall screen (§3.6), when one is wanted.
7. **The docs, last and in full:** the architecture pages and their diagrams (the three layers, the
   three meeting points, the live page's hub), the concepts page, the README and the examples' guide,
   so what is described is what was built.
