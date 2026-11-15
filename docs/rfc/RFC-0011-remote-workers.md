# RFC-0011 — Remote workers: routines on any machine, the mini orchestrating

**Status:** Accepted (operator, 2026-11-15: the three questions as proposed: the link is a mode of
the host agent's binary, the worker's version must be flotillad's, the X3 first). Not built.
**Builds on:** [ADR-0006](../adr/ADR-0006-node-agent.md) (host agents: they dial in, mutual TLS, the
host CA, enrolment and revocation), [ADR-0005](../adr/ADR-0005-agent-pipeline.md) (workers per role),
[RFC-0010](RFC-0010-bugfix-on-the-platform.md) §9 (bugfix's fix and push workers on the X3, its first
user).

## Motivation

Every worker runs today on the mini, beside flotillad, on its internal Docker network. The bugfix
fleet's first live runs (2026-11-15) showed that heavy agentic work does not belong there: orrery's
suite took more than 20 minutes on the mini where the X3 takes 195 s, and it competed with the mini's
services. The operator's direction:

- the mini orchestrates and serves; it does no dev work;
- heavy work runs on the X3 now, and on other machines later (the DGX, the MacBook, the VPS);
- placement is a generic capability, usable by any flow, not a bugfix detail.

## Proposal

### 1. A worker runs wherever it is declared

`flotillad.yaml` names each worker as today; a remote worker has no URL, it connects:

```yaml
workers:
  ops-read: {url: "http://worker-ops-read:8081", token_file: /run/flotilla/worker-ops-read.token}
  bugfix: {remote: x3}         # a remote worker: it connects in from the node named x3
  bugfix-act: {remote: x3}
```

Routine specs do not change: a routine names its worker, and the installation decides where that
worker lives. Moving a worker between machines is a config change and an install, not a spec change.

### 2. It dials in, like a host agent

A **worker link** (a mode of the existing Go agent binary, `flotilla-node link`) runs beside the worker
on the remote machine. It dials flotillad's agent endpoint (the tailnet node `flotilla-hosts`, TCP
passthrough to flotillad, ADR-0006 §2), authenticates with its own client certificate, and keeps one
connection open. flotillad sends a run down it; the link hands it to the worker over localhost (the
worker's existing `/v1/runs` API, unchanged), and sends the result back.

- **No inbound port** on the remote machine, and **no route out** for flotillad: its containers stay
  on internal networks.
- **No request held open through a proxy:** a 20-minute run is one frame out and one frame back on a
  connection that heartbeats.
- **The framing is the host agents' (`internal/hostwire`)**, with three new frame types: `worker_hello`
  (the worker's name, its version, the routines' image), `run` (flotillad to the link: the run request
  as today's HTTP body) and `run_result` (the result as today's HTTP response).

### 3. Identity: a worker is not a host

- **Its own certificate namespace:** `worker:<name>@<node>`, issued by the same intermediate CA through
  the same enrolment (a single-use token, 15 minutes, carried over SSH), renewed over the connection,
  revoked with `flotilla worker revoke`.
- **A worker certificate cannot send host frames, and a host certificate cannot take runs:** flotillad
  checks the namespace on every frame. A remote worker never gets host-action rights, and the signed
  host executions of ADR-0006 stay the only way an action runs on a host.
- **A worker certificate takes only its own worker's runs**, and a result is accepted only for a run
  id flotillad sent on that connection.

### 4. When the machine is off

- A routine whose worker is not connected is **skipped quietly** (no run recorded, no case event), as
  a paused or busy routine is since 2026-11-15. Its cases wait; the stuck-state timer is the only limit.
- A connection lost mid-run makes the run `unknown`, as a host execution's is. For an acting routine
  the case is tried again after the backoff; an executing routine's case (an approved PR) escalates
  as unknown and is never run again (RFC-0010 §3's at-most-once).
- The UI's Workers view shows each remote worker: its node, version, connected since, last heartbeat.

### 5. What lives on the remote machine

One compose stack per machine, generic: the link, the worker(s) for the roles placed there, and an
egress proxy with that machine's allowlist. The secrets the roles need (a model key, the GitHub
App's key) live on that machine, mode 600; flotillad never holds them. `flotilla worker install
<node>` does the enrolment and brings the stack up over SSH, as `flotilla node install` does for
agents. On the X3 it follows `infra/x3/AGENTS.md` (the deploy checkout, user units).

### 6. What a remote routine cannot do in v1

It cannot reach flotillad's worker listener (the ledger, case reads, note proposals): those are on
flotillad's internal network. Routines that need them (triage's api ledger, ops/case-review) stay on
the mini. v2 carries those calls over the link.

### 7. First user, and the done check

The bugfix fleet: `worker-bugfix` and `worker-bugfix-act` move to the X3 with their egress proxy;
`bugfix/intake` and `bugfix/pr-check` stay on the mini. With RFC-0010 §9's narrower tests, a fix run
is minutes on the X3. **Done** when a code_bug case on the mini is fixed by a run on the X3 to a
shadow plan, the X3 switched off leaves the cases waiting without events, and switching it on
resumes them.

## Open questions (the operator's)

1. **The link: a mode of the host agent binary, or its own binary.** *Proposed:* a mode (`flotilla-node
   link`): one Go binary to release and install, the same TLS and framing code. The alternative keeps
   the host agent's binary free of anything that is not host actions.
2. **The worker's image on the remote machine.** *Proposed:* the same `ghcr.io/chipi/flotilla-worker-*`
   images, at the tag flotillad runs; a worker at another version is refused at `worker_hello`. The
   alternative allows a window of versions.
3. **Which machines first.** *Proposed:* the X3 only, now; the DGX when a routine needs a GPU.

## Alternatives considered

- **flotillad dials the worker through its egress proxy** (RFC-0010 §9's first idea): a 20-minute
  request held open through Smokescreen, an allowlist entry per machine, and a route out of
  flotillad's network for every remote worker.
- **flotillad on the tailnet directly:** breaks the rule that flotillad's containers have no route out.
- **A queue** (NATS, Redis): a new dependency and service for what one connection per worker does.
- **Run the work in the host agent itself:** mixes the host-action rights (sudoers, signed executions)
  with code that runs repos and models.

## Discussion

- 2026-11-15, the operator: "I want to run heavy agentic work like this on the X3 … the mini as
  orchestrator and services platform, no dev work … this orchestration should be a generic capability
  and we could use it for other flows also … and on other machines as well."
- 2026-11-15, the operator: "all good, suggestions accepted".
