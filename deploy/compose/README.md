# Installing flotilla on a host

One compose stack (ADR-0002): `flotillad`, one worker per role (triage, ops-read, ops-act-host,
marketing; the bugfix fleet's run elsewhere, as remote workers) and the egress
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
  deployed.tsv              written by deploy/server/deploy.sh
  backups/                  the store's snapshots, one folder per deploy or rollback (see "Roll back")
```

**State and secrets are Docker volumes**, not host folders: `flotilla-state` (the store and its daily
backups), the triage fleet's state (`TRIAGE_STATE_VOLUME`), and `flotilla-flotillad-secrets` /
`flotilla-worker-triage-secrets`. Nothing writes to the host through the VM's file sharing.
`deploy/server/secrets.sh` copies the tokens and the triage secrets file into the secret volumes on
every deploy (mode 600, owned by the reading container's uid); the token files above are read on
the host only, by that script.

`.env`:

```sh
FLOTILLA_CONFIG=<home>/flotilla/config
TRIAGE_ENV_FILE=<the triage fleet's secrets file>     # copied into the worker's secrets volume
TRIAGE_READ_ENV_FILE=<the read-only credentials, for the shadow worker>  # optional: see "The shadow worker"
TRIAGE_STATE_VOLUME=flotilla-triage-state-shadow      # the triage fleet's state (the live state volume)
FLOTILLA_ERRORS_DSN_FILE=<file with SENTRY_DSN=...>   # optional: flotillad's GlitchTip DSN, copied into its secrets volume
FLOTILLA_ERRORS_SHADOW_DSN_FILE=<file with SENTRY_DSN=...>  # optional: the shadow runs' GlitchTip project, copied beside it
TRIAGE_DEPLOY_LOG=<the triage fleet's deploys.tsv>     # optional: deploy.sh appends a row per deploy and rollback (the triage fleet's own)
FLOTILLA_FLEETS_REPO=<checkout of the private fleets repo>  # optional: the installation's own fleets (deploy_fleets.sh)
FLOTILLA_FLEETS_DIR=<FLOTILLA_HOME>/fleets            # optional: mounted over the image's fleets (fleets.yml); unset: the image's
FLOTILLA_ADMIN_TOKEN_FILE=<admin token file>          # deploy_fleets.sh pauses and resumes the triage routines with it
TRIAGE_FILING_GATE=<filing_gate.py>                    # the private fleets repo's triage/filing/scripts/filing_gate.py; unset: the deploy fails unless SF_FILING_GATE_ACCEPT says why
# FLOTILLA_BACKUPS_KEEP=10                             # optional: how many store snapshots deploy.sh keeps
# the services' Docker networks, when they are not the defaults
# NET_BACKEND=backend_default NET_LITELLM=litellm_default NET_GLITCHTIP=glitchtip_default NET_LANGFUSE=langfuse_default
```

A shadow state volume is seeded once from the live fleet's folder, without its secrets file and STOP
flag, and never overwritten after:

```sh
~/flotilla/repo/deploy/server/secrets.sh sha-<7> --seed-state flotilla-triage-state-shadow <the fleet's state folder>
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
errors_shadow_dsn_file: /run/flotilla/errors-shadow.env  # optional: FLOTILLA_ERRORS_SHADOW_DSN_FILE; a shadow run's errors go there
callers_file: /etc/flotilla/callers.yaml
workers:
  triage: {url: "http://worker-triage:8081", token_file: /run/flotilla/worker-triage.token}   # from the secrets volume
disabled_routines: [triage/cycle-shadow]   # without a candidate worker (below); -check refuses a routine whose worker is missing
examples_dir: /opt/flotilla/examples        # optional: the image's example fleets, listed as examples (flotilla status, the Examples page), never run
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
    local-model-host: {endpoint: gateway, model: <model name>, tools: none, cost: zero}
  prices: {}                             # per alias, for cost: table (input_per_mtok, output_per_mtok)
```

`-check` validates `models` with the same rules the Python client applies (shared fixtures:
`runtime/testdata/models-*.json`) and prints the number of aliases. In `worker-triage.env`,
`ROUTINE_HEARTBEAT_MODEL_ALIAS=<alias>` makes the heartbeat call that alias once per run through the
model client, traced: the platform's live check of the models section, the key, the endpoint through
the proxy and the routine's spans. Unset, the heartbeat makes no model call. The alias must be one of
the heartbeat spec's `models:` (`fleets/platform/heartbeat/v1/spec.yaml`): a run gets only the aliases
its spec names. Name the alias the
fleets actually decide with (the example: `triage-pro`, the model of `SF_TRIAGE_MODEL`), so the canary
watches the model production uses; the call allows 256 tokens, room for a model that reasons first.

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
  `ROUTINE_BACKUP_TARGET`, e.g. `host:backup-01`); add both workers under `workers` in `flotillad.yaml`
  (`url: http://worker-ops-read:8081`, `token_file: /run/flotilla/worker-ops-read.token`, and the
  same for `ops-act-host`). `secrets.sh` fills their token volumes when the tokens exist.
- **Not yet:** list `ops/backup-monitor` and `ops/rerun-backup` in `disabled_routines`.

They reach VictoriaMetrics through the proxy, already on its allowlist for flotillad's metrics.
`rerun-backup` is capped at record: its plans are recorded and nothing is sent to the host.
`flotilla case list` and `flotilla case show <id>` show what they did.

## The deploy stand-down (RFC-0004 §5.1)

While a deploy changes a host, policy defers every plan on it. `deploy/server/standdown.sh HOST DURATION
WHY` takes that stand-down: a hold on `host:HOST` by the installation's `deploy` caller, ending on
its own. It never fails its caller. `deploy.sh` takes one for 30m before `up` when `.env` names
`FLOTILLA_STANDDOWN_HOST` (e.g. `host-01`); the installation's own deploy script takes one from its
pre-pull hook. To set it up:

1. Add a caller with role `agent` (it may hold, never lift) to `callers.yaml`:
   `- {name: deploy, role: agent, token_sha256: <sha256 of the token>}`
   (`flotilla hash-token < config/deploy.token`).
2. Put the token in `config/deploy.token`; `secrets.sh` copies it into flotillad's secrets volume,
   where the script reads it by path.
3. Set `FLOTILLA_STANDDOWN_HOST=host-01` in `.env`.

## The host agents' endpoint (ADR-0006)

Optional; without it no host can enrol or connect. The CA comes from the operator's machine, offline:
`flotilla ca root DIR` once (the root's key stays there, kept as the backup key is), then
`flotilla ca intermediate DIR config/host-ca`; `secrets.sh` copies the three files there
(`intermediate.crt`, `intermediate.key`, `root.crt`) into flotillad's secrets volume, and refuses a
`root.key` among them. In `config/flotillad.yaml`:

```yaml
hosts:
  listen: 0.0.0.0:8486                   # its own port: mutual TLS, terminated by flotillad
  ca_dir: /run/flotilla                  # the three files above (secrets.sh puts them at its root)
  server_names: [<the name agents dial>]  # in flotillad's server certificate (made at start, renewed in memory)
  heartbeat: 30s                         # optional, the default: the agents' -heartbeat; three missed, dropped
  signing_key_file: /run/flotilla/signing.key   # optional: without it flotillad sends no execution
  signing_key_id: <id>
```

A proxy in front may only pass TCP through. flotillad checks at start that the root signed the
intermediate. Then `flotilla host enrolment NAME` (an operator) prints a single-use token for 15
minutes, which goes to the host inside the SSH session only, on `flotilla-host enrol`'s stdin;
`flotilla host list` and `flotilla host revoke NAME`.

The execution-signing key: `flotilla ca signing-key config/host-ca ID` writes `signing.key`
(`secrets.sh` copies it into flotillad's secrets volume) and prints the line each host's config lists
under `signing_keys`. With it, an executing case on a host is signed and sent (approved `auto` by
policy, or by the operator who approved it), valid for 10 minutes; the host's result moves the case:
done to checking, a precondition that no longer holds closes it, anything else escalates it with the
target still held. A host not connected is retried for 10 minutes, then the case escalates.

## The candidate worker: shadow comparison of a candidate image

ADR-0004's live proof for a change to the triage fleet after the switch: the image under test runs
as a second worker beside the live one, with its own state, and the routine `triage/cycle-shadow`,
capped at `record` by its spec (`max_autonomy`), runs the same command on it. flotillad refuses any
autonomy above the cap, even the operator's, and the scheduler never runs it above the cap whatever
a control says.

1. Seed its state from the live state, once per comparison (a fresh volume name each time):
   `~/flotilla/repo/deploy/server/secrets.sh sha-<7> --seed-state
   flotilla-triage-state-candidate-<date> ~/signal-fleet`
2. `.env`: `COMPOSE_PROFILES=candidate`, `FLOTILLA_CANDIDATE_TAG=sha-<candidate>`,
   `TRIAGE_CANDIDATE_STATE_VOLUME=flotilla-triage-state-candidate-<date>`.
3. Its own worker token, never the live worker's (flotillad refuses to start, and `-check` says
   "workers triage and triage-candidate share a token file", when two different workers share one):
   `FLOTILLA_CONFIG/worker-triage-candidate.token`, 32+ random characters. `secrets.sh` puts it in
   flotillad's secrets volume and in the candidate's own (`worker-triage-candidate-secrets`, with the
   fleet env). `config/flotillad.yaml`: add `triage-candidate: {url: "http://worker-triage-candidate:8081",
   token_file: /run/flotilla/worker-triage-candidate.token}` under `workers`, and take
   `triage/cycle-shadow` out of `disabled_routines`.
4. Deploy (the live tag, unchanged). Compare the two routines' decisions with the installation's own
   tool (the ledgers are each worker's state volume).
5. To end it: put `triage/cycle-shadow` back in `disabled_routines`, drop `COMPOSE_PROFILES`,
   deploy. The candidate's volume stays until removed by hand.

The candidate has its own secrets volume (its worker token and a copy of the fleet env) and shares
the proxy's allowlist: at record the fleet makes no outward change (orchestrator.py forces a dry
run), and the cap keeps it there. It reads the live worker's `worker-triage.env`, but the compose
file sets `ROUTINE_SF_SERVICE=triage-fleet-candidate` and `ROUTINE_SF_GLITCHTIP_SYNC=0` over it,
so its metrics, inbox and traces never count as the live fleet's.

## The shadow worker: shadow versions of a routine (docs/wip/v2-design.md section 4)

A routine folder with more than one version (`routine.yaml`, `v1/`, `v2/`) runs its live version
as always. With `shadows.enabled: true` in `flotillad.yaml`, every live run of it also gives each
shadow version a run on the same frozen input, on a worker of its own, and the two versions'
decisions are paired per subject (`flotilla pairs ROUTINE vN`, the routine page's Pairs section).
A shadow has no effect: what it records is "would run X (blocked: shadow)", never sent, filed or
posted. Shadows start off.

The compose service is `worker-triage-shadow` (profile `shadow`), with its own proxy,
`egress-shadow`:

1. `.env`: add `shadow` to `COMPOSE_PROFILES`; `TRIAGE_READ_ENV_FILE`: a host file of the
   credentials that can only read (a token that can write must not be in it). `secrets.sh` puts it
   in the shadow worker's own volume as `read.env`; without it the shadow worker has no
   credentials. The live worker's `fleet.env` is never in that volume, and the compose file gives
   the shadow worker no `FLOTILLA_ENV_FILES` and no write file (a shadow worker with either
   refuses to start).
2. Its own worker token, never the live worker's: `FLOTILLA_CONFIG/worker-triage-shadow.token`,
   32+ random characters. `secrets.sh` puts it in flotillad's volume and in the shadow worker's.
   `flotillad.yaml`: under `workers`, `triage-shadow: {url: "http://worker-triage-shadow:8081",
   token_file: /run/flotilla/worker-triage-shadow.token, shadow: true}`; flotillad calls the
   worker with that token and never accepts it on its workers' listener.
3. Its proxy's allowlist: `FLOTILLA_CONFIG/egress-shadow-acl.yaml`, the same form as
   `egress-acl.yaml` (a shadow reads what the live fleet reads; start from a copy). Every request
   a shadow makes is in `docker logs flotilla-egress-shadow`, apart from the live proxy's.
4. `flotillad.yaml`: `shadows: {enabled: true}`, and `errors_shadow_dsn_file:
   /run/flotilla/errors-shadow.env` with `FLOTILLA_ERRORS_SHADOW_DSN_FILE` in `.env` (a GlitchTip
   project of its own, so a shadow's errors never reach the live project). `flotillad -check` then
   lists each routine's shadow versions and the worker they run on, and refuses a routine with several
   versions whose
   shadow worker (`shadow_worker:` in `routine.yaml`, default `<worker>-shadow`) is not a worker
   declared `shadow: true`.
5. A routine that keeps state (`state:` in `routine.yaml`, paths under the worker's HOME) is
   snapshotted by the live worker when a live run starts, into the `triage-snapshots` volume; the
   shadow worker mounts it read-only and starts from a copy in its own HOME.

The shadow worker has less than the live one (512m and 0.5 CPU against 1g and 1.0, and a low CPU
weight), so a shadow cannot take a live run's room. It mounts none of the live worker's volumes
but the snapshots, read-only, and its state is its own volume (`triage-shadow-state`).

It is on a network of its own, `shadow` (internal: no route out), with its proxy and flotillad;
flotillad joins it to call the worker and to serve its ledger, and nothing else live does. So a
shadow routine cannot point its proxy setting at the live proxy (`egress`) or reach a live worker:
they are not on its network. The proxy it has is `egress-shadow`, whose allowlist is its own.

The worker refuses a shadow run whose environment holds a credential that writes, by name, with
no configuration: a built-in list (`DEFAULT_WRITE_KEYS` in `worker/flotilla_worker.py`: the GitHub,
GlitchTip event-write, Grafana admin, Slack and bugfix-pusher names this repo's routines use),
extended by `FLOTILLA_WRITE_KEYS` (comma-separated; the compose file sets it on the shadow worker)
and by the keys of any `FLOTILLA_WRITE_ENV_FILES` it can read. A write credential whose name is on
neither list is not caught by name; keep it out of `TRIAGE_READ_ENV_FILE`, which is the one file
the shadow worker reads. The shadow proxy is not on Langfuse (a trace push is a write), so leave the
`LANGFUSE_*` keys out of it too. GitHub reads in a shadow use `GITHUB_READ_TOKEN` (a token that can only
read: triage's `filing._gh` takes it for GET requests when it has no `GITHUB_TOKEN`, never for a
write), since `GITHUB_TOKEN` itself is refused by name. A test on the compose file
(`cmd/flotillad/compose_test.go`) holds the limits, the volumes, the network and that every
`*-secrets` volume is external.

Another worker's routines are shadowed by a shadow worker of their own (`<worker>-shadow` by
default), declared the same way. The e2e stack has one, `worker-ops-read-shadow` in
`deploy/e2e/docker-compose.e2e.yml`, for the example fleets on `ops-read` (its model: the ops image
in shadow mode, a low CPU weight, the `shadow` network and `egress-shadow`, only its own worker
token). `secrets.sh` puts `FLOTILLA_CONFIG/worker-ops-read-shadow.token` in flotillad's volume and
the worker's when the file exists, and flotillad's `NO_PROXY` must name the new worker, or its calls
go through the proxy and come back 407. This compose file ships none: which workers get a shadow
worker is the installation's choice.

To end shadows: `shadows.enabled: false` (a shadow version is then listed and promotable, and does
not run), drop `shadow` from `COMPOSE_PROFILES`, deploy. The shadow volumes stay until removed by
hand.

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
2. Import its namespace from its own state, once (counts only are printed). `run_tokens` is
   `required` unless flotillad.yaml says `accept` (the one-release way back; `-check` warns): every
   run has its own token, and a worker's own token opens nothing on `worker_listen`. So the import
   and the check are the operator's. Put an admin's (or the bootstrap operator's) token in a file in
   the worker's tmpfs for the duration, pass it as `--token-file` (or `FLOTILLA_API_TOKEN_FILE`)
   instead of `/run/flotilla/worker.token`, and remove it after:

   ```sh
   printf %s "$TOKEN" | docker exec -i <worker container> sh -c 'umask 077 && cat > /tmp/operator.token'
   docker exec -w /opt/flotilla/signal-fleet/mvp -e FLOTILLA_API_URL=http://flotillad:8485 \
     -e NO_PROXY=flotillad -e FLOTILLA_ROUTINE=<routine id> <worker container> \
     python3 ledger_sync.py import --token-file /tmp/operator.token
   docker exec <worker container> rm /tmp/operator.token
   ```

   Without a token file they say "operator token required". flotillad serves the operator's import
   and reads of a named ledger on `worker_listen` (and on the full API); it takes no row from an
   operator, and the import is an event naming who. `pad` is local (it rewrites filed.tsv) and needs
   no token. The compose files no longer give the workers the worker token as a fallback for a
   routine's ledger calls: a run uses its own. `gate_ledger_backend_replay.py` also writes rows,
   which no token outside a run may do under `required` (the operator's is import and read only, a
   run's is confined to its routine's namespace): run it against a flotillad in
   `run_tokens: accept`, with the worker's own token, before the switch (the e2e does). Upgrading to
   this build is one deploy: `flotillad -check` runs while the previous tag's workers still answer and
   prints `worker W: run-token capable` or `NOT run-token capable` for each worker it can reach
   without refusing; `up` recreates the local workers with flotillad; the start then refuses a local
   worker still without the capability. Upgrade the remote workers and host agents after the mini
   (flotillad first: an agent of this build is refused by a flotillad of the previous build with
   `json: unknown field "caps"`); until then their links are refused under `required`.
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

The freeze runs every routine at `record`, so nothing outward happens, and leaves each routine's own
autonomy as it is. An agent, an operator or an admin pulls it with `flotilla autonomy freeze` (or on
the UI's Now page). Only an operator or an admin lifts it, with a reason:
`flotilla autonomy unfreeze --reason "…"`. A run already going keeps the autonomy it started with.
While it holds, `flotilla status` starts with a `FROZEN` line, and its AUTONOMY column shows what runs
and SET what is set. STOP on a fleet is stronger: no runs at all.

A hold keeps flotilla's actions off one host or target for a while; while it lasts, policy defers
every plan there (RFC-0004 §5.1). An agent, an operator or an admin holds:
`flotilla hold host:host-01 2h --reason "disk work"`; `flotilla hold list` shows the active ones;
only an operator or an admin lifts one early, with a reason: `flotilla hold lift <id> --reason "…"`.

The UI listens on `ui_listen` (for example `0.0.0.0:8486`) and answers only `ui_host` (the
`flotilla.<tailnet>` name). Put both in `flotillad.yaml`. flotillad joins the `ui` network
(`NET_UI`, default `flotilla_ui`), which is internal. The installation's caddy-tailscale proxy joins
that network as an external one and serves the node with `bind tailscale/flotilla` and
`reverse_proxy flotillad:8486`. The proxy terminates TLS and plays no part in identity; Funnel is never
on.

An older flotillad refuses a store a newer one has moved forward ("database schema N is newer than
this flotillad"), and every schema step is one-way. So a deploy that adds a schema step cannot be
rolled back by starting the old image: see "Roll back" below. Before the first deploy that adds a
step, run the new image read-only against a copy of the real store.

## Deploy, roll back, operate

```sh
~/flotilla/repo/deploy/server/deploy.sh sha-<7>        # DEPLOY PASS / DEPLOY FAIL (rolls back on an unhealthy up)
~/flotilla/repo/deploy/server/deploy.sh --rollback
docker exec flotillad flotilla status
docker exec flotillad flotilla runs triage/cycle -n 20
docker exec flotillad flotilla stop triage            # any caller; starting again is the operator's
```

### The filing gate

A deploy runs the triage fleet's filing gate (`TRIAGE_FILING_GATE`) against the commit running now.
A deploy fails when the gate fails, and also when it did not run: its script is missing. Either is
accepted by naming the reason: `SF_FILING_GATE_ACCEPT="<why>" deploy.sh sha-<7>`. Only a first
install (no earlier deploy to compare with) passes without it, and the output says `NOT RUN (no
earlier deploy ...)`. A rollback never runs the gate.

### The installation's own fleets

The images carry flotilla's fleets, the examples. An installation runs its own from a private repo
(vision plan step 7, route B): `FLOTILLA_FLEETS_DIR` makes `deploy.sh` add `fleets.yml`, which
mounts that folder read-only over `/opt/flotilla/fleets` in flotillad and the triage workers (the
specs, and the playbooks, skills and harness settings the triage worker reads).

```sh
~/flotilla/repo/deploy/server/deploy_fleets.sh --diff <commit>   # what it changes against the running fleets
~/flotilla/repo/deploy/server/deploy_fleets.sh <commit>          # FLEETS DEPLOY PASS / FLEETS DEPLOY FAIL
~/flotilla/repo/deploy/server/deploy_fleets.sh --rollback
```

Each commit of `FLOTILLA_FLEETS_REPO` is exported once to `~/flotilla/fleets-<sha7>/`, read-only;
`~/flotilla/fleets` is a symlink to the deployed one. A deploy lists the files that differ from what
the triage worker runs, runs the running tag's `flotillad -check` with the new folder, pauses the
triage routines (resumed on any exit), points the symlink, recreates flotillad and the triage
workers, and prints every routine whose spec hash changed and every routine version whose state or hash
changed (a deploy that adds a `v2/` prints one new shadow line). A live or previous version whose folder
changed is frozen: `-check` refuses it before anything is paused, and the deploy ends
`FLEETS DEPLOY FAIL: versions ...`. `fleets-deployed.tsv` records each one.
An image deploy (`deploy.sh`) keeps the mounted fleets. A fleets commit that needs a newer flotilla
is refused by `-check`: deploy the tag first.

### Snapshots

Before every `up`, a deploy and a rollback alike, `deploy.sh` snapshots the store: a throwaway
container of the worker image with no network (flotillad's own image has no shell) copies
`flotilla.db` from the state volume with SQLite's online backup, so the copy is consistent beside a
running flotillad, and streams it to the host. It lands in `~/flotilla/backups/<tag>-<UTC time>/`
(`flotilla.db`, `SCHEMA` with the store's newest schema step, `WHY`; owner-only, as the store holds
sessions and approvals). `<tag>` is the tag being deployed, so the snapshot is of the store as that
deploy found it. The newest 10 are kept (`FLOTILLA_BACKUPS_KEEP`); only folders named like that are
ever deleted. A snapshot that cannot be made, or is not a sound database, stops the deploy before
anything is stopped. A first install has no store and takes none.

The snapshot copies the database only; the first deploy that carries it must be run by the new
script, so check out its commit in `~/flotilla/repo` first (the script that runs is the one in the
checkout).

### Roll back

`deploy.sh --rollback` goes back to the tag deployed before the current one. Its `-check` opens the
store read-only and fails, with nothing stopped, when the store is newer than that tag's flotillad;
for a tag older than that check, deploy.sh reads the tag's schema steps from its
`internal/store/store.go`. When the tag cannot run the store, the rollback restores a snapshot:

1. it picks the newest snapshot whose schema the tag accepts (taken before the deploy that moved the
   schema); if there is none it stops saying so, with nothing stopped, restored or changed;
2. it snapshots the store as it is now (the newer schema, kept for a way forward), stops flotillad,
   replaces `flotilla.db` (and removes its `-wal` and `-shm`) with the picked snapshot, and runs
   `-check` again;
3. it brings the stack up and prints exactly what it restored.

**Rows written after the snapshot are lost on such a rollback** (runs, ledger entries, approvals,
cases since that deploy started). The pre-rollback snapshot keeps them, for reading, not for
merging. A deploy whose `up` is unhealthy falls back to the previous tag the same way, restoring the
snapshot taken for that deploy. `deploy.sh` prints `rolled back to <tag> (running and healthy)` only
when that tag is running and healthy; otherwise it ends `DEPLOY FAIL` and says which tag is running.

## Test it before a host has it

`deploy/e2e/run.sh` brings this compose file up on a development Docker engine with stand-in
services, and checks the heartbeat, egress (allowed and refused), telemetry, auth and hardening.
