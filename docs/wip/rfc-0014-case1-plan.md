# RFC-0014 case 1: ask the second brain, the build plan (2026-11-26)

RFC-0014 §4.5's case 1 and its §7 acceptance. Someone asks a question; flotilla reads the company's
documents and tasks live, drafts an answer that cites what it read, and a person approves it before
it is posted in the asker's thread. This note is what exists, what is missing, the order of work,
and the decisions it needs. RFC-0014 is amended where the build differs.

## What exists (checked 2026-11-26)

- **The hook** (`internal/hook`): an `ask` hook is config only, a new entry in `hooks:` with kind
  `question`. The installation has `smoke` and `meeting`.
- **Live reads** (§4.8, `rfc-0014-step5-plan.md`): a station's `reads:`, the MCP client, each read
  recorded with the run and replayed to its shadows. The e2e routine `e2e/reads` is the template. Off
  on the installation: no reads worker, no read servers in n8n.
- **The directory** (`internal/directory`, `store.PersonBy`): synced from the people file every
  hour. **Nothing looks a person up yet.**
- **`plumbing.reply`**: in the example registry (`host/registry.json`: kind `question`, params
  `channel` enum, `thread` text 64, `text` text 3000; risk medium, reversible, 3 per target a day)
  and its send routine (`fleets/plumbing/send-reply`), whose sender already refuses a text without
  the agent's label and a crowd call. **Not in the installation's registry** (it has
  `create_tasks` and `post_decisions` only).
- **Approvals:** in the UI and the CLI. The installation has no Slack connector (ADR-0005), so case
  1's answers are approved in the UI, as case 3's were.
- **Earned autonomy** counts each plan's own outcome (RFC-0013's 2026-11-23 amendment).

## Missing, by layer

| Missing | Layer | Where |
| --- | --- | --- |
| The asker resolved to a person or "unknown" when the case opens | flotilla | flotillad, at the hook |
| The example fleet `knowledge/answer`: reads, a cited answer, a `plumbing.reply` plan | flotilla | `fleets/knowledge/` |
| The citation gate: every citation is a read this run recorded | flotilla | the answer routine, after the model |
| The thread and channel taken from the case, never from the model | flotilla | the answer routine |
| The e2e section `ask` and a reads mutation in no-effect (your 3a C) | flotilla | `deploy/e2e` |
| The answers' scores (sources cited, approved as sent) on the routine's evaluation | flotilla | the answer routine and `eval` |
| Where questions come from and answers go (the chat) | installation | your decision (below) |
| n8n: read-only MCP servers for Google Docs and Google Tasks, with a read-only credential | installation | the homelab session; you paste the credential |
| n8n: the `ask` flow (chat to the hook) and the `reply` callback (send and status) | installation | the homelab session |
| The reads worker, its proxy, its tokens, a model alias and key for the station | installation | `deploy/compose/README.md`, "The reads worker" |
| `plumbing.reply` in the private registry, the `ask` hook, chat ids in the people file | installation | the private fleets repo, host config |

## Order of work

Everything in steps 1 to 4 is built and proven in the e2e stack against stubs, with nothing on the
installation. Step 5 needs the installation's side; step 6 is for later.

1. **The asker at the hook** (built 2026-11-26: `internal/hook` `Asker`,
   `internal/api/hooks.go` `resolveAsker`, a `hook.asker` event; the data is two flat keys,
   `asker` and `asker_name`). A hook may name one field as its asker, with the tool its ids belong
   to (`asker: {field: user, tool: slack}`). When the case opens, flotillad looks the id up in the
   directory and writes `asker: {email, name}` or `asker: unknown` into the case's data, with an
   event. It is a fact recorded once, so every later step and its replay read the same answer, and
   no model or routine decides who someone is. Tests: a known id, an unknown one, an id removed
   from the people file resolving to "unknown" on the next case.
2. **The example fleet `knowledge`, routine `answer`.** Triggered by `question` cases, one station
   `answer` with `reads:` on two servers (`docs`: search and read; `tasks`: list), a turn limit
   and a per-run read cap. The prompt asks for an answer that cites each source by the link its
   read returned. Code after the model:
   - every citation must match a read this run recorded (its tool, its link); an answer with no
     matching citation is not sent: the case escalates as "no sourced answer";
   - the text gets the agent's label, a length cap, and no crowd call;
   - the thread and channel come from the case's data;
   - it plans `plumbing.reply` at `propose`, with the cited sources in the plan's detail, so the
     approver sees what each claim rests on.
3. **The e2e section `ask`** (built 2026-11-26: `deploy/e2e/run.sh` section `ask`,
   `reads_check.py`, the stub's question script; the library is second-brain's real `sb serve`
   from a checkout, `SB_DIR`; the mutant is `shadow-reads-live`, in `no-effect.yml`; it found that
   the answer's label was not the sender's, fixed in `knowledge/answer.py`) (§7's row): a signed
   `ask` call opens a `question` case; the asker resolves to a person, and a second call to
   "unknown"; the reads are recorded with their links; the answer cites one; the reply waits for
   approval and, approved, sends one signed callback; rejected, sends nothing; a shadow replays
   from the record with the stub servers down. The scripted stub model gets a question prompt
   (`deploy/e2e/stub.py`). Then the **reads mutation** in `no-effect.yml`: a shadow that reads
   live instead of from the record must turn the run red.
4. **The scores.** Each answer's two scores, recorded through the existing evaluation: sources
   cited (from the plan) and approved as sent (from the plan's outcome).
5. **The installation's run** (§7: ten real questions, each answered in its thread, cited, approved
   before it was sent). The homelab session builds n8n's flows; you paste the Google credential and
   add the chat ids to the people file; I set flotilla's side and deploy.
6. **Later, before `reply` may earn `auto`**: the remaining gates for auto (a known asker, the
   thread the case's, 3 a day per asker, which the registry already declares), and `plumbing.retract`
   from a correction case, counted as a failed execution so the breaker drops `reply` back to
   `propose`. None of it changes anything while `reply` stays at `propose`, so it waits until the
   first ten answers show it is worth promoting.

## Decided (the operator, 2026-11-26)

All four recommendations below: **1 A**, Slack through n8n (the operator connects the Slack app
first; steps 1 to 4 do not need it); **2** one Drive folder and one task list; **3** Docs and Tasks
only, the knowledge volume later; **4** the auto gates and `retract` after the live run.

## The decisions as they were put

1. **Where questions come from and answers go.** The installation has no Slack connector.
   - **A. Slack, through n8n** (RFC-0014's `#ask` channel): n8n's Slack trigger calls the hook, its
     reply callback posts in the thread. *Con:* a Slack app and its bot token in n8n.
   - **B. Google Chat, through n8n**: the same flows, in the Google account the reads use.
   - **C. No chat yet: a Google Doc as the inbox.** A question is a line in a "Questions" doc; n8n
     sees the change, calls the hook, and the reply callback writes the answer under it. *Pro:* no
     chat app, one Google credential for everything. *Con:* not the RFC's chat experience; the
     thread is a heading, not a chat thread.
   - Recommended: **A** if you use Slack day to day, otherwise **C**; flotilla's side is the same
     for all three (the hook's fields and the reply's `thread` and `channel`).
2. **What the reads may see.** Live reads are declared, never discovered. Recommended: one Google
   Drive folder for documents (with the decisions doc case 3 writes, so the two cases close a loop)
   and one task list, each named in the n8n flow; nothing outside them is readable.
3. **The knowledge volume's code docs** (RFC-0014 §8 step 6 names them). The volume is built on
   the branch `k4-knowledge`, parked by the operator on 2026-11-16 with its merge blockers listed
   in `8b16eaf` (replay, rollback, a dark deploy, untied declarations) and a replay to pass first;
   `doc_index` and `doc_read` do not exist. Recommended: **case 1 without it**, Docs and Tasks
   only; the code docs come with the knowledge plan, as a third read source, when a replay shows
   questions about code it cannot answer.
4. **The auto gates and `retract` (step 6) after the live run,** not with it. Recommended: yes;
   `reply` stays at `propose` until then, so they protect nothing earlier.

## Where this differs from RFC-0014

- **The asker is resolved by flotillad at the hook,** not by the routine: one recorded fact per case.
- **The citation gate runs in the answer routine,** which holds the run's reads, not in the execute
  routine's pre-check, which does not; the plan carries the sources, and the send routine keeps its
  own gates (the label, no crowd call).
- **The routine is `knowledge/answer`,** not `knowledge-desk/answer`: a fleet name in one word, as
  the other example fleets have.

## Not covered by this plan

- The cost of a question: unmeasured until the e2e runs with a real model. The station's per-run
  limits cap it; the first live answers measure it.
- How large the documents get against the read caps (§4.5's open question): the first live replays
  measure it.
- Whether n8n's Google Docs node can search a folder's contents, or only list and read files; the
  homelab session checks before building the `docs` server.
- A session to an MCP server across an n8n restart, and a wrong token (step 5's "not covered").
