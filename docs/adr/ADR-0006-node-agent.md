# ADR-0006 — The flotilla node agent: host actions and connectors, on any host

**Status:** Accepted (operator, 2026-11-09), after its open questions were answered and an advisor
review's changes applied the same day.
**Date:** 2026-11-09 **Builds on:** [ADR-0005](ADR-0005-agent-pipeline.md) (writes run through an
actuator, designed in its own ADR), [ADR-0002](ADR-0002-run-in-docker.md) (deploys over SSH).

## Context

ADR-0005 needs host writes (restart a container, rerun a backup job) to run through a privileged
component on each host, decided by policy in flotillad and executed under strict limits. The operator,
2026-11-09: think beyond this homelab. How anyone deploys host agents and communication connectors
should follow one standard, for deployment and for security, and be able to meet compliance needs.
The mini, the DGX and the production VPS are three installations of that standard.

What the standard needs from a host: an account with sudo for the operator, reached over SSH.

Facts on the first host, the mini (checked 2026-11-09): the operator account has passwordless sudo,
and root LaunchDaemons are installed by `infra/mini-setup.sh`; a dedicated account with exactly
listed sudo commands already exists (`_dockerdev`, checked with `visudo -c`); the production Docker
engine is driven as `_dockerhost`. The backup job (`com.homelab.db-backup`) is a root LaunchDaemon,
so `launchctl kickstart system/…` needs sudo. The agent connects out, so whether containers can
reach a listener on the Mac does not matter.

## Decision

### 1. One agent, `flotilla-node`, in Go

A single static binary per platform (macOS and Linux, amd64 and arm64), sharing its protocol and
signing code with flotillad. Roles, enabled by the host's config:

- **actuator:** runs the host's allowed writes;
- **connector:** talks to a communication provider (Slack first) on flotillad's behalf, with that
  provider's credentials, so flotillad holds none (decided 4: on the mini, the same instance as the
  actuator).

### 2. It connects out

The agent dials flotillad and keeps one long-lived connection; flotillad sends work down it. No host
opens an inbound port, so the Docker VM on the mini, a VPS firewall and the DGX's network make no
difference. flotillad exposes one endpoint for agents and **terminates the mutual TLS itself**; a
proxy in front may only pass TCP through. How the endpoint is reached is the installation's choice
(decided 1). A lost connection means no executions: flotillad marks the host unreachable, and an
execution in flight becomes `unknown` (ADR-0005).

The agent opens exactly two outbound connections: flotillad, and the provider when it is the
connector.

### 3. Identity

- **Enrolment:** `flotilla node install` mints an enrolment token through flotillad's API with the
  operator's token: bound to the node name, single use, valid for 15 minutes, carried to the host
  only inside the SSH session. The agent generates its key pair on the host and sends a certificate
  request; flotillad's intermediate CA issues a client certificate for that node name (30 days),
  renewed over the connection. The private key never leaves the host.
- **The node id** is the name in the agent's certificate. Executions name it (§4).
- **Revocation needs no access to the host:** `flotilla node revoke <host>` marks the certificate
  revoked in flotillad, which checks every connecting certificate against its node table (it is the
  only verifier, so no CRL or OCSP). `remove` is revoke plus uninstall over SSH. An agent down past
  its certificate's expiry is enrolled again.
- The agent pins the **root** CA (so the intermediate can rotate); flotillad's server certificate
  comes from the same CA. The agent's config lists the accepted execution-signing public keys: one,
  or two during a rotation (§11).

### 4. Signed executions

flotillad signs every execution with an Ed25519 key only it holds. The envelope carries: execution
id, case id, node id, capability, parameters, the plan's hash, how it was approved (`auto` or the
operator), `signed_at` and `expires_at`. **What is signed is the exact bytes sent:** the agent
verifies the signature over the bytes it received, then decodes them; there is no canonicalization
on either side.

flotillad signs after the approval, so the expiry is short: 10 minutes from `signed_at` by default.
The agent runs an execution only if:

- the signature verifies against an accepted key;
- it is not expired and not signed more than 10 minutes in the future;
- the node id is this agent's;
- the capability is in **this host's allowlist** (§5), and the parameters pass its checks;
- `auto` executions are within **this host's autonomy ceiling**, and the capability is within its
  **rate limit on this host** (§5).

**An execution id seen before:** if the envelope is byte-identical to the one recorded, the agent
returns the recorded result and runs nothing (a resend after a lost result); if it differs, the agent
refuses it and logs a forgery attempt.

The agent reports its clock in every heartbeat; `flotilla node list` shows the offset, and a skew
above 30 seconds alerts.

A compromised flotillad still cannot make an agent run anything its host does not list, nor more
often than the host allows.

### 5. The host's allowlist, ceiling and limits

Each host has a reviewed config in git (the installation's private config), installed with the agent
and changed only by a reviewed deploy:

- the capabilities this host allows, each mapped to an **exact command template** with typed,
  validated parameters, and its own **rate limit** (at most N executions per hour on this host),
  enforced by the agent;
- the sudoers lines, generated from the same file and checked with `visudo -c` before install. **A
  parameter that reaches a privileged command is an enum:** the generator writes one sudoers line per
  allowed value, with absolute paths and no wildcards. Patterns are allowed only for parameters the
  agent checks itself and passes to an unprivileged command;
- the highest autonomy this host accepts (a new host: propose).

The agent never runs a shell, never builds a command from free text, and refuses anything not in the
file. Model-written commands (ADR-0005: decided later) would arrive, if ever, as one more allowlisted
capability with its own rules.

### 6. Least privilege

- A dedicated user (`_flotilla` on macOS, `flotilla` on Linux), no login, no shell.
- `sudo` without a password only for the exact lines in §5; Docker through the engine's own user
  (on the mini: `sudo -u _dockerhost docker …`), never membership of a Docker group.
- **The binary, the allowlist, the sudoers file and the service unit are root-owned** and not
  writable by the agent's user, so a compromised agent cannot widen its own allowlist. The private
  key, the certificate and the store are owned by the agent's user, mode 600.

### 7. Execution

- **Idempotent by execution id:** the agent commits `started` for the id before it runs the command,
  and the result after. On restart, a `started` row with no result is reported `unknown` and never
  run again.
- **Preconditions are local only:** files (the newest backup's age in the backup directory),
  `launchctl print`, `systemctl is-active`, `docker inspect`. When they no longer hold, the result is
  `precondition_false` (ADR-0005: resolved itself). Checks that read metrics are the acting worker's.
- **Limits:** a timeout per capability (past it: `unknown`, the target held); output captured and
  size-capped.

### 8. Audit

- A local append-only log (JSONL in a root-owned directory), each entry hash-chained to the previous,
  collected off the host by the host's log collector (on the mini: Alloy into VictoriaLogs). No
  shipping code in the agent.
- Every result the agent sends carries its log's current chain head; flotillad stores the head per
  node. A log that no longer leads to the stored head has been rewritten.
- Every entry: who approved, what ran (the resolved command), when, the result, the output's hash.
- **Before the first write at propose:** `flotilla node audit <host>` compares the host's log with
  flotillad's record of the case; a mismatch alerts.

### 9. Release and install

- **v1:** the operator builds the binary from a tagged commit and installs it over SSH; SSH gives the
  integrity, and the builder is the person with root on the host.
- **Before the second host:** releases built by CI for each platform and signed with a key kept only
  in CI's secret store (anyone with admin on the repository can reach it; keyless signing with the CI
  identity is the alternative when images get signed). The release public key is pinned in the CLI
  and the agent; the installer verifies before anything runs.
- **Install, upgrade, revoke, remove** from one place: `flotilla node install|upgrade|revoke|remove
  <host>`, run by the operator over SSH. Install copies the binary, creates the user, installs the
  service (launchd or systemd) and the sudoers lines, writes the host's config and enrols it.
- **Upgrades are pushed** this way: explicit, one host at a time. Self-update comes later, when there
  are more hosts, and only with signed releases.

### 10. Inventory and health

- `flotilla node list`: each host's version, last contact, clock offset, allowlist hash, autonomy
  ceiling, and certificate expiry.
- Alerts: an agent not connected for N minutes; a certificate close to expiry; clock skew.
- **Drift** (later, before the second host): the CLI records the installed allowlist hash in `nodes`
  at install and upgrade; the agent reports its hash on connect; a difference alerts.

### 11. Keys and rotation

- The root CA offline with the operator; the intermediate CA and the execution-signing key in
  flotillad's secrets volume (decided 2).
- **Execution-key rotation:** add the new public key to each host's config by a reviewed deploy,
  switch the signer, remove the old key.
- The written procedures for this, for the intermediate CA and for a host's certificate come
  **before the first write at propose**.

### 12. v1 scope

v1 is one host (the mini), one write (`backup.rerun_observability`), in shadow: flotillad records the
plan and sends nothing to the host. v1 builds §1 to §7, the log and chain head of §8, the operator
build of §9, `node list` and the disconnected alert of §10. The connector is **outbound only**
(notifications); relaying approvals comes with the first promotion to propose, since v1 approves by
CLI.

### 13. What this gives for compliance

- **Audit trail** per action, on the host and in flotillad, tamper-evident once the chain head is
  anchored in flotillad.
- **Separation of duties:** agents plan, policy decides, the operator approves where required, the host
  executes, each with its own identity and key.
- **Least privilege** per host and per action, with a per-host rate limit.
- **Change control:** a host's allowlist changes only through a reviewed deploy.
- **Key management:** per-host keys generated on the host; short-lived certificates; revocation;
  release signing separate from execution signing.

## Consequences

**Positive:**

- One way to add a host: install the agent, write its allowlist. The same on the mini, the DGX and the
  VPS, and for anyone else running flotilla.
- No inbound ports on hosts; the reachability problems of each network disappear.
- A compromised control plane is limited to each host's allowlist and rate limits.

**Negative:**

- New code with the most privilege in the system; it needs its own tests, review and a security-minded
  e2e before its first write.
- New moving parts: a certificate authority, two signing keys, an agent endpoint on flotillad.
- New dependencies, approved when built (ADR-0003): a websocket library for Go if the node protocol
  uses one (a TLS stream with length-prefixed frames would not); Slack's Socket Mode needs one, and
  the Socket Mode client moves from Python (ADR-0003, pending) to Go.

**Neutral:**

- ADR-0002's actuator exception is no longer needed: nothing holds the Docker socket.
- **Amends ADR-0005** (decision 4; RFC-0004 §2, §3.6, §3.8, §4, §7, §12 row 9 and §14):
  - workers execute repo writes; flotillad sends signed host writes to host agents, not the acting
    worker. The acting worker plans and runs the check; `ops-act-host` holds no host key;
  - the connector, not flotillad, holds the provider's credentials;
  - flotillad still holds no fleet credential: the execution-signing key is flotilla's own, and what
    it can do is bounded by every host's allowlist and rate limit.

## Alternatives considered

- **An inbound service on each host** (flotillad or a worker calls it): each network needs its own
  route in (the Docker VM's gateway, VPS firewalls, the DGX), and every host exposes a port. Rejected.
- **SSH forced commands:** no idempotency, no local audit, no signed requests without extra work, and
  flotilla's workers would hold SSH keys to every host. Rejected (RFC-0004 §12, decision 12).
- **Configuration management** (Ansible, Salt) for both install and execution: a second control plane
  beside flotillad, and no per-action policy. Rejected for execution; possible later for installing
  at scale.
- **Rust or Python for the agent:** Rust adds a second language and shares nothing with flotillad;
  Python needs an interpreter and packages on every host, in the most privileged component. Go chosen
  (operator, 2026-11-09).
- **Self-update from v1:** convenient, but only safe with release signing fully separate; pushed upgrades
  first.

## Decided (operator, 2026-11-09)

1. **The agent endpoint is generic:** flotillad listens on one configurable port and requires mutual
   TLS from every agent. How agents reach it is the installation's choice (a tailnet, a VPN, a private
   network, the internet): mutual TLS is the protection, so nothing assumes Tailscale. This homelab
   publishes it on its tailnet only.
2. **Keys split by duty:** the CA's root offline with the operator (as the backup decryption key is),
   an intermediate CA in flotillad's secrets volume; the execution-signing key in flotillad's secrets
   volume; the release-signing key only in CI (from the second host, §9).
3. **Signing:** Ed25519 from Go's standard library, over the exact bytes sent (§4), in our own small
   envelope shared by flotillad and the agent, and tested hard (no dependency).
4. **The Slack connector is a role of the host agent on the mini, one instance with both roles.**
   Accepted trade-off: a breach of that process exposes the host's writes, the Slack tokens, and,
   once the connector relays approvals, the ability to forge approvals for any write Slack may
   approve, on any host. The low and medium risk limit, the daily cap and switching to CLI-only
   approvals bound it. Only the node configured as the provider's connector may relay approvals;
   flotillad refuses relayed approvals from any other node. A second instance under its own user
   remains the fix, decided again at the first promotion to propose.
5. **The local store is SQLite,** with the pure-Go driver flotillad already uses; `started` is
   recorded before the command runs (§7).

## References

- [ADR-0005](ADR-0005-agent-pipeline.md) and [RFC-0004](../rfc/RFC-0004-agent-pipeline.md): the
  pipeline this agent executes for.
- [ADR-0002](ADR-0002-run-in-docker.md): deploys over SSH; [ADR-0003](ADR-0003-dependencies-and-images.md):
  dependencies.
- [Security](../security.md) T13 to T17; [Deployment](../architecture/deployment.md).
