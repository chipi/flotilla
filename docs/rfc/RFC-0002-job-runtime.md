# RFC-0002 — the job runtime: harness, context, memory, external knowledge, evaluation

**Status:** Proposed, revision 2. The operator settled the first open choices on 2026-11-08 (§10).
Revision 1 was reviewed by an independent advisor the same day; revision 2 takes in that review
(§13). Under review before phase 3 code.
**Date:** 2026-11-08
**Inputs:**
- [RFC-0001](RFC-0001-fleet-platform.md) §9, phase 3, and [Components](../architecture/components.md);
- how the triage fleet works today (`signal-fleet/mvp/`), §2, with file and line citations;
- the operator, 2026-11-08: the runtime is OpenAI-API compatible, with LiteLLM as one deployment
  and not a requirement. Harness, context, memory and external knowledge are named core concepts
  from the start. Each fleet configures them its own way, and triage is only the first use case;
  the concepts grow as other fleets need more.

**Numbering:** RFC-0001 and the genesis log name RFC-0002 as the SEO fleet. That proposal is not
written yet. It becomes RFC-0003, and this one takes RFC-0002.

## 1. Goals and non-goals

**Goals:**
- Five core concepts with a fixed meaning: **harness**, **context**, **memory**, **external
  knowledge** and **evaluation**. Each one has:
  - a definition;
  - rules that do not change;
  - a spec section through which a fleet configures it;
  - named extension points, so a new fleet adds a capability instead of bending the core.
- One model client for any OpenAI-compatible endpoint.
- The triage fleet moved onto the runtime without a change in behaviour, proven the way §8 says
  for each kind of change.

**Non-goals:**
- A memory shared by every agent. This is the fleets' memory. Claude sessions and the X3 assistant
  keep their own and read flotilla's through MCP (operator, 2026-11-08).
- Semantic or vector search, until a real case needs it (operator, 2026-11-08).
- Agents that write their own prompts, memory or specs.
- Model-written queries (SQL, PromQL, LogsQL). Tools are named and typed (§4). A fleet that needs
  model-authored queries needs its own RFC.
- Streaming responses.
- Adopting a third-party agent framework.

## 2. What triage does today

Read from the code, 2026-11-08:

- **The loop** (`triage.py:437-514`). A chat-completions call in JSON mode at temperature 0
  (`triage.py:268-269`). The model answers either `{"action":"probe",…}` or
  `{"action":"decide",…}`.
  - **One probe per turn.** `MAX_PROBES` counts turns (`triage.py:446, 472`).
  - **Bad JSON:** a corrective message is appended and the loop continues, so corrections use up
    steps (`triage.py:467-471`).
  - **No decision once the probes run out:** one extra call asks for a flat decision
    (`triage.py:483-494`).
  - **An empty completion** retries the turn (`triage.py:458-464`).
  - **The model's replies are sent back cut to 600 characters** (`triage.py:469, 477`). Tool results
    are cut to 1,800 (`_trim`).
  - **Limits:** a 90 s timeout per call. No token cap and no wall-time cap.
- **Errors** (`triage.py:278-290, 452-457`; `orchestrator.py`):
  - 401, 403 and 402 raise `TriagerDown`: the cycle fails closed with one aggregate issue.
  - Other transport errors raise `TriageDeferred`: the signal is retried next cycle, up to
    `DEFER_LIMIT` cycles, then escalated.
  - A cycle in which every call deferred is treated as an outage.

  This was the fix for the 2026-08-13 flood, when one wiped key produced about 90 escalations.
- **Before the model:** a deterministic operational-state gate (`operational_class`,
  `triage.py:100-117`), and class hints, which are prompt text added to the user message
  (`triage.py:146-169`). `PROMPT_SHA` covers only the system prompt (`triage.py:242-246`).
- **Gates after the model.** Intent, cleanup, dismiss-corroboration and edge. The edge gate queries
  VictoriaLogs and VictoriaMetrics live (`triage.py:398`, `guards.py`).
- **Tools.** Named probes with arguments; the model never writes a query (`probes.py:3-5`).
- **Memory.** The model has none. Dedup runs in code before the model, over four keys
  (fingerprint, group, normalised and alert keys) plus the issue's live open state on GitHub
  (`filing.py:39-40, 367-373, 562`).
- **Evaluation:**
  - **The 15 replay gates** (`replays.sh`) prove the deterministic code. None calls the model:
    they run with a dummy key or a stubbed triager.
  - **The model path** is proven by `freeze.py` and `score.py`. They take a frozen probe table and
    call the live model k times, against a fixed bar: no false dismissals, no misroutes, at most
    5% escalations (`score.py:167-184`).
  - **What is recorded:** `capture.py` stores the probe table, the text shown, the final decision
    and the decision before the gates. The per-turn messages and raw replies are not stored.
- **Cost.** A price table keyed by model name (`config.py:63-70`).

## 3. The model client

One client for every model call.

**Wire format.** `POST {base_url}/chat/completions`, the OpenAI Chat Completions API, with no
streaming. LiteLLM, vLLM, Ollama, OpenRouter and OpenAI accept it.

**Configuration** (not in git). Endpoints carry defaults, and an alias can override them, because
one endpoint can front models with and without tool calling. The DGX vLLM model has no tool calling
today; the X3 Ollama model has it.

```yaml
models:
  endpoints:
    gateway:
      base_url: http://litellm:4000/v1
      key_file: /run/flotilla/litellm.key
      defaults: { tools: native, json: object, cost: reported }
  aliases:
    triage-default: { endpoint: gateway, model: fleet-triage-pro }
    local-dgx:
      endpoint: gateway
      model: NVFP4/Qwen3-30B-A3B-Instruct-2507-FP4
      tools: none
      cost: zero
  prices:   # for aliases with cost: table
    some-alias: { input_per_mtok: 0.14, output_per_mtok: 0.28 }
```

**Capabilities, per alias:**
- `tools`:
  - `native`: `tools` and `tool_calls`;
  - `json`: the JSON protocol triage uses today;
  - `none`.
- `json`: `schema` (`json_schema`), `object` (`json_object`) or `none`. The answer is validated
  against the job's output schema by the harness either way.
- `cost`:
  - `reported`: the endpoint's response says what the call cost;
  - `table`: the price table in config;
  - `zero`: a declared local model;
  - `unknown`.

**Cost.** LiteLLM reports cost in the `x-litellm-response-cost` header. Checked live on 2026-11-08:
a 5-token call to `fleet-triage-flash` returned `9e-07`, and the body's `usage.cost` was the same.
- An `unknown` cost is never counted as zero.
- A fleet with an `unknown` alias must declare `tokens_per_day` in its budget, or its spec fails
  validation.

**What the client records per call:**
- the alias;
- the model or deployment that answered;
- tokens, latency, cost and where the cost came from.

The response's `model` field does not identify the model behind an alias: LiteLLM returned the alias
name, `fleet-triage-flash`. On LiteLLM the client therefore also records `x-litellm-model-id`, which
identifies the deployment that answered. A fallback inside the gateway is then visible.

**Fallback.**
- **Job fallbacks:** declared in the job's model policy.
- **Gateway fallbacks and retries:** LiteLLM's own `fallbacks` and `num_retries` are installation
  config, recorded through the deployment id above.

**Typed errors.** The runtime keeps triage's behaviour (§2) as platform behaviour, for every fleet:

| Error | When | Harness outcome |
|---|---|---|
| `AuthOrBilling` | 401, 403, 402 | `endpoint_down`: the job stops for this run; one aggregate signal per run, not one per case |
| `Transient` | timeout, 429, 5xx, connection | `deferred`: the case is retried next run, up to the job's `defer_limit`, then escalated |
| `EmptyCompletion` | no content and no tool call | the turn is retried |

A run in which every case deferred is `endpoint_down`.

**Reasoning output.** The client strips `reasoning_content` and inline `<think>…</think>` before the
answer is parsed. Both local models today are non-thinking `Instruct-2507` builds.

**Tracing.**
- Every call gets its own OpenTelemetry span, inside the run's trace.
- On LiteLLM the request also carries `metadata.existing_trace_id`, as triage does today, so the
  Langfuse trace joins up.

**AGENTS.md rule 5** says: every model call goes through the one model client, to an
OpenAI-compatible endpoint, under the job's model policy; LiteLLM is the reference deployment; no
undeclared fallback. Revision 2 adds: an `external` step (§4) points its coding harness at the same
endpoint, under the fleet's own key and budget, and the runtime records that step's inputs and
outputs.

## 4. Core concept: the harness

**Definition.** The harness runs one model-driven step:
1. build the context (§5);
2. offer the declared tools;
3. answer tool calls until the model returns an answer that matches the output schema, or a limit is
   reached;
4. check the answer with the gates;
5. record everything (§8).

It is the same code for every fleet. Fleets differ only in what they configure.

**Rules that do not change:**
- **Tools are named building blocks with typed arguments.**
  - The model never writes a query language.
  - Every tool checks its scope and size cap, and returns its provenance.
  - Read-only is enforced by the credential the tool uses (a read-only database role, viewer
    tokens), not by inspecting what the model asked for.
- **No writes inside the harness.** Every write is an output: a GitHub issue, comment, PR or label,
  a finding, a page, a push. Outputs run after the gates, and only when the job's stage allows it or
  after the operator's approval (RFC-0001 §5, Components).
- **Gates read only the run's recorded evidence.**
  - A lookup a gate needs (the edge gate's live queries, GitHub's open state for dedup) is made as a
    recorded tool call.
  - The run has a frozen `now`, set at start.
  - Replay reads the records, not the live systems.
- **Hard limits:** `max_steps` counts model turns; `max_tool_calls` counts tool calls; there is a
  timeout per call and one for the whole step.
  - Where a fleet's protocol is one tool call per turn, the harness refuses a second tool call in
    the same turn.
  - Limits are set from what the evaluation corpus measured, not guessed. A limit that would cut a
    run that completes today is a behaviour change (§8).
- **Every request and reply is recorded:** the exact messages, the raw reply, the tool calls and the
  tool results.
- **Left out on purpose:** planning turns, sub-agents, compaction mid-run, self-modification.

**Configured per fleet** (spec `harness:`):

```yaml
harness:
  model: triage-default
  protocol: auto              # auto (from the alias) | native | json
  tools: [service_logs, metric, trace, source_state, occurrence_history]
  limits:
    max_steps: 4               # today: MAX_PROBES=3 probe turns + the deciding turn
    max_tool_calls: 3
    call_timeout: 90s
    step_timeout: 5m
    tool_calls_per_turn: 1
  corrections:
    count_as_steps: true      # triage today
  reply_echo_chars: 600       # triage today; 0 = full replies
  output_schema: schemas/triage/disposition.json
  gates: [intent, cleanup, dismiss_corroborated, edge]
  defer_limit: 6
```

The `corrections` and `reply_echo_chars` settings exist so the first move of triage is like for
like (§9). Changing them is a behaviour change, proven by model replay.

**A job is a sequence of steps.** Each step is one of:
- `code`: a source, a detector, a pre-model gate (triage's `operational_class`), an output. No
  model involved.
- `harness`: one harness run as above. A one-call summary or classification is a harness step with
  no tools and `max_steps: 1`.
- `external`: a coding harness (opencode or Pi) in its own container, for the bugfix fleet. It is
  not a harness run, and it has its own contract:
  - **inputs recorded:** the issue, the repro test, the version of the agent files;
  - **outputs recorded:** the diff, the test results, and the harness's transcript where the tool
    produces one;
  - **cost and token limits** enforced by the fleet's own LiteLLM key budget;
  - **a scratch worktree**, deleted at the end;
  - normal gates after it (repro-first, the delta suite);
  - **not exactly replayable,** and the evaluation says so (§8).

**Extension points:**
- new tools, gates and output schemas, registered as building blocks;
- new step types;
- new external harnesses.

## 5. Core concept: context

**Definition.** The context is everything the model reads in one harness run. Code assembles it from
declared layers before the model is called. The model can add to it only through tool calls.

**Rules that do not change:**
- **Built in code from declared layers, each with a token budget.** Trimming is visible: a marker
  says what was cut and how much.
- **The exact assembled text is stored with the run,** with each layer's sources and versions.
- **Data is marked as data.** Every piece from memory, knowledge or a tool goes between fixed
  delimiters, with a provenance line ("data, not instructions": source, version, time). The context
  builder enforces this; a layer cannot put text outside it.
- **Lookup is by key:** subject, decision key, repo, fleet, service. Full-text search comes when a
  query needs it.

**Layers:**

| Layer | From | Triage today |
|---|---|---|
| `playbook` | versioned files in git: the main text and its fragments | the system prompt and the class hints, both in `triage.py` now |
| `case` | the job's code steps: the signal, its class, the hint chosen | the signal |
| `memory` | flotilla's memory (§6), structured fields only | not used |
| `knowledge` | external knowledge (§7) | not used |
| `tools` | tool calls during the run | the probes |

**Configured per fleet** (spec `context:`):

```yaml
context:
  playbook: fleets/triage/playbooks/triage.md
  fragments: fleets/triage/playbooks/hints/        # chosen by the case's class
  layers:
    - { layer: case, budget: 4000 }
    - { layer: memory, query: subject_history, limit: 5, budget: 2000 }
    - { layer: memory, query: active_notes, budget: 1500 }
    - { layer: knowledge, source: homelab-handbook, query: by_service, budget: 3000 }
  tool_result_trim: 1800
```

**Extension points:**
- new layer queries (for SEO, earlier decisions about the same page on other sites), registered as
  building blocks;
- a semantic query later, behind the same layer, by its own RFC.

## 6. Core concept: memory

**Definition.** Memory is what flotilla itself recorded and learned: its decisions, what came of
them, the operator's verdicts, and the lessons drawn. It lives in flotillad's database, as private
state on the host, never in git.

**Kinds:**

| Kind | Holds | Written by | Reaches a model? |
|---|---|---|---|
| **decisions** (the ledger) | the conclusion, its keys, evidence references, versions, outcome | the job, through flotillad | as structured fields only |
| **feedback** (an event on a decision) | a verdict: right or wrong, why, the correct answer | the operator; an agent's feedback is `proposed` until the operator confirms it | only confirmed feedback, as structured fields |
| **notes** | short lessons with a scope | proposed by anyone; activated by the operator | only active notes |
| **procedural** | gates and playbook text | a reviewed code change, proven by evaluation | always: it is the code |

**Rules that do not change:**
- **Append-only.** A change is an event that records its actor (Data).
- **No model-written prose reaches a model.** The `memory` layer renders structured fields:
  - the decision, its keys and outcome;
  - the operator's verdict;
  - the time;
  - the note's id and text (the text is the operator's or approved by them).

  It never renders a model's `reason` or `question` text. A model's earlier reasoning steering a
  later decision is self-written memory, and would carry any text injected into that reasoning.
- **A note reaches a model only once the operator has activated it** (operator, 2026-11-08).
  - Activating a note changes decisions, so activation needs a model-replay run for the note's
    scope (`flotilla notes activate <id> --evaluated <run id>`), as AGENTS.md rule 3 requires.
  - Every note records:
    - who proposed it and who activated it;
    - its scope (fleet, job, subject pattern);
    - its review-by date;
    - what it supersedes.
  - A note nobody confirms by its review-by date is retired, and the operator is told.
- **Notes are a stage on the way to code.** A note that holds up becomes a gate or playbook text
  through an evaluated change, then is retired.
- **Recurrences are events on a decision,** not new decisions. In the triage ledger they had grown to
  most of the rows; a comment at `orchestrator.py:181-183` records 10,764 of 11,700 GlitchTip
  recurrence rows as identical repeats by 2026-10-01.
- **Jobs read memory through the API,** from the worker. Workers never open the database. If
  flotillad is unreachable, a job that needs memory for dedup does not run its cycle. It never acts
  without dedup.

**Storage** (additions to [Data](../architecture/data.md)):
- **`decision_keys(decision_id, kind, value)`:** a decision has as many keys as its fleet's dedup
  needs. For triage these are fingerprint, group, normalised and alert, looked up in that order by a
  registered building block.
- **Feedback** is an `events` row of kind `feedback`. No separate table.
- **`notes`:** with the fields above.
- **Full-text search:** an FTS5 index when a query first needs it. FTS5 works with the pinned driver
  (modernc.org/sqlite v1.62.0): a throwaway test on 2026-11-08 created an FTS5 table and matched a
  term.

**How the operator and agents consult it** (one query API behind all of these):
- **CLI** (with the API, in phase 3):
  - `flotilla ledger search|show|history`;
  - `flotilla notes list|propose|activate|retire`;
  - `flotilla feedback add`.

  `activate`, `retire` and confirming feedback need the operator token.
- **UI and MCP** in phase 5.
  - MCP read tools: `ledger_search`, `subject_history`, `decision_get`, `notes_search`.
  - MCP write tools, `feedback_propose` and `note_propose`, come in phase 5 too.
  - MCP has no tool to activate a note, confirm feedback or approve anything.
- **A daily JSONL export** to the state directory, for grep, backup, and handing corpora to
  fleet-eval-data.

**Configured per fleet** (spec `memory:`):

```yaml
memory:
  keys: [fingerprint, group, norm, alert]       # lookup order
  outcomes: { job: triage/outcomes, every: 1d }
  notes: { scope: [triage], max_active_in_context: 10, review_after: 90d }
```

Retention is not a setting: the ledger is kept forever (Data).

**Extension points:**
- new key kinds and lookup orders;
- outcome checkers ("did the issue close?", "did the page's ranking move?");
- new note scopes.

## 7. Core concept: external knowledge

**Definition.** External knowledge is reference material written by people or third parties, which
flotilla reads and does not own:
- the homelab operations handbook;
- a repo's ADRs and READMEs;
- runbooks;
- vendor documentation;
- a site's sitemap read as a document.

It is not:
- **fleet data:** measurements a fleet collects itself, such as Search Console rows, crawl
  snapshots or Umami counts. These are sources whose output is stored under state (Data, "Fleet
  data"). They reach the model through the `case` layer or tools.
- **memory:** what flotilla wrote.
- **tools:** live state fetched during a run.

**Rules that do not change:**
- **Declared, never discovered.** A fleet reads only the sources its spec names, and each source is
  in config.
- **Read-only, versioned, read from a snapshot.**
  - A source is read at a recorded version (a commit sha, a file hash, a fetch time).
  - A run reads that version, not the live source.
  - Replay reads the same version.
- **Data, not instructions.** It enters the context between the delimiters of §5. A README can be
  wrong, or written to steer an agent.
- **Private stays private.** Snapshots of private sources stay in state on the host.

**To start, one kind: `files`.** A read-only mount of a checkout on the host, versioned by its git
commit, with queries that are path maps. Configured per installation, used per fleet:

```yaml
# config.yaml (this installation)
knowledge:
  homelab-handbook:
    kind: files
    root: /knowledge/agentic-ai-homelab     # read-only mount of the checkout
    version: git                            # the checkout's HEAD commit
    paths: [docs/recipes/**, docs/adr/**, infra/**/README.md]
    maps:
      by_service: { litellm: infra/litellm/README.md, glitchtip: infra/glitchtip/README.md }

# a fleet spec
knowledge:
  sources: [homelab-handbook]
  queries: [by_service]
```

**Extension points:**
- a `git` kind that fetches on a schedule;
- an `http` kind for vendor documentation;
- full-text and later semantic queries.

Each is added when a fleet needs it.

**First use in triage.** The handbook entry for the service a signal names. This is new behaviour:
it ships after the like-for-like move, proven by model replay (§9, step 6).

## 8. Core concept: evaluation

**Definition.** Evaluation is how a change is proven before it ships. AGENTS.md rule 3 requires
that a change which changes decisions is replayed first. There are three modes, because they prove
different things.

| Mode | Drives the run with | Proves | Pass criterion |
|---|---|---|---|
| **code replay** | recorded inputs; the model stubbed or absent | the deterministic steps: classifiers, dedup, gates, filing | each gate's explicit criteria, with the "nothing else changes" invariant (`replay-proven-fix`) |
| **exact replay** | recorded requests, replies and tool results; the recorded reply stands in for the model | the harness and the context builder: parsing, loop control, trimming, gates after the model | decisions identical; and, for a harness change meant to change nothing, requests byte-identical |
| **model replay** | frozen tool results; the live model, k runs | anything that changes what the model sees or which model answers: playbook, fragments, layers, notes, limits, the model | the fleet's scorer bar (triage: `score.py`) |

**Which change needs which mode:**

| Change | Mode |
|---|---|
| a code step, a gate, dedup, an output | code replay |
| the harness code, the client, the context builder, meant to change nothing | exact replay |
| a playbook or fragment, a context layer or budget, a limit, activating a note, a different model | model replay |
| an `external` step | not replayable. Its gates (repro-first, the delta suite) and the operator's review of the diff are the proof |

**Rules that do not change:**
- **A change ships only with its mode's evaluation.** The evaluation run's id is recorded with the
  change (commit message, note activation).
- **Corpora with production data live in the private `fleet-eval-data` repo.** Labels use the
  scorer's format (triage: `ground_truth.disposition`, `correct_autonomous`, `work_type`).
- **A replay that cannot reproduce its own recorded history proves nothing,** and fails.
- **Every run records what exact replay needs** (§4). This starts before the move: `capture.py`
  gains the per-turn messages and raw replies in agentic-ai-homelab first, since the live fleet runs
  that code until the switch. A corpus then builds up while phase 2's shadow runs.

**Configured per fleet** (spec `evaluation:`):

```yaml
evaluation:
  code_replays: signal-fleet/replays.sh
  scorer: { run: signal-fleet/mvp/score.py, k: 3, bar: fleets/triage/eval/bar.yaml }
  corpus: fleet-eval-data:projects/signal-fleet-replays
```

**Extension points:**
- a scorer per fleet (SEO: findings against what the operator accepted; the daily brief: a rubric);
- new corpus kinds.

## 9. Moving triage onto the runtime

Each step is evaluated in its mode, and the fleet session agrees each one before code.

1. **Record what exact replay needs** (`capture.py`, in agentic-ai-homelab first, then ported).
   - *Proof:* the capture tests, and one shadow cycle's records read back.
2. **Model client** under `triage.py`'s `_call`.
   - *Proof:* a golden-request test (the request body byte-identical to today's), and the error
     classes against recorded failures (the `eval_triager_failure_replay.py` cases). Then phase
     2.7's shadow comparison.
3. **Playbook and hints to files** (`fleets/triage/playbooks/triage.md`, `hints/*.md`).
   - *Proof:* the assembled system and user messages are byte-identical to today's for every case in
     the corpus, so `PROMPT_SHA` is unchanged.
4. **The loop to the harness,** with `corrections.count_as_steps: true` and `reply_echo_chars: 600`.
   The probes are registered tools, the gates registered gates, and the edge gate's live queries
   recorded tool calls.
   - *Proof:* exact replay over the corpus from step 1, decisions identical. Then the code replays.
5. **The ledger client and dedup through the API.** `decision_keys` holds the four keys.
   `dispositions.tsv` and `filed.tsv` are imported with their original timestamps, marked
   `imported`, with recurrences as events. The mirror keeps writing the TSVs while the Hub and the
   fleet session's gates read them.
   - *Proof:* the dedup and filing code replays, and the import's row counts against the TSVs.
6. **New behaviour, one at a time:**
   - the `memory` layer;
   - then the `knowledge` layer;
   - then lifting today's quirks (full reply echo, corrections outside the step count).

   - *Proof:* model replay, each against the scorer's bar.

## 10. Decisions (operator, 2026-11-08)

1. Both tool protocols are supported, chosen per alias. Triage stays on JSON at first.
2. Notes are activated by the operator only.
3. No semantic search until a case needs it.
4. This is the fleets' memory, not a shared memory for every agent.
5. This RFC is RFC-0002, and the SEO fleet becomes RFC-0003.
6. Harness, context, memory and external knowledge are the core concepts, each configured per fleet;
   triage is the first use case, and the concepts grow with the next fleets.
7. Revise after the advisor's review, with evaluation as a fifth core concept (operator, 2026-11-08).

## 11. Open questions

- **Triage's outcome checker:** which outcome is recorded (the issue closed, the signal recurred
  within 7 days, the operator's feedback), and how often it runs.
- **The note review period:** 90 days is proposed.
- **When the TSV mirror retires:** once the Hub reads the API and the fleet session's gates read the
  export. Agreed with the fleet session at §9 step 5.
- **The context and limit budgets for the local 30B models:** measured on the corpus, not chosen up
  front.
- **How vLLM and Ollama handle `json_schema`, parallel tool calls and fields they don't know:** not
  verified. Checked against the deployed versions before an alias is marked `native` or `schema`.

## 12. Alternatives considered

- **Requiring LiteLLM.** Simpler, but anyone without it could not run flotilla, and it ties the
  public repo to one gateway. Rejected by the operator.
- **A framework's agent loop and memory** (LangGraph, Letta, Hermes). These bring planning,
  sub-agents and memory the agent writes itself, the opposite of a replayable, operator-gated
  harness; and the goal is to learn by building it (RFC-0001 §1).
- **Letting the model write its own memory, including its earlier reasoning in context.** Rejected:
  a wrong lesson or an injected line would apply to every later case with nobody having seen it.
- **"Replay" as one mode.** Rejected after review: recorded replies cannot prove a playbook change,
  and a live model cannot prove a byte-level harness change.
- **Vector search from the start.** Not needed for lookups by key; it adds an embedding model, a
  dependency and an index to keep consistent. Deferred.
- **Reading external knowledge live during the run.** Rejected: runs would depend on the source
  being up, and a replay could not see what the model saw.

## 13. Discussion

- **2026-11-08, revision 1:** drafted after the operator's review of the four concepts and their
  answers to five questions (§10).
- **2026-11-08, advisor review of revision 1** (a separate read-only agent, which read the RFC
  against the triage code). It found, and revision 2 changes:
  - **"Decisions identical on replay" was wrong.** The replay gates never call the model, and
    recorded replies cannot prove a playbook change. Evaluation became a core concept with three
    modes (§8), and per-turn recording a first step (§9).
  - **§2 misdescribed the loop** (corrections, the 600-character echo, limits that do not exist).
    Corrected, with settings that keep the first move like for like.
  - **Capabilities were per endpoint.** They are now per alias.
  - **Limits:** `max_tool_calls` and the per-turn rule added.
  - **The `external` kind broke the harness rules.** It is now a step type with its own contract.
  - **Agent feedback and the model's own reasons reached the context without the operator.**
    Feedback is now proposed until confirmed, the memory layer is structured fields only, and
    activating a note needs a model replay.
  - **The error taxonomy** (fail closed, defer, empty completion) is now platform behaviour.
  - **The ledger:** `decision_keys` for the four dedup keys; recurrences as events; dedup fails
    closed without flotillad.
  - **External knowledge vs fleet data:** the boundary is drawn so the two do not overlap.
  - **Tools:** named and typed, with read-only enforced by credentials; model-written queries are a
    non-goal.
  - **Gates:** they read recorded evidence only, with a frozen `now`.
  - **Budgets:** `tokens_per_day` and the `zero` cost state.
  - **Reasoning output** is stripped before parsing; streaming is a non-goal.
  - **Cut for size:** the feedback table, knowledge FTS and the git kind, MCP write tools before
    phase 5, per-fleet retention, the `single` kind.
- **2026-11-08, checked live while revising:** LiteLLM's response carries
  `x-litellm-response-cost`, but its `model` field is the alias, so the deployment id is recorded to
  make fallbacks visible (§3).
