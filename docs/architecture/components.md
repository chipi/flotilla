# Components

Three programs of our own, each with one purpose, plus the egress proxy:

- **flotillad** (Go), the control plane: decides and records;
- **the runtime** (Python), inside workers: diagnoses and plans;
- **`flotilla-host`** (Go), the host agent: executes what is signed and allowed on its host.

flotillad never runs fleet code and holds no fleet credentials; the runtime never decides what it
may do; the host agent never runs anything it was not sent, signed, and allowed by its own host.

The host-agent path is [ADR-0006](../adr/ADR-0006-node-agent.md), which amends ADR-0005: flotillad
signs host writes and sends them to the host agent; the acting worker plans and checks.

## flotillad (Go)

| Component | Responsibility |
| --- | --- |
| **Registry** | loads `fleets/<fleet>/*.yaml` and the registry of tools and actions; refuses a spec that does not validate, or that offers an action as a tool inside an agent's loop |
| **Triggers** | starts routines on **time** (every, cron) or on an **event** on a case (handed over to a kind, a check due); checks STOP, pause and budget first |
| **Runner** | asks a worker to start a run with its spec, run id, trace id, deadline and environment (`FLOTILLA_MODELS`, the trace parent); enforces the timeout; records cost |
| **Cases** | one record per problem with its keys, status, holder, lease and timeline ([Data](data.md)); a **transition table** enforced on every status change; timers for stuck states; the per-target lock |
| **Policy** | decides each planned write. It checks, in order: the registry; autonomy (the lowest of the routine's autonomy, the action's autonomy, the host's autonomy cap, taint, the freeze); locks and limits; holds. Then it runs the write, asks the operator, or refuses. It executes only flotilla's own controls itself |
| **Guardrails** | per-host and platform caps on executions, the circuit breaker, the host incident rule, the autonomy freeze, the recurrence rule |
| **Workers' listener** | `worker_listen` in flotillad's config: a second listener for the workers' routines, worker tokens only. Today it serves the ledger and nothing else ([RFC-0002](../rfc/RFC-0002-job-runtime.md) §9 step 5). With fleet packs it also serves approved pack snapshots by hash ([ADR-0013](../adr/ADR-0013-fleet-packs.md), not built) |
| **Packs** (ADR-0013, not built) | hashes the built-in and installation fleet packs, keeps pending and approved snapshots, shows a change, records approvals, rollbacks and prunes, and sets changed routines to shadow |
| **Approvals** | proposals waiting on the `approvals` channel; an approval (UI, CLI, or the operator's pinned Slack user within limits) is bound to the plan's hash and starts the execution |
| **Host gateway** | the endpoint host agents connect to (mutual TLS); the certificate authority's intermediate issues and renews their certificates; revocation; `flotilla host list` |
| **Signer** | signs every execution with flotillad's Ed25519 key: execution id, case, host id, action, parameters, plan hash, approval, signed and expiry times; the signature covers the exact bytes sent |
| **Channels** | routes `alerts`, `approvals` and `notifications` to the provider's connector |
| **Budgets and autonomy** | per routine and fleet: spend and tokens per day; autonomy controls, with who changed them |
| **API, CLI, UI, MCP** | one API behind all of them. The UI's main screen is the case timeline. MCP has no approve tool |
| **Telemetry** | metrics (retried pushes), structured logs, OpenTelemetry traces, errors |

## Workers and the runtime (Python)

A **worker** is a long-running container that runs routines for **one role**, holding only that role's
credentials and toolchain (`triage`, `ops-read`, `ops-act-host`, …). Each run is a process in it.

| Runtime component | Responsibility |
| --- | --- |
| **Harness** | the one loop for a model step: context in, tools offered, an answer of the step's output type (decision, prose, plan), validated (`jsonschema`); limits on turns, calls, tokens, time, cost; everything recorded for replay |
| **Context builder** | layers with budgets: playbook, the case (structured events only, with the skills loaded by rule), the skills index, memory and knowledge, tool results; untrusted content wrapped as data; the run's **taint** derived from what entered it |
| **Lookup client** | memory: exact lookups of flotilla's records by key; knowledge: search over plugged-in sources (path maps, full-text, semantic), by trust tier |
| **Tool calls** | tools, scoped and size-capped, recorded; actions only as entries of a plan |
| **Model client** | any OpenAI-compatible endpoint; aliases with capabilities; cost from the endpoint, a price table, or unknown (never zero); typed errors; a span per call |
| **Case client** | opens, updates and hands over cases through flotillad's API; never writes the database |
| **Executor** | carries out an approved plan's repo writes (issues, PRs) with the role's credentials; host writes go from flotillad, signed, to the host agent |

## The host agent: `flotilla-host` (Go)

One static binary per platform, installed per host as a service under a dedicated user
([ADR-0006](../adr/ADR-0006-node-agent.md)).

| Role | Responsibility |
| --- | --- |
| **Actions** (role) | runs an execution only if its signature verifies, it is unexpired and never seen, the host id is this agent's, the action is in this host's allowlist with valid parameters and within its rate limit, and an `auto` execution is within this host's autonomy cap; checks preconditions locally; runs the exact command template (never a shell); records `started` before it runs (SQLite); a hash-chained audit log, collected off the host |
| **Connector** | holds a provider's credentials (Slack); posts what flotillad sends to a channel; relays approval clicks (with the plan hash and nonce) back to flotillad; outbound only in v1 |

## Interfaces

| From → to | What | Contract |
| --- | --- | --- |
| flotillad → worker | start run, cancel run | versioned JSON |
| worker → flotillad | case events, plans, results, run end | versioned JSON over the API |
| host agent → flotillad | connect (mutual TLS), results, relayed approvals | versioned protocol over one outbound connection |
| flotillad → host agent | signed executions, messages for a channel | signed envelopes |
| any client → flotillad | the public API (CLI, UI, MCP) | OpenAPI document, versioned |
