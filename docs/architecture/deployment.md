# Deployment

**Status:** proposed in [ADR-0002](../adr/ADR-0002-run-in-docker.md), waiting for the operator's
decision.

flotilla runs on the homelab Mac mini: Intel, macOS 15.7, 64 GB once the memory upgrade is fitted.
It runs in Docker, in the same colima VM as the homelab's other services.

```
 Mac mini (macOS) ── colima VM (Linux) ──────────────────────────────────────────────────────────┐
 │                                                                                                │
 │  flotillad            ── tailnet: API, UI, MCP (via the homelab's Caddy / tailnet node)        │
 │   volume: state (SQLite, events)                                                               │
 │   no fleet secrets, no Docker socket                                                           │
 │        │ local network (flotilla internal)                                                     │
 │        ├── worker-triage     secrets: Grafana read, GlitchTip read/write, GitHub (issues),       │
 │        │                     LiteLLM key fleet-triage                                            │
 │        ├── worker-ops        secrets: Grafana read, Home Assistant notify, LiteLLM key ops       │
 │        ├── worker-seo        secrets: Search Console read, Umami read, LiteLLM key seo           │
 │        │                     volume: seo data (Parquet)                                          │
 │        └── worker-bugfix     secrets: GitHub App (branches, PRs), LiteLLM key bugfix             │
 │                              toolchain: git, language toolchains, the coding harness            │
 │                                                                                                │
 │  existing: LiteLLM, VictoriaMetrics/Logs/Traces, Grafana, GlitchTip, Langfuse, Alloy, Caddy …  │
 └────────────────────────────────────────────────────────────────────────────────────────────────┘
```

## Why one worker per fleet

- **Credentials:** each fleet's secrets are mounted only into its own worker. A compromised SEO run
  cannot reach the bugfix fleet's GitHub App.
- **Toolchains:** the bugfix fleet needs git and build tools; the others do not.
- **No Docker socket anywhere.** flotillad does not start containers per run. That would need the
  Docker socket, which is root on the VM. Workers are long-running, and each run is a process inside
  its fleet's worker.
- **Resources:** each worker has its own memory and CPU limits.

## Hardening, every container

- Non-root user.
- Read-only root filesystem; writable volumes only where needed.
- All Linux capabilities dropped; `no-new-privileges`.
- Memory and CPU limits.
- Egress limited to what the fleet needs (LiteLLM, its sources, GitHub). The homelab has no
  per-container egress policy today; how to enforce this (an egress proxy per worker, or VM
  firewall rules) is still to be designed, and is listed under "Not decided".
- Images:
  - built from pinned bases;
  - one image per role, built in CI and tagged `sha-<7>`;
  - pulled by digest.

## Deploy and rollback

- **Deploy:** a new image tag for each container, then `docker compose up -d`. specs travel inside
  the image, so a spec change is a deploy.
- **Rollback:** the previous tag.
- **Observability:** every deploy writes an `ops_event/v1` deploy event, so the homelab's deploy
  checks and alert stand-downs see it.
- **State:** the database lives on a volume and survives restarts. Schema migrations run at
  flotillad start-up, forward-only, with a backup taken first.

## Development and tests

- **Development:** the dev Docker engine (the homelab's ADR-0009), never the production VM.
- **End-to-end tests:** the whole stack in the dev engine, with a stub LiteLLM and stub sources
  (see [Testing](../testing.md)).

## Not decided

- How the UI and MCP are reached from the tailnet: a caddy-tailscale node of its own, like the
  homelab's other services (its ADR-0006), is the default.
- How per-worker egress limits are enforced.
- Whether `worker-bugfix` builds and tests code inside its own container, or delegates to the dev
  engine.
