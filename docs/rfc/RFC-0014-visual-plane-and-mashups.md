# RFC-0014 — The visual plane and mashups: flotilla seen live, and as the governed step in business processes

**Status:** Draft, for the operator's review. The boundaries in §2 and the mashups' design were
decided on 2026-11-22; a first review (an advisor's, the same day) found where the first draft did
not match the code, and this version follows it; a second review (2026-11-22) found six blockers,
four fixed by it and two decided by the operator (a chat post's reversibility, a meeting's
follow-ups). Open: the visual plane's look, spend and history (§6). Nothing is built until the
operator says so.
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
| M5 the check | Four end-to-end cases carry the design (§4.5); cases 1-3 are each built as an example fleet with an end-to-end acceptance test (§7). |
| M6 where flotilla stops | In and out through the plumbing (A); reads live, through read-only MCP tools (C, read-only); no connectors of flotilla's own (B never) (§4.1, §4.8). |
| M7 the plumbing | n8n, run by the installation's operations; flotilla takes no dependency on it (§4.7). |
| M8 people | From the company's directory, read-only; an installation without one keeps a people file in its host config (§4.6). |
| M9 order | The hook first, then the cases 3, 1, 2 in turn, then the visual plane (§8). |
| Docs | Updated last, in full, architecture and diagrams included (§8). |
| V7 look, spend, history | A board with flotilla's world in the details; a budget fill for all, amounts for roles that see cost; "now" plus today's counts (§3.5). |
| Open | Chat events (§4.3). |

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

**Decided (2026-11-22):** an operations board's layout with flotilla's own world in the details. A
lane per fleet; cases as cards moving across their kind's stage columns; a "waiting for a person"
column for approvals; routines and their runs in the lane's header. flotilla's world (the sea,
convoys, vessels, wakes, cargo, a ship at anchor) is in the names, the icons and the motion, never at
the cost of reading the board in seconds. Two mockups first (the board, and the full sea world) as
the contract, at laptop and phone widths; the operator picks from the pictures. It must be readable
without colour alone (shape and label too), honour reduced motion, and keep a legend on screen.

**Spend:** a fill against the day's budget for everyone; the amounts besides, only for a role that
already sees flotilla's cost page (V2).

**History:** only "now" (the hub's snapshot, then notices), plus a strip of today's counts (cases
opened, approved, runs failed) that the hub keeps as it goes. A replay of the day would read the
store, which the stream never does (§3.3); it is a page of its own later, if wanted.

### 3.6 Later: a wall screen

A big screen in a shared room is the same page with two differences:

- **A signed-in session on the screen's browser,** with a role that sees only what anyone passing may
  see. The roles today are not that (`viewer` reads case contents, runs, events and the ledger:
  `internal/api/authorize.go:46-48`, `api.go:175-179`), so the wall needs one.
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
3. **Read:** a routine reads what the company knows (wiki pages, documents, tasks, chat threads) live,
   through read-only MCP tools it declares, each read recorded with its source (§4.8). Code docs stay
   in flotilla's knowledge volume (§4.4).

People rarely open flotilla: they ask, approve and read results in their own tools. The operator and
staff use flotilla's pages for the ledger, the approvals and the live view (§3).

### 4.1 Where flotilla stops (decided: A for in and out, C read-only for reads, B never)

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

**Decided (operator, 2026-11-22):** A for what comes in and what goes out. C for reads, read-only,
in this RFC (§4.8): a second brain answering from last night's copy is not good enough. A writing MCP
tool stays out; every write is `plumbing.call` (§4.6). B not at all.

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
- **Where it listens.** On a listener of its own, `hooks_listen`, which serves `POST /hooks/<name>`
  and `/healthz` and nothing else; `hooks:` without it fails the config. Not the API listener: that
  one can stay on loopback, and the plumbing's network would otherwise reach every route behind a
  bearer token. flotillad joins the plumbing's internal network, and nothing outside the
  installation's network reaches the listener (§5). The e2e stack's stub plumbing calls it the same
  way. Not covered: the workers' listener and the UI listener bind every interface of flotillad's,
  so the plumbing's network reaches them too; each still takes its own credential (a run's token, a
  session), and nothing there opens without one.
- **It opens one case and nothing else.** The case's kind starts its acting routine under the kind's
  case limits and canonical-key dedupe (`internal/spec/spec.go:210-226`, `internal/store/cases.go:108-126`),
  as every case does. There is no "start a routine": a bare run would skip both and leave no case.
- **The payload is data.** Only the declared fields are kept, capped in size, with credentials
  redacted before the store (the runtime's patterns in `runtime/toolrec.py`, ported to Go, since
  flotillad has no redaction of its own today), between
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
company's other knowledge too, and that changes during the day, so it is read live (§4.8), not copied
into the volume. A nightly export into the volume stays possible later, for one source that live
lookups prove too slow or too costly to search; it would be one more declared source with its own
VERSION (the export's time), under the same rules: declared, never discovered; read-only; data, never
rules; added only after a replay shows it helps.

### 4.5 End-to-end cases

Four cases, each step marked with its layer (R records, P plumbing, F flotilla) and whether flotilla
has it today. They are the check that the three layers and three meeting points (§4.0) carry real
work. Each becomes a disabled example fleet once its missing parts exist.

**Case 1. Ask the second brain** (a knowledge desk).

| Step | Layer | Today |
| --- | --- | --- |
| Someone asks a question in the chat's `#ask` channel. | R | |
| The plumbing sees the message and calls the hook `ask` (the text, the asker's chat id, the thread). | P | hook: missing (§4.2) |
| The hook opens a case of kind `question`; its text is data. The asker's chat id is looked up in the directory: a known person, or "unknown". | F | cases: yes; the directory: missing (§4.6) |
| `knowledge-desk/answer` searches and reads the wiki, the documents and the tasks live through its read-only tools, and the code docs in the knowledge volume, and drafts an answer citing each source with its link and time. | F (reads R) | live reads: missing (§4.8); the volume: planned (§4.4) |
| Its action `reply` starts at `propose`: a person approves the first answers in chat. | F | approvals in chat: yes (ADR-0005); the action: missing (§4.6) |
| flotilla calls the plumbing back; the reply lands in the thread. | F to P to R | callback: missing (§4.6) |
| The question, the sources read, the answer and who approved it are in the ledger; the answers are scored (sources cited, approved as sent). | F | ledger, evaluation: yes |
| After enough approved answers, the operator promotes `reply` to `auto` (autonomy is per action, not per kind; a chat post is reversible, §4.6), under the gates in §4.6: the asker's thread only, a known asker, a source cited from the run's own reads, three a day per asker. | F | earned autonomy: yes; the gates: missing |
| Someone marks an auto answer wrong; a correction case runs `plumbing.retract`, and the retraction counts as a failed execution, so `reply` drops back to `propose`. | F to P to R | the breaker: yes (`internal/policy/policy.go:221-223`); retract: missing |

**Case 2. Who did what yesterday** (a team brief).

| Step | Layer | Today |
| --- | --- | --- |
| A schedule in flotilla starts `team/brief` each morning. | F | yes |
| It reads the code host directly (merged changes, reviews), and the tasks and chat threads of the last day live through its read-only tools. | F (reads R) | code reads: yes; live reads: missing (§4.8) |
| Each name in the brief is a person from the directory, the same person across the code host, the tracker and chat. | F | the directory: missing (§4.6) |
| It writes the brief: what moved, what is stuck, who is waiting on whom. | F | a brief: yes (the operator's) |
| Its action `post` to the team's channel (fixed in the declaration, one a day, labelled) starts at `propose`. It is reversible like `reply`, but many read it unasked, so it is promoted later, on its own score. | F to P to R | the action: missing (§4.6) |

**Case 3. A meeting into decisions and follow-ups.**

| Step | Layer | Today |
| --- | --- | --- |
| A meeting's transcript lands in a shared folder. | R | |
| The plumbing sees the new file and calls the hook `meeting` (the file's text). | P | hook: missing |
| `meetings/notes` extracts the decisions and the follow-ups. | F | |
| The meeting stays one case. Its first stage plans `plumbing.create_tasks`: the list of follow-ups (who, what, by when), at `propose`. A follow-up is a task a person does in the tracker, not a flotilla case: flotilla has no work on it once it exists. | F | case stages: yes; the action: missing |
| A person approves the list once in chat; the plumbing creates the tasks. A wrong item is closed in the tracker. | F to P to R | approvals: yes |
| Its second stage plans `plumbing.post_decisions`, at `propose`: the decisions go to the wiki's decisions page once approved, so the second brain finds them on its next live read. | F to P to R | the action: missing |

**Case 4. Code, as today** (triage and bugfix).

| Step | Layer | Today |
| --- | --- | --- |
| An alert or an error arrives (the error tracker, the monitoring). | R | |
| triage polls them on its schedule and decides: file, attach, dismiss, escalate. | F | yes |
| A filed issue becomes a case; bugfix's stages draft a fix, reproduce, review. | F | yes |
| The pull request is opened only on a person's approval. | F to R | yes (the platform's own effect) |

Case 4 is the proof that the core already works: cases, stages, approvals, earned autonomy, shadows,
the ledger. Cases 1-3 add the three meeting points around it.

**Decided for the cases (2026-11-22):** all three are built, in the order 3, 1, 2 (§8), each as an
example fleet with its acceptance test (§7). Reads are live (§4.8); people come from the directory
(§4.6). **Still open:** how large a company's documents get against the knowledge plan's "cheapest
lookup first", which the first replays of case 1 measure.

### 4.6 What flotilla must add for them

- **The hook** (§4.2).
- **`plumbing.call`: one declared action per callback, one shared way to send them.** It is how a
  routine creates a task, posts a reply or files a page without flotilla owning a connector.
  - **One action per callback,** never one generic action: risk, reversibility, the kinds it serves,
    its limits and its earned autonomy all belong to one action name (`internal/registry/registry.go:70-82`;
    autonomy per action name, `internal/api/plans.go:21`), so `plumbing.create_tasks`,
    `plumbing.post_decisions`, `plumbing.reply`, `plumbing.post` and `plumbing.retract` are five
    actions with five ladders. A list (`create_tasks`' follow-ups) travels as one `text` param of
    bounded size whose shape (at most N items, each who, what and by when) the execute routine
    checks before sending; a list kind joins the registry's params only when a second action needs
    one. Each is an `executor: routine` action (ADR-0017) whose params are the callback's payload
    shape (enums, text with a maximum), so policy refuses a payload outside it before anything is
    sent (`internal/policy/policy.go:173-176`).
  - **One shared sender.** Each action's execute routine is a small spec around one shared
    `plumbing/call.py`: it signs the payload with HMAC-SHA256 and posts it to the callback. The
    callback's URL and its secret file are an installation setting keyed by the action's name, never
    in the spec. One execute routine per action, as ADR-0017 has it (`internal/spec/spec.go:231-233`).
  - **A worker of its own.** The callback secrets live in that worker's environment, and its proxy
    ACL admits only the plumbing's host.
- **What a chat post may earn** (decided 2026-11-22). Today the cap reads only `reversible`
  (`internal/policy/policy.go:214-216`, `internal/registry/registry.go:93-98`); `risk` only picks
  where a person approves (`internal/api/slack.go:50-56`). Two changes, both with the build:
  - **`reversible` means "the executor can take it back".** A chat post can: the bot edits or deletes
    its own message and posts a correction, through `plumbing.retract`. So `reply` and `post` are
    `reversible: yes`, and a person can approve them in Slack (which refuses `no`,
    `slack.go:54-56`). A sent email stays `no`: nothing recalls it. ADR-0017 decision 3 is amended
    to say so. Before declaring it, the installation checks that its chat's bot token can edit and
    delete its own messages in every channel kind it posts to (shared channels with other companies
    included).
  - **`risk: high` also caps at `propose`,** forever, at the same three places. A message to a
    customer or the public is declared high. No action is declared high today, so no past decision
    changes (replayed as ADR-0017's amendment was).
  - **The gates for `reply` at auto,** deterministic, after the model, in the execute routine's
    pre-check: the thread and channel come from the case, never from the plan; the asker resolves in
    the directory (an "unknown" asker keeps that plan at `propose`); a fixed agent label leads the
    text; at least one citation, each matching a read this run recorded (§4.8); a maximum length;
    no `@channel`, `@here` or mention outside the thread; `limits.per_target_per_day: 3`, the asker
    being the target. A retracted auto answer is recorded as a failed execution, so the breaker
    (`policy.go:221-223`) drops `reply` back to `propose`; there is no score-driven demotion in the
    code today, and none is added.
  - **Rejected:** a per-action override of the cap (a declaration that needs one is wrong, and it
    would be a second control beside the registry), and keeping chat at `propose` forever (it takes
    the second brain's value away).
- **Live reads** through read-only MCP tools (§4.8).
- **People from the directory.** A chat user, a code-host login and a tracker account mean the same
  person everywhere, for "who asked", for names in a brief, and for who approved:
  - **The source is the company's directory, read-only:** Google Workspace's directory, Microsoft
    Entra, or an LDAP directory. flotillad pulls it on a schedule with a read-only credential and
    keeps a copy, so a directory outage never stops a case. Each person is keyed by their email, with
    their ids in each tool as attributes (a custom schema in Google, extension attributes in Entra,
    custom attributes in LDAP); where a tool gives the email itself, the match is by email.
  - **No directory, then a file** (decided 2026-11-22). A company without one (this installation)
    keeps a people file in its host config: mode 600, each person's email, name and ids in each tool.
    It is one more source kind, `file`, read on the same schedule as a directory, through the same
    sync, so the ledger entries and the acceptance tests are the same. A directory
    service of its own (lldap was assessed) was rejected: a container, a backup and credentials to
    buy only a web page for editing and an LDAP that nothing here uses.
  - **Who someone is, never what they may do.** The directory names people; what a person may
    approve stays in flotilla's roles and ADR-0005's pinned users. A person the directory does not
    know may ask a question but approves nothing; that refusal is today's Slack pin
    (`internal/api/slack.go:130`), not the directory's. Each sync's changes are in the ledger; a
    person removed from the directory resolves to "unknown" on the next lookup. A plan records no
    approver until it is approved, so there is nothing to void. `flotillad -check` warns when a
    pinned Slack user is not in the directory.
  - **One reader per source kind,** in Go on the standard library: `file` first, for the
    installation; Google and Entra (HTTP and JSON) when a company needs them; a plain LDAP reader
    would be a dependency decision of its own, made only when a company brings an LDAP-only
    directory.

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

### 4.8 Live reads (option C, read-only)

A routine reads the company's tools while it works, through MCP servers it declares as tools. Reads
only: every write is `plumbing.call` (§4.6), so a read tool can never have an effect.

- **Declared per routine** in its spec: the server and the tools it may call, each by name. The list
  is the allowlist: a tool not on it is refused before the call. flotilla cannot tell a reading tool
  from a writing one (MCP's `readOnlyHint` is advisory, and its spec tells clients not to trust it),
  so read-only rests on two things: the declaration, reviewed like any spec change, and a read-only
  credential behind the server.
- **Which servers.** For the installation: n8n's MCP server trigger first, so the tools' credentials
  stay in the plumbing and one server fronts the wiki, the documents and the tracker (to confirm when
  n8n is set up). Its operations must give those read workflows read-only credentials to the wiki and
  the tracker, separate from the ones the `plumbing.call` workflows use. A tool maker's own MCP
  server where one is better, declared in the same way, with a read-only credential in a mode-600
  file the host config names.
- **Through the one runtime.** The runtime gains an MCP client (the streamable HTTP transport, which
  the standard library can speak; a library is a dependency decision).
- **The record, the replay's source.** A live read gives another answer tomorrow, so each run keeps
  its reads in full: per station, `{tool, args, result, ok}` in the envelope's
  `recorded_tool_results` format (`runtime/input.schema.json:59`), the whole result up to the per-call
  cap, stored with the run's frozen input (`internal/input`). A replay feeds them back through
  `FLOTILLA_RECORDED_TOOLS` (`runtime/offline_run.py:160-182`), where a miss fails the run and is
  never sent live. The episode's tool record is not this source: it keeps a result only up to 8192
  characters, above that a hash (`runtime/adapter.py:315`), and a wiki page is routinely longer.
- **A shadow reads the recording, not the tools.** A read through n8n starts a workflow execution
  that n8n saves: a record in a third system, which the no-effect rule forbids (ADR-0020 §8) and the
  e2e's store diff cannot see. So a shadow of a routine with read tools is served the live run's
  recorded results, which also makes the pair compare like with like. The shadow proxy admits no
  read-tool host.
- **The result is data.** Between delimiters (RFC-0002 §5), never as rules; credentials redacted by
  the runtime's patterns (`runtime/toolrec.py`) before the record; capped in size per call and in
  calls per run.
- **Egress.** The allowlist is a worker's proxy ACL, not a routine's (`deploy/compose/docker-compose.yml`),
  so every routine on a worker reaches its hosts. The routines with read tools get a worker of their
  own, whose ACL admits the declared servers' hosts and nothing else.
- **Spend.** Calls and their time count against the routine's budget like model calls.

## 5. Not in scope

- A flow engine or a visual flow editor (M2).
- A connector catalogue, and MCP tools that write (§4.1).
- The visual plane as a control surface.
- A wall screen in a shared room, until it is wanted (§3.6).
- More than one installation on one flotilla.
- Reaching `/live` or a hook from outside the installation's network.
- Anything that changes how routines decide: the visual plane only watches, a hook only opens cases.

## 6. Ideas to bounce, and open questions

**Open:** chat events (§4.3), assessed later. Which moments catch the eye on the board (an approval
waiting, a shadow disagreeing, a routine late) is settled in the mockups (§3.5).

**Decided (2026-11-22):**

- **The look, spend, history:** a board with flotilla's world in the details; a fill for everyone
  and the amounts for roles that see cost; "now" plus today's counts (§3.5).

- **§4.1:** A for in and out, C read-only for reads, B never (M6).
- **The first hook caller:** n8n, for case 3: a meeting transcript landing in a folder opens a case
  of kind `meeting`. Its first recorded calls are the replay set for the hook (project rule 3).
- **The cases:** all three, in the order 3, 1, 2, each with an acceptance test (§7).
- **The plumbing:** n8n (§4.7).
- **Freshness:** reads are live (§4.8); no nightly export to begin with.
- **People:** from the directory; an installation without one keeps a people file in its host
  config, read as a directory source (§4.6). No directory service on the installation.
- **The order:** the hook first, the visual plane after the cases (§8).
- **A chat post's reversibility:** `yes`, with `plumbing.retract`; `risk: high` caps at `propose`;
  `reply` may earn auto under its gates (§4.6).
- **A meeting's follow-ups:** tasks in the tracker, proposed as one list from the meeting's own case,
  not flotilla cases. A follow-up becomes a case only if flotilla is later meant to work on it
  (chasing its owner, say); then the plumbing would open it through a hook, one call per item.

## 7. How we would know it works

- **The visual plane:** a scripted check with three staff who have not seen it, three questions each
  (what is busy, what waits for a person, what is late), answered in a minute. An e2e that the page
  shows nothing a person's role does not already see elsewhere, and that a stopped stream shows as
  stale within one heartbeat.
- **The hook:** an e2e of a signed call opening one case; the same delivery id opening none; an
  unsigned, stale or oversized call refused; a payload naming another kind, title or target ignored;
  a payload carrying instructions kept as data.
- **`plumbing.call`:** an e2e of an approved action reaching its callback, signed, with its payload
  shape; a payload outside the action's params refused by policy before anything is sent; an action
  at `propose` never sent before its approval; two callback actions keeping separate autonomy; a
  `risk: high` action refused auto; `reply` at auto refused by each of its gates (a thread not the
  case's, an unknown asker, no recorded citation, `@channel`, the fourth of a day); a retraction
  dropping `reply` back to `propose`.
- **Live reads:** an e2e of a declared read tool called and recorded in full (a result above 8192
  characters included); an undeclared tool refused before the call; a replay served from the record
  with the server down; a shadow of the routine served the live run's record, calling no server.
- **The directory:** an e2e of a sync adding, changing and removing a person, each change in the
  ledger; a removed person resolving to "unknown" on the next lookup; `flotillad -check` warning on
  a pinned Slack user who is not in the people file.

**The cases' acceptance.** Each case is accepted twice: an e2e in the e2e stack, run in CI against a
stub plumbing, stub read tools and a people file, with the stack's scripted stub model
(`deploy/e2e/stub.py`) extended for the new prompts (a transcript gives fixed decisions and
follow-ups; a question gives an answer citing the stub tool's source); and a live run on the
installation with n8n, the real tools and the people file, its ledger read back by a script. A case
is done when both pass.

| Case | The e2e passes when | The live run passes when |
| --- | --- | --- |
| 3. Meeting to follow-ups | A signed `meeting` call opens one case; its decisions and follow-ups are extracted from a scripted transcript; the first stage plans one `plumbing.create_tasks` with the list in its fixed shape; approving it sends one callback, rejecting it sends nothing; the second stage plans `plumbing.post_decisions`, sent only on its own approval; no other case is opened; the same delivery again opens nothing. | A real meeting note landing in the folder ends with its follow-ups as tasks in the tracker and its decisions on the wiki page, each list approved once in chat; every step in the ledger with its actor. |
| 1. Ask the second brain | A signed `ask` call opens a `question` case; the asker resolves to a person or to "unknown"; the routine's read-tool calls are recorded with their sources; the drafted answer cites at least one source with its link; the `reply` waits for approval and, approved, sends one `plumbing.call`; an unknown asker's approval is refused; a replay with the tools stubbed out gives the same answer. | Ten real questions in `#ask` get answers in their threads, each citing its sources, each approved before it was sent; their scores (sources cited, approved as sent) are on the routine's evaluation. |
| 2. Who did what yesterday | A scheduled run reads the recorded code-host, tracker and chat results; every name in the brief is a directory person; a person with three accounts appears once; the `post` waits for approval and, approved, sends one `plumbing.call`. | Five mornings' briefs posted to the team's channel after approval, each name a real person once, each claim traceable in the ledger to the read that supports it. |
| 4. Code, as today | The existing triage and bugfix e2e suites stay green. | The live fleets keep running through the whole build (project rule 7). |

## 8. Order of work (decided 2026-11-22)

0. The installation's side, alongside: n8n, by its operations (asked 2026-11-22); the people file in
   host config, by the operator, before case 1; the Slack connector (ADR-0005: its connector host,
   team, UI URL and pinned user) before case 3's live run, or that run approves in the UI; and
   `routine_actions.max_autonomy: auto` (ADR-0017's amendment) for any callback action meant to
   earn auto.
1. The hook: `hooks:` config, the signature, window, nonce, caps and rate, the Go credential
   redaction, and its §7 e2e; and a `smoke` hook whose case kind closes itself on arrival, so the
   plumbing's manual smoke-test flow proves the path end to end without opening real work.
2. `plumbing.call` (§4.6): the shared sender, the first callback actions and their worker, and its
   §7 e2e.
3. **Case 3**, the meeting to follow-ups: the example fleet, its n8n flows on the installation, and
   its acceptance (§7).
4. The directory reader (§4.6) and its §7 e2e.
5. Live reads (§4.8) and their §7 e2e.
6. **Case 1**, the second brain: the example fleet, the knowledge volume's code docs as the knowledge
   plan has them by then, and its acceptance.
7. **Case 2**, the team brief, and its acceptance.
8. The visual plane: the hub at the write sites, and `/live` (the snapshot and the stream, the
   freshness mark, the safe controls), the mockup pass first, as the contract; after §6's open
   questions are answered.
9. The wall screen (§3.6), when one is wanted.
10. **The docs, last and in full:** the architecture pages and their diagrams (the three layers, the
   three meeting points, the live page's hub), the concepts page, the README and the examples' guide,
   so what is described is what was built.
