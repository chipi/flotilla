# Example fleets

These are flotilla's **example fleets**: what the images carry, what CI and the e2e stack run, and the
starting point for an installation's own. They are shaped like a real installation's, and every fact
in them is a placeholder: the repos are `example/...`, the sites are `example.com`, the addresses are
documentation addresses.

| Fleet | Routines | What it shows |
| --- | --- | --- |
| `triage` | `cycle`, `cycle-shadow` | alerts and errors triaged by a model, filed or dismissed by code gates; a candidate model compared in shadow before any switch |
| `bugfix` | `intake`, `fix`, `open-pr`, `pr-check` | a bug issue becomes a case, a coding agent fixes it under gates, a person approves the PR plan and always merges |
| `jobs` | `backup-monitor`, `rerun-backup`, `verify-backup` | a monitor opens a case, an acting routine plans a rerun on a host, the host agent runs only what its own config allows, a check verifies it; shadow, propose, then auto |
| `review` | `case-review`, `process-change` | closed cases become proposed lessons and process changes with their evidence; a person approves every change |
| `deps` | `scan`, `upgrade` | vulnerable or outdated pins become cases and one-line upgrade PRs; no model: code finds and plans, the repo's CI tests, a person approves and merges |
| `sites` | `site-check` | what a site serves live (robots, sitemaps, head tags, broken links, certificate expiry), read only; findings are cases |
| `platform` | `heartbeat`, `model-contract` | the platform's own canary and the model aliases' contract checks |

## An installation's own fleets

An installation keeps its fleets in a private repo of its own, in this same layout, and runs them
instead of these: `deploy/compose/fleets.yml` mounts that folder over `/opt/flotilla/fleets`, and
`deploy/mini/deploy_fleets.sh <commit>` deploys a commit of it (`deploy/compose/README.md`, "The
installation's own fleets"). Its facts live there, never in flotilla:

| File | Holds |
| --- | --- |
| `triage/installation.json` | which repo each error project and alert instance files into, the ops repo, the repos the bugfix chain builds, the assistant backend, the edge instance, the probes' log and metric labels, the scrubber's own rules (`signal-fleet/mvp/installation.py`) |
| the repos file a deps scan names (`--repos`) | the repos, their requirements file, and whether outdated pins count (`deps/common.py`) |
| the sites file a site-check spec names (`--sites`) | the sites, their pages and what to check on each (`seo/common.py`) |
| the routine specs | the repos and models each routine uses (`bugfix/*.yaml`'s `--repo`, the model aliases), and the names each job routine runs with (`BACKUP_ACTION`, `BACKUP_METRIC` on the `jobs` commands) |
| an action registry of its own | the installation's own actions, named in `flotillad.yaml`'s `registry_files` and in the ops workers' `FLOTILLA_REGISTRY`; the image carries the examples' (`host/registry.json`) |

More facts are host config, not files here: the repos `github.open_pr` may act on
(`action_enums` in `flotillad.yaml`; the registry declares example repos), what each host's agent
runs for an action (its own config on that host), and the private replay gates
(`make replay` with `SF_REPLAYS`). `scripts/installation_facts.py`, run by `make lint`, keeps
installation facts out of flotilla's code.
