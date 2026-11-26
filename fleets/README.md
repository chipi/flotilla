# Example fleets

These are flotilla's **example fleets**: what the images carry, what CI and the e2e stack run, and
the starting point for an installation's own. They are shaped like a real installation's, and every
fact in them is a placeholder: the repos are `example/...`, the sites are `example.com`, the
addresses are documentation addresses.

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
| `plumbing` | `send-create-tasks`, `send-post-decisions`, `send-reply`, `send-post`, `send-retract`, `check` | plumbing.call (RFC-0014): an approved plan posted, signed, to the installation's plumbing tool, which writes it where it belongs; the check asks it whether the delivery is done there. No connector of its own, no URL or secret in the specs |
| `meetings` | `notes`, `decisions` | a meeting into its follow-ups and decisions (RFC-0014 case 3): the plumbing's signed hook opens one case per meeting; one gated model call reads the notes as data; the follow-ups go to the tracker as one approved list, then the same case plans the decisions for the wiki, approved on its own (a plan's `then`) |

The business examples (`crm`, `campaigns`, `outbound`, `renewals`) act on stand-in systems: JSON
files seeded from each fleet's `seed/` (`business/store.py`), carried out by `business/apply.py` as
routine actions (ADR-0017). They run on the `ops-read` worker so the e2e runs them end to end; an
installation gives them their own worker with its CRM's, campaign tool's or mail service's
credentials, and replaces `business/store.py` with an adapter to that system.

## The meetings example: a meeting into its follow-ups and decisions

`meetings` is RFC-0014's case 3, flotilla as the governed step between an organisation's own tools.
Flotilla holds no connector to any of them: the installation's plumbing tool (n8n on the reference
installation) brings the meeting in and writes the results out.

```text
notes land in a folder --plumbing--> POST /hooks/meeting (signed) --> a meeting case
  meetings/notes: one model call (station extract) reads decisions and follow-ups; code gates the shape
    plans plumbing.create_tasks (the follow-ups, one list)       then: {stage: decisions, ...}
    a person approves once --> plumbing/send-create-tasks --signed--> plumbing --> tasks in the tracker
    plumbing/check asks the plumbing: done --> verified, and the case goes on to its stage decisions
  meetings/decisions: plans plumbing.post_decisions (one labelled text)
    a person approves once --> plumbing/send-post-decisions --> plumbing --> the decisions page
    plumbing/check: done --> the case closes, verified
```

- **The notes are data.** The task text puts them between `<meeting-notes>` markers they cannot
  close; the answer must be exactly `{"decisions": [...], "follow_ups": [{who, what, by}]}`, each
  field a bounded text and `by` a real date, or the case escalates. Nothing the model says picks an
  action, a stage or a target.
- **A follow-up is a task, not a case.** flotilla proposes the list once; a wrong item is closed in
  the tracker. A rejected list sends nothing, and the case never reaches its decisions.
- **One case, two plans,** through a plan's `then` ([RFC-0013](../docs/rfc/RFC-0013-bugfix-clarifier.md)
  §2, amended 2026-11-22). No follow-ups: `notes` hands the case straight to `decisions`. Neither:
  the case resolves itself.
- **Workers:** `notes` on `triage` (it needs the model key and the gateway), `decisions` on
  `ops-read` (no model), the sends and the check on `plumbing` (the callbacks' secrets, a proxy that
  reaches only the plumbing).

To run it on an installation, copy `fleets/meetings/` into its own fleets repo and add:

| Where | What |
| --- | --- |
| `flotillad.yaml` `hooks:` | the hook `meeting`: kind `meeting`, target `file` (a pattern for the file names), fields `file`, `date` and `text` (its maximum the longest notes), its secret file |
| `flotillad.yaml` `models:` | the alias `meetings-notes`, a model that answers in JSON |
| the plumbing worker | `ROUTINE_PLUMBING_CREATE_TASKS_URL` and `_SECRET_FILE`, the same for `POST_DECISIONS` (`deploy/compose/README.md`, "The plumbing worker") |
| the plumbing tool | a flow per callback (`send`: make it, write the delivery id into it, answer `{"ref"}`; `status`: find it by the delivery id, answer `{"done", "ref"}`), and the flow that watches the notes' folder and calls the hook |
| autonomy | both actions stay at `propose`, a person approving each list |

## A routine: a file, or a folder with versions

A routine is either one flat file, `fleets/<fleet>/<routine>.yaml` (the routine with one version,
`v1`), or a folder ([ADR-0020](../docs/adr/ADR-0020-routine-versions-stations-harnesses.md)):

```text
fleets/<fleet>/<routine>/routine.yaml       identity: worker, trigger, detects, autonomy, budget, acceptance, ...
fleets/<fleet>/<routine>/v<n>/spec.yaml     one version: run, steps, skills, prompts, models, ...
fleets/<fleet>/<routine>/v<n>/<texts>       the files that version's spec names (paths stay inside v<n>/)
```

Never both for one routine. Every example is a folder. Each has one version, `v1`, identical to the
flat file it was (`internal/spec/move_equiv_test.go` compares every field and the run environment
against a copy of the flat specs, `internal/spec/testdata/flat-before-step5/`), with two exceptions
that gained something on purpose: `triage/cycle`'s `v1` declares its model step as a station (the
harness block, the prompt and `SF_TRIAGE_MODEL` became the `decide` station, run by
`signal-fleet/mvp/orchestrator_station.py`, the same requests to the model), and it has a `v2`, the
candidate that was the routine `triage/cycle-shadow`; `platform/heartbeat` has a `v2` whose model
call is the `probe` station. A prompt a version declares is a file inside its `v<n>/` (bugfix's
`fix/v1/rules.md` is a copy of `bugfix/fleet_rules.md`, kept equal by a test; triage's
`v1/system.md` is `playbooks/triage.md` with its two placeholders written as slots, which triage.py
still reads when it runs without flotillad). The class-hint skills stay files of the fleet's
`skills/` folder, which a version's frozen folder does not cover. The first version of a routine
goes live when it is deployed; a later `v<n>/` arrives as a shadow and goes live only when the
operator promotes it (`flotilla versions`, `promote`, `rollback`, `retire`, or the routine page).
The live and the previous version's folders are frozen: a changed one fails `flotillad -check` and
its start, so change a live version by adding a new number. `acceptance:` (a list from `pairs`,
`replay`, `scenario_eval`; `pairs` when absent) names the evidence a promote needs. The lowest
autonomy level is `record`; a spec that still says `autonomy: shadow` (or `max_autonomy: shadow`) is
read as `record`, and `-check` says so. The value a run reads in `FLEETD_STAGE` is `shadow` for
`record`, as before.

A `harness: flotilla` station with `tools: all` may read the company's tools live through MCP servers
it declares (RFC-0014 §4.8):

```yaml
stations:
  answer:
    harness: flotilla
    tools: all
    reads:
      - server: wiki                       # MCP_WIKI_URL and MCP_WIKI_TOKEN_FILE on the worker
        tools: [search_pages, get_page]    # the allowlist, each by its exact name
        max_calls: 5                       # per run, this server, all stations (default 10)
        max_result_chars: 65536            # per call; over it the result is cut visibly
        timeout: 15s
```

The model calls `wiki.get_page`. Reads only: every write is `plumbing.call`. Each read is recorded
with the run and a replay or a shadow is served the record; a read the record lacks fails it.

## Never silent: `trigger.case.on_record`

An acting routine's `trigger.case` block may say what a plan recorded at the record level does to
its case ([ADR-0023](../docs/adr/ADR-0023-never-silent-and-answers-in-the-core.md)):

```yaml
trigger: {case: {kind: failed_backup, on_record: watch}}
```

- **`close`** (the default, and what a spec without the field gets): the case closes as
  `plan_recorded`, as before.
- **`watch`**: the case moves to `watching` and stays open.
  - The monitor's later detections of the same key attach to it as repeats.
  - The operator gets a notice at once, at 4 hours, at 24 hours, then every 24 hours, at most 7
    (`case_timers`: `watch_notify`, `watch_ack_snooze`).
  - The case closes as `resolved_itself` when the monitor's next successful run no longer reports
    the key. A failed run clears nothing.
- Any other value is refused when the spec loads.

## Seeing the examples on an installation

The flotillad image carries these fleets a second time at `/opt/flotilla/examples`, where an
installation's own fleets (mounted over `/opt/flotilla/fleets`) do not reach. With `examples_dir:
/opt/flotilla/examples` in its flotillad.yaml, `flotilla status` lists every example (state
`example`) and the UI has an Examples page (linked from Fleets): each example's description,
trigger, worker, versions and flow. They are a catalogue, not routines: ids are
`example-<fleet>/<routine>`, nothing in them is scheduled, reaches the store, a worker, a metric or
the brief, and a request that names one (a run, a dry run, a version control) is refused with "an
example". `-check` needs none of their workers or model aliases. To run one, copy its folder into
the installation's own fleets repo under a name of your own and deploy it with `deploy_fleets.sh`:
its first version goes live as any routine's does. Unset, the installation sees no examples.

## The e2e over the examples

The examples ship `v1` only (a version that does something different is the installation's to
write), so the e2e makes a shadow for each one it can: `deploy/e2e/make_shadows.py` copies a
routine's `v1/` to `v2/` and adds a marker file (the version's hash is over its files, and the copy
would otherwise be `v1` itself). The `v2` runs the example's real command. `deploy/e2e/matrix.py`
then prints one row per routine on the `ops-read` worker and ends `NOEFFECT PASS n / SKIP m
(reasons) / FAIL k`:

| Cell | What it proves |
| --- | --- |
| `versions` | `v1` live, `v2` a shadow with a hash of its own |
| `shadow` | a shadow run of `v2` on the live run's input ended done and wrote a shadow record, so the next cell is not the shadow doing nothing |
| `unchanged` | across every shadow run, nothing the routine's live side holds changed: its rows in every store table, the worker's state files, the cases, and what the outward stand-ins saw from the shadow's proxy |
| `promote`, `rollback` | `v2` goes live and back: `v1` previous, then live again |
| `previous` | the image runs the previous version: a run after the rollback is `v1`'s and ends ok (a routine that waits for a case has none to run on) |
| `state` | backward-readable state: the routine's stores gain a key and a column `v1` does not know (`extend_state.py`), `v1` runs again, and its exit and the cases it decides are those of the run before |

The row `(all)` is what the shadows must leave alone and no single routine owns: the day's brief,
the Slack asks (the stand-in host's agent is no connector and logs each plan flotillad asks it to
post), and the host's audit log; `e2e/hostile-brief`'s shadow tries to file a plan that would be
posted and to add a brief line, and those cells must hold. Before the shadows exist every routine
gets a live run (`matrix.py drive`: new records, plans approved, acting routines and checks run). A
routine that still has no live run on the stack is a `SKIP` with that reason; the routines on the
`ops-act-host` and `bugfix` workers have no shadow worker in the stack (a remote worker's routine
cannot be shadowed at all), so they are not in the matrix. `E2E_SECTIONS=noeffect bash
deploy/e2e/run.sh` runs the matrix and the sections it reads (CI's `no-effect` check), with four
mutation checks that must go red (`deploy/e2e/mutant.sh`).

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
