# RFC-0014 — The visual plane and mashups: flotilla seen live, and as the governed step in business processes

**Status:** Draft, for the operator's review. Nothing is built from it until the open questions are
decided. Four boundaries of the visual plane and three of mashups were decided on 2026-11-22 (§2);
the rest is proposal.
**Builds on:** [the two ideas](../wip/ideas-2026-11-19.md) (2026-11-19), [ADR-0001](../adr/ADR-0001-platform-foundations.md)
(one binary, server-rendered UI), [ADR-0005](../adr/ADR-0005-agent-pipeline.md) (approvals outside the
UI), [RFC-0002](RFC-0002-job-runtime.md) §7 (external knowledge), the case stages (RFC-0013 §2),
[ADR-0020](../adr/ADR-0020-routine-versions-stations-harnesses.md) (versions, shadows, pairs).

## 1. What this is for

Two ideas, one aim: that people trust agents with real work because they can see what the agents do
and how they are governed.

- **The visual plane.** A live picture of flotilla at work, for a big screen an organization leaves
  on: fleets and routines moving, cases flowing, approvals waiting, shadows being compared. Every
  shape is a real run, case or approval, so the picture is also the proof.
- **Mashups.** Companies wire agents into how people work (notes in chat, a knowledge desk, small
  "if this, then that" flows). flotilla could be the step in those flows that a business can trust:
  every decision in a ledger, anything outward approved, autonomy earned per action, new versions
  shadowed and rolled back, results evaluated.

## 2. Decided (2026-11-22)

| Boundary | Decision |
| --- | --- |
| V1 audience | Anyone in the room. The view shows shapes and counts, never contents (§3.2). |
| V2 where | Inside flotillad: one page and a live event stream, behind a view-only access. |
| V3 how live | Pushed as it happens: a snapshot on load, then each event as flotillad records it. |
| V4 controls | A few safe ones, none that changes flotilla (§3.4). |
| M1 triggers | One signed inbound webhook, yes (§4.2). Chat events are assessed later, with the rest of §4.3. |
| M2 a business process | A fleet; its steps are case stages (what bugfix and ops already do). No flow engine. |
| M3 knowledge | As the knowledge plan says: the platform's knowledge volume and its lookup tools, each step only after the one before it shows a measured effect (§4.4). |
| Open | Where flotilla stops and a plumbing tool starts (§4.1); chat events (§4.3). |

## 3. The visual plane

### 3.1 What it shows

A projection of what flotillad already records, nothing new to collect:

- each fleet and its routines; a run starting, running, ending (ok, error, skipped);
- a case opened, moving stage to stage, closed, escalated;
- a plan waiting for approval, and the approval;
- a shadow run beside its live run, and whether their decisions agreed;
- spend against the day's budget, as a fill, not a figure (open, §6 Q3);
- freshness: a routine that is late shows as late.

### 3.2 What it never shows

The rule is an allowlist, not a filter: the stream carries only declared fields, and a test pins the
list. Never: case or issue contents, plan bodies, prompts, model output, repo, customer or people
names, hosts, secrets, error texts, costs in money (unless §6 Q3 says otherwise). Fleet and routine
names are shown only where the fleet says so (`public_name:` in `fleet.yaml`, default a neutral
label), since a routine name can itself say too much.

### 3.3 How it works

- **The stream:** `GET /live/events` (server-sent events) on flotillad, fed from the store's event
  writes (runs, cases, approvals, pairs), projected through the allowlist before it leaves. A
  snapshot on connect (`/live/state`), then events; a dropped client reconnects from the last event
  id.
- **The page:** `/live`, server-rendered shell, a small script that draws. No outside libraries at
  first (an open question if the drawing needs one).
- **Access:** a view-only token of its own (a `viewer` caller role) that opens `/live` and nothing
  else, so a screen in a hallway holds no operator power.
- **Load:** one stream per screen; a cap on clients; the projection is computed once per event, not
  per client.

### 3.4 The safe controls

Pick which fleets to show; pause or slow the animation; switch between "the whole flotilla" and one
fleet; density (a small office screen or a wall). Nothing that approves, runs, pauses or reveals
more. The operator's working page ("now") stays where work happens.

### 3.5 The look

flotilla's own world, not a dashboard: the sea, fleets as convoys, routines as vessels, a run as a
wake, a case as cargo moving between stations, a waiting approval as a ship at anchor. Drawn by a
designer pass first (a mockup as the contract, at 1080p and 4K, and legible from across a room).
Ideas to bounce in §6.

## 4. Mashups

### 4.1 Where flotilla stops (open)

**A. flotilla is the governed agent step; plumbing tools keep the plumbing.**

- n8n and its kind keep triggers, connectors and data shuffling. They call flotilla (the webhook
  opens a case; the API starts a routine), and flotilla's actions can call them back.
- flotilla adds what they lack: the ledger, approvals for anything outward, autonomy earned per
  action, versions with shadows and rollback, and evaluation.
- For: no connector catalogue to build, secure and keep up; each side does what it is good at.
- Against: a company without a plumbing tool needs one; the full picture is split across two tools.

**B. flotilla grows its own connectors.**

- Triggers and connectors in flotilla itself (chat, email, documents, trackers), usable alone.
- For: one tool, one picture, governance on every step including the plumbing.
- Against: a large surface that every connector widens (credentials, rate limits, API changes); it
  competes where others are strong and flotilla adds nothing new.

**C. Between them: connectors as tools a routine is given, through MCP.**

- flotilla takes in no connector code: a routine declares MCP servers (a tracker's, a chat's) as
  tools, and their calls go through the same proxy, budget, recording and approval rules as any
  outward action. flotilla already serves MCP; this is the other direction.
- For: reach without a catalogue; governance on each call; the MCP ecosystem does the connectors.
- Against: an MCP server's quality and permissions are its maker's; each one is a new credential
  and a new egress rule to review.

Proposed for review: A first (the webhook and actions), C next where a fleet needs a tool A cannot
give, B not at all. To be settled after §6's discussion.

### 4.2 The inbound webhook (decided in principle)

- `POST /hooks/<name>`: a caller is declared in `callers.yaml` with its own secret; the request is
  signed (HMAC over the body and a timestamp), replay-protected (a window and a nonce),
  size-capped and rate-limited.
- It does one of two things the hook declares: open a case of a kind (its payload as the case's
  data), or start a routine. Nothing else, and never an outward action.
- The payload is untrusted data, as an issue body is: scrubbed, kept as data, never as rules or
  instructions.
- An idempotency key, so a retried call opens one case.
- Every call is in the ledger with its caller, and shows on the visual plane as a case arriving.

### 4.3 Chat events (assessed later)

A message to the bot, a reaction, a thread reply that opens or answers a case, without a plumbing
tool between. To assess with §4.1: the chat platform's app model, what a message may trigger, how a
person is identified (ADR-0005's pinned-user rule), and the noise a busy channel makes.

### 4.4 Knowledge

As the knowledge plan sets it: the platform's knowledge volume (RFC-0002 §7, kind `files`: declared
paths of each project's repo at the deployed release, read-only, with maps from each repo's
`llms.txt`) and the lookup tools (`doc_index`, `doc_read`; full-text search only if replays show the
maps miss). Its order holds: bug fixing reads docs first and must show an effect, then the volume,
then triage. A knowledge desk (§4.5) is a consumer of that volume, not a second store.

### 4.5 Example fleets that would show it

Disabled examples, like the others, until each is walked through:

- **Team brief:** who did what yesterday, from the repo, the tracker and chat; delivered where the
  team works. (The operator's brief is the seed.)
- **Knowledge desk:** a question in, an answer with its sources out, from the knowledge volume.
- **Meeting notes:** notes in (through the webhook), decisions and follow-ups out, each follow-up a
  case that needs a person's go.

## 5. Not in scope

- A flow engine or a visual flow editor (M2).
- A connector catalogue (unless §4.1 says otherwise).
- The visual plane as a control surface, or as the operator's working page.
- Public internet exposure of `/live` or the webhook; reaching them is the installation's network.
- Anything that changes how routines decide: the visual plane only watches, the webhook only opens
  cases.

## 6. Ideas to bounce, and open questions

1. **§4.1:** A, B, C or a mix? And does C (MCP servers as a routine's tools) belong in flotilla's
   next step or later?
2. **The look (§3.5):** the sea and fleets, or another world? Which moments should catch the eye from
   across a room: an approval waiting, a shadow disagreeing, a routine late?
3. **Spend on the wall:** a fill against budget, money figures, or nothing?
4. **Names on the wall:** neutral labels by default with `public_name:` per fleet (§3.2), or real
   routine names?
5. **History on the wall:** only "now", or also a slow replay of the day (yesterday's work at speed)?
6. **The webhook's first users:** which outside service would call it first, and what case kind it
   opens; that example drives the design.
7. **Which example fleet first** (§4.5), as the demonstration of mashups?
8. **The order:** the visual plane first (it needs no new trust surface beyond a viewer token), then
   the webhook, then an example fleet? Or the webhook first?

## 7. How we would know it works

- The visual plane: a person who has never seen flotilla can say, after a minute in front of it, what
  is busy, what is waiting for a person, and that nothing secret is on screen (a check with real
  people, not a metric). The allowlist test, and an e2e that feeds a case with planted secrets and
  finds none of them in the stream.
- The webhook: an e2e of a signed call opening one case, a replayed call opening none, an unsigned or
  stale one refused, and a payload with instructions in it treated as data.
- Mashups: one example fleet run end to end through the webhook, with its decisions in the ledger and
  every outward step approved.
