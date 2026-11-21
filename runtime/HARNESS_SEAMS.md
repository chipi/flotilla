# Harness seams (ADR-0020)

The single source of truth for where a station meets a coding harness. A **station** is one model
step of a routine version, with a name the version chooses and the platform gives no meaning to. The
caller (the version's code; the bugfix chain, `bugfix/fix.py`, is the first) owns what a station is
asked, what it may touch, and what is recorded. A harness adapter (`runtime/harness_<name>.py`) owns
how one CLI is started, fed and read. The runner (`runtime/adapter.py`) owns the process group, the
turn cap, the timeout and the read-only check. The station driver (`runtime/station_run.py`) renders
a station's text and keeps its records. Each place they meet is a **seam**, S1 to S11 below: its
contract, who owns it, how pi satisfies it, and the test that proves it.

Harnesses are adopted **one at a time**, and the seams may change with each adoption. A change to a
seam is made here first, in the same commit as the code and the tests that carry it, and goes in the
amendment log at the end. pi is the reference: it passes every seam, and the
next harness must pass the same end-to-end (see "The minimum seam end-to-end").

Code: `runtime/adapter.py` (the types, the registry, the shared runner, and the "how to add a
harness" contract at its top), `runtime/harness_pi.py`, `runtime/station_run.py`,
`runtime/seam_e2e.py`, the fake harnesses in `runtime/fakeh.py`. The bugfix chain's own use:
`bugfix/pi_review.py` and `bugfix/chain_seam_e2e.py`. Words used: an **episode** is one run of a
station through a harness; a **record** is the episode as `runtime/episode.schema.json` describes it;
the **gateway** is the routine's LiteLLM endpoint.

## What a harness reads: every authored text, declared and hashed, and where it lands

A prompt is every authored text a model reads: the system text **and** the station's user-turn
text. A version declares each as a file under its `prompts:`, the version's hash covers it, and the
driver records what it rendered (name, hash and length, per call). An adapter never adds or edits
authored text, so a hash over the version's text is a hash over what the model reads. Where each
text is placed (the system slot, appended to the system text, the first user turn) is the harness's
business and part of S1 and S2 together; each harness's row says so.

The bugfix chain's texts today, which it has not yet moved onto a version's `prompts:` (the
installation's v2 work, replayed first):

| text | authored in | declared and hashed today | placed by pi |
| --- | --- | --- | --- |
| rules | the spec's `prompts: rules`, else `bugfix/fleet_rules.md` | yes when declared (ADR-0018) | system text, appended |
| fix task | `harness_prompts.fix_prompt` (equal to `pi.sh`'s) | no: code, pinned by tests | first user turn |
| repro task | `bugfix-fleet/agents/repro.md`'s body, then the issue | no: a file in the image | first user turn |
| review task | `Chain.review_prompt` | no: code | first user turn |
| verdict, advisor | `triage_run.sh`, `advisor_run.sh` and their agent definitions | no | their leaves |
| a resumed turn | the chain's follow-up message | no: code | the next user turn |

## The seams

Field names are the code's: `Request`, `Invocation`, `Turn`, `Episode`, `ModelRoute` in
`runtime/adapter.py`.

### S1 Prompt in

- **Contract.** `Request.prompt` is the caller's complete user-turn text, the station's template
  rendered with its slots (`station_run.render`: a slot the caller did not give, or gave and the text
  does not use, is an error before the first model call). The adapter delivers it verbatim, as the
  first user turn of a new session or the next one of a resumed session. It adds no task text. A
  difference it cannot avoid (the system text sent as a prefix because the CLI has no system-prompt
  flag, a harness's submit protocol) goes in `Invocation.notes`, which the episode keeps. Adapters
  send the prompt on stdin (`Invocation.stdin`): no 128 KiB argv limit, nothing in `ps`. The CLI
  must never wait on an inherited stdin.
- **Owner.** The caller owns the text; the adapter owns delivery.
- **pi, the platform's adapter.** The prompt on stdin: `pi -p` with no message argument reads it
  (`prompt_via_stdin`: yes).
- **pi in the bugfix chain.** One argv string: `pi.sh` and `repro.sh` pass `"$PROMPT"` with
  `< /dev/null`, `Chain.review` passes it as the last argument, bounded by `REVIEW_PROMPT_MAX`
  (100000 bytes, under Linux's 128 KiB). pi's review is started with the chain's own stdin, which
  `pi -p` reads when it is a held-open pipe: found 2026-11-18, when the review hung for minutes under
  a test runner and ended once stdin was closed. Not changed: those calls are pinned byte for byte by
  `bugfix/test_pi_golden.py`.
- **Proof.** Unit: `test_adapter.py` (the prompt is on stdin, not in argv), `test_station_run.py`
  (render, slot errors). End-to-end: `test_s1` (what the model received is the rendered text and only
  that; the first call's record names its hash and length).

### S2 System text

- **Contract.** `Request.system_path` is the system text to add, a file the caller has verified
  against its declared hash before any model call (ADR-0018 for the bugfix chain's `rules`), or
  `None`. The adapter delivers it as system text added to the harness's own system prompt; if the
  CLI cannot, as a prefix to the prompt, and says so in `Invocation.notes`. The workspace's own
  `AGENTS.md` or `CLAUDE.md` is never read: the version's texts replace it.
- **Owner.** The caller resolves and checks the file; the adapter delivers it.
- **pi, the platform's adapter.** `--no-context-files` by default (`Request.no_context_files`), and
  `--append-system-prompt <path>` when there is one, for every `tools` value. Whether a station
  without tools gets the text is the caller's choice (it passes none). A caller that must keep a
  call as it was sent before it moved onto the adapter sets `no_context_files=False`; the bugfix
  chain on stations (`bugfix/fix2.py`) does when its station declares no `rules`, as v1 did.
- **pi in the bugfix chain.** `--no-context-files --append-system-prompt <path>` (`--no-context-files`
  alone for the tool-less review: the rules are about using tools).
- **Proof.** Unit: `test_adapter.py`; the chain's goldens and `bugfix/test_declared_prompts.py`.
  End-to-end: `test_s2` (the marker of the system file is in the system text of every request, the
  marker of the workspace's `AGENTS.md` in none); the chain's: `chain_seam_e2e.py`'s `test_s2`,
  which also refuses a declared file that does not hash before any request.

### S3 Model route through the gateway

- **Contract.** `Request.model` is a `ModelRoute(alias, model, base_url, endpoint, key_file,
  key_env, meta)`, made from `FLOTILLA_MODELS` by `adapter.route`. The adapter points the CLI at
  `base_url` in the wire protocol the CLI speaks (chat completions; later `/v1/messages`,
  `/v1/responses`), asks for `model`, and sends the key as `adapter.read_key` returns it: in the
  child's environment, or in a mode-600 file named in `Invocation.files` under `run_dir`, or named
  where the CLI reads it at call time; **never in argv**, never in a file of the workspace. No other
  endpoint, no fallback model. The adapter takes whatever `FLOTILLA_MODELS` gives the routine.
- **Owner.** The caller picks the alias for the station; the adapter wires it.
- **pi, the platform's adapter.** Writes its own `<run_dir>/pi/models.json` through
  `Invocation.files` (a provider `flotilla-<endpoint>`, OpenAI completions, `apiKey` as
  `!cat <key_file>` or `$<key_env>`), sets `PI_CODING_AGENT_DIR`, and passes `--provider` and
  `--model`. pi reads the key at call time; it is written nowhere.
- **pi in the bugfix chain.** `pi_config.write` makes the same file, with pi's catalog's context
  window and prices for each model. The review is sent with the worker's provider (`self.provider`).
- **Proof.** Unit: `test_adapter.py` (route, key, the configuration names the key and holds none).
  End-to-end: `test_s3` (every request is `POST /v1/chat/completions` with `Authorization: Bearer
  <the key>` and the alias's model; the key is in no argv, not on stdin, and in no file of any run).

### S4 Stream to Episode

- **Contract.** `Harness.parse(stream_text, files) -> Episode`, never raising. `Episode(harness,
  station, session_id, turns, answer, error, exit_reason, usage_total, cost_est, raw_path, notes)`;
  `Turn(n, model, text, tools, usage, cost_est, stop, error, time, texts)` where a tool is `{name,
  arg, ok}` and, when the stream carries them, `args`, `result` and `duration_s` (`adapter.action`:
  masked and bounded). `answer` is the last non-empty assistant text, stripped. A stream that is
  empty, garbled or cut by a kill gives an Episode with `error` set and the whole turns it has.
  `exit_reason` is completed, turn_cap, timeout or error; the runner sets the last three from the
  exit code. `adapter.episode_record` serializes it to `runtime/episode.schema.json`, and every
  adapter's episode must validate (`fakeh.check_contract`). What a harness cannot supply is left
  out, never made up.
- **Owner.** The adapter parses; the caller reads only the Episode.
- **pi.** `harness_pi.parse` over pi's `--mode json` stream: the arguments and results of tool calls
  from its `toolResult` messages, a turn's time from its message timestamp. pi's stream has no
  duration for a tool call, so none is recorded.
- **Proof.** Unit: `test_adapter.py`, `test_episode_schema.py` (every pi sample and a real pi 1.0.0
  stream, the schema, the goldens). End-to-end: `test_s4` (the real stream becomes the turns, tools,
  usage and answer the script produced, and a record that validates).

### S5 Turn cap and timeout

- **Contract.** The runner counts `Harness.turn_ended(line)` on each stdout line as it arrives and,
  at `Request.max_turns`, kills the whole process group: exit 125, `exit_reason` turn_cap. The
  timeout does the same: exit 124, timeout. Neither leaves a process behind; both leave the stream
  (the tee). A harness's own cap is set one above the runner's. Declared: `supports_turn_cap`.
- **Owner.** The runner (`adapter.run_process`); the adapter says which line ends a turn.
- **pi.** A `message_end` line of an assistant message; no cap of pi's own.
- **Detached processes.** The group kill alone missed a process pi's bash tool starts and detaches
  (`nohup sleep 300 &`, found 2026-11-19 by this end-to-end: parent 1, a group and session of its
  own). `adapter.run_process` now tags every process it starts (`FLOTILLA_RUN_TAG` in the
  environment) and, after the group kill, kills every process that carries the tag or descends from
  the run, repeating while any is left. The tag is read from `/proc/<pid>/environ`, which a detached
  or reparented descendant keeps; a process that clears its environment escapes it (a child
  subreaper would catch that, and is not used). **Linux only:** where there is no `/proc` (macOS)
  only the group is killed, a detached process outlives the run, and the tests skip with that
  reason; nothing is claimed there.
- **Proof.** Unit: `test_adapter.py` (`Runner`, `Execute`), the chain's `test_fix.py` cases.
  End-to-end: `test_s5` (a grinding station is stopped at its cap long before the timeout, a held
  request is stopped by the timeout, nothing of the harness is left in either, and `turn_ended`
  counts the model calls of the real stream), and the detached-process test.

### S6 Tools and read-only

- **Contract.** `Request.tools` is `all`, `read` or `none`; a harness declares the values it serves
  (`tools_served`) and the runner refuses any other before it starts one. A harness that has switches
  for it turns off writing and the shell for a read station, and declares `enforces_readonly`. One
  that does not is asked in the prompt, and the runner compares the workspace with itself after the
  station (`adapter.workspace_digest`, every file but `.git` and the run's folder): a change is a
  fatal stop, `adapter.ReadOnlyViolation`, with the paths and a message that says the harness cannot
  enforce it. The station driver writes the episode, its record and its calls lines before it
  raises, so the stop is on record. A harness that enforces it is not digested: its switches are
  its word.
- **Owner.** The caller states the need; the adapter turns it into the CLI's switches; the runner
  checks the rest.
- **pi.** Enforced: `all` leaves pi's own four tools, `read` is `--tools read,grep,find,ls`,
  `none` is `--no-tools`. The chain's verdict and advisor leaves pass `PI_TOOLS`.
- **Proof.** Unit: `test_adapter.py` (each value's switches, the digest, the check with a harness
  that cannot enforce it, not digesting one that can), the chain's goldens. End-to-end: `test_s6`
  (the tools a request offers: none for `none`, find, grep, ls and read for `read`, a shell and
  writes for `all`), and in `test_seam_fakeh.py` a harness that ignores the switch is stopped.

### S7 Session and resume

- **Contract.** `Harness.can_resume` and `Harness.resume_after_kill` are declared. After an episode
  the caller keeps `episodes/<n>-<station>.json` with `resume_ref` = `Harness.resume_ref(ep)`
  (normally the session id); a later call gets it as `Request.resume`, and `build` makes the resume
  invocation, or raises (never a fresh session in silence). A session whose run was killed resumes
  from the reference the caller kept (the session id in the harness's own store, which the caller
  checkpoints with the run directory) when the adapter declares `resume_after_kill`.
- **Owner.** The caller owns the reference, the checkpoint and the follow-up text; the adapter owns
  the resume invocation. The bugfix chain's checkpoint (ADR-0019) stays the chain's: it names the
  harness of each role and starts fresh when its flags differ.
- **pi, the platform's adapter.** `--session <file>`: the file under `<run_dir>/pi/sessions` that
  the reference names (a session id, or a path inside the run directory; any other path is refused),
  with the line a kill cut trimmed first. A reference with no saved session raises (`HarnessError`).
- **pi in the bugfix chain.** `PI_RESUME_SESSION=<session file>` to `pi.sh`, which adds
  `--session <file>`; `trim_session` drops the line a kill cut; a run killed in the middle of a fix
  continues it with `INTERRUPTED`.
- **Proof.** Unit: `test_adapter.py` (the resume invocation, the trim, the refusals),
  `bugfix/test_episodes.py` (the checkpoint, the episode file as a session). End-to-end: `test_s7` (a
  follow-up arrives in the same conversation with its earlier turns; a station killed in the middle
  and resumed by the session it left goes on with its tool results from before the kill), and the
  chain's own kill and resume in `chain_seam_e2e.py`.

### S8 Calls recording

- **Contract.** Each model call is one line of the run's calls file (`FLOTILLA_CALLS_FILE`), in the
  in-process client's format (`at, alias, endpoint, model, deployment, tokens_in, tokens_out,
  cost_usd, cost_source, latency_s, status, error, finish_reason, step, decision_key, role, episode,
  episode_seq, tools`) plus `harness` and `station` (and `role` empty), and `texts` (name, hash,
  length) on the call that read the rendered texts. A tool in a line is `{name, arg, ok}`: the calls
  file is not the record. `station_run.calls_lines` writes them; a tool's argument is masked.
- **Owner.** The caller writes the lines from the Episode; the adapter supplies turns.
- **pi in the bugfix chain.** `callrec_pi.session_lines` over pi's session files, one line per
  assistant message, `role` coder, reviewer, verdict or advisor, no `harness` field (absent means pi).
- **Proof.** Unit: `test_station_run.py` (the line, byte for byte), `bugfix/test_callrec_pi.py`,
  `bugfix/test_episodes.py`. End-to-end: `test_s8` (one line per request, the scripted tokens, an
  episode number per session, the tools with their `ok`, the texts).

### S9 Cost

- **Contract.** The gateway's bill is authoritative and the cost's source is always set: the driver
  takes an optional `meter` (a callable that returns the gateway's spend so far) and the episode's
  cost is the difference over the run, `source: gateway`. A harness's own figure is kept as
  `Turn.cost_est` and `Episode.cost_est`, an estimate, and is the record's cost only when there is
  no meter, labelled `harness-estimate`; with neither, `unknown`. Declared: `reports_cost`. The
  bugfix chain's meter (`spend.Meter`, LiteLLM's `/key/info`) and its sharing of a key's spend over
  the episodes stay the chain's (`bugfix/spend.py`).
- **Owner.** The caller and `spend.py`; the driver takes the meter.
- **Proof.** Unit: `test_station_run.py`, `bugfix/test_spend.py`, `bugfix/test_episodes.py`.
  End-to-end: `test_s9` (every record's cost is the gateway's, and their sum is the endpoint's);
  the chain's `chain_seam_e2e.py` for the spend file.

### S10 Phone-home off

- **Contract.** Nothing reaches a host but the gateway. `Harness.phone_home_env()` and the CLI's own
  configuration switch off update checks, telemetry, model-catalog fetches and plugin downloads; the
  egress proxy carries no vendor host. `harness_probe.py` (run by the installation) checks each
  against the proxy's log.
- **Owner.** The adapter.
- **pi, the platform's adapter.** `PI_OFFLINE=1` and `PI_SKIP_VERSION_CHECK=1`. **pi in the bugfix
  chain:** the worker image sets them (`worker-bugfix.Dockerfile`), so its `phone_home_env()` is
  empty; pi uses Node's environment proxy (`NODE_USE_ENV_PROXY=1`).
- **Proof.** End-to-end: `test_s10` (a proxy standing where the network is saw nothing in any of
  the runs) and its control (the same station pointed at a host that is not on `NO_PROXY` does reach
  the proxy, so silence means something).

### S11 Workdir, HOME and the session store

- **Contract.** The CLI runs with `Request.workdir` as its cwd and HOME = `run_dir` (the driver
  sets it, whatever the caller's environment had). Its sessions, config and caches land under
  `run_dir`, where a checkpoint can keep them and the run's end deletes them. Nothing is written to
  the worker's own HOME.
- **Owner.** The driver sets HOME; the adapter puts its store under `run_dir`.
- **pi.** `PI_CODING_AGENT_DIR=<run_dir>/pi`: `models.json`, `auth.json` and
  `sessions/<cwd>/*.jsonl` there.
- **Proof.** End-to-end: `test_s11` (the cwd and HOME of every invocation, the session store under
  the run directory, the worker's HOME left empty).

## Capabilities

Each harness declares each capability, `yes` or `no`, as attributes of its adapter
(`adapter.CAPABILITIES` names and explains them). A spec that asks for one the harness does not
declare is rejected before any model call: `adapter.missing(h, wants)`, and flotillad's `-check`
against `fleets/harnesses.json` (a step with hooks on pi is refused, for one), the same table as data,
generated by `python3 runtime/adapter.py capabilities --write`. `test_adapter.py` keeps the rows below
and that file equal to the adapters. Adding a harness adds its row.

<!-- capabilities:begin -->
| harness | tools_served | can_resume | resume_after_kill | enforces_readonly | supports_hooks | reports_cost | reports_usage | supports_turn_cap | native_system_prompt | prompt_via_stdin |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| pi | all, read, none | yes | yes | yes | no | yes | yes | yes | yes | yes |
<!-- capabilities:end -->

## The minimum seam end-to-end

A harness is adopted when this passes for it, with its real CLI binary. `runtime/seam_e2e.py` is
the shared part; `runtime/test_seam_<name>.py` is the harness's run of it (`test_seam_pi.py` is the
reference). The suite is run in CI over the fake harness that speaks to a model
(`test_seam_fakeh.py`), because CI has no pi, so every seam is exercised there and the
harness-specific parts on a machine that has the CLI.

- **The CLI** is the real binary on PATH, started through the station driver. Without the binary
  the test is skipped, with the reason.
- **The workspace** is a small directory: `src/a.js` returns 1, and an `AGENTS.md` it must never
  read.
- **The model** is `ScriptedEndpoint`, a stdlib HTTP server on loopback that speaks the protocol the
  harness uses (chat completions today; `/v1/messages` and `/v1/responses` are added by the adoption
  of the first harness that needs them) and answers from a script: a tool call that writes the
  file, then a final text. It records the path, the `Authorization` header, the model, the system
  text, the conversation and the tools offered, and its spend grows with each call (the driver's
  meter).
- **The network** is a `RecordingProxy` as the egress proxy, with the gateway on `NO_PROXY`: any
  request that reaches it is a host that is not the gateway.
- **The runs**, through `station_run.run_station` with names the suite chooses: (1) four stations,
  one for each `tools` value, one of them resumed by its session; (2) a station that never stops,
  with a turn cap, whose first tool call starts a background process; (3) a station whose request is
  never answered, with a timeout; (4) a station run as a process, killed with SIGTERM to its group
  while its second request is in flight, then resumed by the session it left (for a harness that
  declares `resume_after_kill`); (5) a station pointed at a host that is not the gateway (the
  control for S10).
- **The checks** are `SeamSuite`: one test per seam, S1 to S11. A harness's subclass sets `NAME`,
  the tool names (`READ_TOOLS`, `WRITE_TOOLS`), `SESSIONS`, `session_ref_after_kill()` and, for a
  fake, `unavailable()` and `prepare()`; it adds the protocol to the endpoint if it is a new one.

Not covered by it: that a real model behaves; the verdict and advisor roles of the bugfix chain
of v1 (their leaves run pi as before); any harness version other than the one on the machine that
runs it
(pi: 1.0.0 here, 0.80.10 in the worker image; the bugfix chain's two end-to-ends were also run on
0.80.10 once, below); a harness that is not pi; CI, which has no pi.

## Adopting a harness: the order

1. Add its row to the capability table (regenerate `fleets/harnesses.json`), and the seams that
   change here, in the same commit as the code.
2. The adapter and its unit tests (`test_adapter_<name>.py`, samples in `runtime/testdata/`,
   synthetic ones marked until a real run is recorded), then `test_seam_<name>.py` until every seam
   passes.
3. Only then the image, the installation's probe, and the stations that name it.

## Not done, and not verified

- Detached processes on a system without `/proc` (S5).
- Declaring and hashing the bugfix chain's texts other than `rules` (above).
- The verdict and advisor roles through adapters, in the bugfix chain of v1: `triage_run.sh` and
  `advisor_run.sh` run pi as before, as do its fix and repro leaves and its review
  (`bugfix/pi_review.py`). `bugfix/fix2.py` (a version on stations) serves all five through
  adapters.
- Every harness but pi: no adapter, no row, no end-to-end.
- `/v1/messages` and `/v1/responses` in the scripted endpoint.
- pi 0.80.10, the version in the worker image: the end-to-ends ran on 1.0.0. They were also run
  once on 0.80.10 (2026-11-21, node 22, a throwaway npm prefix): `bugfix/test_seam_fix2.py` (14),
  `bugfix/test_seam_pi.py` (13) and `runtime/test_seam_pi.py` (14), all green; and again on 2026-11-22
  inside the bugfix worker image itself (sha-dc3315d: node v22.23.4, pi 0.80.10, Python 3.11.2; no
  network, the tree mounted read-only), the same three, all green. That `PI_OFFLINE` is
  what keeps pi off the network was not shown on either version: with and without it pi made no
  request outside the gateway in `-p` mode.

## Amendment log

- 2026-11-18: created with pi as the reference; S1-S11, the capability table, and the seam
  end-to-end, in `bugfix/`.
- 2026-11-19: the layer is the platform's (build step 4), moved to `runtime/`. No roles: a station
  is a free label and an adapter serves a `tools` value (`tools_served`); `Request` replaces
  `RoleRequest` (`system_path`, no `readonly`). S1 and S3: pi's generic adapter (prompt on stdin, its
  own models.json). S4: the episode record and its schema; a tool's arguments and result. S5:
  the sweep of detached processes (same day, after the suite found them). S6: the runner's check
  of the workspace is built. S7: resume by the reference,
  after a kill. S8: `station` and `texts` in a calls line. S9: the driver's meter. The seam
  end-to-end runs through the station driver, with the chain out of the loop and a fake harness in
  CI.
