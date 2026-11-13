# RFC-0007 — The operator UI: from mockup v3 to pages flotillad serves

**Status:** Proposed (2026-11-13), revision 3: the operator's decisions in §9.
**Builds on:** [ADR-0001](../adr/ADR-0001-platform-foundations.md) (the API serves a small web UI in
the same binary; no separate frontend build), [RFC-0001](RFC-0001-fleet-platform.md) §5, §8 and §10.3
(the UI's scope, its security, served by the Go binary), [ADR-0008](../adr/ADR-0008-autonomy.md)
(autonomy and its caps), [ADR-0009](../adr/ADR-0009-host-agent-names.md) (worker, host agent).
**Design sources:** `PRODUCT.md` (who it is for, the five principles), `DESIGN.md` and
`internal/ui/static/flotilla.css` (the design system, "The Control Room"), and the clickable mockup
v3 (a private design canvas, sample data; critiqued twice, 24/40 then 25/40, with every finding
fixed).

## 1. Motivation

The operator works flotilla from a terminal today: `flotilla status`, `flotilla runs`, `pause`,
`resume`, `autonomy`, and SSH to read logs. On a phone that is not possible at all, and PRODUCT.md
makes the phone a first-class place to work: an approval, or a check that nothing is wrong, must be
complete on a phone.

The mockup settled what the UI is: its screens, their grouping (Decide, Watch, Govern, Record), the
decision box with its states, the brakes, and the phone layout. `DESIGN.md` turned it into tokens
and components. What is not settled is how it is served, how the operator signs in, which screens
the API can already feed, and the order of work. This RFC proposes those.

The mockup's files are not the UI. They are a design canvas's artboards with sample data; the UI is
Go templates and the stylesheet, served by flotillad (ADR-0001). What carries over is the design.

## 2. Goals and non-goals

**Goals:**

- The operator can see, on a phone or a laptop, what the fleets did, what is wrong, and what waits
  for them; and act: run, pause, resume, change autonomy, stop or start a fleet.
- Later, decide: approve or reject a plan, with everything needed in view (the decision box).
- Every page meets DESIGN.md: no sideways scroll at 360 to 430px, 44px targets, WCAG 2.2 AA.
- The UI adds no authority. It does exactly what the CLI can do, through the same checks, recorded
  in the same event log under the operator's name.

**Non-goals:**

- A separate frontend build, a JavaScript framework or a single-page app (ADR-0001).
- Screens for things that do not exist yet. Cases, policy, host agents, the brief and knowledge get
  their screens when their backends exist (§6).
- Replacing Grafana. Metrics dashboards stay there; the UI links to them where they explain a run.
- MCP. It is a separate interface (RFC-0001 §5), and an agent never approves.

## 3. Serving

- **flotillad serves the UI**, from the `internal/ui` package: Go `html/template` pages, the embedded
  stylesheet, and a few small scripts. The pages work without JavaScript: every action is a form that
  POSTs and redirects. Scripts add the conveniences the mockup has: the five-second undo countdown,
  copy to clipboard, and the reveal of a changed plan.
- **A listener of its own** (`ui_listen`), separate from the full API on loopback and the workers'
  ledger listener. It is reachable on the tailnet only (RFC-0001 §8). The address is
  `https://flotilla.<tailnet>`: `tailscale serve` on the mini gives the name and HTTPS, and proxies
  to the listener. It plays no part in signing in (§4).
- **The UI calls the server's own functions**, not its HTTP API, but through the same authorization
  path. A UI action is the same control write as the CLI's, with the same checks: `resume` and
  `autonomy` are operator-only, and `pause` and `stop` are open to agents too. The event log records
  `via: ui`.
- **Fonts are served from the binary.** Inter and JetBrains Mono, as woff2, embedded under
  `static/fonts/`. Nothing is fetched from a third party; the UI must work with the tailnet as its
  only network.
- **Freshness:** pages render from the store at request time. A small script refreshes the Now and
  Runs pages every 30 s while they are visible. There is no push channel in the first slices.

## 4. Users and signing in

flotilla has its own simple user management, and sign-in rests on tokens. Authentication is
flotilla's own and does not depend on the network it runs on (operator, 2026-11-13). Tailscale
carries the UI's address and HTTPS (§3), never the identity.

- **Users** are people. Each user has a name, a role and a token, stored in flotillad's store: the
  token's SHA-256 only, never the token itself. Users are separate from the callers file, which keeps
  the agents' and automation's tokens.
- **Roles,** each one including the one below it:
  - `admin` is an operator who also decides who and what this installation trusts:
    1. **users and sessions:** add, rotate and revoke users, change roles, end anyone's sessions;
    2. **the installation's caps:** the highest autonomy any routine may reach here (ADR-0013: the
       installation's cap, a fourth cap in "lowest wins"; RFC-0006 §7), which bounds every
       operator's per-routine autonomy;
    3. **trusting fleet packs:** approving a pack set that is not built in (ADR-0013, approval by set
       hash): new behaviour on the installation, the same kind of decision as adding a person;
    4. **the audit view:** user and trust changes with who made them (every operator still sees the
       whole event log).
  - `operator` sees everything and does the day's work: run, pause, resume, autonomy per routine, stop
    and start a fleet, the freeze (anyone may pull it; unfreezing needs a reason), and later
    approvals. Nothing an operator needs day to day waits for an admin.
  - `viewer` sees everything and changes nothing.

  Approvals and every loosening action stay with operators and admins; an agent never reaches them
  (RFC-0001 §8).
- **Managing users:** `flotilla user add <name> --role admin|operator|viewer` prints the new token
  once. `flotilla user list`, `user role <name> <role>`, `user rotate <name>` (prints a new token
  once) and `user revoke <name>`. An admin can do the same in the UI, on a Users page under Govern.
  Every change is an event with the acting admin's name.
- **There is always an admin.** The last admin cannot be revoked or lose the role.
- **The first user is an admin.** On a new installation, with an empty user table,
  `flotilla user add <name> --role admin` run on the host (through `docker exec`, as the CLI runs
  today) creates it; any other role is refused until an admin exists. After that, only an admin adds
  users. There is no default user and no default password.
- **Signing in:** the user pastes their token once per device. flotillad checks its hash and starts
  a server-side session, with an `HttpOnly`, `Secure`, `SameSite=Strict` cookie that holds only a
  random session id, for 30 days. Revoking or rotating a user ends their sessions at once. "Sign out
  everywhere" ends the user's own sessions.
- **The same token works in the CLI and the API.** A user's token is a bearer token with the user's
  role, so `flotilla` on a laptop and the UI act as the same person. The event log records the
  user's name and `via: ui`, `via: api` or `via: cli`.

**Always:**

- Every POST carries a CSRF token, besides the `SameSite=Strict` cookie.
- Sign-in attempts are rate-limited, and failures are logged without the token.
- Loosening actions need a typed reason (DESIGN.md, the brakes); the reason goes into the event's
  detail.
- Approvals never come through MCP.

## 5. Screens and what feeds them today

From the mockup's 16 screens. **Today** means the store or the API holds the data now; **later**
names the backend that is still missing.

| Group | Screen | Data | Today |
| --- | --- | --- | --- |
| Decide | Now | routines and their health (`status`), recent runs and failures (`runs`, `events`), what waits for the operator | **partly**: health and runs today; "waits for you" when cases exist |
| Decide | Approvals | plans waiting, with their hash | later: cases and policy (ADR-0005) |
| Watch | Cases, Case | cases, their timeline, the decision | later: cases (ADR-0005) |
| Watch | Runs | `runs`: id, routine, trigger, start, duration, status, spend, trace id | **yes** |
| Watch | Run (the mockup's Trace) | one run, its events; its trace in VictoriaTraces and Langfuse, linked | **yes** (spans linked, not drawn in the first slice) |
| Watch | Fleets, Fleet | routines per fleet, controls, last runs, the pack (ADR-0013) | **yes**, pack hashes when packs land |
| Watch | Hosts, Host | host agents, their allowlists and executions | later: host agents (ADR-0006) |
| Govern | Autonomy | each routine's autonomy, the caps, the brakes | **partly**: autonomy per routine and fleet STOP today; per-action levels with ADR-0008's full model |
| Govern | Models and spend | model aliases, spend by fleet | **partly**: aliases from config, run spend from `runs`; tokens and engines from LiteLLM later |
| Govern | Knowledge | memory and sources | later: ADR-0010 |
| Record | History | decisions and filings over time | **yes**: the ledger's namespaces (`ledger/entries`, `ledger/filings`) |
| Record | Brief, Earlier briefs | the daily brief | later: `ops/daily-brief` (RFC-0001 §9.4) |

## 6. The order of work

Each slice is deployed when it is done, and each ends the same way (§7).

**Slice 1 — watch and act on what exists.**

- Users (§4): the store's users table with the `admin`, `operator` and `viewer` roles, the
  `flotilla user` commands, the first admin, sign-in and sessions, and the Users page for admins.
  The installation's cap and pack trust get their admin pages when the cap's setting and the packs
  land (ADR-0013).
- The shell: the navigation (four groups, the pages of the active group, the phone bottom bar), and
  the fonts.
- Now, as health plus what is wrong: failed or stale routines, failed runs. It shows no decisions
  yet.
- Runs and a run's page.
- Fleets and a fleet's page, with its controls: run now, pause, resume, autonomy, stop and start
  the fleet.
- History, from the ledger.
- Autonomy, with the brakes as they exist today: per-routine autonomy and fleet STOP.

**Slice 1b — one platform-wide freeze** (§9, question 4). The mockup's "Freeze all autonomy" caps
every routine at `propose` with one switch, and unfreezing asks for a reason. This needs a new
control in flotillad (a cap in ADR-0008's sense, applied by the scheduler and recorded). It is
small, but it is a behaviour change, so it is decided apart from the UI.

**Slice 2 — decide.** Approvals and Case, with the decision box and every state the mockup tested:
the changed plan with its diff, the undo window, a failed send, CLI-only plans and the plan hash. It
comes when cases and the policy exist. The mockup's states are the contract that API has to meet.

**Slice 3 — the rest,** as each backend lands: Hosts with host agents, Brief with the ops fleet,
Knowledge with ADR-0010, Models with LiteLLM's numbers.

## 7. How each slice is checked

- Go tests for the handlers:
  - a page renders from a seeded store;
  - each action writes the same control as the CLI;
  - an agent's or a missing identity is refused;
  - a POST without its CSRF token is refused.
- A Playwright check at 360, 390 and 1440px: no sideways scroll, no target under 44px, no console
  error. This is the mockup's `check.js`, turned into a test in the repo that runs in the e2e.
- `ui_test.go` keeps the stylesheet's colours equal to DESIGN.md.
- An impeccable critique of the real pages, and DESIGN.md updated when the design moves.
- Deploy with `deploy.sh`, then `deploy-verify` for flotilla.

## 8. Alternatives considered

- **A separate SPA (React or similar) talking to the API.** Rejected by ADR-0001: a second build and
  a second deployable, for a UI one operator uses.
- **Serve the mockup's canvas files.** They need the canvas runtime and hold sample data; they are a
  design, not an app.
- **Grafana only.** Grafana shows metrics but cannot act (pause, autonomy, approve), and it is no
  place for the decision box.
- **Tailscale identity** (`tailscale serve` identity headers). It needs nothing typed, but it ties
  flotilla's authentication to one network product. The operator chose flotilla's own users and
  tokens, so that it stands on its own wherever it runs (2026-11-13).
- **Passwords.** They are one more secret to store and reset. A token is already what the CLI and the
  API use, and a password manager keeps it as well as a password.
- **Users in the callers file** (YAML, deployed with config). It cannot change at run time, and
  adding a person would mean a deploy; people are managed live, agents by config.

## 9. Decisions (operator, 2026-11-13)

1. **Signing in:** flotilla's own simple user management, with tokens (§4); Tailscale is not used
   for authentication.
2. **The UI's address:** `https://flotilla.<tailnet>` through `tailscale serve`, which provides the
   name and HTTPS only.
3. **Slice 1's scope:** as in §6, with users added.
4. **The platform-wide freeze:** built now, as slice 1b.

5. **Roles:** `admin`, `operator` and `viewer` (§4). An admin is an operator who also manages users
   and sessions, the installation's caps, trust in fleet packs, and sees the audit view. The first
   user of an installation is an admin, and there is always one.

## 10. Discussion

- 2026-11-13: proposed after mockup v3 and DESIGN.md (flotilla d804f02).
- 2026-11-13, revision 2: the operator chose flotilla's own users and tokens over Tailscale
  identity, the `tailscale serve` address, slice 1 as proposed, and the freeze now (§9).
- 2026-11-13, revision 3: the `admin` role (§4, §9.5): user management, the installation's caps, pack
  trust and the audit view; the first user is an admin, and the last admin cannot be removed.
