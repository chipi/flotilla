# RFC-0006 — Fleet packs: flows as files, shared between people and promoted between installations

**Status:** Draft, revision 2 (2026-11-12): the operator's decisions on revision 1 applied (§12).
Nothing is built from it until it is accepted.
**Builds on:** [ADR-0004](../adr/ADR-0004-job-runtime.md) (the runtime, steps and the harness),
[ADR-0007](../adr/ADR-0007-routine.md) (routines), [ADR-0008](../adr/ADR-0008-autonomy.md) (autonomy),
[ADR-0011](../adr/ADR-0011-tools-and-actions.md) (tools and actions),
[ADR-0012](../adr/ADR-0012-skills.md) (skills), [RFC-0002](RFC-0002-job-runtime.md) §4 and §9.

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
name: triage                 # the fleet's name in an installation; the folder's name
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
spec as one in `fleets/<fleet>/` today.

**Single routines.** An installation may take one routine from a pack, not the whole fleet (one
better daily brief without the rest of its fleet). The routine brings the playbooks, skills and
defaults it names; `-check` refuses it when it names something the pack holds only for another
routine and the installation does not have.

## 3. Text only, never code

A pack **never brings code**. It names tools, gates, actions and step types from the installation's
registry; it cannot add one. A new building block is a reviewed code change to flotilla (or to an
installation's registered extension), exactly as ADR-0011 has it for actions. This is ADR-0012's
"skills never grant anything", applied to whole flows:

- `-check` refuses a pack that requires a tool, gate, action or capability the installation does not
  have, and names each one missing;
- a pack's files are read, never executed; a `scripts/` folder is ignored, as for skills;
- a pack cannot raise its own autonomy (§6) or widen a host's allowlist.

So installing someone else's pack is reviewing text: prompts, thresholds and the order of building
blocks, not a program.

## 4. Installation facts stay out: slots

Today the triage fleet's code holds facts about one installation: which GitHub repository gets which
error project (`filing.REPO_MAP`), the host names in its gates, the GitLab-or-GitHub choice. A pack
cannot carry those and still run elsewhere.

A pack declares **slots**, the facts it needs, by name and kind. The installation fills them in its
config, next to its other facts (hosts, endpoints, credentials):

```yaml
# flotillad.yaml (the installation's)
packs:
  triage:
    path: /opt/flotilla/packs/triage       # a folder; how it got there is the installation's business
    routines: [cycle]                      # optional: only these routines
    slots:
      repo_for_project: {podcast-pipeline: acme/podcast, orrery: acme/orrery}
      error_tracker: glitchtip
      alert_source: grafana
    defaults: {quiet_days: 10}             # optional overrides of the pack's defaults (§6)
```

`-check` refuses a pack whose slots are not all filled, or filled with the wrong kind. A run gets its
slots as its routines get their configuration today.

### What travels and what stays

| Travels with the pack (behaviour) | Stays in the installation (facts) |
| --- | --- |
| routine specs: steps, triggers, limits, skill rules | slots: repositories, projects, hosts |
| playbooks and skills | endpoints, credentials, the egress allowlist |
| the model **alias** each routine uses, and its harness settings | what each alias maps to: endpoint, model, key (flotillad's `models:`) |
| defaults: thresholds, quiet days, budgets | overrides of those defaults, the autonomy cap and controls |
| | the installation's history, ledgers and run records |

So a model change has two forms. Moving a routine to another **alias** is a pack change: it travels
like any other. Pointing an **alias** at another model is an installation change, repeated on the next
installation's config, outside the pack. Software outside flotilla that a change needs (an endpoint,
a model server) is out of scope; the promotion record names it as a precondition (§7).

## 5. Where packs come from, and knowing exactly what ran

A pack is **a folder on the installation's file system**. flotilla assumes nothing about how it got
there: copied by hand, unpacked from an archive, written by its operator, checked out from any
version control. No mechanism here depends on one.

| Layer | Where |
| --- | --- |
| **built in** | flotilla's repo, `fleets/<name>/`, inside the images |
| **the installation's** | the folders named in the installation's config |

- A fleet's name is unique in an installation; no layer shadows another.
- **The content hash is the version.** flotillad hashes every file of a pack (a manifest of path and
  sha256, hashed in turn). A pack's identity anywhere, in any installation, is that hash; the author's
  `version` is a label.
- **Runs read the approved snapshot, never the folder.** On approval (§6) flotillad copies the pack's
  files into its own state, keyed by the hash. A folder edited afterwards changes nothing that runs
  until its new hash is shown and approved; a deleted folder stops nothing either.
- **Every run records** the pack's name, layer and hash, and the hash of each file it read, so a
  record says exactly which flow decided what. Any other identifier the operator gives (a version
  control revision, a ticket) is stored beside the hash as a label, never instead of it.
- A rollback goes back to the previous approved snapshot; snapshots are kept until removed by hand.

## 6. Trust: installing and changing a pack is shown, approved, recorded, and starts at shadow

Whoever can write a pack's folder can propose a change, so no change takes effect unseen:

1. **A new pack, a changed pack, or a changed override** is detected by its hash. flotillad shows what
   changed: the files, with their differences, and the defaults the installation overrides with their
   values before and after. An override is called out as a **warning**, so a default never changes
   silently.
2. **The operator approves** it, with the operator's token. Until then the installation keeps running
   the previous approved snapshot, or nothing for a new pack.
3. **The approval is an audit event:** who approved, when, the pack, the hash before and after, the
   changed files and overrides. Refusals are recorded the same way.
4. **The changed routines run at `shadow`**, whatever their specs say, until the operator raises them.
   Autonomy is earned in each installation, routine by routine (ADR-0008); the author's settings are
   an upper bound, never a starting point.

Trust therefore rests on what the operator saw and approved, not on where the files came from: no
list of trusted sources is needed, and none is implied.

## 7. Installations and promotion

### Non-impact mode

An installation can be **capped**: `max_autonomy: shadow` in its config caps every routine of every
pack, whatever a control says, as a routine's own `max_autonomy` caps it today. A capped installation:

- runs every routine in shadow: outputs are drafted and recorded, never sent;
- keeps its own store, ledgers and run records;
- may read the same signals as the acting installation, or its own (dev data differs from test data;
  flotilla does not assume they are comparable).

It can be a separate machine, or a second flotilla on the same machine (a second compose project with
its own state). Inside one installation, the candidate worker (RFC-0002 §9) stays the tool for one
image beside the live routine.

### Promotion is moving the same files

1. The pack runs in a capped installation, which records everything the evaluation needs (§8).
2. `flotilla pack record <pack>` writes a **promotion record**: the pack, its content hash and labels,
   the routines, the installation it ran in and over what period, where its run records were
   exported, the overrides it ran with, the preconditions outside the pack (an alias the next
   installation must map, a service it needs), and a free reference to the evaluation's result (an
   evaluation harness report), filled by whoever judged it.
3. The next installation receives the same folder, by any means, and the record. flotillad checks
   that the folder's hash is the one in the record (a mismatch is shown, never silently accepted),
   shows the change (§6) with the record beside it, and the routines start at shadow there.

No tier has a fixed name or count: dev → test → prod, or sandbox → prod, or ten.

### What is needed for this, beyond packs

- **The installation-wide autonomy cap** (above).
- **Outputs only through actions.** A capped installation is non-impact only if nothing writes outward
  except through actions the cap holds. Today the triage fleet enforces shadow in its own code
  (`orchestrator.py` forces a dry run); with packs, every outward write must be an action decided by
  policy (ADR-0005), so the cap holds whatever a pack's text says.
- **Read access without write access.** A capped installation's credentials must be unable to write
  (viewer tokens, read-only database roles): non-impact by credential, not by care (RFC-0002 §4).

## 8. Evaluation is the evaluation harness's; flotilla provides the input

Judging whether a change is better (a model, a playbook, a skill, a whole flow), against a baseline,
is an **evaluation harness's** job, outside flotilla. flotilla does not compare installations or score
behaviour. Its part is to make every run a good input:

- **Complete run records:** every model call as sent and answered, every lookup and tool result, the
  gates' decisions, every drafted or sent output (the exact captures of RFC-0002 §9 step 1).
- **Exactly what ran:** each record carries the pack, content hash, routine, overrides, model alias
  and what it mapped to, the installation and whether it was capped.
- **Export:** `flotilla records export` writes a period's records as files in one documented format,
  the same for a capped and an acting installation, for the evaluation harness to read.
- **Production data rules travel with the records:** records are written with the installation's
  data rules (mode 600, scrubbing before anything leaves the host); what an evaluation may read is the
  evaluation's own policy (for the triage fleet, its held-out split).

## 9. What must become declarative first

The format can be designed now; the triage fleet becomes a real pack only after:

1. **The routine as steps** (RFC-0002 §4): the orchestrator's sequence (sources, the operational gate,
   triage, dedup, filing, the stale sweep) as declared steps over registered building blocks, not a
   Python program. Steps 2 to 5 of RFC-0002 §9 moved the model step and dedup this way; the rest
   follows.
2. **Installation facts as slots:** `REPO_MAP` and `INSTANCE_REPO_MAP`, the edge gate's instance, the
   test-signal markers, the substrate projects.
3. **Outputs as actions** (ADR-0005, ADR-0011), so the cap of §7 holds by construction.

Until then the triage fleet stays a built-in fleet in `fleets/triage/`, and packs start with fleets
that are declarative from the start (the ops fleet's daily brief, RFC-0001 phase 4).

## 10. Order of work

1. This RFC, reviewed and decided.
2. The manifest, the `requires` check, content hashing and recording, for the built-in fleets.
3. Installation folders, slots, overrides with their warning, approval of a new or changed pack, and
   "changed routines start at shadow".
4. The installation-wide autonomy cap.
5. Record export in one format, and the promotion record.
6. The ops fleet as the first pack; the triage fleet once §9 is done.

## 11. Vocabulary

| Term | Meaning |
| --- | --- |
| **fleet pack** | a folder of text (manifest, routine specs, playbooks, skills, defaults) that defines a fleet or a single routine; shared by copying files |
| **slot** | an installation fact a pack needs, declared by name and kind, filled in the installation's config |
| **override** | an installation's value for one of a pack's defaults; shown with a warning and approved |
| **capped installation** | an installation whose config caps every routine at `shadow`: non-impact mode |
| **promotion record** | what moves with a pack to the next installation: its hash, where and how it ran, where its records are, its preconditions, a reference to the evaluation's result |

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

## 13. Alternatives considered

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
