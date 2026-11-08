# RFC-0002 — the job runtime: harness, context, memory, external knowledge

**Status:** Proposed. The operator settled the open choices on 2026-11-08 (§9); the text is under
review before phase 3 code.
**Date:** 2026-11-08
**Inputs:**
- [RFC-0001](RFC-0001-fleet-platform.md) §9, phase 3, and [Components](../architecture/components.md);
- how the triage fleet works today (`signal-fleet/mvp/triage.py`, `config.py`), §2;
- the operator, 2026-11-08: the runtime is OpenAI-API compatible, with LiteLLM as one deployment
  and not a requirement. Harness, context, memory and external knowledge are established from the
  start as named core concepts. Each fleet configures them its own way, and triage is only the
  first use case: the concepts grow as other fleets need more.

**Numbering:** RFC-0001 and the genesis log name RFC-0002 as the SEO fleet. That proposal is not
written yet. It becomes RFC-0003, and this one takes RFC-0002.

## 1. Goals and non-goals

**Goals:**
- Four core concepts with a fixed meaning: **harness**, **context**, **memory** and **external
  knowledge**. Each one has:
  - a definition;
  - rules that never change;
  - a spec section through which a fleet configures it;
  - named extension points, so a new fleet adds a capability instead of bending the core.
- One model client for any OpenAI-compatible endpoint.
- The triage fleet moved onto the runtime with its decisions unchanged, proven by replay.

**Non-goals:**
- A memory shared by every agent. This is the fleets' memory. Claude sessions and the X3 assistant
  keep their own and read flotilla's through MCP (operator, 2026-11-08).
- Semantic or vector search. It comes when a real case needs it (operator, 2026-11-08).
- Agents that write their own prompts, memory or specs.
- Adopting a third-party agent framework.

## 2. What triage does today

- **The harness.** A chat-completions call in JSON mode at temperature 0. The model answers either
  `{"action":"probe",…}` or `{"action":"decide",…}`. Native tool calling is not used. Around the
  call:
  - a step cap, one corrective turn for bad JSON, and a defer on an empty reply;
  - deterministic gates after the answer.
- **The context.** A playbook string inside `triage.py`, the signal, and the probe results the model
  asks for. Each probe result is stored with the exact trimmed text the model saw (`shown`). That
  record is what makes the replays possible.
- **The memory.** The model has none. "Have we seen this before" is answered in code before the
  model is called, by dedup over the TSV ledgers. Lessons ended up as gates in code
  (`operational_class`, the assistant-failover case).
- **External knowledge.** None, apart from what the probes fetch live.
- **Coupling to the gateway:**
  - setting names `OPENROUTER_URL` and `OPENROUTER_KEY`;
  - `via_gateway()`, which decides whether Langfuse metadata is sent;
  - a price table keyed by model name.

So triage already has a minimal harness. The runtime turns what triage does into named,
configurable parts, and keeps its behaviour.

## 3. The model client

One client for every model call.

- **Wire format:** `POST {base_url}/chat/completions` (OpenAI Chat Completions). LiteLLM, vLLM,
  Ollama, OpenRouter and OpenAI all accept it.
- **Endpoints** are declared in config (not in git):

  ```yaml
  models:
    endpoints:
      gateway:
        base_url: http://litellm:4000/v1
        key_file: /run/flotilla/litellm.key
        capabilities: { tools: native, json_mode: true, reports_cost: true }
    aliases:
      triage-default: { endpoint: gateway, model: fleet-triage-pro }
    prices:            # only for endpoints that do not report cost
      some-model: { input_per_mtok: 0.14, output_per_mtok: 0.28 }
  ```

- **Specs name aliases**, never providers or raw model names. With LiteLLM an alias is passed
  through to the gateway's own alias. Without it, an alias maps to a model name on any endpoint.
- **Tool protocol per endpoint:**
  - `native`: `tools` and `tool_calls`;
  - `json`: today's JSON protocol.

  The harness sees one interface. Triage stays on `json` at first so its replays stay identical
  (operator, 2026-11-08). The DGX Qwen has no tool calling enabled today.
- **Cost, in this order:**
  1. what the endpoint reports. LiteLLM 1.96.2 on the mini sets `x-litellm-response-cost` in
     `proxy/common_request_processing.py` (read in its source on 2026-11-08, not yet seen on a live
     response);
  2. the config price table;
  3. otherwise `unknown`.

  An unknown cost is never counted as zero. The job's budget then falls back to its token cap.
- **Fallback** is declared in the job's model policy and recorded. The client stores the model the
  response names, so a fallback inside the gateway shows up as well.
- **Telemetry:** every call gets its own OpenTelemetry span (alias, model, tokens, latency, cost and
  where the cost came from). On a LiteLLM endpoint the Langfuse trace metadata is added too.
- **AGENTS.md rule 5** becomes: "Every model call goes through the one model client, to an
  OpenAI-compatible endpoint, under the job's model policy. LiteLLM is the reference deployment. No
  undeclared fallback."

## 4. Core concept: the harness

**Definition.** The harness is the fixed machinery that runs one model-driven step:
1. build the context (§5);
2. offer the declared tools;
3. answer tool calls until the model gives an answer that matches the output schema, or a limit is
   reached;
4. check the answer with the gates;
5. record everything.

**Rules that do not change:**
- The harness is the same code for every fleet. Fleets differ in what they configure, not in harness
  code.
- **Tools in the loop are read-only.** Every write is an output after the gates, allowed by the stage
  or by the operator's approval.
- **Hard limits** on steps, tokens and wall time. A malformed answer gets one corrective turn.
- **Every model request and reply is recorded.** A run can then be replayed exactly, with the
  recorded replies standing in for the model, so a playbook or gate change is measured against
  recorded runs.
- **Left out on purpose:**
  - planning turns;
  - sub-agents;
  - compaction mid-run;
  - self-modification.

  Each makes runs harder to replay and audit, and harder for a 30B local model to follow.

**Configured per fleet** (spec `harness:`):

```yaml
harness:
  kind: loop                # loop | single | external
  model: triage-default     # an alias (§3)
  protocol: auto            # auto | native | json
  tools: [service_logs, metric, trace, source_state, occurrence_history]
  limits: { max_steps: 6, max_tokens: 40000, timeout: 5m }
  output_schema: schemas/triage/disposition.json
  gates: [intent, cleanup, dismiss_corroborated, edge]
```

**Extension points:**
- **Harness kinds.**
  - `loop`: the default.
  - `single`: one call and no tools, for a summary or a classification.
  - `external`: a coding harness (opencode or Pi) as one step in its own container, with its own
    limits, its diff returned as the step's output. This is for the bugfix fleet.
- **Tools, gates and output schemas** are named building blocks in the registry. A fleet adds its own
  without touching the loop.
- **Several steps in a job** (collect, detect, investigate, write a brief) run in sequence. Each
  model step is one harness run.

## 5. Core concept: context

**Definition.** The context is everything the model reads in one harness run. Code assembles it from
declared layers before the model is called. The model can add to it only through tool calls.

**Rules that do not change:**
- **Built in code, by declared layers, each with a token budget.** Trimming is visible: a marker
  says what was cut, and how much.
- **What the model saw is stored with the run:** the exact assembled text, with each layer's sources
  and versions.
- **Every piece carries its provenance:** where it came from, when, and which version.
- **Lookup is by key** (subject, `dedup_key`, repo, fleet), with full-text search as the second
  step. No semantic search yet.

**Layers:**

| Layer | From | Triage today |
|---|---|---|
| `playbook` | a versioned file in git | the prompt now inside `triage.py` |
| `case` | sources and detectors | the signal |
| `memory` | flotilla's own memory (§6) | not used yet |
| `knowledge` | external knowledge (§7) | not used yet |
| `tools` | tool calls during the run | the probes |

**Configured per fleet** (spec `context:`):

```yaml
context:
  playbook: fleets/triage/playbooks/triage.md
  budget_tokens: 24000
  layers:
    - { layer: case, budget: 4000 }
    - { layer: memory, query: subject_history, limit: 5, budget: 3000 }
    - { layer: memory, query: active_notes, budget: 1500 }
    - { layer: knowledge, source: homelab-handbook, query: by_service, budget: 3000 }
  tool_result_trim: 1800
```

**Extension points:**
- **New layer queries** (for example, decisions about the same page across sites, for SEO) are
  registered building blocks.
- **A future semantic query** would be one more query type behind the same layer, decided by its own
  RFC.

## 6. Core concept: memory

**Definition.** Memory is what flotilla itself recorded and learned: its own decisions, what came of
them, the operator's verdicts, and the lessons drawn. It lives in flotillad's database, as private
state on the host, never in git.

**Four kinds:**

| Kind | What it holds | Written by | Used for |
|---|---|---|---|
| **episodic**: the ledger (`decisions`) | decision, evidence references, model, playbook and spec version, outcome | the job, through flotillad | the `memory` layer; replay corpora |
| **feedback** | a verdict on one decision: right or wrong, why, the correct answer | the operator (UI, CLI); agents through MCP, marked as agents | replay labels; shown in context next time |
| **notes** | short lessons with a scope, e.g. "LiteLLM issues naming the X3 backend are failover, not bugs" | proposed by anyone; **activated only by the operator** | the `memory` layer, when the scope matches |
| **procedural** | gates and playbook text | a reviewed code change, proven by replay | deterministic behaviour |

**Rules that do not change:**
- **Append-only.** A change is a new event that records its actor (`data.md`).
- **A note reaches a model only once the operator has activated it** (operator, 2026-11-08). A job
  or an agent may propose notes. This is AGENTS.md rule 3 applied to memory.
- **Every note records:**
  - who proposed it and who activated it;
  - its scope (fleet, job, subject pattern);
  - a review-by date;
  - which note it supersedes.
- **Notes are a stage on the way to code.** A note that holds up becomes a gate or playbook text
  through a replayed change, and is then retired. This is the path triage's lessons already took.
- **Jobs read memory through the API,** from the worker. Workers never open the database.

**Storage:** the new tables `feedback` and `notes` next to `decisions`, with a full-text index.
FTS5 works with the driver we pin (modernc.org/sqlite v1.62.0): checked 2026-11-08 by a throwaway
test that created an FTS5 table and matched a term in it.

**How the operator and agents consult it** (one query API behind all of these):
- **CLI:** `flotilla ledger search|show|history`, `flotilla notes list|propose|activate|retire`,
  `flotilla feedback add`. `activate` and `retire` need the operator token.
- **UI:** the ledger and notes views, in phase 5.
- **MCP:**
  - read tools: `ledger_search`, `subject_history`, `decision_get`, `notes_search`;
  - write tools: `feedback_add`, `note_propose`;
  - no tool to activate a note or approve anything.
- **A daily JSONL export to the state directory.** It can be grepped and backed up, and it is the
  hand-off format for corpora in fleet-eval-data.

**Configured per fleet** (spec `memory:`):

```yaml
memory:
  record: [decisions, outcomes]
  outcome_check: { every: 1d, job: triage/outcomes }   # fills outcome on earlier decisions
  notes: { scope: [triage], max_active_in_context: 10 }
  retention: forever
```

**Extension points:**
- new decision kinds per fleet;
- outcome checkers, e.g. "did the filed issue close?", or "did the page's ranking move?" for SEO;
- new note scopes.

## 7. Core concept: external knowledge

**Definition.** External knowledge is what flotilla reads but does not own or write. Examples:
- the homelab operations handbook;
- a repo's ADRs, READMEs and code;
- a service's runbook;
- a site's sitemap or Search Console history;
- vendor documentation.

It differs from memory, which flotilla wrote itself, and from tools, which fetch live state during a
run.

**Rules that do not change:**
- **Declared, never discovered.** A fleet reads only the knowledge sources its spec names. Each
  source is in config, with its location and access.
- **Read-only, with a version.** Every piece the model sees records its source and version (commit
  sha, file hash or fetch time). A replay reads the same version.
- **Snapshots.** A source is indexed into a snapshot in the state directory, with a refresh schedule
  and a recorded version. A run reads the snapshot, not the live source, so runs are reproducible
  and do not depend on that source being up at that moment.
- **Untrusted as instructions.** Knowledge goes into the context as quoted data, never merged into
  the playbook. A repo's README can be wrong, or written to steer an agent. That is the same reason
  there is an egress allowlist (ADR-0002).
- **Private stays private.** Snapshots of private sources live in state on the host, never in git.

**Configured per installation** (config) **and used per fleet** (spec):

```yaml
# config.yaml (this installation)
knowledge:
  homelab-handbook:
    kind: git
    repo: https://github.com/chipi/agentic-ai-homelab
    paths: [docs/recipes/**, docs/adr/**, infra/**/README.md]
    refresh: 1h
  podcast-adrs:
    kind: git
    repo: https://github.com/chipi/podcast_scraper
    paths: [docs/adr/**]
    refresh: 1d

# a fleet spec
knowledge:
  sources: [homelab-handbook]
  queries: [by_service, search]
```

**Kinds to start with:** `git` (paths in a repo, at a commit) and `files` (a folder on the host).
Each is chunked by file and heading, with a full-text index.

**Extension points:**
- new kinds (`http` for a vendor doc, `dataset` for Search Console history as Parquet);
- new queries (by service, by repo path, by error class);
- a semantic index later, behind the same queries.

**Triage, first use.** The homelab handbook's entry for the service an alert names, so the
investigation starts from the recorded design (ports, known states) instead of rediscovering it.
This is new behaviour. It comes after the like-for-like move (§8 step 4) and, like any change,
ships only when proven by replay.

## 8. Moving triage onto the runtime

Each step is replay-proven against the recorded cases before it replaces the previous one, and the
fleet session agrees each one before code.

1. **Model client** under `triage.py`'s `_call`: aliases, endpoint capabilities, cost from the
   endpoint. Decisions must stay identical on the replay corpora.
2. **Playbook to a file** (`fleets/triage/playbooks/triage.md`), loaded by the context layer.
   `PROMPT_SHA` stays the hash of the same text.
3. **The loop to the harness** (`kind: loop`, `protocol: json`), with the probes as registered tools
   and the existing gates as registered gates. The recorded probe traces replay through it.
4. **The ledger client.** Decisions go to flotillad. `dispositions.tsv` and `filed.tsv` are imported
   with their original timestamps and marked `imported`, and the mirror keeps writing the TSVs as
   long as the Hub and the fleet session's gates read them.
5. **New behaviour, one at a time, each by replay:** the `memory` layer (subject history, active
   notes), then the `knowledge` layer (the handbook).

## 9. Decisions (operator, 2026-11-08)

1. Both tool protocols are supported, chosen per endpoint. Triage stays on JSON at first.
2. Notes are activated by the operator only.
3. No semantic search until a case needs it.
4. This is the fleets' memory, not a shared memory for every agent.
5. This RFC is RFC-0002, and the SEO fleet becomes RFC-0003.
6. Harness, context, memory and external knowledge are the core concepts, each configured per fleet.
   Triage is the first use case, and the concepts grow with the next fleets.

## 10. Open questions

- The outcome checker for triage: which outcome is recorded (issue closed, signal recurred within 7
  days, operator feedback), and how often it runs.
- The review-by period for notes, and what happens when it passes (a reminder, or the note retired
  automatically).
- Whether knowledge snapshots are built by flotillad or by a job of the platform fleet. The proposal
  is a `platform/knowledge-refresh` job, so the work is scheduled, observed and stale-alerted like
  every other job.
- How large the context layers' budgets are for the local 30B models. This is measured on the
  replay corpora, not chosen up front.

## 11. Alternatives considered

- **Requiring LiteLLM.** Simpler, but anyone without it could not run flotilla, and it ties the
  public repo to one gateway. Rejected by the operator.
- **A framework's agent loop and memory** (LangGraph, Letta, Hermes). These give planning,
  sub-agents and memory the agent writes itself. That is the opposite of a replayable, operator-gated
  harness, and the goal is to learn by building it (RFC-0001 §1).
- **Letting the model write its own memory.** Fast learning, but a wrong lesson is then applied to
  every later case with nobody having seen it. Rejected in favour of proposed notes the operator
  activates.
- **Vector search from the start.** Not needed for lookups by key, and it adds an embedding model, a
  dependency and an index to keep consistent. Deferred.
- **Reading external knowledge live during the run.** Fewer moving parts, but runs would depend on
  the source being up, and a replay could not see what the model saw. Rejected in favour of
  versioned snapshots.

## 12. Discussion

- 2026-11-08: drafted after the operator's review of the four concepts and their answers to the
  five questions (§9).
