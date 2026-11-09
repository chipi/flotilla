# Installing flotilla on a host

One compose stack (ADR-0002): `flotillad`, one worker per fleet (`worker-triage` today) and the egress
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
    worker-triage.env       the installation's overrides for jobs (JOB_*); no secrets
    smokescreen.yaml        the proxy: listen address, which private ranges it may reach
    egress-acl.yaml         the proxy: which host names jobs may reach
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

`config/flotillad.yaml` (every field is required except `timezone`, `otlp_traces_url`, `disabled_jobs`,
`models`):

```yaml
listen: 127.0.0.1:8484                 # inside the container: the CLI runs there (docker exec)
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
disabled_jobs: [triage/cycle-shadow]   # without a candidate worker (below); -check refuses a job whose worker is missing
models:                                 # optional (ADR-0004): the aliases jobs name; every run gets it as FLOTILLA_MODELS
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
`JOB_HEARTBEAT_MODEL_ALIAS=<alias>` makes the heartbeat call that alias once per run through the model
client, traced: the platform's live check of the models section, the key, the endpoint through the proxy
and the job's spans. Unset, the heartbeat makes no model call.

`config/callers.yaml` (`flotilla hash-token < token-file` prints the hash):

```yaml
callers:
  - {name: operator, role: operator, token_sha256: <64 hex>}
  - {name: <agent name>, role: agent, token_sha256: <64 hex>}
```

The **operator's** token stays with the operator: stage changes, resuming a job and starting a
STOPped fleet need it, and no agent ever holds it (AGENTS.md rule 2). The CLI inside the container
(`cli.token`) uses an **agent** token; the operator passes theirs per call:

```sh
docker exec -e FLOTILLA_TOKEN="$(cat <operator token file>)" flotillad flotilla stage triage/cycle propose
```

`config/worker-triage.env`: the triage fleet reads its services by these URLs, all through the proxy.

```sh
JOB_SF_SERVICE=triage-fleet                 # triage-fleet-shadow while it runs beside fleetd
JOB_SF_VM_URL=http://victoriametrics:8428
JOB_SF_VL_URL=http://victorialogs:9428
JOB_SF_VT_URL=http://victoriatraces:10428
JOB_SF_GRAFANA_URL=http://grafana:3000
JOB_SF_GLITCHTIP_URL=http://<glitchtip web container>:8080
JOB_SF_OPENROUTER_URL=http://litellm:4000/v1/chat/completions
JOB_LANGFUSE_HOST=http://langfuse-web:3000
JOB_HEARTBEAT_ALLOWED_URL=http://litellm:4000/health/liveliness
JOB_HEARTBEAT_DENIED_URL=http://example.com/
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

## The candidate worker: shadow comparison of a candidate image

ADR-0004's live proof for a change to the triage fleet after the switch: the image under test runs
as a second worker beside the live one, with its own state, and the job `triage/cycle-shadow`,
capped at `shadow` by its spec (`max_stage`), runs the same command on it. flotillad refuses any
stage above the cap, even the operator's, and the scheduler never runs it above the cap whatever a
control says.

1. Seed its state from the live state, once per comparison (a fresh volume name each time):
   `~/flotilla/repo/deploy/mini/secrets.sh sha-<7> --seed-state
   flotilla-triage-state-candidate-<date> ~/signal-fleet`
2. `.env`: `COMPOSE_PROFILES=candidate`, `FLOTILLA_CANDIDATE_TAG=sha-<candidate>`,
   `TRIAGE_CANDIDATE_STATE_VOLUME=flotilla-triage-state-candidate-<date>`.
3. `config/flotillad.yaml`: add `triage-candidate: {url: "http://worker-triage-candidate:8081", token_file:
   /run/flotilla/worker-triage.token}` under `workers`, and take `triage/cycle-shadow` out of `disabled_jobs`.
4. Deploy (the live tag, unchanged). Compare with
   `deploy/mini/shadow-compare.sh --volume flotilla-triage-state-candidate-<date> --since <seed time>`.
5. To end it: put `triage/cycle-shadow` back in `disabled_jobs`, drop `COMPOSE_PROFILES`, deploy. The
   candidate's volume stays until removed by hand.

The candidate shares the triage worker's secrets volume and the proxy's allowlist: in shadow the fleet
makes no outward change (orchestrator.py forces a dry run), and the cap keeps it there.

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
