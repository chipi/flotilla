# ADR-0002 — Run flotilla in Docker, one worker per fleet

**Status:** Accepted (operator, 2026-11-08)
**Date:** 2026-11-08

## Context

ADR-0001 puts flotilla on the homelab mini, as the `claude` account. Today the fleets run as host
processes:
- fleetd as a LaunchDaemon under the operator's account;
- the triage fleet as `python3` from the git checkout, with the mini's system Python 3.9;
- the brief as a launchd Node job.

Their credentials sit in files the whole account can read. The operator asked whether flotilla
should run in Docker.

## Decision

Run flotilla in Docker on the mini, like the homelab's other services:
- **flotillad** in one container, with the state volume and no fleet secrets;
- **one long-running worker container per fleet**, holding only that fleet's secrets and toolchain;
  each run is a process inside its fleet's worker;
- **no Docker socket** in any container;
- every container hardened, with deny-by-default egress (see
  [Deployment](../architecture/deployment.md)).

**Which engine, and who deploys** (operator's conditions, 2026-11-08: "run like any other Docker
thing we have", and agents must be able to redeploy it):
- flotilla is one more compose stack in the mini's production engine (the homelab's ADR-0009),
  next to the observability backend, GlitchTip and the others: a compose file in the flotilla
  checkout under the operator account, brought up with `docker compose up -d` there.
- Agents redeploy it the way they redeploy those stacks today: over SSH to the operator account
  with the key the homelab's operations handbook names, then `docker compose pull && docker compose
  up -d` in the checkout.
- The production engine's socket is `root:admin 0660`, reachable from the operator account only, so
  this replaces "as the `claude` account" in ADR-0001's decision 2. Inside the containers every
  process runs as a non-root user.
- Development and end-to-end tests use the dev engine, never the production one.

The defaults for everything the deployment leaves open are in
[Deployment → Defaults](../architecture/deployment.md#defaults).

## Consequences

**Positive:**
- **Isolation:** a run cannot read the host, the operator's files, or another fleet's credentials.
- **A known runtime:** pinned Python and Go per image, not the host's Python 3.9.
- **Same operation as the other homelab services:** compose, logs through Alloy, deploy events,
  rollback to a previous tag.
- **Resource limits** per fleet.

**Negative:**
- More moving parts: images, a registry, compose files, an egress proxy.
- The bugfix fleet's worker needs a full toolchain.
- Debugging goes through `docker exec` and logs, not a shell in the checkout.
- Deploys go through the operator account's SSH access, as for every production stack on the mini;
  the `claude` account on the mini cannot deploy it.

**Neutral:**
- The migration's phase 2 (flotillad running the triage fleet unchanged) runs the existing code
  inside a worker image, instead of from the checkout.

## Alternatives considered

- **Host processes under launchd** (as today). Simplest, but no isolation between fleets, and the
  host's runtime versions.
- **A container per run, started by flotillad.** Strongest isolation, but flotillad would need the
  Docker socket, which is root-equivalent on the VM. Rejected for that.
- **One container for everything.** Less to run, but every fleet's secrets in one place. Rejected.
- **The `claude` account's own engine.** It would let agents deploy, but ADR-0009 retires that
  account's private colima, and a production service would then live outside production's
  monitoring and backups. Rejected.
