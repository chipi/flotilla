# Example fleets

These are flotilla's **example fleets**: what the images carry, what CI and the e2e stack run, and the
starting point for an installation's own. They are shaped like a real installation's, and every fact
in them is a placeholder: the repos are `example/...`, the sites are `example.com`, the addresses are
documentation addresses.

| Fleet | Routines | What it shows |
| --- | --- | --- |
| `triage` | `cycle` (`v1`, and `v2` a candidate) | alerts and errors triaged by a model on one station, filed or dismissed by code gates; a candidate model compared as a shadow version before any switch |
| `bugfix` | `intake`, `fix`, `open-pr`, `pr-check` | a bug issue becomes a case, a coding agent fixes it under gates, a person approves the PR plan and always merges |
| `jobs` | `backup-monitor`, `rerun-backup`, `verify-backup` | a monitor opens a case, an acting routine plans a rerun on a host, the host agent runs only what its own config allows, a check verifies it; shadow, propose, then auto |
| `review` | `case-review`, `process-change` | closed cases become proposed lessons and process changes with their evidence; a person approves every change |
| `deps` | `scan`, `upgrade` | vulnerable or outdated pins become cases and one-line upgrade PRs; no model: code finds and plans, the repo's CI tests, a person approves and merges |
| `sites` | `site-check` | what a site serves live (robots, sitemaps, head tags, broken links, certificate expiry), read only; findings are cases |
| `platform` | `heartbeat` (`v1`, and `v2` with its model call as a station), `model-contract` | the platform's own canary and the model aliases' contract checks |
| `crm` | `scan`, `fix`, `apply-update`, `apply-merge`, `check-field`, `check-merge` | sales ops: CRM fields normalized and duplicates merged; allowed fields only, never a field a person edited; a field fix can earn auto, a merge is always approved |
| `campaigns` | `scan`, `decide`, `apply-launch`, `apply-hold`, `check` | marketing: launch checks in code decide; a launch is always approved, a hold can earn auto |
| `outbound` | `scan`, `draft`, `send`, `check` | a first email per prospect, every personal sentence citing a fact; do-not-contact, consent and a daily cap in code; every send approved |
| `renewals` | `scan`, `flag`, `apply-flag`, `check` | customer success: a risk score in code, replayed over past renewals, flags the account for its owner; it never writes to the customer |

The business examples (`crm`, `campaigns`, `outbound`, `renewals`) act on stand-in systems: JSON files
seeded from each fleet's `seed/` (`business/store.py`), carried out by `business/apply.py` as routine
actions (ADR-0017). They run on the `ops-read` worker so the e2e runs them end to end; an installation
gives them their own worker with its CRM's, campaign tool's or mail service's credentials, and replaces
`business/store.py` with an adapter to that system.

## A routine: a file, or a folder with versions

A routine is either one flat file, `fleets/<fleet>/<routine>.yaml` (the routine with one version, `v1`),
or a folder ([ADR-0020](../docs/adr/ADR-0020-routine-versions-stations-harnesses.md)):

```text
fleets/<fleet>/<routine>/routine.yaml       identity: worker, trigger, detects, autonomy, budget, acceptance, ...
fleets/<fleet>/<routine>/v<n>/spec.yaml     one version: run, steps, skills, prompts, models, ...
fleets/<fleet>/<routine>/v<n>/<texts>       the files that version's spec names (paths stay inside v<n>/)
```

Never both for one routine. Every example is a folder. Each has one version, `v1`, identical to the flat file it was
(`internal/spec/move_equiv_test.go` compares every field and the run environment against a copy of the flat
specs, `internal/spec/testdata/flat-before-step5/`), with two exceptions that gained something on purpose:
`triage/cycle`'s `v1` declares its model step as a station (the harness block, the prompt and `SF_TRIAGE_MODEL`
became the `decide` station, run by `signal-fleet/mvp/orchestrator_station.py`, the same requests to the model),
and it has a `v2`, the candidate that was the routine `triage/cycle-shadow`; `platform/heartbeat` has a `v2` whose
model call is the `probe` station. A prompt a version declares is a file inside its `v<n>/` (bugfix's `fix/v1/rules.md`
is a copy of `bugfix/fleet_rules.md`, kept equal by a test; triage's `v1/system.md` is `playbooks/triage.md` with its
two placeholders written as slots, which triage.py still reads when it runs without flotillad). The class-hint skills
stay files of the fleet's `skills/` folder, which a version's frozen folder does not cover. The first version of a routine goes live when it is deployed;
a later `v<n>/` arrives as a shadow and goes live only when the operator promotes it (`flotilla versions`,
`promote`, `rollback`, `retire`, or the routine page). The live and the previous version's folders are
frozen: a changed one fails `flotillad -check` and its start, so change a live version by adding a new
number. `acceptance:` (a list from `pairs`, `replay`, `scenario_eval`; `pairs` when absent) names the
evidence a promote needs. The lowest autonomy level is `record`; a spec that still says
`autonomy: shadow` (or `max_autonomy: shadow`) is read as `record`, and `-check` says so. The value
a run reads in `FLEETD_STAGE` is `shadow` for `record`, as before.

## An installation's own fleets

An installation keeps its fleets in a private repo of its own, in this same layout, and runs them
instead of these: `deploy/compose/fleets.yml` mounts that folder over `/opt/flotilla/fleets`, and
`deploy/server/deploy_fleets.sh <commit>` deploys a commit of it (`deploy/compose/README.md`, "The
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
