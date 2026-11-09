# ADR-0006 — The flotilla node agent: host actions and connectors, on any host

**Status:** Proposed (2026-11-09). Nothing is built from it until the operator accepts it.
**Date:** 2026-11-09
**Builds on:** [ADR-0005](ADR-0005-agent-pipeline.md) (writes run through an actuator, designed in its
own ADR), [ADR-0002](ADR-0002-run-in-docker.md) (deploys over SSH; the actuator is the one exception to
"no Docker socket").

## Context

ADR-0005 needs host writes (restart a container, rerun a backup job) to run through a privileged
component on each host, decided by policy in flotillad and executed under strict limits. The operator,
2026-11-09: think beyond this homelab. How anyone deploys host agents and communication connectors
should follow one standard, for deployment and for security, and be able to meet compliance needs.
The mini, the DGX and the production VPS are three installations of that standard.

Facts on the first host, the mini (checked 2026-11-09): the operator account has passwordless sudo,
and root LaunchDaemons are installed by `infra/mini-setup.sh`; a dedicated account with exactly
listed sudo commands already exists (`_dockerdev`); the production Docker engine is driven as
`_dockerhost` (the weekly prune). Containers resolve `host.docker.internal` to the Docker VM's gateway
(192.168.5.2); whether a Mac-local listener is reachable from containers was not established (the
probe was faulty), which the outbound design below makes irrelevant.

## Decision

### 1. One agent, `flotilla-node`, in Go

A single static binary per platform (macOS and Linux, amd64 and arm64), sharing its protocol and
signing code with flotillad. Roles, enabled by the host's config:
- **actuator:** runs the host's allowed writes;
- **connector** (later): talks to a communication provider (Slack first) on flotillad's behalf, with
  that provider's credentials, so flotillad holds none.

### 2. It connects out

The agent dials flotillad and keeps one long-lived connection (TLS, a websocket); flotillad sends
work down it. No host opens an inbound port, so the Docker VM on the mini, a VPS firewall and the
DGX's network make no difference. flotillad exposes one endpoint for agents on the tailnet, behind
mutual TLS (open question 1). A lost connection means no executions: flotillad marks the host
unreachable, and an execution in flight becomes `unknown` (ADR-0005).

### 3. Identity

- **Enrolment:** at install the agent generates its key pair on the host; it presents a one-time
  enrolment token; flotillad's certificate authority issues a short-lived client certificate (30 days),
  renewed over the connection. The private key never leaves the host.
- **Revocation:** removing a host revokes its certificate; a revoked agent cannot connect.
- The agent pins flotillad's CA and the public key flotillad signs executions with (§4).

### 4. Signed executions

flotillad signs every execution request with an Ed25519 key only it holds: execution id, case id,
host, capability, parameters, the plan's hash, how it was approved (`auto` or the operator), an expiry
and a nonce. The agent runs it only if:
- the signature verifies;
- it is not expired, and its execution id was never seen (replay);
- the host is this host;
- the capability is in **this host's allowlist** (§5), and the parameters pass its checks;
- `auto` executions are within **this host's autonomy ceiling** (§5).

A compromised flotillad still cannot make an agent run anything its host does not list.

### 5. The host's allowlist and ceiling

Each host has a reviewed config in git (the operator's private config repo), installed with the
agent and changed only by a reviewed deploy:
- the capabilities this host allows, each mapped to an **exact command template** with typed,
  validated parameters (enums, patterns, checks such as "the container carries the `restartable`
  label");
- the sudoers lines, generated from the same file and checked with `visudo -c` before install;
- the highest autonomy this host accepts (a new host: propose).

The agent never runs a shell, never builds a command from free text, and refuses anything not in the
file. Model-written commands (ADR-0005: decided later) would arrive, if ever, as one more allowlisted
capability with its own rules.

### 6. Least privilege

- A dedicated user (`_flotilla` on macOS, `flotilla` on Linux), no login, no shell.
- `sudo` without a password only for the exact lines in §5; Docker through the engine's own user
  (on the mini: `sudo -u _dockerhost docker …`), never membership of a Docker group.
- The agent's files (binary, config, key, state) owned by root or the agent's user, mode-restricted.

### 7. Execution

- **Idempotent by execution id:** a local store records every execution id and its result; a repeat
  returns the recorded result and runs nothing.
- **Preconditions checked on the host** (a metric read, `launchctl print`, `systemctl is-active`):
  when they no longer hold, the result is `precondition_false` (ADR-0005: resolved itself).
- **Limits:** a timeout per capability (past it: `unknown`, the target held); output captured and
  size-capped.

### 8. Audit

- A local append-only log, each entry hash-chained to the previous, shipped to VictoriaLogs.
- flotillad records the same execution on the case; the two records can be compared, so a mismatch
  (an execution the agent ran that flotillad did not sign, or the reverse) is detectable.
- Every entry: who approved, what ran (the resolved command), when, the result, the output's hash.

### 9. Release and install

- **Releases** are built by CI for each platform and signed with a key CI alone holds; the agent's
  installer verifies the signature before anything runs.
- **Install, upgrade, remove** from one place: `flotilla node install|upgrade|remove <host>`, run by the
  operator over SSH (the keys and the practice ADR-0002 already uses). Install copies the verified
  binary, creates the user, installs the service (launchd or systemd) and the sudoers lines, writes the
  host's config and enrols it.
- **Upgrades are pushed** this way in v1: explicit, one host at a time. Self-update (the agent fetches
  signed releases itself) comes later, when there are more hosts; it depends on the release key being
  separate from flotillad.

### 10. Inventory and health

- `flotilla node list`: each host's version, last contact, allowlist hash, autonomy ceiling, and
  certificate expiry.
- Alerts: an agent not connected for N minutes; a certificate close to expiry; a host's allowlist hash
  differing from the reviewed config (drift).

### 11. What this gives for compliance

- **Audit trail** per action, on the host and in flotillad, tamper-evident.
- **Separation of duties:** agents plan, policy decides, the operator approves where required, the host
  executes, each with its own identity and key.
- **Least privilege** per host and per action.
- **Change control:** a host's allowlist changes only through a reviewed deploy.
- **Key management:** per-host keys generated on the host; short-lived certificates; revocation;
  release signing separate from execution signing.

## Consequences

**Positive:**
- One way to add a host: install the agent, write its allowlist. The same on the mini, the DGX and the
  VPS, and for anyone else running flotilla.
- No inbound ports on hosts; the reachability problems of each network disappear.
- A compromised control plane is limited to each host's allowlist.

**Negative:**
- New code with the most privilege in the system; it needs its own tests, review and a security-minded
  e2e before its first write.
- New moving parts: a certificate authority, two signing keys, an agent endpoint on flotillad.
- One new dependency: a websocket library for Go (ADR-0003, approved when built).

**Neutral:**
- ADR-0002's actuator exception becomes this agent; nothing in a container holds the Docker socket.

## Alternatives considered

- **An inbound service on each host** (flotillad or a worker calls it): each network needs its own
  route in (the Docker VM's gateway, VPS firewalls, the DGX), and every host exposes a port. Rejected.
- **SSH forced commands:** no idempotency, no local audit, no signed requests without extra work, and
  flotilla's workers would hold SSH keys to every host. Rejected (ADR-0005 decision 12).
- **Configuration management** (Ansible, Salt) for both install and execution: a second control plane
  beside flotillad, and no per-action policy. Rejected for execution; possible later for installing
  at scale.
- **Rust or Python for the agent:** Rust adds a second language and shares nothing with flotillad;
  Python needs an interpreter and packages on every host, in the most privileged component. Go chosen
  (operator, 2026-11-09).
- **Self-update from v1:** convenient, but only safe with release signing fully separate; pushed upgrades
  first.

## Open questions

1. **flotillad's agent endpoint on the tailnet:** how it is published (today flotillad publishes no
   ports): a Caddy route with mutual TLS terminated in flotillad, or a dedicated tailnet listener.
2. **Where the CA and the two signing keys live,** and how they are rotated (the operator's offline
   copy, as for the backup encryption key?).
3. **The signing format:** Go's standard Ed25519 with our own small envelope (no dependency), or an
   existing tool (minisign, cosign) for releases.
4. **The connector role:** whether the Slack connector runs on the mini's agent or as its own service.
5. **The agent's local store:** SQLite (a dependency already in flotillad) or an append-only file.
