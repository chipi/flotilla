# RFC-0007 — The operator UI: from mockup v3 to pages flotillad serves

**Status:** Proposed (2026-11-13), revision 4: the operator's decisions in §9, and an independent
review taken in (§10).
**Builds on:** [ADR-0001](../adr/ADR-0001-platform-foundations.md) (the API serves a small web UI in
the same binary), [RFC-0001](RFC-0001-fleet-platform.md) §6 (interfaces), §8 (security) and §10.3 (the
UI served by the Go binary), [ADR-0005](../adr/ADR-0005-agent-pipeline.md) (cases, the freeze),
[ADR-0008](../adr/ADR-0008-autonomy.md) (autonomy and caps), [ADR-0013](../adr/ADR-0013-fleet-packs.md)
(packs, the installation's cap).
**Amends, on acceptance:** ADR-0005 and ADR-0008 (the freeze lowers to `shadow`, §6), ADR-0013 decision
4 (pack approval is an admin's, §4), `docs/architecture/deployment.md` (operator identity, §4) and
PRODUCT.md (several people; the UI calls the server's functions, §3). §9 lists them.
**Design sources:**

- `PRODUCT.md`;
- `DESIGN.md` and `internal/ui/static/flotilla.css` (the design system, "The Control Room");
- the clickable mockup v3, a private design canvas with sample data, critiqued twice (24/40, then
  25/40), with every finding fixed.

## 1. Motivation

The operator works flotilla from a terminal today: `flotilla status`, `runs`, `pause`, `resume`,
`autonomy`, and SSH to read logs. On a phone that is not possible, and PRODUCT.md makes the phone a
first-class place to work: an approval, or a check that nothing is wrong, must be complete on a
phone.

The mockup settled what the UI is:

- its screens and their grouping (Decide, Watch, Govern, Record);
- the decision box with its states;
- the brakes;
- the phone layout.

`DESIGN.md` turned it into tokens and components. This RFC settles the rest: how the UI is served,
who signs in and what each role may do, which screens the store can feed today, and the order of
work.

The mockup's files are not the UI. They are a design canvas's artboards with sample data; the UI is
Go templates and the stylesheet, served by flotillad. What carries over is the design.

## 2. Goals and non-goals

**Goals:**

- On a phone or a laptop, people can see what the fleets did, what is wrong and what waits for
  them, and act within their role.
- Later, an operator can decide in the UI: approve or reject a plan with everything needed in view
  (the decision box).
- Every page meets DESIGN.md: no sideways scroll at 360 to 430px, 44px targets, WCAG 2.2 AA.
- **The UI adds no authority and takes none away.** Every action is checked by the same
  `authorize(caller, action)` the API uses (§4), and every rule the UI enforces is also enforced by
  the API, such as a required reason. There is no UI-only check and no UI-only power.

**Non-goals:**

- A separate frontend build or a JavaScript framework. ADR-0001 deferred this; nothing here needs it.
- Screens for what does not exist yet. Cases, policy, host agents, the brief and knowledge get their
  screens with their backends (§6).
- Replacing Grafana. Metrics stay there; the UI links to them.
- MCP: a separate interface, and an agent never approves (RFC-0001 §8).

## 3. Serving

- **Pages:** flotillad serves the UI from `internal/ui`: Go `html/template` pages, the embedded
  stylesheet and fonts (Inter and JetBrains Mono as woff2; nothing fetched from a third party), and
  a few small scripts. Every action is a form that POSTs and redirects, so the pages work without
  JavaScript. Scripts add the conveniences:
  - the undo countdown;
  - copy to clipboard;
  - the changed-plan reveal;
  - a 30 s refresh of Now and Runs while they are visible.
- **The address:** `https://flotilla.<tailnet>`, through a caddy-tailscale node of flotilla's own,
  as `deployment.md` already sets out. It is a `caddy-tailscale` container joined to a new small
  network, `ui`, that is not internal. Only flotillad's UI listener and that container are on `ui`;
  flotillad stays on its internal `flotilla` network for everything else.
- **What the proxy does and doesn't do:** it terminates TLS and reverse-proxies to the listener.
  It plays no part in identity (§4). Tailscale Funnel is never enabled.
- **The UI listener** (`ui_listen`, on the `ui` network only) serves two things:
  - the UI pages, which need a session (§4);
  - the `/v1` API, for **user** bearer tokens only, so `flotilla` on a laptop works as that person.

  It refuses:
  - the ledger routes;
  - worker tokens;
  - callers-file tokens, which stay on loopback inside the container;
  - a session cookie on `/v1`;
  - any `Host` other than the configured name.
- **Hardening:**
  - Read, write and idle timeouts on the listener.
  - Headers: CSP `default-src 'self'; script-src 'self'; frame-ancestors 'none'`, plus
    `X-Content-Type-Options` and `Referrer-Policy`.
  - Everything from runs, events and the ledger is untrusted text (RFC-0001 §8) and renders only as
    escaped text, never as `template.HTML`.
- **One authorization path:** the UI's handlers call the server's own functions, not its HTTP API.
  Both the API middleware and those handlers call one `authorize(caller, action)` (§4). The event
  log records the actor as `user:<name>`, with `via: ui` from the UI handlers. `via: api` covers
  the CLI too; the server cannot tell a CLI from another client without trusting a header the
  client sets about itself, so it does not try.

## 4. Users, roles and signing in

Authentication is flotilla's own, and doesn't depend on the network it runs on (operator,
2026-11-13). Users are people. Agents, workers and automation keep their tokens in the callers file
and never sign in.

**Users** live in flotillad's store, with:

- a unique name;
- a role;
- the SHA-256 of their token, uniquely indexed. The token itself is never stored. Tokens are
  generated by the server: 256 bits from `crypto/rand`, never chosen by a person.
- created and revoked times. A revoked user is kept, so the audit log stays readable and a name is
  never reused.

Names are unique across users and callers. A user's actor is `user:<name>`, so no user can be
mistaken for `flotillad`, `worker:<x>` or a caller.

**Roles,** each including the one below it:

- `admin`: an operator who also decides who and what this installation trusts:
  1. users and sessions;
  2. trusting fleet packs that are not built in (ADR-0013, approval by set hash), and rolling them
     back or pruning them;
  3. the audit view: user, session and trust events, with who made them.

  The installation's cap stays config (`max_autonomy`, ADR-0013's non-impact mode): admins see it
  read-only and change it with a config change and a deploy.
- `operator`: the day's work, as in the table below. Nothing an operator needs day to day waits for
  an admin.
- `viewer`: sees everything and changes nothing.

**Callers stay outside this order:**

- `agent` (the callers file): reads, and only the actions that make things safer or that it runs
  today (run now, pause, stop, freeze). It never signs in, and never reaches users, packs or any
  loosening action.
- `worker`: the ledger only, on its own listener.
- The callers-file `operator` token: bootstrap and break-glass only (below). It cannot sign into the
  UI, and once an admin exists it is refused for everything else.

**What each may do.** `authorize()` implements this table, and a test walks every row for every
role:

| Action | worker | agent | viewer | operator | admin |
| --- | --- | --- | --- | --- | --- |
| Read pages, status, runs, events | – | yes | yes | yes | yes |
| Read the ledger (History) | own namespace | – | yes | yes | yes |
| Write the ledger | own namespace | – | – | – | – |
| Run now, pause a routine, stop a fleet | – | yes | – | yes | yes |
| Pull the freeze | – | yes | – | yes | yes |
| Resume, start a fleet, raise autonomy, lower autonomy | – | – | – | yes | yes |
| Unfreeze | – | – | – | yes | yes |
| Approve or reject a plan (slice 2) | – | – | – | yes | yes |
| Users and sessions; trust, roll back or prune packs | – | – | – | – | yes |
| The audit view | – | – | – | – | yes |

Loosening actions (resume, start, raising autonomy, unfreeze) need a reason, required by the API
for every caller and stored in the event's detail. Lowering autonomy and the other tightening
actions need none.

**The first admin, and break-glass:**

- With the user table empty, `flotilla user add <name> --role admin` is accepted only with the
  callers-file `operator` token. The CLI on the host passes it by `docker exec`, as today. Agent
  tokens are refused, and so are other roles.
- The check that no admin exists and the insert run in one transaction.
- The same token can add an admin again when every admin has been revoked or lost (break-glass).
  That event is marked as break-glass.
- There is no default user and no default password.

**There is always an admin.** Revoking the last admin, demoting the last admin (themself included),
and two demotions at once are all refused: the count and the change run in one transaction. A store
restore brings back users and sessions as they were then. The restore procedure says so, and
rotates every token afterwards.

**Managing users:**

- `flotilla user add <name> --role admin|operator|viewer` prints the new token once.
- `user list`, `user role <name> <role>`, `user rotate <name>` (prints a new token once),
  `user revoke <name>`, `user sessions <name> --end`.
- Admins do the same on the Users page under Govern. It shows a new token once, served with
  `Cache-Control: no-store`.

**Sessions:**

- Signing in means pasting your token once per device. flotillad checks its hash and creates a
  fresh session.
- The `__Host-flotilla` cookie (`HttpOnly`, `Secure`, `SameSite=Strict`) carries a random 256-bit
  session id; the store keeps only its hash.
- A session lasts 30 days from sign-in, absolute, and ends after 7 days idle.
- The user and role are read on every request, so a demotion or revocation takes effect at once.
  Rotating ends the user's sessions.
- "Sign out everywhere" ends your own sessions; an admin can end anyone's.
- Every POST carries a CSRF token.

**Sign-in failures** are rate-limited globally (every request comes through the proxy, so there is
no per-address key). The limit is generous; the tokens' 256 bits are the real protection. Every
failure is an event, without the token or its hash.

**The audit events:**

- user added; role changed (old → new); token rotated; user revoked;
- signed in; sign-in failed; signed out everywhere; an admin ended someone's sessions;
- break-glass used;
- packs trusted, rolled back or pruned.

The audit view is a filtered view of the event log, not a secret: agents can read `/v1/events`.

## 5. Screens and what the store holds today

The mockup has 16 screens. **Today** means flotillad's store holds the data now. **New query** means
the data is there and slice 1 adds the read. **Later** names the backend that is missing.

| Group | Screen | Today |
| --- | --- | --- |
| Decide | Now | health, stale and failed routines and failed runs: **today** (`status`, `runs`); "waits for you" **later** (cases) |
| Decide | Approvals | **later**: cases and policy (ADR-0005) |
| Watch | Cases, Case | **later**: cases (ADR-0005) |
| Watch | Runs | **today**: id, routine, trigger, start, duration, status, spend, trace id |
| Watch | Run | **new query**: the run row and its `detail` (`Store.Run(id)`); traces linked out to VictoriaTraces and Langfuse. The scheduler writes no events per run today, so the page shows none. |
| Watch | Fleets, Fleet | **today**: routines, controls, last runs, worker health; pack hashes **later** (ADR-0013) |
| Watch | Hosts, Host | **later**: host agents (ADR-0006) |
| Govern | Autonomy | **today**: per-routine autonomy and fleet STOP; **new query**: each routine's `max_autonomy` and the installation's cap (config, read-only); per-action levels **later** (ADR-0008's full model) |
| Govern | Users | **new**: §4 |
| Govern | Models and spend | **partly**: aliases from config, spend from runs; tokens and engines **later** (LiteLLM) |
| Govern | Knowledge | **later**: ADR-0010 |
| Record | History | **new query**: the ledger store holds filings and entries per namespace, but reads them oldest-first and only for workers. Slice 1 adds a newest-first, paged, filtered read, and a list of namespaces, for viewer and up. Live `triage/cycle` has a namespace only after the promotion on 2026-11-14 (`docs/wip/promotion-plan.md`). About 12,000 of the candidate's ~14,300 entries are recurrences, so History groups them. |
| Record | Brief, Earlier briefs | **later**: `ops/daily-brief` |

## 6. The order of work

**Slice 1: users, then watch and act on what exists.**

- A store schema step: users, sessions, and the indexes and cleanup they need.
- The new reads from §5: `Store.Run`, the ledger read for viewers, `max_autonomy` in `status`.
- `authorize()` and its table. The API middleware moves onto it, and the reason is required on
  loosening actions.
- The `flotilla user` commands, the first admin, sign-in, sessions and the Users page.
- The UI listener and the caddy-tailscale node (§3).
- The shell: navigation, the phone bottom bar, the fonts.
- The pages: Now (health and what is wrong), Runs and Run, Fleets and Fleet with their controls,
  History, Autonomy.

**Slice 1b: the freeze.** ADR-0005 and ADR-0008 already decided it (`flotilla autonomy freeze`).
This slice builds it per routine, ahead of capabilities, with one change to its meaning (operator,
2026-11-13): **a freeze lowers every routine to `shadow`, so nothing outward happens.** For today's
triage fleet, `propose` still files GitHub issues and comments, so lowering to `propose` would not
stop its outward writes.

- What runs is min(effective autonomy, `shadow`). The freeze never overwrites a routine's stored
  control, so unfreezing restores it.
- A run already in flight keeps the autonomy it started with.
- The scheduler's autonomy check applies the freeze, and `status` shows effective and set autonomy
  side by side.
- Who can pull it and who can lift it are in §4's table; unfreezing needs a reason.
- STOP stays the stronger brake: no runs at all.

**Slice 2: decide.** Approvals and Case, with the decision box and every state the mockup tested:
the changed plan with its diff, the undo window, a failed send, CLI-only plans and the plan hash. It
comes when cases and the policy exist, and the mockup's states are the contract that API must meet.
A Slack approver (ADR-0005) maps to a flotilla user with the `operator` role, by a recorded Slack
user id.

**Slice 3:** the rest, as each backend lands: Hosts, Brief, Knowledge, Models.

## 7. How each slice is checked and shipped

- **Go tests:**
  - every row of §4's table for every role;
  - the first-admin and always-an-admin rules, including two demotions at once;
  - sessions: fixation, revocation, demotion taking effect at once, expiry;
  - CSRF;
  - the UI listener's refusals (§3);
  - each page from a seeded store;
  - untrusted text escaped.
- **The browser check** at 360, 390 and 1440px: no sideways scroll, no target under 44px, no console
  error. It is the mockup's `check.js`, moved into the repo's e2e, with Playwright as a dependency
  (approved by the operator, 2026-11-13; ADR-0003 records it when it is added).
- `ui_test.go` keeps the stylesheet equal to DESIGN.md.
- An impeccable critique of the real pages.
- **Schema and rollback:** slice 1 adds a store schema step, and an older flotillad refuses a newer
  store. So:
  - before the deploy, the new image runs read-only against a copy of the mini's real store;
  - the deploy takes a store copy first;
  - rolling back restores that copy, then deploys the previous tag;
  - slice 1 is not deployed inside the promotion's rollback window.
- **Deploy:** `deploy.sh`, then `deploy-verify` for flotilla.

## 8. Alternatives considered

- **A separate SPA talking to the API.** ADR-0001 deferred it. It would add a second build and a
  second deployable for a UI a few people use.
- **Serve the mockup's canvas files.** They need the canvas runtime and hold sample data.
- **Grafana only.** It cannot act, and it is no place for the decision box.
- **Tailscale identity** (`tailscale serve` identity headers). It needs nothing typed, but it ties
  flotilla's authentication to one network product. The operator chose flotilla's own users
  (2026-11-13).
- **Passwords.** One more secret to store and reset, where a token already serves the CLI and the API.
- **Users in the callers file.** It cannot change at run time; people are managed live, agents by
  config.
- **Freeze at `propose`** (ADR-0008's first meaning). It does not stop today's triage fleet from
  filing.

## 9. Decisions (operator, 2026-11-13)

1. **Signing in:** flotilla's own users and tokens; Tailscale is not used for authentication.
2. **The UI's address:** `https://flotilla.<tailnet>`, through a caddy-tailscale node of its own.
   It provides TLS and the name only.
3. **Slice 1's scope:** as in §6, with all three roles and the Users page.
4. **The freeze:** built now as slice 1b, and it lowers every routine to `shadow`, so nothing goes
   outward.
5. **Roles:** `admin`, `operator`, `viewer`. The first user is an admin, and there is always one.
6. **The installation's cap:** config, shown to admins read-only.
7. **The callers-file `operator` token:** bootstrap and break-glass only.
8. **Playwright** as a repo dependency, for the browser check in the e2e (§7): approved.

**Amendments on acceptance:**

- ADR-0005 and ADR-0008: the freeze lowers to `shadow`.
- ADR-0013 decision 4: approving, rolling back and pruning packs is an admin's.
- `deployment.md`, operator identity: flotilla's own users, not the front proxy's identity.
- PRODUCT.md:
  - several people, with roles;
  - the UI calls the server's functions under the same authorization as the API.

## 10. Discussion

- 2026-11-13: proposed after mockup v3 and DESIGN.md (flotilla d804f02).
- 2026-11-13, revisions 2 and 3: flotilla's own users and tokens, the address, slice 1 as proposed,
  the freeze now, the `admin` role.
- 2026-11-13, revision 4, an independent review. It found:
  - **two blocking gaps:** no permission model (the existing check would let a viewer token run,
    pause and stop, and refuse admins), and a first-admin bootstrap that could only be built by
    letting an agent mint the admin or by an unauthenticated endpoint;
  - **wrong "today" claims in §5**, all mine: run events, History through the API, the caps in
    `status`;
  - **a serving topology that did not fit the deployment:** flotillad sits on an internal network
    with nothing published;
  - the schema step breaking `deploy.sh --rollback`;
  - conflicts with ADR-0005, ADR-0008 and ADR-0013;
  - reasons enforced only in the UI;
  - session, token, page-hardening and audit details;
  - wrong citations: RFC-0001 §5 for §6, and ADR-0001 "rejected" for "deferred".

  All are taken in above. The operator decided the freeze's meaning, the installation's cap, the
  old operator token, and slice 1's scope.
