# Example fleets

These are flotilla's **example fleets**: what the images carry, what CI and the e2e stack run, and the
starting point for an installation's own. They are shaped like a real installation's, and every fact
in them is a placeholder: the repos are `example/...`, the sites are `example.com`, the addresses are
documentation addresses.

| Fleet | Routines | What it shows |
| --- | --- | --- |
| `triage` | `cycle`, `cycle-shadow` | alerts and errors triaged by a model, filed or dismissed by code gates; a candidate image compared in shadow |
| `bugfix` | `intake`, `fix`, `open-pr`, `pr-check`, and a per-repo profile (`intake-podcast`, `fix-podcast`) | a bug issue becomes a case, a coding agent fixes it under gates, a person approves the PR plan |
| `ops` | `backup-monitor`, `rerun-backup`, `verify-backup`, `case-review`, `process-change` | a monitor opens a case, an acting routine plans a fix on a host, a check verifies it, a review proposes what to learn |
| `seo` | `site-check`, `traffic`, `search-console` | what a site serves live, its visits and its search reach, as findings |
| `platform` | `heartbeat`, `model-contract` | the platform's own canary and the model aliases' contract checks |

## An installation's own fleets

An installation keeps its fleets in a private repo of its own, in this same layout, and runs them
instead of these: `deploy/compose/fleets.yml` mounts that folder over `/opt/flotilla/fleets`, and
`deploy/mini/deploy_fleets.sh <commit>` deploys a commit of it (`deploy/compose/README.md`, "The
installation's own fleets"). Its facts live there, never in flotilla:

| File | Holds |
| --- | --- |
| `triage/installation.json` | which repo each error project and alert instance files into, the ops repo, the repos the bugfix chain builds, the assistant backend, the edge instance, the probes' log and metric labels, the scrubber's own rules (`signal-fleet/mvp/installation.py`) |
| `seo/sites.json` | the sites, their pages, analytics ids and search properties (`seo/common.py`) |
| the routine specs | the repos and models each routine uses (`bugfix/*.yaml`'s `--repo`, the model aliases) |

Two more facts are host config, not files here: the repos `github.open_pr` may act on
(`action_enums` in `flotillad.yaml`; the registry declares example repos) and the private replay gates
(`make replay` with `SF_REPLAYS`). `scripts/installation_facts.py`, run by `make lint`, keeps
installation facts out of flotilla's code.
