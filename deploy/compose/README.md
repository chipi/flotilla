# Installing flotilla on a host

One compose stack (ADR-0002): `flotillad`, one worker per role (`worker-triage` today) and the egress
proxy (`egress`, Smokescreen). The repo holds the compose file and the images; everything about one
installation lives on the host, outside git. Placeholders below are in `<angle brackets>`.

## Layout (`~/flotilla` on the host)

```text
~/flotilla/
  repo/                     git clone https://github.com/chipi/flotilla.git repo
  .env                      the installation (compose and the deploy scripts read it)
  config/                   read-only into the containers: 644 for the YAML, 600 for the tokens
    flotillad.yaml
    callers.yaml            who may call the API, by token hash
    cli.token               the token the CLI inside flotillad uses (an agent token: see below)
    worker-triage.token     flotillad <-> worker, 32+ random characters
    worker-triage.env       the installation's overrides for routines (ROUTINE_*); no secrets
    smokescreen.yaml        the proxy: listen address, which private ranges it may reach
    egress-acl.yaml         the proxy: which host names routines may reach
  deployed.tsv              written by deploy/mini/deploy.sh
```

**State and secrets are Docker volumes**, not host folders: `flotilla-state` (the store and its daily
backups), the triage fleet's state (`TRIAGE_STATE_VOLUME`), and `flotilla-flotillad-secrets` /
`flotilla-worker-triage-secrets`. Nothing writes to the host through the VM's file sharing.
`deploy/mini/secrets.sh` copies the tokens and the triage secrets file into the secret volumes on
every deploy (mode 600, owned by the reading container's uid); the token files above are read on
the host only, by that script.

`.env`:

```sh
FLOTILLA_CONFIG=<home>/flotilla/config
TRIAGE_ENV_FILE=<the triage fleet's secrets file>     # copied into the worker's secrets volume
TRIAGE_STATE_VOLUME=flotilla-triage-state-shadow      # the triage fleet's state (shadow copy, or live after the switch)
FLOTILLA_ERRORS_DSN_FILE=<file with SENTRY_DSN=...>   # optional: flotillad's GlitchTip DSN, copied into its secrets volume
TRIAGE_DEPLOY_LOG=<the triage fleet's deploys.tsv>     # optional: deploy.sh appends a row per deploy and rollback (mirror-test's until the switch)
TRIAGE_FILING_GATE=<filing_gate.py>                    # optional: default ~/fleet-eval-data/projects/signal-fleet-filing/scripts/filing_gate.py
# the services' Docker networks, when they are not the defaults
# NET_BACKEND=backend_default NET_LITELLM=litellm_default NET_GLITCHTIP=glitchtip_default NET_LANGFUSE=langfuse_default
```

A shadow state volume is seeded once from the live fleet's folder, without its secrets file and STOP
flag, and never overwritten after:

```sh
~/flotilla/repo/deploy/mini/secrets.sh sha-<7> --seed-state flotilla-triage-state-shadow <the fleet's state folder>
```

`config/flotillad.yaml` (every field is required except `timezone`, `otlp_traces_url`,
`disabled_routines`, `case_timers`, `models` and `hosts`):

```yaml
listen: 127.0.0.1:8484                 # inside the container: the CLI runs there (docker exec)
worker_listen: 0.0.0.0:8485            # optional: the ledger only, for the workers' routines (RFC-0002 §9 step 5)
specs_dir: /opt/flotilla/fleets         # in the image
state_dir: /var/lib/flotilla
environment: operations
timezone: <Region/City>                 # the budget day
vm_url: http://victoriametrics:8428     # through the proxy
otlp_traces_url: http://victoriatraces:10428/insert/opentelemetry/v1/traces
errors_dsn_file: /run/flotilla/errors.env     # optional: secrets.sh copies FLOTILLA_ERRORS_DSN_FILE here
callers_file: /etc/flotilla/callers.yaml
workers:
  triage: {url: "http://worker-triage:8081", token_file: /run/flotilla/worker-triage.token}   # from the secrets volume
disabled_routines: [triage/cycle-shadow]   # without a candidate worker (below); -check refuses a routine whose worker is missing
case_timers: {detected_alert: 30m, handed_over_escalate: 24h}   # optional, these are the defaults: a case detected that long alerts, handed over that long escalates
models:                                 # optional (ADR-0004): the aliases routines name; every run gets it as FLOTILLA_MODELS
  endpoints:
    gateway:
      base_url: http://litellm:4000/v1   # any OpenAI-compatible endpoint; through the egress proxy
      kind: litellm                      # litellm adds Langfuse trace metadata; openai for anything else
      key_env: OPENROUTER_API_KEY        # a variable in the fleet's secrets file (or key_file: a path in the worker)
      defaults: {tools: native, json: object, cost: reported}
  aliases:
    triage-default: {endpoint: gateway, model: fleet-triage-pro}
    local-dgx: {endpoint: gateway, model: <model name>, tools: none, cost: zero}
  prices: {}                             # per alias, for cost: table (input_per_mtok, output_per_mtok)
```

`-check` validates `models` with the same rules the Python client applies (shared fixtures:
`runtime/testdata/models-*.json`) and prints the number of aliases. In `worker-triage.env`,
`ROUTINE_HEARTBEAT_MODEL_ALIAS=<alias>` makes the heartbeat call that alias once per run through the
model client, traced: the platform's live check of the models section, the key, the endpoint through
the proxy and the routine's spans. Unset, the heartbeat makes no model call.

`config/callers.yaml` (`flotilla hash-token < token-file` prints the hash):

```yaml
callers:
  - {name: operator, role: operator, token_sha256: <64 hex>}
  - {name: <agent name>, role: agent, token_sha256: <64 hex>}
```

The **operator's** token stays with the operator: autonomy changes, resuming a routine and starting
a STOPped fleet need it, and no agent ever holds it (AGENTS.md rule 2). The CLI inside the container
(`cli.token`) uses an **agent** token; the operator passes theirs per call:

```sh
docker exec -e FLOTILLA_TOKEN="$(cat <operator token file>)" flotillad flotilla autonomy triage/cycle propose
```

`config/worker-triage.env`: the triage fleet reads its services by these URLs, all through the proxy.

```sh
ROUTINE_SF_SERVICE=triage-fleet                 # the candidate worker overrides it (triage-fleet-candidate)
ROUTINE_SF_VM_URL=http://victoriametrics:8428
ROUTINE_SF_VL_URL=http://victorialogs:9428
ROUTINE_SF_VT_URL=http://victoriatraces:10428
ROUTINE_SF_GRAFANA_URL=http://grafana:3000
ROUTINE_SF_GLITCHTIP_URL=http://<glitchtip web container>:8080
ROUTINE_SF_OPENROUTER_URL=http://litellm:4000/v1/chat/completions
ROUTINE_LANGFUSE_HOST=http://langfuse-web:3000
ROUTINE_HEARTBEAT_ALLOWED_URL=http://litellm:4000/health/liveliness
ROUTINE_HEARTBEAT_DENIED_URL=http://example.com/
```

`config/smokescreen.yaml`: private addresses are refused unless a range is allowed; allow only the
services' networks.

```yaml
ip: 0.0.0.0
port: 4750
acl_file: /etc/smokescreen/acl.yaml
allow_missing_role: true       # one proxy per worker set: no client certificates, the default ACL applies
allow_ranges: [<subnet of each service network>]
```

`config/egress-acl.yaml`: everything else is refused and logged.

```yaml
version: v1
services: []
default:
  project: flotilla
  action: enforce
  allowed_domains: [victoriametrics, victorialogs, victoriatraces, grafana, litellm, <glitchtip web container>, langfuse-web, api.github.com]
```

## The ops workers: pipeline v1 (RFC-0004 §10)

The image carries `fleets/ops/` (`backup-monitor` on `ops-read`, `rerun-backup` on `ops-act-host`).
A host must do one of two things before deploying a tag that has them, or flotillad's `-check`
refuses to start, before anything changes:

- **Run them:** add `ops` to `COMPOSE_PROFILES` in `.env`; create `config/worker-ops-read.token` and
  `config/worker-ops-act-host.token` (32+ random characters each) and `config/worker-ops.env`
  (`ROUTINE_VM_URL`, `ROUTINE_BACKUP_INSTANCE`, the backup metric's `instance` label, and
  `ROUTINE_BACKUP_TARGET`, e.g. `host:mini`); add both workers under `workers` in `flotillad.yaml`
  (`url: http://worker-ops-read:8081`, `token_file: /run/flotilla/worker-ops-read.token`, and the
  same for `ops-act-host`). `secrets.sh` fills their token volumes when the tokens exist.
- **Not yet:** list `ops/backup-monitor` and `ops/rerun-backup` in `disabled_routines`.

They reach VictoriaMetrics through the proxy, already on its allowlist for flotillad's metrics.
`rerun-backup` is capped at shadow: its plans are recorded and nothing is sent to the host.
`flotilla case list` and `flotilla case show <id>` show what they did.

## The deploy stand-down (RFC-0004 §5.1)

While a deploy changes a host, policy defers every plan on it. `deploy/mini/standdown.sh HOST DURATION
WHY` takes that stand-down: a hold on `host:HOST` by the installation's `deploy` caller, ending on its
own. It never fails its caller. `deploy.sh` takes one for 30m before `up` when `.env` names
`FLOTILLA_STANDDOWN_HOST` (e.g. `mini`); the homelab's `mini-deploy.sh` takes one from its pre-pull
hook. To set it up:

1. Add a caller with role `agent` (it may hold, never lift) to `callers.yaml`:
   `- {name: deploy, role: agent, token_sha256: <sha256 of the token>}`
   (`flotilla hash-token < config/deploy.token`).
2. Put the token in `config/deploy.token`; `secrets.sh` copies it into flotillad's secrets volume,
   where the script reads it by path.
3. Set `FLOTILLA_STANDDOWN_HOST=mini` in `.env`.

## The host agents' endpoint (ADR-0006)

Optional; without it no host can enrol or connect. The CA comes from the operator's machine, offline:
`flotilla ca root DIR` once (the root's key stays there, kept as the backup key is), then
`flotilla ca intermediate DIR OUT`; the three files in `OUT` (`intermediate.crt`,
`intermediate.key`, `root.crt`) go to flotillad's secrets volume. In `config/flotillad.yaml`:

```yaml
hosts:
  listen: 0.0.0.0:8486                   # its own port: mutual TLS, terminated by flotillad
  ca_dir: /run/flotilla/host-ca          # the three files above
  server_names: [<the name agents dial>]  # in flotillad's server certificate (made at start, renewed in memory)
```

A proxy in front may only pass TCP through. flotillad checks at start that the root signed the
intermediate. Then `flotilla host enrolment NAME` (an operator) prints a single-use token for 15
minutes, which goes to the host inside the SSH session only, on `flotilla-host enrol`'s stdin;
`flotilla host list` and `flotilla host revoke NAME`.

## The candidate worker: shadow comparison of a candidate image

ADR-0004's live proof for a change to the triage fleet after the switch: the image under test runs
as a second worker beside the live one, with its own state, and the routine `triage/cycle-shadow`,
capped at `shadow` by its spec (`max_autonomy`), runs the same command on it. flotillad refuses any
autonomy above the cap, even the operator's, and the scheduler never runs it above the cap whatever
a control says.

1. Seed its state from the live state, once per comparison (a fresh volume name each time):
   `~/flotilla/repo/deploy/mini/secrets.sh sha-<7> --seed-state
   flotilla-triage-state-candidate-<date> ~/signal-fleet`
2. `.env`: `COMPOSE_PROFILES=candidate`, `FLOTILLA_CANDIDATE_TAG=sha-<candidate>`,
   `TRIAGE_CANDIDATE_STATE_VOLUME=flotilla-triage-state-candidate-<date>`.
3. `config/flotillad.yaml`: add `triage-candidate: {url: "http://worker-triage-candidate:8081", token_file:
   /run/flotilla/worker-triage.token}` under `workers`, and take `triage/cycle-shadow` out of `disabled_routines`.
4. Deploy (the live tag, unchanged). Compare with
   `deploy/mini/shadow-compare.sh --volume flotilla-triage-state-candidate-<date> --since <seed time>`.
5. To end it: put `triage/cycle-shadow` back in `disabled_routines`, drop `COMPOSE_PROFILES`,
   deploy. The candidate's volume stays until removed by hand.

The candidate shares the triage worker's secrets volume and the proxy's allowlist: in shadow the fleet
makes no outward change (orchestrator.py forces a dry run), and the cap keeps it there. It reads the
live worker's `worker-triage.env`, but the compose file sets `ROUTINE_SF_SERVICE=triage-fleet-candidate`
and `ROUTINE_SF_GLITCHTIP_SYNC=0` over it, so its metrics, inbox and traces never count as the live
fleet's.

## The ledger in flotillad (RFC-0002 §9 step 5)

A routine's ledger is its namespace in flotillad's store (`triage/cycle`, `triage/cycle-shadow`); workers
reach it on `worker_listen`, which serves the ledger and nothing else. The backend is chosen per
service in `.env`: `TRIAGE_LEDGER_BACKEND` (the live worker) and `TRIAGE_CANDIDATE_LEDGER_BACKEND`
(the candidate), each `tsv` by default. With `api`, dedup reads the store; both TSVs are still written
exactly as before (the readers' contract), and an empty namespace is refused rather than read as
"nothing ever filed".

To move a worker to `api`, after a deploy whose flotillad has `worker_listen`:

1. Pause the routine (`flotilla pause <routine id>`) and let a run in flight end: between the import
   and the switch the TSVs must not move on without the store.
2. Import its namespace from its own state, once (counts only are printed):
   `docker exec -w /opt/flotilla/signal-fleet/mvp -e FLOTILLA_API_URL=http://flotillad:8485
   -e FLOTILLA_API_TOKEN_FILE=/run/flotilla/worker.token -e NO_PROXY=flotillad
   -e FLOTILLA_ROUTINE=<routine id> <worker container> python3 ledger_sync.py import`
3. Check the store renders both TSVs byte for byte: the same with `check`.
4. Set the service's backend to `api`, deploy (the live tag, unchanged), and resume the routine (the
   operator's token).

`filing.backfill_alert_keys` and `filing.retire_glitchtip_project` edit filed.tsv only; after one,
re-import the namespace (a new namespace, or the routine's after an operator removes it).

When `filing.FILED_COLS` gains a column (`linked_via`, 2026-11-12), a filed.tsv written by the older
code keeps the old header until the routine's next filing rewrites it, and `import` refuses it. Run
`ledger_sync.py pad` (the same `docker exec`, with the routine paused) before step 2; it rewrites the
file with every column and the rows unchanged, and needs no store.

## Users and the UI (ADR-0014)

People sign in with their own token; agents and workers keep the callers file. On a new installation:

1. Create the first admin, with the callers-file operator token the CLI inside flotillad uses:
   `docker exec flotillad flotilla user add <name> --role admin`. The answer shows the token once;
   keep it in a password manager. After that the operator token is retired, except for adding an admin
   again (break-glass, recorded as `user.break-glass`).
2. Use your own token from then on, in the UI, or with
   `docker exec -e FLOTILLA_TOKEN=<token> flotillad flotilla …`. Loosening needs a reason:
   `flotilla resume <routine> --reason "…"`, the same for `start`, and for `autonomy` when it
   raises.
3. Add people: `flotilla user add <name> --role admin|operator|viewer`; `user list`, `user role`,
   `user rotate`, `user revoke`.

The freeze runs every routine at `shadow`, so nothing outward happens, and leaves each routine's own
autonomy as it is. An agent, an operator or an admin pulls it with `flotilla autonomy freeze` (or on
the UI's Now page). Only an operator or an admin lifts it, with a reason:
`flotilla autonomy unfreeze --reason "…"`. A run already going keeps the autonomy it started with.
While it holds, `flotilla status` starts with a `FROZEN` line, and its AUTONOMY column shows what runs
and SET what is set. STOP on a fleet is stronger: no runs at all.

A hold keeps flotilla's actions off one host or target for a while; while it lasts, policy defers
every plan there (RFC-0004 §5.1). An agent, an operator or an admin holds:
`flotilla hold host:mini 2h --reason "disk work"`; `flotilla hold list` shows the active ones;
only an operator or an admin lifts one early, with a reason: `flotilla hold lift <id> --reason "…"`.

The UI listens on `ui_listen` (for example `0.0.0.0:8486`) and answers only `ui_host` (the
`flotilla.<tailnet>` name). Put both in `flotillad.yaml`. flotillad joins the `ui` network
(`NET_UI`, default `flotilla_ui`), which is internal. The homelab's caddy-tailscale proxy joins that
network as an external one and serves the node with `bind tailscale/flotilla` and
`reverse_proxy flotillad:8486`. The proxy terminates TLS and plays no part in identity; Funnel is never
on.

The users step adds store schema step 5, and an older flotillad refuses a newer store. So before the
first deploy with it:

- run the new image read-only against a copy of the real store;
- deploy with a store copy taken first;
- to roll back, restore that copy, then deploy the previous tag.

## Deploy, roll back, operate

```sh
~/flotilla/repo/deploy/mini/deploy.sh sha-<7>        # DEPLOY PASS / DEPLOY FAIL (rolls back on an unhealthy up)
~/flotilla/repo/deploy/mini/deploy.sh --rollback
docker exec flotillad flotilla status
docker exec flotillad flotilla runs triage/cycle -n 20
docker exec flotillad flotilla stop triage            # any caller; starting again is the operator's
```

## Test it before a host has it

`deploy/e2e/run.sh` brings this compose file up on a development Docker engine with stand-in
services, and checks the heartbeat, egress (allowed and refused), telemetry, auth and hardening.
