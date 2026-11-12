# RFC-0006 — Fleet packs: flows as files, shared between people and promoted between installations

**Status:** Draft, revision 5 (2026-11-12): the operator's decisions on revision 1 applied (§12);
revision 3 draws the boundary between development tests and evaluation, and adds the replay entry
point and the input contract (§8); revisions 4 and 5 take in two independent reviews (§15).
Nothing is built from it until it is accepted.
**Builds on:** [ADR-0004](../adr/ADR-0004-job-runtime.md) (the runtime, steps and the harness),
[ADR-0007](../adr/ADR-0007-routine.md) (routines), [ADR-0008](../adr/ADR-0008-autonomy.md) (autonomy),
[ADR-0011](../adr/ADR-0011-tools-and-actions.md) (tools and actions),
[ADR-0012](../adr/ADR-0012-skills.md) (skills), [RFC-0002](RFC-0002-job-runtime.md) §4 and §9.
**Amends, on acceptance:** ADR-0012 decision 2 and RFC-0005 §3 (skill sources and layers, §5);
ADR-0008 and ADR-0005 decision 4 (a fourth cap, the installation's, §7). §13 lists the docs.

## 1. Why

A fleet's behaviour lives more and more in text: routine specs, playbooks, skills, harness settings.
Skills showed how far plain files go: a folder with a `SKILL.md`, copied to share it, loaded by any
harness that reads the format. This RFC applies the same idea to whole fleets and single routines,
for three uses.

**Sharing between people.** Five people run flotilla. One finds a better triage flow, another adapts
it to a different error tracker, a third writes a better daily brief. Sharing is copying a folder;
using it is loading that folder.

**Promotion between installations.** A team wants to try a new routine, a changed playbook, a new skill
or another model without any material effect, and move it to the installation that acts once it is
satisfied. That is development, test, acceptance, production, or just "a sandbox and the real one":
flotilla does not fix the number of tiers or their names. An installation can run in **non-impact
mode**: nothing it runs changes anything outside it. The same files then move to the next one.

**Predictability.** People who run operations trade capability for predictability: when AI acts, they
want to gate what happens and know the net result of a change before it takes effect. The judgement
of "better or worse" is made by an evaluation harness against a baseline, outside flotilla (§8);
flotilla's part is to run the change without effect, record everything the evaluation needs, and move
exactly what was evaluated.

All three need the same thing: a flow that is a self-contained set of files, with no installation's
facts inside, which flotilla can check, run safely somewhere new, record exactly, and move unchanged.

## 2. What a fleet pack is

A **fleet pack** is a folder. It may hold a whole fleet or a single routine:

```text
<pack>/
  FLEET.md            the manifest: frontmatter, then a human description
  routines/*.yaml     routine specs (trigger, steps, harness settings, skill rules, autonomy)
  playbooks/*.md      the routines' own instructions
  skills/<name>/SKILL.md
  defaults/*.json     tunable values with their defaults (thresholds, quiet days, limits)
```

**`FLEET.md` frontmatter:**

```yaml
name: triage                 # the fleet's name in an installation (the folder's name is not read)
version: 1.4.0               # the author's label; flotilla records the content hash (§5)
description: Triage alerts and errors into issues, dismissals and cleanups.
requires:
  tools: [event_detail, occurrence_history, service_logs, metric, trace]
  gates: [intent, cleanup, dismiss_corroborated, edge]
  actions: [github.file_issue, github.comment, glitchtip.resolve]
  models: {tools: json}      # the capability its harness needs (RFC-0002 §3)
slots:                       # the installation's facts it needs, by name (§4)
  repo_for_project: map      # error project -> repository
  error_tracker: source
  alert_source: source
```

Everything else in a pack is the format flotilla already reads. A routine spec in a pack is the same
spec as one in `fleets/<fleet>/` today. The built-in fleets do not follow this layout yet (their
specs sit at the fleet's root, with `*-default.json` files beside them); they move to it in §10
step 2, so one layout and one hashing rule serve both layers.

**Single routines.** An installation may take one routine from a pack, not the whole fleet (one
better daily brief without the rest of its fleet). The routine brings the playbooks, skills and
defaults it names; `-check` refuses a routine that names a playbook, skill or default that neither
the pack nor flotilla's built-in default skills (`skills/`, RFC-0005 §3) hold.

## 3. Text only, never code

A pack **never brings code**. It names tools, gates, actions and step types from the installation's
registry; it cannot add one. A new building block is a reviewed code change to flotilla (or to an
installation's registered extension), exactly as ADR-0011 has it for actions. This is ADR-0012's
"skills never grant anything", applied to whole flows:

- `-check` refuses a pack that requires a tool, gate, action or capability the installation does not
  have, and names each one missing;
- a pack's files are read, never executed. `-check` refuses a file outside the layout of §2 (a
  `scripts/` folder included), a symbolic link and a file over a size cap, as the skill loader
  refuses links and caps size today; so every file a pack holds is hashed and shown (§5, §6);
- a pack cannot raise its own autonomy (§6), widen a host's allowlist or raise a budget above the
  installation's cap (§7).

So installing someone else's pack is reviewing text: prompts, thresholds and the order of building
blocks, not a program.

## 4. Installation facts stay out: slots

Today the triage fleet's code holds facts about one installation: which GitHub repository gets which
error project (`filing.REPO_MAP`), the host names in its gates, and the issue tracker (GitHub only
today; another tracker would be a slot). A pack cannot carry those and still run elsewhere.

A pack declares **slots**, the facts it needs, by name and kind. The installation fills them in its
config, next to its other facts (hosts, endpoints, credentials):

```yaml
# flotillad.yaml (the installation's)
environment: operations                 # exists today: the telemetry label
installation: acme-test                  # this flotilla's name; required with packs (below)
packs:
  ops:                                   # must equal the manifest's `name`, or -check refuses
    path: /opt/flotilla/packs/ops          # a folder; how it got there is the installation's business
    routines: [brief]                      # optional: only these routines
    slots:
      repo_for_project: {web: acme/web, api: acme/api}
      error_tracker: glitchtip
    overrides: {quiet_days: 10}            # optional: the installation's values for the pack's defaults (§6)
```

(The example is the ops fleet, the first pack in §9. Triage stays built in until §9 is done, and an
installation pack named like a built-in fleet is refused.)

`-check` refuses a pack whose slots are not all filled, or filled with the wrong kind. A run gets
its pack and values as environment JSON, as it gets `FLOTILLA_SKILLS` and `FLOTILLA_HARNESS` today:
`FLOTILLA_PACK` (name, layer, pack hash, set hash: §5, §6), `FLOTILLA_SLOTS` (the slot values) and
`FLOTILLA_DEFAULTS` (the pack's defaults with the installation's overrides applied). A slot value is
a fact, but decisions depend on it (where an issue is filed), so a changed slot value is shown and
approved like an override (§6) and recorded with every run (§8).

**`installation` and `environment`.** `environment` is today's required telemetry label, pushed to
every run as `FLOTILLA_ENVIRONMENT`. `installation` names this flotilla: two installations may share
an environment (a capped and an acting one on one homelab), never an installation name. Every run
row, record, metric and span carries both.

### What travels and what stays

| Travels with the pack (behaviour) | Stays in the installation (facts) |
| --- | --- |
| routine specs: steps, triggers, limits, skill rules | slots: repositories, projects, hosts |
| playbooks and skills | endpoints, credentials, the egress allowlist |
| the model **alias** each routine uses, and its harness settings | what each alias maps to: endpoint, model, key (flotillad's `models:`) |
| defaults: thresholds, quiet days, budgets | overrides of those defaults; the autonomy and budget caps; controls |
| | the installation's history, ledgers and run records |

So a model change has two forms. Moving a routine to another **alias** is a pack change: it travels
like any other (once the alias is in the routine spec; today triage reads it from its fleet's
environment, §9). Pointing an **alias** at another model is an installation change, repeated on the
next installation's config, outside the pack. Software outside flotilla that a change needs (an
endpoint, a model server) is out of scope; the promotion record names it as a precondition (§7).

## 5. Where packs come from, and knowing exactly what ran

A pack is **a folder on the installation's file system**. flotilla assumes nothing about how it got
there: copied by hand, unpacked from an archive, written by its operator, checked out from any
version control. No mechanism here depends on one.

| Layer | Where | Approved by |
| --- | --- | --- |
| **built in** | flotilla's repo, `fleets/<name>/`, inside the images | the deploy: flotillad's `start` event records the image tag and each built-in pack's hash; §6's steps do not apply |
| **the installation's** | the folders named in the installation's config | the operator, by hash (§6) |

- A fleet's name is unique in an installation; no layer shadows another. Skill names stay unique
  across every layer, packs included, and `-check` refuses a duplicate.
- **Skills from packs.** ADR-0012 decision 2 allows skills only from versioned git, in three layers.
  This RFC amends it: each installation pack is a skill layer of its own, and its content hash plus
  the operator's recorded approval take the place of "pinned by commit". The built-in layers are
  unchanged.
- **The content hash is the version.** flotillad hashes every file of a pack. The manifest is one
  line per file, `<sha256 hex>  <relative path>\n`, paths sorted bytewise, UTF-8, `FLEET.md`
  included; the **pack hash** is the sha256 of that text, written `sha256:<hex>`. A change to this
  rule is a new prefix. `-check`'s refusals (§3) mean no file is left out of it. A pack's identity
  anywhere, in any installation, is that hash; the author's `version` is a label.
- **Runs read the approved snapshot, never the folder.** For the built-in layer the image is the
  snapshot: flotillad hashes `fleets/<name>/` in its image, the worker reads the same bytes from its
  own image, and the deploy pins both. Installation pack folders are mounted read-only into
  flotillad only, never into a worker (ADR-0002: a run cannot read the host). flotillad copies a
  pack's files into its own state, keyed by the pack hash, when it shows a change (§6), and a run
  uses only an approved snapshot: the worker fetches the files it needs from flotillad's worker API,
  `/v1/packs/<hash>/<path>`, using `FLOTILLA_PACK` (§4), and caches them in its run's tmpfs. A folder
  edited afterwards changes nothing that runs until its new hash is shown and approved; a deleted or
  missing folder stops nothing either (`-check` warns, the snapshot runs).
- **The snapshot API** serves approved snapshots only (the current one and earlier approved ones),
  never a pending one, and only paths listed in that snapshot's manifest, so no path leaves it. Any
  worker-role caller may read it: a pack holds no secret (slots and credentials are config, §4).
- **Every run records** the pack's name, layer and hash, and the hash of each file it read, so a
  record says exactly which flow decided what. Any other identifier the operator gives (a version
  control revision, a ticket) is stored beside the hash as a label, never instead of it.
- **Rollback:** `flotilla pack rollback <pack> [<set hash>]` returns to an approved set this
  installation approved before (the previous one by default); any other hash is refused. It is an
  audit event like an approval (§6.3), and the routines keep their autonomy, since the set ran before.
- **Retention:** a snapshot that run records name is kept and is exported with them (§8), so a
  recorded hash always resolves to its text; other approved snapshots are kept until removed by
  hand. A pack has at most one **pending** snapshot; a newer change replaces it. A pack's total size
  is capped, as each file's is (§3).

## 6. Trust: installing and changing a pack is shown, approved, recorded, and starts at shadow

Whoever can write a pack's folder can propose a change, so no change takes effect unseen:

What is approved is a **set**: the pack's files and the installation's values for it (slots and
overrides). Its **set hash** is the sha256 of the pack hash followed by the slot and override values
as canonical JSON (keys sorted, no whitespace). A change to the files, a slot value or an override
is a new set hash.

1. **Detection** runs at `-check`, at flotillad's start and on demand (`flotilla pack show <pack>`),
   never by polling. A new or changed set is copied into the pack's **pending snapshot** (the files
   and the values) and shown against the approved set: the files with their differences, the
   overrides and slot values before and after. An override is called out as a **warning**, so a
   default never changes silently.
2. **The operator approves a set hash,** with the operator's token (`flotilla pack approve <pack>
   <set hash>`). An approval naming anything but the pending set's hash is refused, so what runs is
   exactly what was shown, whatever the folder or the config holds by then: a run gets the values of
   the approved set, not of the current config. Until then the installation keeps running the
   previous approved set, or nothing for a new pack.
3. **The approval is an audit event:** who approved, when, the pack, the pack hash and set hash
   before and after, the changed files, overrides and slot values. Refusals are recorded the same way.
4. **The changed routines run at `shadow`**, whatever their specs say, until the operator raises them.
   A routine is changed when anything it depends on differs from the approved set: a file it reads
   (its spec, playbooks, skills, defaults; a file two routines share changes both), or a slot or
   override it uses. The approval sets each changed routine's autonomy control (ADR-0008) to
   `shadow`. Autonomy is earned in each installation, routine by routine; the author's settings are
   an upper bound, never a starting point.

Trust therefore rests on what the operator saw and approved, not on where the files came from: no
list of trusted sources is needed, and none is implied.

## 7. Installations and promotion

### Non-impact mode

An installation can be **capped**: `max_autonomy: shadow` in its config caps every routine of every
pack, whatever a control says, as a routine's own `max_autonomy` caps it today. It is a fourth cap
beside ADR-0008's three (routine, capability, host) and joins ADR-0005's "lowest wins". Beside it,
`max_day_usd` caps any routine's daily budget, so a pack's spec cannot raise spend. A capped
installation:

- runs every routine in shadow: outputs are drafted and recorded, never sent. **Non-impact** means
  no action on the systems the fleets act on (trackers, issues, error trackers). Model calls,
  telemetry and metrics (a metrics push included; RFC-0002 §4's "a push" means a git push) are not
  outputs; they carry the installation's name (§4);
- keeps its own store, ledgers and run records;
- may read the same signals as the acting installation, or its own (dev data differs from test data;
  flotilla does not assume they are comparable).

It can be a separate machine, or a second flotilla on the same machine: a second compose project
with its own state, once the compose file's fixed container names and shared network names are
parametrised per installation (below). Inside one installation, the candidate worker (RFC-0002 §9)
stays the tool for one image beside the live routine.

### Promotion is moving the same files

1. The pack runs in a capped installation, which records everything the evaluation needs (§8).
2. `flotilla pack promote-record <pack>` writes a **promotion record**: the pack, its pack hash and
   set hash, its labels, the routines, the installation it ran in and over what period, where its
   run records were exported, the overrides it ran with, the preconditions outside the pack (an
   alias the next installation must map, a service it needs), and a free reference to the
   evaluation's result (an evaluation harness report), filled by whoever judged it.
3. The next installation receives the same folder, by any means, and the record. flotillad checks
   that the folder's hash is the one in the record (a mismatch is shown, never silently accepted),
   shows the change (§6) with the record beside it, and the routines start at shadow there.

No tier has a fixed name or count: dev → test → prod, or sandbox → prod, or ten.

### What is needed for this, beyond packs

- **The installation-wide autonomy and budget caps** (above), and the installation's name.
- **The pack mount and the snapshot API** (§5): a read-only mount into flotillad, and the worker
  API that serves approved snapshots by hash.
- **A second installation on one host:** the compose file's container names and its networks
  parametrised per installation, as `FLOTILLA_VOLUME_PREFIX` already does for the secrets volume.
- **Outputs only through actions.** A capped installation is non-impact only if nothing writes outward
  except through actions the cap holds. Today the triage fleet enforces shadow in its own code
  (`orchestrator.py` forces a dry run); with packs, every outward write must be an action decided by
  policy (ADR-0005), so the cap holds whatever a pack's text says. The triage fleet's inbox push
  (gauges and log lines to the metrics and log stores) runs in shadow too; it is telemetry, and
  carries the installation's name like the rest.
- **Read access without write access.** A capped installation's credentials must be unable to write
  (viewer tokens, read-only database roles): non-impact by credential, not by care (RFC-0002 §4).

## 8. Evaluation is the evaluation harness's; flotilla provides the input

Judging whether a change is better (a model, a playbook, a skill, a whole flow), against a baseline,
is an **evaluation harness's** job, outside flotilla. flotilla does not compare installations or score
behaviour. Its part is to make every run a good input.

### The test that sorts a check

- **A development test** has its right answer fixed by the code's intent before it runs, judges a
  **code change**, and passes or fails: "old and new give the same bytes" (a refactor), or "these
  recorded cases now act as the fix says" (a replay-proven fix). No model, no labels, no
  comparison of alternatives. It **stays in flotilla**, as a gate.
- **An evaluation** judges **quality**: against outcome labels or gold answers, between variants,
  models, prompts or periods, with a score, or with a model in the loop. It **lives outside flotilla**.

| Place | Holds | Does not hold |
| --- | --- | --- |
| **flotilla** | the system; its recording (captures, filing and sweep records, run rows); the export; the replay entry point; development tests and deploy gates | scores, labels, gold answers, variants, held-out splits, production data in the tree (corpora come from a private path, `SF_EVAL_DATA` for triage) |
| **a fleet's evaluation project** (for triage: the private `fleet-eval-data`) | that fleet's freezes, corpora, readers from flotilla's records to items, outcome rules and labels, its held-out split | generic evaluation machinery |
| **the evaluation harness** | the generic machinery: datasets, runs, scoring, rounds, label import, splits | any one fleet's data or readers |

Applied to what exists:

- **The replays stay, renamed.** flotilla's 19 `eval_*` scripts in `signal-fleet/mvp/` (the 15
  `replays.sh` ran and the four same-result replays) are development tests (same result, or expected
  actions on recorded cases). They were renamed `gate_*` (15442c3), so "eval" means one thing.
  `evaldata.py` and `SF_EVAL_DATA` keep the word: they name the evaluation project's data.
- **The scorer moves out, after the entry point.** `score.py` and the model replay as a scored
  evaluation (RFC-0002 §8) are evaluation; they move once the replay entry point can run a model
  replay (below). The exact replay of a model step, byte for byte, is a refactor test and stays.
- **Freezing is recording.** `freeze.py` drives the live model k times to collect tool calls and
  records the live tools' results for a model replay to use; it stays in flotilla as a mode of the
  replay entry point (tools and model live, both recorded).
- **The shadow comparison splits.** Live against candidate as a deploy check (does the candidate run,
  where does it differ, in counts) stays as a development test of a candidate image;
  judging which side is better, by reading the differences against outcomes, moves out.

### The replay entry point

Running flotilla's own code over a record needs its internal seams (the clock, the GitHub
client, the filing ledger, the model call, the gate lookups). Today an evaluation project patches
them, and every change to those seams has needed an agreement. Instead, flotilla offers a small,
**versioned replay entry point**: run this decision, sweep or step over a record, writing nothing,
and return the actions it would take together with a full record of the run, so a replay is itself
a record under the input contract. Each seam is chosen per run:

| Seam | Recorded | Live |
| --- | --- | --- |
| tools | the record's tool results (exact and model replay) | the installation's tools, read-only (freezing) |
| model | the record's replies (exact replay) | an alias, k runs (model replay, freezing) |
| gate lookups | the record's lookups | the installation's, read-only |
| clock | the record's `now` | the real clock (freezing) |

Whatever is live in a run is recorded in the record it returns.

Evaluation projects and the evaluation harness call it and never patch internals. Its interface is
agreed with the triage fleet's evaluation project before it is built; until then the seams agreed
in RFC-0002 §9 steps 2 to 5 stay frozen.

### The input contract

What flotilla promises about its records, so an evaluation can rely on them:

1. **Complete run records:** every model call as sent and answered, every lookup and tool result, the
   gates' decisions, every drafted or sent output (the exact captures of RFC-0002 §9 step 1).
2. **A schema version per record kind** (capture, filing record, sweep record, ledger rows), additive
   within a version; renaming or removing a field is a new version, with a note.
3. **Exactly what ran, on every record:** flotilla's commit (the image tag), the pack, its pack hash
   and set hash (§6), the hash of every file the decision depended on (a manifest flotilla emits, so
   no reader keeps that list by hand), the routine, overrides, slot values, model alias and what it
   mapped to.
4. **Stage and namespace on every record and run row:** live, shadow or candidate; the ledger
   namespace; the installation's name and whether it was capped. A shadow row must never read as a
   live one.
5. **Which code ran when:** flotillad's `start` event (it records `start` and `stop` with the
   version today) also carries the image tag and each built-in pack's hash; with the approval and
   rollback events (§6) it says which code and which sets ran when, and the export includes them. A
   deploy script's own log stays the installation's and is not promised.
6. **Stable record ids,** so an evaluation can name a record it read without printing its content.
   flotilla's export and comparison tools print ids and counts by default; printing a record's
   content is an explicit show command (`case show`, RFC-0004) or flag (the comparison's `--names`),
   so an evaluation project can record each such read under its own rules.
7. **Records are never rewritten or deleted.** The export carries a sha256 manifest, so a frozen copy
   can be verified.
8. **Export:** `flotilla records export` writes a period's records as files in one documented format,
   the same for a capped and an acting installation, scrubbed before anything leaves the host;
   unscrubbed records stay on the host, mode 600. It includes the pack snapshots the records name.
9. **flotilla does not know the held-out split.** Which records are an evaluation's test side is the
   evaluation's decision, kept outside; nothing in flotilla needs it.

## 9. What must become declarative first

The format can be designed now; the triage fleet becomes a real pack only after:

1. **The routine as steps** (RFC-0002 §4): the orchestrator's sequence (sources, the operational gate,
   triage, dedup, filing, the stale sweep) as declared steps over registered building blocks, not a
   Python program. Steps 2 to 5 of RFC-0002 §9 are built for the model step and dedup; their
   candidate runs are pending, and the rest follows.
2. **Installation facts as slots:** `REPO_MAP` and `INSTANCE_REPO_MAP`, the edge gate's instance, the
   test-signal markers, the substrate projects.
3. **Outputs as actions** (ADR-0005, ADR-0011), so the cap of §7 holds by construction.
4. **The model alias in the routine spec** (`harness.model`, RFC-0002 §4), out of the fleet's
   environment (`SF_TRIAGE_MODEL` today).

Until then the triage fleet stays a built-in fleet in `fleets/triage/`, and packs start with fleets
that are declarative from the start (the ops fleet's daily brief, RFC-0001 phase 4).

## 10. Order of work

1. This RFC, reviewed and decided.
2. The built-in fleets moved to §2's layout (`routines/`, `defaults/`; the spec loader's glob and
   the images' `COPY fleets` follow), then the manifest, the `requires` check, content hashing and
   recording for them, and flotillad's `start` event carrying each built-in pack's hash.
3. Installation folders, the installation's name, the read-only mount into flotillad, the snapshot
   API on the workers' listener, slots, overrides with their warning, approval of a set, rollback,
   and "changed routines start at shadow".
4. The installation-wide autonomy and budget caps.
5. Record export in one format with the input contract (§8), and the promotion record.
   The replay entry point does not wait for packs: it goes first, as an RFC-0002 amendment (the
   `eval_*` → `gate_*` rename is done, 15442c3). The scorer moves out after the entry point.
6. The ops fleet as the first pack; the triage fleet once §9 is done.

## 11. Vocabulary

| Term | Meaning |
| --- | --- |
| **fleet pack** | a folder of text (manifest, routine specs, playbooks, skills, defaults) that defines a fleet or a single routine; shared by copying files |
| **slot** | an installation fact a pack needs, declared by name and kind, filled in the installation's config |
| **override** | an installation's value for one of a pack's defaults (config key `overrides:`); shown with a warning and approved |
| **capped installation** | an installation whose config caps every routine at `shadow`: non-impact mode |
| **promotion record** | what moves with a pack to the next installation: its hashes, where and how it ran, where its records are, its preconditions, a reference to the evaluation's result |
| **pack hash** | the sha256 of a pack's file manifest (§5) |
| **set hash** | the sha256 of the pack hash and the installation's slot and override values: what an approval names (§6) |

## 12. Decided (operator, 2026-11-12)

1. **The name is "fleet pack".**
2. **Single routines too,** not only whole fleets.
3. **Overrides are allowed.** Installing or changing a pack that overrides defaults shows a warning
   with the values and needs approval; an override counts as a change (§6).
4. **No comparison between installations, and no evaluation in flotilla.** Installations' data may
   differ, so they are not compared; judging a change is the evaluation harness's job, and flotilla
   provides complete, exactly labelled run records as its input (§8).
5. **A pack is any folder.** How it got there (copied, unpacked, written by hand, version control)
   does not matter, and no mechanism assumes one: the content hash records what ran, the approved
   snapshot is what runs, and the operator's recorded approval (§6) stands in for a list of trusted
   sources.
6. **The boundary of §8:** development tests stay in flotilla; evaluation in any form lives in the
   evaluation harness and the fleet's evaluation project (the triage fleet's evaluation project
   proposed it; the operator approved it).
7. **A versioned replay entry point** in flotilla, in place of patched internals.
8. **flotilla's `eval_*` replays are renamed `gate_*`.**

## 13. Docs amended on acceptance

- ADR-0012 decision 2, RFC-0005 §3 and its decision 2 (§10): a pack is a skill layer; its hash and
  the operator's approval stand for "pinned by commit" (§5). RFC-0005 §6: a skill load from a pack
  records the pack's name, its hashes and the approval event, in place of a commit.
- ADR-0008 ("three places cap it") and ADR-0005 decision 4 ("lowest wins"): the installation's cap
  (§7).
- `docs/architecture/deployment.md`: the read-only pack mount into flotillad; the installation's name.
- `docs/architecture/components.md`: the snapshot API on the workers' listener; in code, the
  comments that say the workers' listener serves the ledger and nothing else (`internal/api`).
- RFC-0002 §4: "a push" among outputs means a git push; a metrics push is telemetry (§7).
- RFC-0002 §8: the replay entry point's seams and `freeze.py` (§8 here).

## 14. Alternatives considered

- **Export and import commands with an archive format:** a second format beside folders; copying
  files already shares them.
- **Packs that may carry code:** the most flexible, and the end of "installing a pack is reviewing
  text"; a wrong or hostile pack would run arbitrary code with the fleet's credentials.
- **Git sources only, pinned by commit:** exact, but it excludes a hand-written or copied folder; the
  content hash gives the same exactness without requiring git.
- **A fixed dev/test/acceptance/prod model:** imposes one team's process; "the same files move to the
  next installation" covers it and any other.
- **Evaluation inside flotilla** (comparing installations, scoring changes): ties flotilla to one way
  of judging and to installations whose data are comparable; an evaluation harness against a baseline
  does it better, from flotilla's records.

## 15. Discussion

- **2026-11-12, independent review of revision 3** (a separate read-only agent, which checked the
  RFC's claims against the tree). It confirmed the code claims (the dry run forced in shadow, the
  repository maps, the routine cap, the scripts and paths named) and found, and revision 4 changes:
  - **Skills from a folder contradicted ADR-0012 decision 2.** A pack is now a skill layer of its
    own, and the amended docs are listed (§13).
  - **The replay entry point could not run a model replay,** the mode that moves out. Its seams are
    now chosen per run (recorded or live), it returns a record of the run, `freeze.py` is one of its
    modes, and the scorer moves only after it exists.
  - **Workers had no way to read an approved snapshot.** The pack folders are mounted into flotillad
    only, and workers fetch snapshots by hash from its worker API; slots and overrides arrive as
    environment JSON.
  - **Approval was not bound to a hash, and the built-in layer's path was unsaid.** A change is
    copied into a pending snapshot when shown, the operator approves that hash, and for built-in
    packs the deploy is the approval.
  - **Smaller fixes:** the GitLab claim removed; slot values approved and recorded like overrides;
    files outside the layout, links and oversized files refused, so every file is hashed;
    snapshots that records name are kept and exported; the installation cap amends ADR-0008 and
    ADR-0005; non-impact defined (model calls and telemetry are not outputs) and the inbox push
    named; the deploy record comes from flotillad's ops events, not the deploy script's log;
    printing a record's content is an explicit flag; the fleet's name comes from the manifest; an
    installation name; a budget cap; the alias into the spec as a prerequisite; `overrides:` as the
    config key; `promote-record` named apart from `records export`.
- **2026-11-12, second independent review, of revision 4** (a fresh read-only agent). It found the
  earlier blockers resolved in the text except one half of B4, and revision 5 changes:
  - **The approval did not bind slot and override values.** A config-only change left the pack
    hash as it was, so the values could change between "shown" and "approved". An approval now names
    a set hash (the pack hash and the values), and a run gets the approved set's values (§6).
  - **The built-in fleets did not fit §2's layout,** so §10 step 2 would have refused them. They
    move to the layout in that step.
  - **Smaller fixes:** rollback as its own command (an approval could not name an old hash); one
    pending snapshot per pack, detection at `-check`, start and on demand, a total size cap;
    `installation` defined beside the existing `environment` label; `FLOTILLA_PACK` and effective
    defaults in the run's environment; built-in packs read from the image; the snapshot API's rules
    (approved only, manifest paths only, any worker, nothing secret); flotillad's `start` event as
    the deploy record, in step 2; a second installation on one host needs parametrised container
    names; the inbox push decided as telemetry; a routine's skills may come from the default layer;
    the manifest's exact format; the example uses the ops fleet; "changed routine" defined; step 3
    holds the mount, the API and the installation name; RFC-0005 §6 and §10 and RFC-0002 §4 in §13;
    `ledger show`, which does not exist, dropped; the rename written as done.
