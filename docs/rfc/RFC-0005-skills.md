# RFC-0005 — Skills in the harness

**Status:** Draft, revision 3 (2026-11-11): the operator's decisions (§10) and two advisor reviews
applied; the second cut what no v1 user needs. Nothing is built from it until it is accepted.
**Builds on:** [ADR-0004](../adr/ADR-0004-job-runtime.md) (the harness and context),
[ADR-0005](../adr/ADR-0005-agent-pipeline.md) (cases, policy),
[ADR-0010](../adr/ADR-0010-memory-and-knowledge.md) (memory and knowledge),
[ADR-0011](../adr/ADR-0011-tools-and-actions.md) (tools and actions),
[RFC-0002](RFC-0002-job-runtime.md) §4, §5 and §9 (spec limits, context templates, triage's hints to
files).

## 1. Why

A **skill** is a folder with a `SKILL.md`: a name, a "use when" description and instructions. It is
the format Claude Code and opencode already read; the operator keeps dozens of them. A skill packages
know-how once, and an agent pulls it in only when it needs it, so the context stays small and the
know-how is reused across routines and fleets.

flotilla's harness has one fixed text per routine, the **playbook**, and the triage fleet adds **class
hints**: fragments its code picks by the signal's class. Both are skills in all but name. This RFC
makes skills part of flotilla's own harness from the start.

## 2. What a skill is in flotilla

The same format, so a skill written for flotilla also reads in Claude Code and opencode:

```text
skills/<name>/
  SKILL.md                 frontmatter, then the instructions
  references/, scripts/    optional in the format; the harness reads only SKILL.md (§5)
```

**Frontmatter:** flotilla requires `name` (equal to the folder name; the spec's name rule) and
`description` (not empty, at most 1024 characters; the index shows it as written). Every other field
is ignored and grants nothing, including another harness's `allowed-tools`.

**What a load inserts:** the `SKILL.md` body after the frontmatter, as written, with leading and
trailing whitespace trimmed.

**Playbook and skills:** the playbook says what the routine is for and is always in its context
(decided 1). Skills are reusable know-how loaded when needed. Triage's class hints become skills
loaded by rule (§4).

## 3. Where skills come from

Skills are instructions: they come only from **trusted, versioned git sources**, never from
knowledge. Three layers, each pinned per deploy, so every deploy has one exact set of skills and a
rollback restores the previous set (decided 2):

| Layer | Where | Ships how |
| --- | --- | --- |
| **flotilla's default skills** | flotilla's repo, `skills/` | inside the images, with the image tag |
| **a fleet's skills** | `fleets/<fleet>/skills/` (triage's class hints) | inside the images, like the specs |
| **the installation's skills** | a repo and path in the installation's config, pinned by full commit | filled by the deploy (below) |

A name is unique across all three layers; the check refuses a duplicate, and no layer shadows
another.

The installation layer is filled by the deploy from a host checkout at the pinned commit, the way
RFC-0002 §7 fills the knowledge volume; only `SKILL.md` is copied, never `scripts/`. Its deploy
mechanics (volume, record in `deployed.tsv`, rollback) are designed when the first installation
skill is adopted.

## 4. Which skills a routine gets, and how they load

A routine's spec lists the skills it may use; the default is none:

```yaml
skills:
  rule:                                  # loaded by code, before the model's first turn
    - when: {class: external-transient}
      load: [hint-external-transient]
  on_demand: [disk-pressure-triage, deploy-blips]   # the model may load these
```

- **By rule** (code picks): `when` is an equality match on the structured fields the routine's code
  steps put in the case layer (triage: `class`; Ops: `kind`). Rules are tried in spec order; every
  rule that matches loads, each skill once, in that order. The same in every replay.
- **On demand** (the model picks): the context holds an **index**, one line per allowed skill,
  `- <name>: <description>`, in the order of the spec's `on_demand` list. `load_skill(name)` returns
  the skill's instructions; it is a tool the runtime provides, offered only to routines with
  `on_demand`.
- **Limits:** a load is a model turn; it counts against `max_skill_loads` (default 2) and against
  neither `max_steps` nor `max_tool_calls`, so the limits the replay corpus measured still hold.
  Loading the same skill twice returns the same text and counts again.
- **Models:** `load_skill` is a tool, so RFC-0002 §4's rule applies (refused on an alias with
  `tools: none`; `tools: json` can call it, as it can a probe); the check applies it to the routine's
  declared fallback aliases too.

## 5. Skills never grant anything

The harness never runs or reads a skill's scripts. A script becomes a **tool** or an **action** only
by a reviewed change to the registry that names it as the implementation: a read-only script becomes
a tool, run in the routine's worker with that role's credentials; a script that changes something
becomes an action, planned by an agent, decided by policy, run by a host agent's `actions` role
under that host's allowlist.

A skill can teach an agent how to use a tool or when to plan an action; it can never grant one: the
harness offers a routine only its `harness.tools` (RFC-0002 §4), and a call to any other tool gets the
unknown-tool error as its answer. A check-time declaration of the tools a skill relies on is added
when a skill needs one.

The operator's existing skills come later, one at a time, each rewritten for the routine's tools
(they name operator paths and SSH targets, and say "Use when asked…"); their scripts become
registered tools first, with the worker's credentials, never the operator's; skills that change state
stay out until their steps are actions.

## 6. Check, context and recording

- **The check:** flotillad's `-check` (before every deploy) verifies that each skill a routine lists
  exists in exactly one layer, its frontmatter is valid (§2), its size is under the cap, and
  `on_demand` is declared only with models that can call tools (§4). It refuses a `SKILL.md` or an
  index over its character cap (set from the first real skills). A skill is never trimmed at run
  time: an instruction cut in half is worse than none.
- **Paths:** the loader reads `<layer>/<name>/SKILL.md` only, with `name` the folder's name under the
  spec's name rule, and follows no symlink.
- **Where skills render:** where a layer renders is the fleet's context template (RFC-0002 §5). For
  triage's like-for-like move, rule-loaded skills render inside the `case` template, after the
  `SIGNAL:` text and one blank line, exactly as the hint is appended today. Rendering them anywhere
  else is a behaviour change, proven by model replay. The index renders after the playbook.
- **Instructions, not data:** a skill enters outside the data delimiters that wrap memory and
  knowledge, which is why only trusted, reviewed git sources may provide one. Loading a skill does
  not taint the run; a tool result taints by its source's trust, as before.
- **Recording:** every load records the skill's name, layer, commit and content hash, as a
  `skill.load` span; exact replay serves the recorded text; the case page's "What the agent looked
  at" lists the skills loaded, beside memory and knowledge.

## 7. Evaluation

A changed skill is a changed prompt: it is replayed like a playbook change (ADR-0004's evaluation
modes). The evaluation can force a skill as if rule-loaded, so a changed on-demand skill is tested on
every recorded case, not only those where the model chose it (`SF_NO_CLASS_HINT` in `triage.py` is the
same switch in the other direction). For on-demand skills two questions are measured per model alias:
did the agent load the skill when the case needed it, and did loading it improve the decision. Not
verified yet: how well the cheaper models (DeepSeek through OpenRouter, the local Qwen) choose skills
on their own; the first on-demand skill ships in shadow with that measurement. Triage's first
`on_demand` use also changes its system prompt (the tool menu gains `load_skill`): a model-replay
change.

## 8. Vocabulary

| Term | Meaning |
| --- | --- |
| **skill** | a `SKILL.md` folder of reusable know-how, from a trusted git source, pinned per deploy; loaded by rule or on demand |
| **playbook** | a routine's own instructions, always in its context |
| **skills index** | the names and descriptions of the skills a routine may load on demand |

## 9. v1, in two parts

- **With RFC-0002 §9 step 3** (triage's playbook and hints to files): the loader over the two
  in-image layers, the `skills.rule` spec field, rule loading rendered in the `case` template,
  recording, and the check (§6). Triage's four class hints become rule-loaded skills and must replay
  byte-identical to today's prompt.
  - Each hint is one line with no line break today, so its `SKILL.md` body keeps it on one line;
    `fleets/*/skills/` goes in `.markdownlintignore`. Wrapping a hint is a behaviour change (model
    replay).
  - The worker image gains `COPY fleets` (the same commit as flotillad's), so the harness in the
    worker reads the same bytes the check saw; today only flotillad's image has `fleets/`.
  - `internal/spec/spec.go` parses strictly and has no `harness` or `skills` field yet; both are new
    fields.
- **With RFC-0002 §9 step 4** (the loop to the harness): `skills.on_demand`, the index, `load_skill`,
  `max_skill_loads`.
- **Later, with the first skill that needs them:** the installation layer's deploy mechanics,
  reading `references/`, and a check-time declaration of the tools a skill relies on.

## 10. Decided (operator, 2026-11-11)

1. **Playbooks stay the routine's own text,** not skills.
2. **Skills are pinned per deploy,** in three layers (§3): flotilla's defaults and the fleets' skills
   ship in the images; the installation's skills repo is pinned by commit in config and filled by the
   deploy. Which repo and path the homelab uses is set when the first installation skill is adopted.
3. **Agents do not write skills in v1.** Later, at most as a pull request the operator reviews.

## 11. Docs amended on acceptance

- ADR-0004's status line: amended by this RFC's ADR (the `fragments` setting and RFC-0002 §9 step 3's
  `hints/*.md` become `skills.rule` and `fleets/triage/skills/hint-*/SKILL.md`). RFC-0002's text stays
  as a record.
- `docs/architecture/components.md` (the context builder) gains the rule skills and the skills index.
- `docs/testing.md`: "a playbook or fragment" becomes "a playbook or skill".
- `docs/architecture/data.md` (the spec row) and `docs/security.md` T7 add skills to what changes only
  by a reviewed commit and a deploy.
- `docs/architecture/index.md`: the vocabulary gains "skill".

## 12. Alternatives considered

- **Switch harness** to one with skills built in (the Claude Agent SDK, Claude Code headless,
  opencode): model lock-in or a second runtime, and losing policy, taint, recording and budgets in the
  loop.
- **Everything in the playbook:** no reuse, and the context grows with every case kind.
- **MCP prompts** as the carrier: no on-demand loading by the model, no trust per source, and a server
  to run for what is a folder of files.
- **Let skills run their scripts** (as Claude Code does): a wrong or injected skill would be arbitrary
  execution; the registry and policy exist to prevent exactly that.
- **Count skill loads against the step or tool-call limits:** a load would cost a turn the replay
  corpus measured; a separate limit keeps the corpus valid.
