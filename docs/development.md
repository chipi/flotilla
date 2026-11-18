# Developing flotilla

From a fresh clone to the example fleets running on your machine, then the gates every change
passes.

## What you need

| Tool | For | Notes |
| --- | --- | --- |
| Docker (with Compose v2), openssl | the demo, the e2e stack, and Go if you do not install it | the stack builds every image locally |
| Python 3 | the workers' and fleets' unit tests, the docs | standard library only; the docs build makes its own `.venv` |
| Go 1.26 | flotillad, the CLI and the host agent | optional: `scripts/go.sh` runs the pinned Go image instead |
| GNU make, git | every target below | |

The demo and the e2e use the Docker context named `dev` (a development engine kept apart from any
production one). On a machine with one engine, set `DOCKER_CONTEXT_DEV=default`, or create the
context once: `docker context create dev --docker host=unix:///var/run/docker.sock`.

## See it run: `make demo`

```bash
git clone https://github.com/chipi/flotilla && cd flotilla
make demo          # DOCKER_CONTEXT_DEV=default make demo, on a machine with one engine
```

It builds the images, starts flotillad, the workers, the egress proxy, a TLS front and stand-ins for
everything outside (GitHub, an error tracker, Grafana, a model gateway with a scripted model, the
example sites, a package index), then runs the example fleets once so the UI has cases to show. When
it prints `DEMO UP`, open <https://localhost:8443> (accept the local certificate once) and sign in
with the token in `.e2e/demo-admin.token`. `DEMO_PORT` changes the port.

What to look at:

- **Now:** what the fleets handled in the last 24 hours, and what needs you;
- **Approvals:** plans waiting for a person; empty in the demo, since every example runs in
  `shadow` (it records what it would do) until a person promotes it on **Govern**;
- **Cases:** each case's timeline, the plan, why policy decided as it did, and what the agent read;
- **Fleets:** every fleet's routines, runs, cost and logs; a STOP for each fleet and for all.

`make demo-down` removes it all. Nothing in the demo reaches the internet: the stand-ins answer
every outside call, and the bugfix fleet's coding step is off (it needs a real coding model and
toolchain).

## The gates

| Command | What it checks | Time |
| --- | --- | --- |
| `make lint` | gofmt, go vet, every Python file compiles, `llms.txt` is current, no installation facts in the code | seconds |
| `make test-unit` | Go tests (both modules), the worker's and every fleet's unit tests | ~1 min |
| `make ci-fast` | both of the above: what every push must pass | ~1 min |
| `make test-e2e` | the whole platform in containers with the stand-ins: every example fleet end to end, the UI in Chromium, a host agent, a remote worker | ~10 min |
| `make docs-validate` | markdownlint and the strict docs build | ~30 s |
| `make replay` | an installation's replay gates over its recorded cases (from its private fleets repo) | varies |

Without Go installed: `make ci-fast GO=scripts/go.sh GOFMT="scripts/go.sh gofmt"`.

`make docs-serve` serves these docs at <http://127.0.0.1:8765>.

## Where things are

| Path | What |
| --- | --- |
| `cmd/`, `internal/` | Go: flotillad (scheduler, store, cases, policy, API, UI, MCP), the `flotilla` CLI, the `flotilla-host` host agent |
| `worker/`, `runtime/` | the worker service and the Python runtime: the model client, the harness, skills, recording |
| `fleets/` | the example fleets: specs, playbooks, skills, example facts ([README](https://github.com/chipi/flotilla/blob/main/fleets/README.md)) |
| `signal-fleet/`, `bugfix/`, `deps/`, `ops/`, `seo/`, `business/` | the fleets' code |
| `host/registry.json` | the action registry: every action a routine may plan, its parameters, its executor and its check |
| `deploy/` | compose files, the server deploy kit, the remote-worker stack, the host agent's installer, the e2e stack |

## Making a change

1. Read the decision behind the code you change: [the ADRs](adr/README.md), starting with
   [ADR-0001](adr/ADR-0001-platform-foundations.md), and
   [ADR-0004](adr/ADR-0004-job-runtime.md) before runtime or fleet work.
2. A change to what a fleet decides (a playbook, gate, detector or spec) is replayed against recorded
   past cases before it ships.
3. Every model call goes through the one model client; what leaves the platform is decided by code
   after the model, never by the model.
4. `make ci-fast` before every push, `make test-e2e` for anything that crosses a container boundary.
5. No installation facts in the code: `make lint` fails on a host name, a tailnet address, a home
   path or a real repo. An installation's facts live in its own private fleets repo and host config.

## Your own fleets

Start from an example in `fleets/`, keep your fleets in a private repo of your own, and mount it over
the examples (`deploy/compose/fleets.yml`, `deploy/server/deploy_fleets.sh`). Each fleet's facts
(repos, sites, hosts) live in its config files, not in its code.
