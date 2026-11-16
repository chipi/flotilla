# Remote workers on a machine (RFC-0011)

A worker can run on any machine on the tailnet. flotillad, on the mini, reaches it through a **link**
beside it that dials flotillad's host endpoint under the worker's own identity, `w-<worker>-<node>`.
This folder is the stack for one machine: today the bugfix fleet's two workers on the X3 (`omarchy`).

## What runs

| Service | What it holds | Network |
| --- | --- | --- |
| `worker-bugfix` | its worker token; no model key (intake, intake-podcast and pr-check call no model) | `inner` only (out through `egress`) |
| `worker-bugfix-fix`, `worker-bugfix-fix-podcast` | one per fix routine: its worker token, only its own routine's LiteLLM key, its own scratch (`scratch-fix/`, `scratch-fix-podcast/`) | `inner` only |
| `worker-bugfix-act` | its worker token, the GitHub App's key (`app/`, once made) | `inner` only |
| `link-bugfix`, `link-bugfix-act`, `link-bugfix-fix`, `link-bugfix-fix-podcast` | the identity's key and certificate (`state/<identity>/`), the worker's token | `inner` and `out` |
| `egress` | this machine's allowlist | `inner` and `out` |

Nothing is published. A link at another version than flotillad's is refused (logged as an error on the
mini): deploy the same tag on both.

## Install (once per machine)

On the machine, as the operator's user, in `FLOTILLA_REMOTE` (the X3: `~/flotilla-remote`):

1. **Layout**, mode 700: `secrets/`, `app/`, `state/w-bugfix/`, `state/w-bugfix-act/`, `state/w-bugfix-fix/`,
   `state/w-bugfix-fix-podcast/`, `scratch/`, `scratch-fix/`, `scratch-fix-podcast/`.
2. **`secrets/`**, each file mode 600, never printed:
   - `worker-bugfix.token`, `worker-bugfix-act.token`, `worker-bugfix-fix.token`,
     `worker-bugfix-fix-podcast.token`: 32 random bytes as hex each (`openssl rand
     -hex 32`); they stay on this machine, between each link and its worker.
   - `bugfix-model-fix.key`, `bugfix-model-fix-podcast.key`: the LiteLLM `fleet-bugfix-fix` and
     `fleet-bugfix-fix-podcast` keys, one per fix routine ($1 a day each), alone in their files.
3. **`root.crt`**: flotilla's host root certificate (the mini's `~/flotilla/config/host-ca/root.crt`).
4. **`smokescreen.yaml`** and **`egress-acl.yaml`**:

   ```yaml
   # smokescreen.yaml
   ip: 0.0.0.0
   port: 4750
   acl_file: /etc/smokescreen/acl.yaml
   allow_missing_role: true
   allow_ranges: [100.64.0.0/10]   # the tailnet: the LiteLLM gateway on the mini
   ```

   ```yaml
   # egress-acl.yaml
   version: v1
   services: []
   default:
     project: flotilla-remote
     action: enforce
     allowed_domains: [homelab, github.com, codeload.github.com, api.github.com, registry.npmjs.org,
                       release-assets.githubusercontent.com, objects.githubusercontent.com,
                       pypi.org, files.pythonhosted.org]   # pypi: fix-podcast's venv
   ```

5. **`.env`** (no secrets in it):

   ```bash
   FLOTILLA_REMOTE=/home/<user>/flotilla-remote
   FLOTILLA_UID=1000
   FLOTILLA_GID=1000
   FLOTILLA_NODE=omarchy
   FLOTILLA_TAG=sha-<7>              # flotillad's
   FLOTILLA_HOSTS_ADDR=flotilla-hosts.<tailnet>:8487
   FLOTILLA_HOSTS_NAME=flotilla-hosts.<tailnet>
   ```

6. **Enrol each link** with a token an operator mints on the mini
   (`docker exec flotillad flotilla host enrolment w-bugfix-omarchy`), passed on stdin only:

   ```bash
   docker run --rm -i --user 1000:1000 -v ~/flotilla-remote:/r --dns 100.100.100.100 \
     --entrypoint /usr/local/bin/flotilla-host ghcr.io/chipi/flotilla-worker-bugfix:$FLOTILLA_TAG \
     enrol -identity w-bugfix-omarchy -state /r/state/w-bugfix -root /r/root.crt \
     -addr $FLOTILLA_HOSTS_ADDR -server-name $FLOTILLA_HOSTS_NAME < token-file
   ```

   and the same for `w-bugfix-act-omarchy` into `state/w-bugfix-act`, `w-bugfix-fix-omarchy` into
   `state/w-bugfix-fix` and `w-bugfix-fix-podcast-omarchy` into `state/w-bugfix-fix-podcast`.
   Delete the token files after.
7. **On the mini**, in `flotillad.yaml`: `workers: {bugfix: {remote: w-bugfix-omarchy}, bugfix-act:
   {remote: w-bugfix-act-omarchy}, bugfix-fix: {remote: w-bugfix-fix-omarchy}, bugfix-fix-podcast:
   {remote: w-bugfix-fix-podcast-omarchy}}`, and the `gateway-bugfix-fix` and
   `gateway-bugfix-fix-podcast` endpoints' `base_url` the gateway as this machine reaches it
   (`http://homelab:4001/v1`). Drop `bugfix` from the mini's `COMPOSE_PROFILES`.
8. **Up**, from the deploy checkout: `docker compose --env-file ~/flotilla-remote/.env -f
   deploy/remote/docker-compose.yml up -d` (the X3: a user unit runs it).

The tailnet must let this machine reach `flotilla-hosts:8487` (chipi/infra `tailscale/policy.hujson`).

## Verification

- On the mini: `docker logs flotillad | grep "remote worker connected"` names both workers.
- A routine on them runs: `docker exec flotillad flotilla runs bugfix/intake` shows `ok`.
- Stop the stack: their routines show `skipped … worker bugfix is not connected`, and their cases wait.

## Troubleshooting

- **`remote worker refused: its version is not flotillad's`:** set `FLOTILLA_TAG` to flotillad's.
- **No connection:** from the machine, `bash -c 'echo > /dev/tcp/flotilla-hosts.<tailnet>/8487'`;
  refused means the tailnet ACL.
- **A routine's network denied:** `docker logs flotilla-remote-egress | grep '"allow":false'`; add the
  domain only after reading what asked for it.
