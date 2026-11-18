# Harness seams (ADR-0020)

The single source of truth for where the bugfix chain meets a coding harness. The chain (`bugfix/fix.py`) owns what a role
is asked, what it may touch, how it is stopped and what is recorded; a harness adapter (`bugfix/harness_<name>.py`) owns how
one CLI is started, fed and read. Each place they meet is a **seam**, S1 to S11 below: its contract, who owns it, how pi
satisfies it today, and the test that proves it.

Harnesses are adopted **one at a time**, and the seams may change with each adoption. A change to a seam is made here first,
in the same commit as the code and the tests that carry it, and goes in the amendment log at the end. pi is the reference:
it passes every seam today, and the next harness must pass the same end-to-end (see "The minimum seam end-to-end").

Code: `bugfix/harness.py` (the types, the registry, the shared runner, and the "how to add a harness" contract at its top),
`bugfix/harness_pi.py`, `bugfix/harness_prompts.py`, `bugfix/seam_e2e.py`. Words used: a **role** is repro, fix, verdict,
advisor or review; an **episode** is one run of a role through a harness; the **gateway** is the routine's LiteLLM endpoint.

## What a harness reads: every authored text, declared and hashed, and where it lands

A prompt is every authored text a model reads: the system text **and** the role's user-turn task text. The installation's rule
is that each is declared and hashed (ADR-0018 does it for `rules`). Where each is placed (the system slot, appended to the
system text, the first user turn) is the harness's business and part of S1 and S2 together; each harness's row says so.

| text | authored in | declared and hashed today | placed by pi |
| --- | --- | --- | --- |
| rules | the spec's `prompts: rules` (fleets/&lt;fleet&gt;/…), else `bugfix/fleet_rules.md` | yes when the spec declares it (ADR-0018: `declared_prompts.resolve`, fail closed); the default file is not hashed | system text, appended: `--append-system-prompt <file>` |
| fix task | `harness_prompts.fix_prompt` (equal to `pi.sh`'s) around the verdict's task and the chain's note | no: code, pinned by `test_harness_prompts.py` and the goldens | first user turn, in argv |
| repro task | `bugfix-fleet/agents/repro.md`'s body, then the issue | no: a file in the image, pinned the same way | first user turn, in argv |
| review task | `Chain.review_prompt` | no: code | first user turn, in argv |
| verdict, advisor | `triage_run.sh`, `advisor_run.sh` and their agent definitions | no: files in the image | first user turn, in argv (their leaves; wave 2) |
| a resumed turn | the chain's follow-up message (a review's, the gates', the interruption's) | no: code | the next user turn of the same session |

Not done: declaring and hashing every text above except `rules`. They are code or image files today, held still by tests, not
by a declared hash. That is the installation's v2 work; this document names it so the adapters are built for it: an adapter
never adds or edits authored text, so a hash over the chain's text is a hash over what the model reads.

## The seams

Field names are the code's: `RoleRequest`, `Invocation`, `Turn`, `Episode`, `ModelRoute` in `bugfix/harness.py`.

### S1 Prompt in

- **Contract.** `RoleRequest.prompt` is the chain's complete user-turn text. The adapter delivers it verbatim, as the first user
  turn of a new session or the next one of a resumed session. It adds no task text. A difference it cannot avoid (rules sent as a
  prefix because the CLI has no system-prompt flag, a harness's submit protocol) goes in `Invocation.notes`; the chain keeps each
  note on `Episode.notes` and as a `harness` step row in the calls file. New adapters send it on stdin (`Invocation.stdin`): no
  128 KiB argv limit, nothing in `ps`. The CLI must never wait on an inherited stdin.
- **Owner.** The chain owns the text (`harness_prompts`, `Chain.review_prompt`, `Chain.attempt`'s note); the adapter owns delivery.
- **pi today.** One argv string: `pi.sh` and `repro.sh` pass `"$PROMPT"` with `< /dev/null`, `Chain.review` passes it as the last
  argument, bounded by `REVIEW_PROMPT_MAX` (100000 bytes, under Linux's 128 KiB). pi's review is started with the chain's own
  stdin, which `pi -p` reads when it is a held-open pipe: found 2026-11-18, when the seam end-to-end's review hung for minutes
  under a test runner and ended once stdin was closed. Production has run the review this way; what the worker's stdin is was
  not examined here. The end-to-end closes stdin. Not changed here: pi's calls are pinned byte for byte by the goldens.
- **Proof.** Unit: `test_pi_golden.py` (argv and prompts), `test_harness_prompts.py` (the Python prompts equal the shell's),
  `test_harness.py` (`Execute`: the prompt is on stdin, not in argv). End-to-end: `test_s1`.

### S2 Rules and system text

- **Contract.** `RoleRequest.rules_path` is the declared `rules` file, already verified against its hash before any step runs
  (`declared_prompts.resolve`; a file that does not hash to what flotillad loaded ends the case with `prompt: …`, before any
  model call), or `None`. The adapter delivers it as system text added to the harness's own system prompt; if the CLI cannot, as
  a prefix to the prompt, and says so in `Invocation.notes`. The repository's own `AGENTS.md` or `CLAUDE.md` is never read: the
  fleet's rules replace it. A review with `--review-tools none` gets no rules (they are about using tools).
- **Owner.** The chain resolves and checks the file; the adapter delivers it.
- **pi today.** `--no-context-files --append-system-prompt <path>` (`--no-context-files` alone for the tool-less review).
  `native_system_prompt`: yes.
- **Proof.** Unit: goldens (`rules_given_and_review_reads`, `…_has_no_tools`, `no_rules`), `test_declared_prompts.py`. End-to-end:
  `test_s2` (the marker of the rules file is in the system text of fix and repro, the marker of the repository's `AGENTS.md`
  is in none, the tool-less review has neither; a declared file is delivered; one that does not hash is refused before a request).

### S3 Model route through the gateway

- **Contract.** `RoleRequest.model` is a `ModelRoute(alias, model, base_url, endpoint, key_file, key_env, meta)`, made from
  `FLOTILLA_MODELS` by `harness.route`. The adapter points the CLI at `base_url` in the wire protocol the CLI speaks (chat
  completions; later `/v1/messages`, `/v1/responses`), asks for `model`, and sends the key as `harness.read_key` returns it:
  in the child's environment, or in a mode-600 file named in `Invocation.files` under `run_dir`; **never in argv**, never in a
  file of the repository. No other endpoint, no fallback model. Alias names are per routine (`<fleet>-<routine>-<role>`), a
  convention: an endpoint names a key's variable or file, which each worker resolves to its own key, so routines could share
  an alias; per-routine names keep them apart in the calls (`run_calls.alias`), the Models page and `models:`. The adapter
  takes whatever `FLOTILLA_MODELS` gives the routine.
- **Owner.** The chain picks the alias per role; the adapter wires it.
- **pi today.** `pi_config.write` makes `<run_dir>/pi/models.json`: a provider `flotilla-<endpoint>` (OpenAI completions) with
  `apiKey: "!cat <key_file>"` or `"$<key_env>"`, so the key is read by pi at call time and written nowhere. argv: `--provider`,
  `--model`. The review is sent with the worker's provider (`self.provider`), as it always was.
- **Proof.** Unit: `test_harness.py` (`Types`: route and key), `pi_config` in `test_fix.py`. End-to-end: `test_s3` (every request
  is `POST /v1/chat/completions` with `Authorization: Bearer <the key>` and the alias's model; the key is in no argv and no file
  of the killed run's directory).

### S4 Stream to Episode

- **Contract.** `Harness.parse(stream_text, files) -> Episode`, never raising. `Episode(harness, role, session_id, turns, answer,
  error, exit_reason, usage_total, cost_est, raw_path, notes)`; `Turn(n, model, text, tools[{name, arg, ok}], usage{input, output,
  cache_read, cache_write, reasoning} or None, cost_est, stop, error)`. `answer` is the last non-empty assistant text, stripped
  (exactly what `fix.assistant_text` gives for pi). A stream that is empty, garbled or cut by a kill gives an Episode with `error`
  set and the whole turns it has. `exit_reason` is completed, turn_cap, timeout or error; the runner sets the last three from the
  exit code. `Invocation.outputs` names files the run writes that `parse` needs (a trajectory, a last-message file).
- **Owner.** The adapter parses; the chain reads only the Episode.
- **pi today.** `harness_pi.parse` over pi's `--mode json` stream; `fix.assistant_text`, `fix.stream_error` and `fix.usage` are
  wrappers over the same functions.
- **Proof.** Unit: `test_harness.py` (`PiAdapter`, `check_contract` on every sample in `testdata/pi/`), the parse golden.
  End-to-end: `test_s4` (the real stream of five episodes becomes the turns, tools, usage and answer the script produced).

### S5 Turn cap and timeout

- **Contract.** The chain's cap counts `Harness.turn_ended(line)` on each stdout line as it arrives and, at `--fix-max-turns` or
  `--repro-max-turns`, kills the whole process group: exit 125, `exit_reason` turn_cap. The timeout does the same: exit 124,
  timeout. Neither leaves a process behind; both leave the stream (the tee). A harness's own cap is set one above the chain's.
  Declared: `supports_turn_cap`.
- **Owner.** The chain and the shared runner (`harness.run_process`); the adapter says which line ends a turn.
- **pi today.** A `message_end` line of an assistant message; no cap of pi's own.
- **Proof.** Unit: `test_harness.py` (`Runner`, `Execute`), `test_fix.py` (the cap and timeout cases). End-to-end: `test_s5` (a
  grinding fix is stopped at turn 1 long before the timeout, a held request is stopped by the timeout, no process of either is
  left, and `turn_ended` counts the model calls of the real stream).

### S6 Tools and read-only

- **Contract.** `RoleRequest.tools` is `all`, `read` or `none`, and `readonly` is true for verdict, advisor and review. A harness
  that has switches for it turns off writing and the shell for a read role, and declares `enforces_readonly`. One that does not
  is asked in the prompt and the chain verifies the worktree after the role: a change is a fatal stop with a clear message
  (the lead's decision 4, for Codex and dsh). That check is built with the wiring of the roles (step 5), not yet.
- **Owner.** The chain states the need; the adapter turns it into the CLI's switches.
- **pi today.** Enforced: `--tools read,grep,find,ls` or `--no-tools`; a fix keeps pi's own four. The verdict and advisor leaves
  pass `PI_TOOLS`.
- **Proof.** Unit: goldens (`review_tools_read`, `PI_TOOLS` of the leaves). End-to-end: `test_s6` (the tools a request offers:
  none for the default review, find, grep, ls and read for `--review-tools read`, a shell and writes for fix and repro).

### S7 Session and resume

- **Contract.** `Harness.can_resume` and `Harness.resume_after_kill` are declared. After an episode the chain keeps
  `episodes/<n>-<role>.json` with `resume_ref` = `Harness.resume_ref(ep)` (normally the session id); a later call gets it as
  `RoleRequest.resume`, and `build` makes the resume invocation, or raises (never a fresh session in silence). The checkpoint
  (ADR-0019) names the harness of each role (`harness`, `repro_harness`, `review_harness`; absent means pi); a run whose flags
  differ from the checkpoint's starts fresh. A non-pi checkpoint's session is the episode file, a path inside the run directory.
  A harness that cannot resume takes the `--no-resume` path. A resumed run records only its own calls (the `.recorded` marker).
- **Owner.** The chain owns the checkpoint and the follow-up text; the adapter owns the resume invocation.
- **pi today.** `PI_RESUME_SESSION=<session file>` to `pi.sh`, which adds `--session <file>`; `trim_session` drops the line a kill
  cut; a run killed in the middle of a fix continues it with `INTERRUPTED`.
- **Proof.** Unit: `test_episodes.py` (the checkpoint's harness keys and the mismatch rule, the episode file as a session and
  nothing outside the run directory, a resumed run's carry, calls a kill left owing), `test_fix.py` (`Durable`), goldens
  (`…_resumes_the_session`). End-to-end: `test_s7` (a review's follow-up arrives in the same conversation with its earlier turns;
  a run killed in the middle of the fix and started again with `FLOTILLA_RESUME` goes on in that session and finishes).

### S8 Calls recording

- **Contract.** Each model call is one line of the run's calls file (`FLOTILLA_CALLS_FILE`): `callrec_pi`'s line format (`at, alias,
  endpoint, model, deployment, tokens_in, tokens_out, cost_usd, cost_source, latency_s, status, error, finish_reason, step,
  decision_key, role, episode, episode_seq, tools`) plus `harness` for every harness but pi (an absent field means pi). A tool's
  argument is redacted by `callrec_pi.clean_arg`. Roles are coder, reviewer, verdict, advisor and (non-pi) reproducer. A harness
  that reports per exec or per step says so in `Episode.notes`.
- **Owner.** The chain writes the lines from the Episode; the adapter supplies turns.
- **pi today.** `callrec_pi.session_lines` over pi's session files, one line per assistant message.
- **Proof.** Unit: `test_callrec_pi.py`, `test_episodes.py`. End-to-end: `test_s8` (one line per request, the scripted tokens, an
  episode number per session, the tools with their `ok`).

### S9 Cost

- **Contract.** The gateway's bill is authoritative: `spend.Meter.episode` reads the key's spend after each episode and the lines
  share it by tokens (`cost_source: gateway-share`). A harness's own figure is kept as `Turn.cost_est` and `Episode.cost_est`,
  an estimate, and is reported only when the gateway cannot be read: `spend.episode_cost`, source `harness-estimate`. Declared:
  `reports_cost`.
- **Owner.** The chain and `spend.py`.
- **pi today.** The gateway; pi's catalog figure (`spend.pi_cost`) is the fallback.
- **Proof.** Unit: `test_spend.py`, `test_episodes.py`. End-to-end: `test_s9` (the spend file is the gateway's figure for the run).

### S10 Phone-home off

- **Contract.** Nothing reaches a host but the gateway. `Harness.phone_home_env()` and the CLI's own configuration switch off update
  checks, telemetry, model-catalog fetches and plugin downloads; the egress proxy carries no vendor host. `harness_probe.py`
  (run by the installation) checks each against the proxy's log.
- **Owner.** The adapter.
- **pi today.** The worker image sets `PI_OFFLINE=1` and `PI_SKIP_VERSION_CHECK=1` (`worker-bugfix.Dockerfile`), so `phone_home_env()`
  is empty; pi uses Node's environment proxy (`NODE_USE_ENV_PROXY=1`).
- **Proof.** End-to-end: `test_s10` (a proxy standing where the network is saw nothing in any of the runs) and its control
  (pi pointed at a host that is not on `NO_PROXY` does reach the proxy, so silence means something).

### S11 Workdir, HOME and the session store

- **Contract.** The CLI runs with `RoleRequest.workdir` (the worktree, `<run_dir>/wt`) as its cwd and HOME = `run_dir`. Its
  sessions, config and caches land under `run_dir`, where the checkpoint can keep them and the run's end deletes them. Nothing is
  written to the worker's own HOME.
- **Owner.** The chain sets HOME and the directories; the adapter puts its store under them.
- **pi today.** `PI_CODING_AGENT_DIR=<run_dir>/pi`: `models.json`, `auth.json` and `sessions/<cwd>/*.jsonl` there.
- **Proof.** End-to-end: `test_s11`.

## Capabilities

Each harness declares each capability, `yes` or `no`, as attributes of its adapter (`harness.CAPABILITIES` names and explains
them). A spec that asks for one the harness does not declare is rejected before any model call: `harness.missing(h, wants)`, and
flotillad's `-check` against this table (a step with hooks on pi is refused, for one). `test_harness.py` keeps the rows below
equal to the adapters. Adding a harness adds its row.

<!-- capabilities:begin -->
| harness | roles | can_resume | resume_after_kill | enforces_readonly | supports_hooks | reports_cost | reports_usage | supports_turn_cap | native_system_prompt | prompt_via_stdin |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| pi | advisor, fix, repro, review, verdict | yes | yes | yes | no | yes | yes | yes | yes | no |
<!-- capabilities:end -->

## The minimum seam end-to-end

A harness is adopted when this passes for it, with its real CLI binary. `bugfix/seam_e2e.py` is the shared part;
`bugfix/test_seam_<name>.py` is the harness's run of it (`test_seam_pi.py` is the reference).

- **The CLI** is the real binary on PATH, started by the chain; a recording wrapper first on PATH keeps its argv, cwd and
  environment and its whole stdout, and then runs the real one. Without the binary the test is skipped, with the reason.
- **The repository** is the fixture of `test_fix.py` plus an `AGENTS.md` it must never read: `src/a.js` returns 1, the issue says
  "value() returns 1, want 2", one test fails until `src/a.js` returns 2.
- **The model** is `ScriptedEndpoint`, a stdlib HTTP server on loopback that speaks the protocol the harness uses (chat completions
  today; `/v1/messages` and `/v1/responses` are added by the adoption of the first harness that needs them) and answers from a
  script: a tool call that writes the file, then a final text. It records the path, the `Authorization` header, the model, the
  system text, the conversation and the tools offered, and answers `/key/info` with a spend that grows with each call.
- **The network** is a `RecordingProxy` as the egress proxy, with the gateway on `NO_PROXY`: any request that reaches it is a host
  that is not the gateway.
- **The runs**, through `Chain` with the verdict and advisor leaves as the test's fakes (wave 2): (1) repro, fix, a review that
  asks for changes, the fix resumed with it, a review that approves, with the calls file and the spend file on; (2) the same with
  `--review-tools read`; (3) a fix that never stops, with a turn cap of 1; (4) a fix whose request is never answered, with a
  timeout; (5) a run as a process, killed with SIGTERM to its group while the fix is in flight, then started again with
  `FLOTILLA_RESUME`.
- **The checks** are `SeamSuite`: one test per seam, S1 to S11, and a control for S10. A harness's subclass sets `NAME`, `BINARY`,
  the tool names (`READ_TOOLS`, `WRITE_TOOLS`), `SESSIONS`, `flags()` (the chain flags that put the three roles on it) and `env()`,
  and `control_sees_other_hosts()`; it adds the protocol to the endpoint if it is a new one.

Not covered by it: that a real model behaves; the verdict and advisor roles (wave 2); any harness version other than the one on
the machine that runs it (pi: 1.0.0 here, 0.80.10 in the worker image); resume of a killed session for a harness whose resume is
not yet tried; CI, which has no pi.

## Adopting a harness: the order

1. Add its row to the capability table, and the seams that change here, in the same commit as the code.
2. The adapter and its unit tests (`test_harness_<name>.py`, samples in `testdata/harness/<name>/`, synthetic ones marked until a real
   run is recorded), then `test_seam_<name>.py` until every seam passes.
3. Only then the chain's flag for it (`--harness`, `--repro-harness`, `--review-harness`), the image, and the installation's probe.

## Not done, and not verified

- Declaring and hashing the texts other than `rules` (above).
- The after-the-fact check of the worktree for a read role of a harness that cannot enforce it (S6), and the startup check of a
  spec's needs against the table (`harness.missing` exists; nothing calls it yet).
- The verdict and advisor roles through adapters; `triage_run.sh` and `advisor_run.sh` run pi as before.
- Every harness but pi: no adapter, no row, no end-to-end.
- `/v1/messages` and `/v1/responses` in the scripted endpoint.
- pi 0.80.10, the version in the worker image: the end-to-end ran on 1.0.0.

## Amendment log

- 2026-11-18: created with pi as the reference; S1-S11, the capability table, and the seam end-to-end.
