# Remote workers on a machine (RFC-0011)

A worker can run on any machine on the tailnet. flotillad, on the control-plane host, reaches it
through a **link** beside it that dials flotillad's host endpoint under the worker's own identity, `w-<worker>-<node>`.
This folder is the stack for one machine: today the bugfix fleet's three workers, each with its
link, on a second machine.

## What runs

| Service | What it holds | Network |
| --- | --- | --- |
| `worker-bugfix` | its worker token; no model key (intake and pr-check call no model) | `inner` only (out through `egress`) |
| `worker-bugfix-fix` | its worker token, only its own routine's LiteLLM key, its own scratch (`scratch-fix/`) | `inner` only |
| `worker-bugfix-act` | its worker token, the GitHub App's key (`app/`, once made) | `inner` only |
| `link-bugfix`, `link-bugfix-act`, `link-bugfix-fix` | the identity's key and certificate (`state/<identity>/`), the worker's token | `inner` and `out` |
| `egress` | this machine's allowlist | `inner` and `out` |

Nothing is published. A link at another version than flotillad's is refused (logged as an error on the
control-plane host): deploy the same tag on both.

## Install (once per machine)

On the machine, as the operator's user, in `FLOTILLA_REMOTE` (for example `~/flotilla-remote`):

1. **Layout**, mode 700: `secrets/`, `app/`, `state/w-bugfix/`, `state/w-bugfix-act/`, `state/w-bugfix-fix/`,
   `scratch/`, `scratch-fix/`.
2. **`secrets/`**, each file mode 600, never printed:
   - `worker-bugfix.token`, `worker-bugfix-act.token`, `worker-bugfix-fix.token`: 32 random bytes
     as hex each (`openssl rand -hex 32`); they stay on this machine, between each link and its worker.
   - `bugfix-model-fix.key`: the LiteLLM `fleet-bugfix-fix` key, per fix routine ($1 a day), alone
     in its file.
3. **`root.crt`**: flotilla's host root certificate (the control-plane host's `~/flotilla/config/host-ca/root.crt`).
4. **`smokescreen.yaml`** and **`egress-acl.yaml`**:

   ```yaml
   # smokescreen.yaml
   ip: 0.0.0.0
   port: 4750
   acl_file: /etc/smokescreen/acl.yaml
   allow_missing_role: true
   allow_ranges: [100.64.0.0/10]   # the tailnet: the LiteLLM gateway on the control-plane host
   ```

   ```yaml
   # egress-acl.yaml
   version: v1
   services: []
   default:
     project: flotilla-remote
     action: enforce
     allowed_domains: [example, github.com, codeload.github.com, api.github.com, registry.npmjs.org,
                       release-assets.githubusercontent.com, objects.githubusercontent.com,
                       pypi.org, files.pythonhosted.org]   # pypi: the api fix routine's venv
   ```

5. **`.env`** (no secrets in it):

   ```bash
   FLOTILLA_REMOTE=/home/<user>/flotilla-remote
   FLOTILLA_UID=1000
   FLOTILLA_GID=1000
   FLOTILLA_NODE=<host>
   FLOTILLA_TAG=sha-<7>              # flotillad's
   FLOTILLA_HOSTS_ADDR=flotilla-hosts.<tailnet>:8487
   FLOTILLA_HOSTS_NAME=flotilla-hosts.<tailnet>
   ```

6. **Enrol each link** with a token an operator mints on the control-plane host
   (`docker exec flotillad flotilla host enrolment w-bugfix-<host>`), passed on stdin only:

   ```bash
   docker run --rm -i --user 1000:1000 -v ~/flotilla-remote:/r --dns 100.100.100.100 \
     --entrypoint /usr/local/bin/flotilla-host ghcr.io/chipi/flotilla-worker-bugfix:$FLOTILLA_TAG \
     enrol -identity w-bugfix-<host> -state /r/state/w-bugfix -root /r/root.crt \
     -addr $FLOTILLA_HOSTS_ADDR -server-name $FLOTILLA_HOSTS_NAME < token-file
   ```

   and the same for `w-bugfix-act-<host>` into `state/w-bugfix-act` and `w-bugfix-fix-<host>` into
   `state/w-bugfix-fix`. Delete the token files after.
7. **On the control-plane host**, in `flotillad.yaml`: `workers: {bugfix: {remote:
   w-bugfix-<host>}, bugfix-act: {remote: w-bugfix-act-<host>}, bugfix-fix: {remote:
   w-bugfix-fix-<host>}}`, and the `gateway-bugfix-fix` endpoint's `base_url` the gateway as this
   machine reaches it (`http://<gateway-host>:4001/v1`). Drop `bugfix` from the control-plane host's
   `COMPOSE_PROFILES`.
   A link that drops whenever its machine sleeps (a laptop) can say so: `bugfix: {remote:
   w-bugfix-<host>, intermittent: true}`. Its `not_connected` then raises no host alert (no ERROR
   log, no GlitchTip issue); Hosts and the host page still show it not connected, and its other
   alerts (clock skew, certificate expiry) still raise. `flotillad -check` prints `intermittent`
   on its line.
8. **Up**, from the deploy checkout: `docker compose --env-file ~/flotilla-remote/.env -f
   deploy/remote/docker-compose.yml up -d` (a user unit can run it).

The tailnet must let this machine reach `flotilla-hosts:8487` (the tailnet's policy file).

## Verification

- On the control-plane host: `docker logs flotillad | grep "remote worker connected"` names all
  three links.
- A routine on them runs: `docker exec flotillad flotilla runs bugfix/intake` shows `ok`.
- Stop the stack: their routines show `skipped … worker bugfix is not connected`, and their cases wait.

## Troubleshooting

- **`remote worker refused: its version is not flotillad's`:** set `FLOTILLA_TAG`
  to flotillad's. It logs at ERROR once per (worker version, flotillad version) pair, and once at
  INFO when the worker is accepted again; the Hosts page shows the mismatch as long as it lasts.
- **No connection:** from the machine, `bash -c 'echo > /dev/tcp/flotilla-hosts.<tailnet>/8487'`;
  refused means the tailnet ACL.
- **A routine's network denied:** `docker logs flotilla-remote-egress | grep '"allow":false'`; add the
  domain only after reading what asked for it.
