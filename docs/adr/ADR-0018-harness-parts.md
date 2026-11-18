# ADR-0018 — Harness parts: the system prompt, unknown tools and hooks

**Status:** Accepted

**Date:** 2026-11-18

**Decided by:** the operator, from the positioning comparison with the industry's harness list
(`docs/wip/positioning.md`, backlog 1 to 3)

## Context

The industry names the parts of an agent's harness: the model, the system prompt, the tools, the
hooks around tool calls, the rules. flotilla's harness (ADR-0004, RFC-0002 §4) had the model, the
tools, the limits and the gates after a decision, and three of those parts were not what the spec
says they are:

- **The system prompt was a fleet's code.** Triage read its playbook from a path fixed in
  `triage.py`. Skills (ADR-0012) are declared in the spec, named, hashed and recorded; the prompt
  every skill is added to was not. A change to it was a code change that no spec hash, run record
  or deploy gate saw. The bugfix fleet's rules for pi (`bugfix/fleet_rules.md`) were a default
  value of a command-line flag.
- **Only triage rejected an unknown tool.** `probes.run_probe` answered `<unknown probe: NAME>`;
  the shared harness called whatever name the model asked for. A second fleet on the harness would
  have passed a model's invented tool name straight to its tool function.
- **Nothing ran around a tool call.** What a tool may be asked, and what a model may read back, was
  each tool's own business. The only checks were the gates, which run once, after the decision.

Each is a part of the trust stack inside one agent. flotilla's outer layers (policy, autonomy,
approvals, the ledger, replay) stand on that base, so the base should be declared in the spec and
enforced by the harness, not by each fleet's habits.

## Decision

### 1. The system prompt is a declared part of a routine

- The spec has `prompts:`, a map from a role to a file under the fleet's folder: `prompts: {system:
  playbooks/triage.md}`. Roles are lower-case names; paths are relative, clean (no `..`), and end
  in `.md` or `.txt`. A routine with a `harness:` must declare `system`. A routine with no harness
  may declare other roles (the bugfix fleet's `rules`, which pi takes as an appended system
  prompt).
- **flotillad checks and hashes.** `internal/prompts` (beside `internal/skills`) runs at start and
  in `-check`: a regular file inside the fleet's folder, no symlink on the way, UTF-8, not empty,
  at most 64 KiB. The spec's own hash covers the prompt files, for a routine that declares prompts
  only: the hash of every other routine, and so the fleet's pack hash, is the hash of its spec
  bytes as before.
- **The run is handed the hash and refuses any other file.** `FLOTILLA_PROMPTS` is `{"system":
  {"path": ..., "sha256": ...}}`, the hash the file had when flotillad loaded the spec;
  `runtime/prompts.py` (and a small copy in `bugfix/`, whose image has no `runtime/`) reads the
  file under the same rules and fails closed when it hashes to anything else. Without the variable
  (an evaluation run, a checkout) the caller's default path is loaded, unverified, and a test keeps
  that default equal to the spec's.
- **Recorded.** flotillad writes a `run.prompts` event with the run id and the expected hashes (no
  schema change); a loaded prompt is a `loaded` record (role, path, sha256), as skills are.
- **Triage's `PROMPT_SHA` does not move.** `_read_playbook` loads the declared file and keeps its
  one trailing-newline strip and the rendering of the probe count and menu in `triage.py`.
- **Step B is later.** The bugfix chain's role prompts that are heredocs in the bake-off scripts
  are not moved by this ADR.

### 2. The harness rejects an unknown tool

`cfg["tools"]` is required: a harness without a declared tool list raises. A request for a name not
in it never reaches the tool function. The model is told `<unknown probe: NAME>` (the text triage's
probes already gave), in the same delimited result block, and the turn is spent, as for any tool
call. The harness reports it through `on_event("tool_unknown", name=...)`; trace entries gain no
key.

Two differences from triage's old path, both on a name the model invented: no `loaded` record is
made for a probe that did not run, and a table replay answers the unknown-tool text instead of a
`TABLE-MISS` marker.

### 3. Hooks around each tool call

- **Declared by name, implemented in code**, like gates: `harness: {hooks: {before_tool: [...],
  after_tool: [{name: redact_credentials, mode: observe}]}}`. `mode` is `enforce` (the default) or
  `observe`. The names are validated in Go as gates are (lower-case, no duplicate, a valid mode)
  and in Python when a run starts: a name no registry knows fails the run before its first model
  call. The list is in `FLOTILLA_HARNESS` and in `fleets/triage/harness-default.json`; a Go test
  keeps spec and default equal.
- **Pure functions, no I/O.** `before_tool(call) -> None | Refuse(reason)`: the tool is not called,
  the model gets `<refused by hook NAME: reason>` as the result, through the unknown-tool path, and
  the turn is spent. `after_tool(call, result) -> None | Replace(text, reason)`: the replacement is
  what the trace and the model's context hold, and the event carries the original's length and
  sha256, never its text. A before hook that raises refuses; an after hook that raises withholds
  the result. In `observe` a hook runs, what it would have done is recorded, and nothing changes.
- **Recorded three ways.** A calls-file step row (`kind: step`, `role: hook`; the worker and
  flotillad keep step rows for any run, so the run's page shows it), a `harness.hook` span, and a
  `hooks` field on the turn that asked for the tool in triage's exact capture (present only when a
  hook acted, so a capture that no hook touched is byte-identical to before).
- **`redact_credentials`** (after_tool, shared in `runtime/hooks.py`) masks API keys, tokens,
  bearer and basic credentials, URL credentials and `NAME=value` for secret-looking names, with the
  rules of `runtime/toolrec.py` minus the e-mail and IPv4 rules, which are for a short argument in
  a record, not for a log a model must read. The installation's triage routines run it in
  `observe`.
- **pi, an external agent, is not hooked here.** pi 0.80.10 (the bugfix image's pin) supports a
  blocking `tool_call` extension and a modifying `tool_result` extension, loaded with `-e` and
  running in print and JSON mode; using it is a later decision (see Consequences).

## Alternatives considered

- **A prompt in the spec text itself.** A long prompt in YAML cannot be reviewed as a document, and
  the model-replay and diff tools already read files. A file under the fleet's folder is versioned
  with the fleet, as skills are.
- **Verify the prompt only in flotillad's `-check`.** It would not catch a mounted fleets folder
  that changed after flotillad started; the run's own check against the handed hash does.
- **Hooks as arbitrary code named in the spec.** The spec would then run what the operator did not
  review with it. Names resolved in a registry in the image keep the code in git, as gates do.
- **Hooks as a second kind of gate.** Gates judge a decision once; a hook acts on a call, before
  the model sees its result. Folding them together would hide which of the two stopped a thing.
- **Tool whitelist enforced by each fleet.** It already existed for triage and was the reason a
  second fleet would have been exposed; one place in the shared loop is the fix.

## Consequences

- **Behaviour is unchanged where it is observed.** Triage's `PROMPT_SHA` and its replays stay as
  they were; hooks added in `observe` change no request and no trace. The `observe` mode is how a
  hook is proved on real runs before it is allowed to change what a model reads.
- **A mounted fleets folder must carry the declared files.** An installation's own fleets folder is
  mounted over the image's: it holds the declared prompt files too. The bugfix fix routine on a
  second machine does not mount it, so the declared `rules` file must be the same bytes in the
  image and in the installation's folder, or that run refuses.
- **A changed prompt is a changed routine.** The spec hash on every run of a routine with prompts
  moves with its prompt files.
- **Not done here:** moving the bugfix chain's heredoc role prompts into files (step B);
  `before_tool` hooks in production use (only `redact_credentials` exists, in observe); hooks
  inside pi, which would mean shipping a pi extension in the bugfix image; making the redaction
  `enforce` (it is decided per routine after its `observe` rows are read).
