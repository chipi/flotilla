# ADR-0002 — Run flotilla in Docker, one worker per fleet

**Status:** Proposed. Waiting for the operator's decision.
**Date:** 2026-11-08

## Context

ADR-0001 puts flotilla on the homelab mini, as the `claude` account. Today the fleets run as host
processes:
- fleetd as a LaunchDaemon under the operator's account;
- the triage fleet as `python3` from the git checkout, with the mini's system Python 3.9;
- the brief as a launchd Node job.

Their credentials sit in files the whole account can read. The operator asked whether flotilla
should run in Docker.

## Decision (proposed)

Run flotilla in Docker in the mini's colima VM, like the homelab's other services:
- **flotillad** in one container, with the state volume and no fleet secrets;
- **one long-running worker container per fleet**, holding only that fleet's secrets and toolchain;
  each run is a process inside its fleet's worker;
- **no Docker socket** in any container;
- every container hardened (see [Deployment](../architecture/deployment.md)).

## Consequences

**Positive:**
- **Isolation:** a run cannot read the host, the operator's files, or another fleet's credentials.
- **A known runtime:** pinned Python and Go per image, not the host's Python 3.9.
- **Same operation as the other homelab services:** compose, logs through Alloy, deploy events,
  rollback to a previous tag.
- **Resource limits** per fleet.

**Negative:**
- More moving parts: images, a registry, compose files.
- The bugfix fleet's worker needs a full toolchain.
- Debugging goes through `docker exec` and logs, not a shell in the checkout.

**Neutral:**
- The migration's phase 2 (flotillad running the triage fleet unchanged) runs the existing code
  inside a worker image, instead of from the checkout.

## Alternatives considered

- **Host processes under launchd** (as today). Simplest, but no isolation between fleets, and the
  host's runtime versions.
- **A container per run, started by flotillad.** Strongest isolation, but flotillad would need the
  Docker socket, which is root-equivalent on the VM. Rejected for that.
- **One container for everything.** Less to run, but every fleet's secrets in one place. Rejected.
