# ADR-0014 — The operator UI, users and roles

**Status:** Accepted

**Date:** 2026-11-13

**Decided by:** the operator, on [RFC-0007](../rfc/RFC-0007-operator-ui.md) (revision 4, after an
independent review whose two blocking findings and corrections it took in)

## Context

The operator works flotilla from a terminal, which is not possible on a phone. PRODUCT.md makes the
phone a first-class place to work. ADR-0001 and RFC-0001 §10.3 already decided that flotillad
serves a small web UI from the same binary, without a separate frontend build. Two things were open
until now: who signs in and what each person may do, and how the UI is reached in the reference
installation, where flotillad sits on an internal Docker network. Mockup v3 and `DESIGN.md` settled
the UI's design.

## Decision

1. **flotillad serves the UI** from `internal/ui`: Go `html/template` pages, the embedded stylesheet
   (`DESIGN.md` is its source) and fonts, and a few small scripts. Every action is a form that works
   without JavaScript.
2. **One authorization path.** The API middleware and the UI handlers both call one
   `authorize(caller, action)`, which implements RFC-0007 §4's table. The UI adds no authority, and
   every rule it enforces is also enforced by the API. For example, loosening actions (resume, start,
   raising autonomy, unfreeze) need a reason in every client.
3. **flotilla's own users,** stored in flotillad's store:
   - names unique across users and callers;
   - server-generated 256-bit tokens, of which the store keeps only the SHA-256;
   - revoked users kept, never renamed or reused;
   - an actor of `user:<name>` in the event log.

   Authentication does not depend on the network flotilla runs on.
4. **Roles** `admin`, `operator` and `viewer`, each including the one below:
   - **admin** adds users and sessions, trust in fleet packs (approve, roll back, prune), and the
     audit view;
   - **operator** is the day's work, including the freeze and, later, approvals;
   - **viewer** only reads.

   Callers (`agent`, `worker`) stay outside this order, as RFC-0007 §4 sets out.
5. **The first user is an admin, and there is always one.**
   - Only the callers-file `operator` token can create the first admin (`user.bootstrap`). Once an
     admin exists, that token can still add an admin, and nothing else (break-glass). flotillad can't
     tell an admin whose token was lost from one who is fine, so this is not limited to "every admin
     is gone". Each use is recorded as `user.break-glass`, by the host-held token. The token cannot
     sign in.
   - Agent tokens never create users.
   - Removing or demoting the last admin is refused.
6. **Sessions:** a token pasted once per device starts a server-side session.
   - The `__Host-` cookie holds only a random id, hashed in the store.
   - A session lasts 30 days, absolute, and ends after 7 days idle.
   - The role is read on every request.
   - Every POST carries a CSRF token.
7. **The address** is `https://flotilla.<tailnet>`, through a caddy-tailscale node of flotilla's own
   on a small `ui` network shared only with flotillad's UI listener. The proxy terminates TLS and
   has no part in identity, and Funnel is never on.
   - The UI listener serves the pages, and `/v1` for user tokens only.
   - It refuses the ledger, worker and callers tokens, cookies on `/v1`, and foreign hosts.
8. **The freeze lowers every routine to `shadow`:** nothing outward happens until it is lifted.
   - It never overwrites a routine's own control.
   - A run in flight keeps the autonomy it started with.
   - Unfreezing needs a reason.
   - STOP stays the stronger brake (no runs).
9. **The installation's cap stays config** (ADR-0013's `max_autonomy`); admins see it read-only.
10. **The order of work** is RFC-0007 §6:
    - slice 1, users and watching and acting on what exists;
    - slice 1b, the freeze;
    - slice 2, deciding, when cases exist;
    - slice 3, the rest.

    Each slice is checked and shipped as in RFC-0007 §7, including the store-copy rollback for its
    schema step. Playwright joins the repo for the browser check (operator, 2026-11-13).

## Consequences

**Positive:**

- People can watch and act from a phone.
- Every permission is one table with one test that walks it.
- The callers file keeps agents and automation, while people are managed live.

**Negative:**

- flotilla now owns authentication: tokens, sessions, CSRF and their tests.
- A store schema step, so the first UI deploy needs the store-copy rollback.
- A second network and a proxy container in the reference installation.

**Neutral:**

- The freeze's meaning changes from ADR-0005 and ADR-0008, which named it but never built it.

## Amends

- [ADR-0005](ADR-0005-agent-pipeline.md) decision 4 and [ADR-0008](ADR-0008-autonomy.md): the freeze
  lowers to `shadow`.
- [ADR-0013](ADR-0013-fleet-packs.md) decision 4: approving, rolling back and pruning packs is an
  admin's.
- `docs/architecture/deployment.md`, operator identity: flotilla's own users, not the front proxy's
  identity.
- `PRODUCT.md`: several people with roles; the UI calls the server's functions under the same
  authorization as the API.

## Alternatives considered

RFC-0007 §8: a separate SPA (deferred by ADR-0001), serving the mockup's canvas files, Grafana only,
Tailscale identity, passwords, users in the callers file, and a freeze at `propose`.
