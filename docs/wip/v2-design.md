# Routine versions, stations and harnesses

**flotilla** runs **fleets**; a fleet runs **routines**, its business processes; a routine has
**versions**; and each version is worked by its **stations**, one per model step. (On the site, a
routine is what the industry calls an agent.)

The operator, 2026-11-18. Changing a business process means shipping a new version of its routine.
A new version runs in shadow beside the live one without any effect on the system, is compared,
and goes live on the operator's word; if it turns out wrong, the operator rolls back. Every routine
moves to this format; a routine that calls no model has versions and no stations.

**Status: the spec is done (2026-11-18).** Reviewed in five rounds by an advisor and by the
installation's fleet session; both gave GO on this text, and on section 6 (evaluation), agreed
with eval-harness. It becomes ADR-0020 once built and the first routine moves to it.

## 1. A routine and its versions

**Files.** A routine is a folder; each version is a folder inside it with everything it declares:

```text
fleets/<fleet>/<routine>/routine.yaml       the routine: identity, shared by every version
fleets/<fleet>/<routine>/v<n>/spec.yaml     a version
fleets/<fleet>/<routine>/v<n>/<texts>       the texts its stations declare; paths stay inside v<n>/
```

**What belongs to the routine** (`routine.yaml`, changed by an ordinary deploy): its name and
description, worker and shadow worker, trigger, the case kinds it detects, enabled, autonomy and
max_autonomy, budget, freshness, resume, brief section, chain, acceptance. Its identity, controls, ledger
namespace, cases, freshness alert and metrics are the routine's; every version shares them.

**What belongs to a version** (`v<n>/spec.yaml`): its `run:` (command, timeout), its stations and
their texts, its flow limits, explain and lanes. A version may change code: its command names the
code it runs. The worker image carries the code of every version that is not retired; new code ships
beside the old (a new module, a new entry point), never edited under a version that uses it.

**States.** Each version is live, shadow, previous or retired:

- **live:** the one that acts. Exactly one per enabled routine.
- **shadow:** runs beside the live one with no effect on the system (section 4). Any number.
- **previous:** the version that was live before the current one; the rollback target.
- **retired:** kept for history; it does not run.

The word shadow belongs to versions only: the lowest autonomy level, today called `shadow` (record the
plan and act on nothing), is renamed `record` in step 1. The value routines read does not change:
`FLEETD_STAGE` stays `shadow` for autonomy `record` (`spec.FleetdStage` maps it), because triage's
dry-run guarantee and the ops routines test that string; a test pins the mapping, and only a new
version of a routine may read the new name.

**Controls.** Which version is live is an operator control in the store, with who set it, when and
why, like autonomy.

- **A routine's first version** goes live when it is deployed; its hash is recorded then. Moving an
  existing routine to this format makes its current spec its v1, live, with the same command,
  environment and values: behaviour identical, proven before the deploy. A v1 made this way is
  exempt from the station rules (section 2) until it is retired, and the files it reads outside
  `v1/` (its playbooks, skills and facts where its code expects them) are hashed in place.
- **A new version** of a routine that has a live one arrives with a fleets deploy (a restart, as
  today) and always as a shadow. Numbers are the author's, unique per routine.
- **Promote** makes a shadow live; the live one becomes previous, the old previous retired. The
  operator chooses the routine's autonomy for it at promote (pre-filled with the current level); it
  is never inherited in silence. Promote is refused when the version fails the checks (section 2),
  or when the evidence its `acceptance:` names is missing or failed (section 6; `pairs` when it
  declares none), unless the operator gives a reason. The live run in flight finishes and
  counts; a shadow run in flight finishes as a shadow; the previous version's plans awaiting
  approval stay approvable (it was live when it made them).
- **Roll back** makes the previous version live at once. The rolled-back version's runs in flight
  are cancelled, as STOP cancels them; its plans awaiting approval are marked rolled back and cannot
  be approved; their cases return to the live version. The rolled-back version is retired, marked
  "rolled back at T by X: why"; there is no previous until the next promote. A fix ships as a new
  number.
- **Retire** stops a shadow; a retired version's folder can be deleted later.

**Rules that keep rollback safe.**

- **Live and previous are frozen.** At load, the hash of the live and the previous version's folder
  must equal the hash recorded when they went live. A changed, missing or renumbered live or
  previous folder fails the load and the deploy, as an invalid spec does today. The only way to
  change a live version is a new number. A shadow may change; every shadow record carries the hash
  it ran with.
- **State stays readable backward.** A version may add to what the routine stores (ledger columns
  and values, columns of its own state files), never remove or rename; readers ignore what they do
  not know. The e2e proves it for each example routine by running the previous version against the
  new version's rows.
- **The image runs every version that is not retired;** the e2e runs the previous version of each
  example routine on the new image.
- **Every record says which version made it:** runs, calls, case events, plans, ledger rows.

**The routine page** lists the versions with their state, hash, when each was deployed and went live,
who promoted it, its results and its pairs; with promote, roll back and retire.

## 2. A version's stations

A station is one model step. A version declares any number of stations, with any names; flotilla
gives a name no meaning.

```yaml
stations:
  <name>:
    harness: pi              # an adopted harness (section 3), or flotilla (our in-process loop)
    model: <alias>           # from the installation's models config
    prompts:                 # every text the model reads: any number, any names, each a file
      <text name>: <file>
    tools: all               # all | read | none
    limits: {max_turns: 20, timeout: 15m, max_output_tokens: 16000}   # per episode
    # harness flotilla only: tool_names and gates (required, may be []), hooks, skills
limits: {}                   # the version's flow limits, any names its code reads
```

- **Texts are templates.** A file may use `{{slot}}`s the version's code fills at run time; the code
  documents its slots and its tests pin them. The hash covers the file; the rendered text is
  recorded with each call. Code that needs a text the version does not declare fails before the
  first model call. The platform checks' probe texts (heartbeat's and model-contract's) are test
  fixtures in code, pinned by their tests.
- **`models:` is derived** from the stations; declaring both is an error.
- **The check before anything runs** (`-check` and at load), against `fleets/harnesses.json`, the
  harnesses' capabilities, shipped with flotillad and generated from the adapters:
  - refused: hooks on a harness without them; a `tools` value a harness does not serve; `resume` on
    a routine whose writing stations run on a harness that cannot resume after a kill;
  - allowed: `read` on a harness that cannot enforce it; the runtime checks the workspace after the
    station, and a change is a fatal stop.
- **A run receives `FLOTILLA_STATIONS`:** per station its harness, model alias, texts (name → path
  and hash), tools, limits, tool names, gates, hooks and skills, and the version's limits. The model
  route comes from `FLOTILLA_MODELS`. No model, text or limit belongs in a version's code or its
  command's flags; each version's tests pin that.
- **Offline:** `flotillad -stations fleets/<fleet>/<routine>/v<n>/ -models <models config>` prints
  what a run of that version gets, with the same checks, so an eval runs exactly what flotillad
  would.

## 3. Harnesses are the platform's

- The harness layer lives in `runtime/`: one adapter per harness (`runtime/harness_<name>.py`), the
  shared runner and the seams. Any routine runs any station on any adopted harness through it. An
  adapter is asked to serve a `tools` value, never a station name; every text it delivers comes from
  the version's `prompts:`.
- **The seams** (`runtime/HARNESS_SEAMS.md`) are the contract each adapter meets, S1 to S11: prompt
  in, system text, the model route through the gateway, the stream read into an Episode, turn cap
  and timeout, tools and read-only, session and resume, call recording with the rendered texts, cost
  (the gateway's figure per station; a harness's own figure only as a labelled estimate), nothing
  reaching any host but the gateway, isolation in the run's folder. A seam may change with an
  adoption; the seams file changes first.
- **Every worker image carries every adopted harness.**
- **Adopted one at a time**: pi (the reference), Claude Code, Codex, opencode, DeepSeek Harness,
  mini-SWE-agent. A harness is adopted when four tests pass:
  1. unit: its command and environment per `tools` value, its parser on sample streams, the Episode
     contract;
  2. seam end-to-end: the real CLI, a fixture repository, a scripted model endpoint recording what
     it receives, a killed and resumed run, one test per seam; no network, no spend;
  3. probe on the installation: one tiny real call per `tools` value through the real gateway and
     proxy;
  4. eval: run by the installation against the live version and the other harnesses.

## 4. A shadow has no effect on the system

A shadow version reads the same input the live version got, and writes only to its own shadow record.
Routines write outward from their own code (issues, comments, files), so "no effect" is enforced in
three layers, and proven by a test.

**How flotillad knows a request comes from a shadow.** flotillad gives every run its own token at
dispatch (a file named by `FLOTILLA_API_TOKEN_FILE`). Every request a run makes is resolved token →
run → the routine, version and state recorded on the run at dispatch; the routine or namespace a
request names is ignored. No run can write under another routine's or version's name. Promote, roll
back and retire never change a run in flight's state: a shadow run in flight at promote finishes as
a shadow and its pairs count; a shadow run in flight when its version is retired is cancelled.

**Layer 1, flotillad.** Everything that passes through flotillad goes to the shadow record:

- a shadow run's rows are keyed `<fleet>/<routine>@v<n>` (runs, calls, spend, metrics); no shadow
  write lands under a live routine's key; freshness, the stale alert and the routine's budget read
  live runs only;
- cases (open, move, close, lease, spend, events), plans, approvals, executions, brief, Slack asks,
  autonomy evidence: none touched; each would-be write is one `shadow_records` row (shadow run,
  version, pairing key, kind, payload);
- a shadow monitor's report is stored whole, with what it would have opened, attached, closed and
  cleared against the live case keys as of its input;
- the ledger: a shadow reads the live rows as of its input plus its own, writes only its own; where
  both filed the same fingerprint, the shadow's own row is current for it.

**Layer 2, the worker.** A shadow run gets:

- **no write credential in any form:** not in its environment (the worker's environment files are
  split into read and write; a shadow run gets the read ones and `FLOTILLA_SHADOW=1`), not in its
  state copy, its HOME, git or `gh` configuration, git remotes or ssh keys. Where reading needs a
  token, the installation gives shadows a read-only one;
- **its own state:** the routine's own state (files it keeps, such as a ledger file) is snapshotted
  by the live worker when the live run starts, before it writes, into a snapshot volume of its own,
  under the live run's id, that only it writes. The routine's shadow worker mounts that volume
  read-only and its run is told the live run's id; it copies that snapshot into its own folder and
  never sees the live volume. A snapshot is deleted when every shadow run on that input has ended;
- **its own model key**, so it can never use up what the live version needs; when a shared limit
  binds, the shadow's call is the one dropped;
- **its own worker** (`routine.yaml`'s `shadow_worker`; by default the installation's
  `<worker>-shadow`): the routine's image with the read environment only. It runs on the live
  worker's host (for a remote live worker, on that remote host), so it can mount the snapshot
  volume, with CPU and memory limits set below the live worker's and the lower priority, so it
  cannot slow or starve a live run; a host without room for both runs the shadow after the live run
  ends. Scheduled inputs that wait while a newer one arrives are dropped and recorded as not run;
  case inputs queue (bounded), so cases, which are few, still get pairs.

**Layer 3, the network and telemetry.** Where the installation's proxy can tell reads from writes, a
shadow's egress allows reads only. A shadow's traces, metrics and errors carry `version` and
`shadow=1`. The installation's routines that read errors and logs (triage) exclude `shadow=1`, and that
change ships to them before any shadow runs (build step 2).

**The input, frozen.** When the live run starts, flotillad records exactly what it handed the run (the
case, the plan, the tick time), the ledger's high-water mark, the live case keys of the kinds the
routine detects, and for a repository the commit to work from: flotillad resolves the branch's head
at dispatch and hands that same commit to the live run and to its shadows. A shadow gets that input,
the ledger as of that mark, the routine's state snapshot, and checks out the same commit. Outside
systems cannot be frozen: a shadow that starts later may see them changed, and its record carries both
start times.

**Pairs, one per decision.** A run may decide several things (a triage tick decides several signals),
so each decision record, live and shadow, carries the input's key plus its subject (the signal, the
case). Pairs are matched per subject; a subject one side decided and the other did not shows as
unpaired. Per side, a pair holds the version and hash, the outcome and detail, the plan with any diff,
per station the turns, tokens, cost (the gateway's figure) and rendered texts, and the input key. The
routine page shows them; they export as JSON, filtered by versions and time, for the installation's
comparisons and evals.

**Evals have the same guarantees.** An eval of a version runs through flotillad as a shadow run on a
given input, or offline with `-stations` and the shadow worker's read-only environment; either way no
write credential is reachable.

**Proven by a test.** The e2e records, before and after a shadow run:

1. every store row that is not the shadow's own (`shadow_records` and the rows keyed `@v<n>`);
2. the routine's state volumes;
3. a recording stand-in for every outside system each example fleet touches (GitHub, the error
   tracker, Grafana, the LLM-trace store, the metrics and logs stores, Slack, the hosts), asserting
   no write arrived;
4. the metrics pushed under the live key, the routine's budget state, and the day's brief and Slack
   asks;
5. the live run's exit unchanged and its wall time within a stated tolerance of the same run without
   a shadow, with a shadow running on the same host under its limits.

Before must equal after, and the shadow must have written a shadow record with its pairing key, so
the test cannot pass because the shadow did nothing.

## 5. The platform's own routines

- heartbeat: one station on `harness: flotilla` for its optional model call.
- model-contract: no stations; it audits. `audits: models` makes flotillad hand it the
  installation's aliases with their routes, through `platform-*` aliases.

## 6. Evaluation is first-class

Agreed with eval-harness and its owner (2026-11-18). Evaluation itself (datasets, scoring, decisions)
stays in eval-harness (RFC-0002 item 9); flotilla owns the recording, the entry point and the formats.

- **The episode schema** (`runtime/episode.schema.json`, versioned): one station's run as the
  harness layer records it, embedded verbatim in eval-harness's transcripts. It holds the station
  name and sequence, the version and spec hash, per turn its time, model, rendered texts and usage
  (input, output, cache and reasoning tokens), per action its name, arguments and result (after
  redaction; above a size cap as a hash and a length) with its duration and success, the gateway's
  cost with its source always set, the session id, how it ended, the notes, and the raw stream's
  path. Every adapter's episode is validated against it.
- **The input schema** (`runtime/input.schema.json`, versioned): the frozen input of section 4. The
  same envelope is a scenario's start: a captured live input exports as the envelope with the ledger
  rows as of its mark and the state snapshot, scrubbed by the installation into its private data. An
  export is written to a folder the operator names, outside any repo, and is production data until
  the installation scrubs it; the episode carries the raw stream's path only, never the stream.
  Optionally it carries recorded tool results: per station, the calls and results of a captured run
  (from its episodes) or a scenario's fixtures, matched by station, tool name and normalised
  arguments, never by sequence. They answer the tools the runtime serves (harness `flotilla`'s tool
  names); a station on an adopted harness gets the envelope's commit in a fixture workspace and no
  network but the gateway. An input built from a dataset item rather than captured live validates
  too: the fields only a live capture has (ledger mark, live case keys, snapshot) are optional.
- **The offline entry point:** `flotillad -stations` and one command that runs a version's own
  `run:` on a given input, in the routine's shadow worker (its image, the read environment, a fresh
  HOME, `FLOTILLA_SHADOW=1`), never in the caller's environment, with a models config of its own (so
  an eval spends on its own key, never a live or shadow key). The run's API is an in-process
  stand-in that serves the envelope (the ledger as of its mark, the case keys, the state snapshot)
  and turns every would-be write into a decision record; the command accepts no live flotillad
  address or token. Given recorded tool results, a station answers its tool calls from them; a call
  with no recorded result fails the run and is never sent live. That is how an input is evaluated
  after its sources changed or expired, and it is RFC-0002 §8's model replay. It writes its
  episodes, calls and decision records to a given folder outside the state folder and the fleets
  tree. A tested contract; the runner that calls it lives with the installation's evaluation data.
- **Acceptance names its evidence.** `routine.yaml` declares what a promote needs: `acceptance:`, a
  list from `pairs`, `replay`, `scenario_eval`; `[pairs]` when absent; every listed kind is
  required. Promote records the acceptance in force and the evidence given (pair counts, gate
  results, eval-harness run ids with their verdicts), and refuses a promote that lacks a declared
  kind, or whose given evidence is a failed gate or verdict, unless the operator gives a reason.
  flotilla stores the verdict; it does not compute it.

## 7. Build order

1. **Routine and versions:** the folder layout, `routine.yaml` and `v<n>/`, the states as an
   operator control, promote, roll back and retire with their rules, the first version live at
   deploy, frozen live and previous, the version on every record, the routine page's versions;
   autonomy `shadow` renamed `record` (`FLEETD_STAGE` unchanged, pinned by a test; the stored values
   move by a one-off store migration, checked read-only on a copy of the real store first; triage's
   own dispositions keep their `stage: shadow`, labelled as v1's word on the decisions tab);
   `acceptance:` and the evidence a promote records.
2. **Shadows with no effect:** the per-run token; the shadow record and every redirect in flotillad;
   the worker's read and write split, state snapshot and shadow worker; telemetry labels, and the
   readers of errors and logs excluding `shadow=1`; the frozen input as `runtime/input.schema.json`
   with its export, and pairs per subject; the no-effect test, which gates step 5.
3. **Stations:** the format, `fleets/harnesses.json` and its check, `FLOTILLA_STATIONS`, `flotillad
   -stations`, texts as templates recorded per call; `runtime/episode.schema.json`; the offline
   entry point with recorded tool results.
4. **The harness layer in `runtime/`:** the adapter interface without station names, pi's adapter
   serving every `tools` value, the seams and pi's seam end-to-end, moved from `bugfix/`; every
   adapter's episode validated against the episode schema.
5. **The example fleets move to this format;** the e2e covers versions, shadows, rollback, backward
   readable state, the no-effect test and an offline run of one example version on an authored
   fixture input (never an exported capture).
6. **Freeze;** the installation moves its routines (each current spec its v1, live, identical; its
   existing candidate routines, such as triage's cycle-shadow, become shadow versions), then writes
   its v2s. Before any promote, the installation's own gates run each routine's previous version
   against the new version's rows and state files (backward-readable state), as the e2e does for the
   example routines.
7. **Harnesses adopted one by one** (section 3).

## 8. Examples (not part of the platform)

- Triage's cycle: one station, `decide`, on `harness: flotilla` with its tool names and gates.
- Bugfix's fix chain: as many stations as its chain has steps, each on whichever adopted harness the
  installation picks.
