# Deployment

**Status:** accepted in [ADR-0002](../adr/ADR-0002-run-in-docker.md) (2026-11-08).

flotilla runs on the homelab Mac mini: Intel, macOS 15.7, 64 GB once the memory upgrade is fitted.
It runs in Docker, in the production colima VM with the homelab's other services, deployed by the
operator account.

```
 Mac mini (macOS) ── colima VM (Linux) ──────────────────────────────────────────────────────────┐
 │                                                                                                │
 │  flotillad            ── tailnet: API, UI, MCP (via the homelab's Caddy / tailnet node)        │
 │   volume: state (SQLite, events)                                                               │
 │   no fleet secrets, no Docker socket                                                           │
 │        │ local network (flotilla internal)                                                     │
 │        ├── worker-triage     secrets: Grafana read, GlitchTip read/write, GitHub (issues),       │
 │        │                     LiteLLM key fleet-triage                                            │
 │        ├── worker-ops        secrets: Grafana read, LiteLLM key ops                               │
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
- Egress denied by default; see [Defaults](#defaults).
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

## Defaults

Set 2026-11-08 at the operator's request ("take reasonable defaults"). Each one is changed by a
commit to this page, with the reason.

| Topic | Default |
|---|---|
| **Egress** | Deny by default. Workers sit on an `internal` Docker network with no route out. The only way out is one egress proxy, [Smokescreen](https://github.com/stripe/smokescreen) (MIT, approved as a dependency by the
operator 2026-11-08), built from a pinned commit. Each worker has its own allowlist of `host:port`, generated from the tools and outputs its fleet's specs declare and kept in config. A denied connection is logged and counted, and any denial raises an alert. flotillad reaches only the observability stack. |
| **Reaching the UI, API and MCP** | A caddy-tailscale node of its own, like the homelab's other services (homelab ADR-0006), with one tailnet ACL grant per port. Never a public interface. |
| **Operator identity** | The UI trusts the tailnet identity caddy-tailscale passes on, and only the operator's identity may approve. The CLI uses an operator token from a mode-600 file in the operator's home. Approvals also come from the operator's pinned Slack user, within ADR-0005's limits. |
| **Agent identity (MCP)** | A token per caller, minted by the operator with the CLI, scoped `read` or `read+act`. No scope can approve. |
| **Bugfix worker** | Builds and tests inside its own container, with its toolchain baked into the image. No Docker inside it, so repos whose tests need Docker are out of scope for now. Each run's worktree is on a scratch volume and deleted when the run ends. |
| **Images** | `ghcr.io/chipi/flotilla-<role>`, public (they hold no secrets or config), built in CI, tagged `sha-<7>`, deployed by digest. Signing (cosign) is added before any fleet reaches `live`. |
| **Credentials** | A GitHub App per fleet instead of personal tokens (installation tokens expire within an hour). A LiteLLM key per fleet with a budget, minted with the homelab's `mint-credential` skill. Other credentials are rotated every 90 days and at once on any suspicion. Each one has a row in the homelab handbook's credentials table. |
| **Resource limits** | flotillad 256 MiB and 0.5 CPU; the triage, ops and SEO workers 1 GiB and 1 CPU each; the bugfix worker 4 GiB and 2 CPUs. These are limits, not reservations, and are revised from measurements after phase 2. |
| **State backup** | A nightly SQLite `.backup` into the mini's offsite backup, with a "backup older than 26 h" alert like the homelab's other backups, and a restore test before phase 3. |
