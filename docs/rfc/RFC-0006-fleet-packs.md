# RFC-0006 — Fleet packs: flows as files, shared between people and promoted between environments

**Status:** Draft, revision 1 (2026-11-12). Nothing is built from it until it is accepted.
**Builds on:** [ADR-0004](../adr/ADR-0004-job-runtime.md) (the runtime, steps and the harness),
[ADR-0007](../adr/ADR-0007-routine.md) (routines), [ADR-0008](../adr/ADR-0008-autonomy.md) (autonomy),
[ADR-0011](../adr/ADR-0011-tools-and-actions.md) (tools and actions),
[ADR-0012](../adr/ADR-0012-skills.md) (skills), [RFC-0002](RFC-0002-job-runtime.md) §4, §8 and §9.

## 1. Why

A fleet's behaviour lives more and more in text: routine specs, playbooks, skills, harness settings.
Skills showed how far plain files go: a folder with a `SKILL.md`, copied to share it, loaded by any
harness that reads the format. This RFC applies the same idea to whole flows, for two uses.

**Sharing between people.** Five people run flotilla. One finds a better triage flow, another adapts
it to a different error tracker, a third writes a better daily brief. Sharing is copying a folder;
using it is loading that folder. Nobody exports or imports in any other sense.

**Promotion between environments.** A team wants to try a new routine, a changed playbook or a new
skill without any material effect, evaluate it the way the real system is evaluated, and only then
move it to the installation that acts. That is development, test, acceptance, production, or just
"a sandbox and the real one": flotilla should not fix the number of tiers or their names. An
installation can run in **non-impact mode**, nothing it runs changes anything outside it, and the
same files move to the next installation once they have earned it.

**Predictability.** People who run operations trade capability for predictability: when AI acts, they
want to gate what happens and know the net result of a change before it takes effect. A model change
is the plainest case: switch the alias a routine uses, run everything on a capped installation,
feed its run records to the evaluation harness, and promote the change only when the numbers are
acceptable. The same holds for a new playbook, a skill or a threshold. Promotion then is not a leap
of faith but a measured step, and the record of what was measured travels with it.

All three need the same thing: a flow that is a self-contained set of files, with no installation's
facts inside, which flotilla can check, run safely somewhere new, evaluate the same way everywhere,
and record exactly.

## 2. What a fleet pack is

```text
<pack>/
  FLEET.md            the manifest: frontmatter, then a human description
  routines/*.yaml     routine specs (trigger, steps, harness settings, skill rules, autonomy)
  playbooks/*.md      the routines' own instructions
  skills/<name>/SKILL.md
  defaults/*.json     tunable values with their defaults (thresholds, quiet days, limits)
  evals/              optional: the pack's own replay corpora and pass criteria
```

**`FLEET.md` frontmatter:**

```yaml
name: triage                 # the fleet's name in an installation; the folder's name
version: 1.4.0               # the author's; flotilla records the commit, not this
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
    source: {git: https://example.org/acme/flotilla-packs.git, path: triage, commit: 3f2c9e1…}
    slots:
      repo_for_project: {podcast-pipeline: acme/podcast, orrery: acme/orrery}
      error_tracker: glitchtip
      alert_source: grafana
```

`-check` refuses a pack whose slots are not all filled, or filled with the wrong kind. A run gets its
slots as its routines get their configuration today.

### What travels and what stays

| Travels with the pack (behaviour) | Stays in the installation (facts) |
| --- | --- |
| routine specs: steps, triggers, limits, skill rules | slots: repositories, projects, hosts |
| playbooks and skills | endpoints, credentials, the egress allowlist |
| the model **alias** each routine uses, and its harness settings | what each alias maps to: endpoint, model, key (flotillad's `models:`) |
| defaults: thresholds, quiet days, budgets | the installation's autonomy cap and its controls |
| the pack's evals | the installation's history, held-out data and ledgers |

So a model change has two forms. Moving a routine to another **alias** is a pack change: it travels
and is evaluated like any other. Pointing an **alias** at another model is an installation change:
it is evaluated in a capped installation the same way, and repeated on the next installation's config,
outside the pack. Software outside flotilla that a change needs (an endpoint, a model server) is out
of scope; the promotion record names it as a precondition.

## 5. Sources, layers and pinning

Packs come from the same kinds of sources as skills (ADR-0012), and are pinned the same way:

| Layer | Where | Pinned by |
| --- | --- | --- |
| **built in** | flotilla's repo, `fleets/<name>/` | the image tag |
| **the installation's** | a git repo, path and commit in the installation's config | the commit |

- A fleet's name is unique in an installation; no layer shadows another.
- The deploy fills the installation layer from the pinned commit, as for skills and knowledge; a
  rollback restores the previous set.
- Every run records its pack's name, layer, commit and the hashes of the files it read, so a replay
  knows exactly which flow ran and a report can say which version decided what.

Sharing is publishing a folder in any git repo. Using it is one entry in the installation's config,
pinned to a commit.

## 6. A pack new to an installation starts at shadow

Whatever its specs say, a routine from a pack that is **new to an installation, or whose commit
changed**, runs at `shadow` there until the operator raises it. Its autonomy is earned in that
installation, routine by routine (ADR-0008); the author's settings are an upper bound, never a
starting point. A pack's evidence from elsewhere (§7) informs the operator's decision; it never makes
it.

## 7. Environments and promotion

### Non-impact mode

An installation can be **capped**: `max_autonomy: shadow` in its config caps every routine of every
pack, whatever a control says, as a routine's own `max_autonomy` caps it today. A capped installation:

- reads the same signals as the one that acts (read-only credentials are enough);
- runs every routine in shadow: outputs are drafted and recorded, never sent;
- keeps its own store and ledgers, never the acting installation's;
- is evaluated exactly as the acting one: the same replays, the same shadow comparison.

It can be a separate machine, or a second flotilla on the same machine (a second compose project with
its own state). Inside one installation, the candidate worker (RFC-0002 §9) stays the tool for one
image against the live routine; a capped installation is the tool for packs, skills and new routines.

### Promotion is moving a commit

Promotion is the next installation pinning the same commit, with the evidence attached:

1. In the capped installation, the pack at commit C runs and is evaluated: replays over the pack's
   corpora and the installation's history, and a comparison with what the acting installation decided
   on the same signals. Its run records (the exact captures of RFC-0002 §9 step 1: every model call
   as sent and answered, every lookup, every drafted output) are the input to the evaluation harness,
   the same input whether the run was capped or acting.
2. `flotilla pack report <pack>` writes a **promotion record**: the pack, commit C, the source
   installation, the evaluation results (counts and pass/fail, no item content), the period, and the
   preconditions outside the pack (an alias the next installation must map, a service it needs).
3. The next installation's operator pins C with that record. flotillad stores the record with the
   deploy, and the routine starts at shadow there (§6).

No tier has a fixed name or count: dev → test → prod, or sandbox → prod, or ten. An installation's
config says which installation it takes promotions from, if any.

### What is needed for this, beyond packs

- **The installation-wide autonomy cap** (above).
- **Outputs only through actions.** A capped installation is non-impact only if nothing writes outward
  except through actions the cap holds. Today the triage fleet enforces shadow in its own code
  (`orchestrator.py` forces a dry run); with packs, every outward write must be an action decided by
  policy (ADR-0005), so the cap holds whatever a pack's text says.
- **A cross-installation comparison.** `shadow_compare` compares two ledgers on one host. Comparing
  a capped installation with the acting one needs each one's decisions as an export (counts by
  default, as `shadow_compare.py` already prints) or a read API between installations.
- **Read access without write access.** The capped installation's credentials must be unable to write
  (viewer tokens, read-only database roles): non-impact by credential, not by care (RFC-0002 §4).

## 8. Evaluation

A changed pack is a changed prompt and a changed flow. It is evaluated as RFC-0002 §8 describes, with
two additions:

- **A pack may carry its evals:** corpora and pass criteria in `evals/`. They are data, run by
  flotilla's own replay tools, never the pack's code.
- **The promotion record holds the evidence** (§7), so the receiving operator sees what was measured,
  where and over what period, before raising autonomy.

Where a pack's evals and an installation's held-out data meet, the installation's rules apply (for the
triage fleet: the held-out split in its evaluation data).

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
2. The manifest, the `requires` check and the recording of pack and commit, for the built-in fleets
   (no new sources yet).
3. The installation-wide autonomy cap and "new or changed pack starts at shadow".
4. The installation layer: git sources pinned by commit, filled by the deploy, slots in config.
5. The promotion record and the cross-installation comparison.
6. The ops fleet as the first pack; the triage fleet once §9 is done.

## 11. Vocabulary

| Term | Meaning |
| --- | --- |
| **fleet pack** | a folder of text (manifest, routine specs, playbooks, skills, defaults, evals) that defines a fleet; shared by copying, loaded pinned by commit |
| **slot** | an installation fact a pack needs, declared by name and kind, filled in the installation's config |
| **capped installation** | an installation whose config caps every routine at `shadow`: non-impact mode |
| **promotion record** | what moves with a pack's commit to the next installation: where it ran, what was measured, the result |

## 12. Open questions

1. **Name:** "fleet pack", or another word (flow, recipe, module)? The glossary's fit with fleet,
   routine and skill is the test.
2. **Granularity:** whole fleets only, or single routines too (one better daily brief without the rest
   of its fleet)?
3. **Defaults the installation overrides:** may an installation override a pack's `defaults/` values,
   and does an override count as a changed pack for §6?
4. **The comparison across installations:** exported files or an API between installations?
5. **Trust of sources:** any git repo the operator names, or an allowlist of sources per installation?

## 13. Alternatives considered

- **Export and import commands with an archive format:** a second format beside git, and a copy that
  loses its history. Folders in git already share, version and review.
- **Packs that may carry code:** the most flexible, and the end of "installing a pack is reviewing
  text"; a wrong or hostile pack would run arbitrary code with the fleet's credentials.
- **A fixed dev/test/acceptance/prod model:** imposes one team's process; "an installation takes
  promotions from another" covers it and any other.
- **Promotion inside one installation only** (the candidate worker): fine for one image against the
  live routine, but it shares the store, the credentials and the state volume with production, so it
  is not non-impact enough for new routines and flows.
