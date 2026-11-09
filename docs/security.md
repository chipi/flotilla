# Security

Security is designed in from the start, not added later. flotilla runs language models against real
data, with credentials and host agents that can change real things, and it lives in a public repo.
This page is the threat model and the controls; each control names where it is enforced. The
decisions behind them are [ADR-0005](adr/ADR-0005-agent-pipeline.md) (the pipeline, policy and
approvals) and [ADR-0006](adr/ADR-0006-node-agent.md) (host agents, proposed).

## What we protect

| Asset | Why it matters |
| --- | --- |
| **Fleet credentials:** GitHub Apps, model keys, analytics, error tracking, observability reads | they can change repos, read private data, spend money |
| **The hosts flotilla may change** | a wrong restart or rerun is an outage |
| **The certificate authority and the signing keys** | whoever holds them can enrol a host agent or sign an execution |
| **The provider account (Slack)** | it carries approvals |
| **The operator's repos and sites** | an unwanted issue, PR or change is visible and can do harm |
| **Read access to the installation** (metrics, logs, errors) | it reveals infrastructure detail |
| **Cases** | the record of every decision, approval and execution; it must be trustworthy |
| **The approval** | the one thing that lets a proposed write happen |
| **The public repo** | anything committed is public forever |

## Who could cause harm

| Actor | How |
| --- | --- |
| **Untrusted content authors** | text the models read: alert annotations, error messages, issue bodies, PR comments, web pages, untrusted recall sources. The main threat: **prompt injection**. |
| **A misbehaving model** | wrong, overconfident or manipulated answers; plans outside its case |
| **Another agent through MCP** | well-meaning but wrong, or itself injected |
| **A compromised flotillad** | it holds the signing key: it could sign executions |
| **A compromised host** | it could lie in results or try to reach other hosts |
| **A taken-over provider account** | someone clicking approve as the operator in Slack |
| **A compromised dependency, image or release** | supply chain |
| **Someone on the network** | being on it is not authorization |
| **Ourselves** | a secret committed to the public repo; a spec or allowlist that grants too much; autonomy promoted too early |

## Threats and controls

| # | Threat | Controls | Enforced by |
| --- | --- | --- | --- |
| T1 | **Prompt injection leads to a harmful action** | Agents call only read capabilities; writes are only planned. Policy in flotillad decides every planned write by code: the registry (capability declared for the case's kind, target is the case's), autonomy, locks, limits. A run that read untrusted content is **tainted** and its plans are capped at propose. Untrusted text is wrapped as data. | registry, harness, policy |
| T2 | **Exfiltration** | No general web or HTTP capability. Each worker reaches only its allowlisted hosts through the egress proxy. | registry, egress proxy |
| T3 | **An agent approves** | MCP has no approve tool; jobs cannot approve. Approval is the operator's: UI, CLI, or the pinned Slack user within limits. Every change records its actor. | flotillad API, events |
| T4 | **Silent cloud egress** | Each job names its model aliases; a local-only alias has no cloud fallback; the model client refuses a model outside the job's list. | model client, endpoint config |
| T5 | **A secret leaks** (repo, logs, traces, prompts) | Secrets in mode-600 files on the hosts, mounted per role, never in git. Secrets scan in pre-commit (gitleaks). The triage fleet scrubs its recorded captures (`scrub.py`). Credentials never enter a model's context. | CI, logging, model client |
| T6 | **One role's credentials used by another** | One worker per role, each mounting only its own secrets. flotillad holds no fleet credentials; host agents hold none but the connector's. | deployment |
| T7 | **A behaviour change sneaks in** | Specs, playbooks, the capability registry and host allowlists change only by a reviewed commit and a deploy. Changes that alter decisions are replayed first. Autonomy is promoted only by the operator. | registry, CI, replay gate |
| T8 | **Supply chain** | Stdlib first. Dependencies pinned (Python by hash) and each one approved. Images from pinned bases, by digest. `flotilla-node` releases signed in CI and verified before install or upgrade. Vulnerability scans in CI. | CI, `flotilla node` |
| T9 | **Container escape** | Non-root, read-only root, capabilities dropped, `no-new-privileges`, no Docker socket, limits. | deployment |
| T10 | **Cases tampered with** | Only flotillad writes the database; workers and host agents report through the API. Events append-only. Host agents keep their own hash-chained audit log, shipped off the host, to compare with flotillad's record. | flotillad, host agents |
| T11 | **Exposure on the network** | API, UI and MCP only on the installation's chosen network. The node endpoint requires mutual TLS. Nothing on a host listens for flotilla. | network, node gateway |
| T12 | **Denial of wallet** | Budgets per job and fleet checked before and during runs; endpoint key budgets as a second limit; turn, call and token limits per harness step. | Triggers, Runner, harness |
| T13 | **A forged or replayed execution** | Every execution is signed (Ed25519) over the exact bytes sent and bound to node id, capability, parameters, plan hash and a 10-minute expiry. The host agent refuses a bad signature, another node, an expired envelope, and a seen id whose envelope differs (an identical resend returns the recorded result). | Signer, host agent |
| T14 | **A compromised flotillad signs harmful executions** | Each host agent enforces its own reviewed allowlist (exact command templates, parameter checks), rate limits and autonomy ceiling, whatever is signed. The allowlist, binary and sudoers are root-owned, so the agent cannot widen them; privileged parameters are enums, one sudoers line each, no wildcards. Commands run without a shell, as a dedicated user, with a generated sudoers limited to the templates. Its audit log shows what ran. | host agent, sudoers |
| T15 | **A rogue or compromised host agent** | Enrolment by a single-use token bound to the node name, valid 15 minutes; short-lived certificates; revocation in flotillad alone (`flotilla node revoke`). An agent can only report on its own host's executions. The host's hash-chained log and flotillad's record are compared; a mismatch raises an alert. | node gateway, policy |
| T16 | **A taken-over Slack account approves** | Slack approval only from the pinned workspace and user, only for low and medium risk that is reversible or regenerable, bound to the plan's hash and a nonce, with a daily cap. High-risk or irreversible writes need the CLI. | Approvals, connector |
| T17 | **Runaway autonomy** (many actions, fast) | Per-host and platform caps on executions; the circuit breaker drops every auto capability to propose; the host incident rule pauses auto on a host; `flotilla autonomy freeze`; the recurrence rule escalates a fix that keeps coming back. | Guardrails |

**Accepted trade-off** ([ADR-0006](adr/ADR-0006-node-agent.md), decided 4): on a host whose agent
has both roles, a breach of that process exposes the host's allowed writes, the Slack tokens and,
once the connector relays approvals, forged approvals for any write Slack may approve, on any host.
T16's limits bound it; only the configured connector node may relay approvals. v1's connector is
outbound only.

## Rules for contributors (also in AGENTS.md)

- Never commit secrets, cases, findings, fleet data, production payloads, or installation specifics.
- A new capability is a read unless it is declared a write, with risk, reversibility, preconditions
  and a check.
- A new dependency needs an explicit yes.
- Anything that grants more (a broader scope, a new write for a kind, a higher ceiling) needs a note
  in the commit saying why.

## Verification

- **Security tests are part of the pyramid** ([Testing](testing.md)):
  - injection fixtures never lead to an executed write, and taint caps them at propose;
  - capabilities refuse out-of-scope requests; policy refuses a write not declared for the kind;
  - MCP refuses an approve call;
  - a local-only job refuses a cloud model;
  - the host agent refuses a bad signature, another host, an expired or replayed execution, a
    capability or parameter outside its allowlist, and an `auto` above its ceiling;
  - a Slack approval from another user, for high risk, or for a changed plan is refused.
- **A threat-model review** with every new fleet, capability kind or host.

The defaults behind T2, T6, T9, T11 and the credential rules are in
[Deployment → Defaults](architecture/deployment.md#defaults).

## Not covered yet

- **ADR-0005's pipeline is accepted and not built** (cases, policy, taint, guardrails, channels,
  approvals); v1 brings the first of it. Until then T1 is covered by the triage fleet's own code
  checks and its shadow stage, and T10 by the append-only `events` table.
- **Redaction** of flotillad's logs and job traces is not built.

- **Host agents are proposed, not built.** T13 to T15 are design until ADR-0006 is accepted and its
  security e2e passes.
- **Image signing:** planned before any fleet reaches `live`, not built.
- **The coding harness** used by the bugfix fleet runs code from the repos it fixes; its security
  review is due before that fleet acts.
- **Model-written commands** (an investigation sandbox) are not part of the design; they get their
  own threat model if they are added.
- **The egress proxy** has been checked for licence and activity; its configuration review and a
  denied-host test are part of the platform's e2e, still to extend per worker.
