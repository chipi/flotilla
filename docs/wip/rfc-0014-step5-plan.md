# RFC-0014 step 5: live reads, the build plan (2026-11-23)

RFC-0014 §4.8 and its §7 e2e. A routine reads the company's tools while it works through MCP servers
it declares; reads only, every write stays `plumbing.call`. This note is the order of work and the
decisions; RFC-0014 §4.8 is amended where the code differs (below).

## Decided (the operator, 2026-11-23)

- **Over the per-call cap**: cut visibly, `…[cut at N of M chars]`, the same cut text to the model
  and the record, the full size kept (`chars`), so a replay is exact. The operator did not answer
  this one; the recommendation stands until they say otherwise.
- **Redaction**: credentials only (`toolrec.redact_credentials`), applied once, before the model and
  the record, so what the model saw is what is recorded. E-mail addresses and IPs stay.
- **Budget**: a per-run cap only (`max_calls`, a timeout per call); each read shows on the run page
  with its time. No daily read cap yet.
- **A shadow asking for a read the live run never made**: the shadow run fails, recorded as a miss
  (like with like).
- **CI** (2026-11-23): `reads` stays out of `no-effect.yml`'s sections. `e2e.yml` runs the full e2e,
  `reads` included, on every push to main, and no-effect would only repeat it. Its value there is a
  mutation that must go red (a shadow reading live, not from the record), added with case 1.

## Where the RFC and the code differ

- The record cannot be stored with the frozen input at start: the input is written at dispatch and
  the reads happen during the run. The record is a second write after the run, joined to the input
  by its key (the live run id) and merged into `recorded_tool_results` when the input is read.
- Shadows are queued at the live run's start; a shadow of a routine with reads waits for its live
  run to end, whatever `shadows.after_live` says, and is not run when the live run's reads are not
  complete.
- "Calls and their time count against the budget like model calls": the budget is in dollars and a
  read costs nothing; the per-run cap and the run's time limit are the bound.

## Order of work

1. **Spec** (done): `stations.<name>.reads: [{server, tools: [...], max_calls, max_result_chars,
   timeout}]` on `harness: flotilla` stations with `tools: all` (the RFC says `tools: read`, but the
   flotilla harness serves only all and none, `fleets/harnesses.json`, and a `read` value would be a
   new harness seam); a read tool's name is `<server>.<tool>`; validated in `internal/spec`, carried
   to the runtime in `FLOTILLA_STATIONS`. The call cap counts per server across the run's stations.
2. **Runtime** (done): `runtime/mcp_client.py` (streamable HTTP on the standard library: initialize,
   notifications/initialized, tools/list, tools/call; JSON and SSE answers; the session id; a
   bearer token read from a file at call time) and `runtime/reads.py` (the allowlist, the caps,
   redaction, the cut, the record line per call in `FLOTILLA_READS_FILE`, fail-closed in a shadow
   without a record). Plugs into the harness's `tool` function; the replay path
   (`FLOTILLA_RECORDED_TOOLS`) is unchanged.
3. **Worker and store** (done): The worker sets `FLOTILLA_READS_FILE` (a file beside the calls file,
   `.flotilla-reads-<run>.jsonl`) and returns its lines as `reads` with `reads_complete` (false for
   an unreadable file, a line that is not a JSON object, or more than `READS_MAX` 200 lines); no key
   for a run with none. flotillad writes them after the run to `input_reads(run_id, lines,
   complete)` (`Store.RecordInputReads`), `Store.Input` joins them (`Input.Reads`, `ReadsComplete`),
   and `input.FromStore` merges them into `recorded_tool_results` (per station, tool/args/result/ok,
   in the order read). The harness takes `reads=` and `station=` (`run_json_step`); only the triage
   `decide` station's entry point passes them, and only when the station declares `reads:` (the
   recorded-tools key stays "" for one that does not). Deviation: the complete flag is not in the
   envelope (the schema allows no new field); step 4 reads it from the store.
4. **Shadow** (done): a version whose stations declare `reads:` (`sched.DeclaresReads`) waits for
   its live run's end, whatever `shadows.after_live` says (`shadowTask.reads`; the wait is the
   `liveDone` channel, closed after the record is written; a queue behind it waits too, as under
   `after_live`), then runs from the record or is not run (`liveReadsRecord`). The record goes to
   the shadow worker as `worker.Request.RecordedTools` (`{"<station>": [{tool, args, result,
   ok}]}`, the envelope's `recorded_tool_results`): the worker writes it mode 600 to
   `.flotilla-recorded-<run>.json` in its temp directory, names it in `FLOTILLA_RECORDED_TOOLS`,
   and removes it after the run; a live run with `recorded_tools` is refused. A shadow worker
   refuses a run whose built environment holds `MCP_*_URL` or `MCP_*_TOKEN_FILE`
   (`check_no_mcp_endpoint`), and flotillad never sets them (tested on the live and the shadow
   request).
   - **What is chosen** (the operator's decision: a read the record lacks fails the shadow,
     recorded as a miss):
     - the live run ended ok with reads, complete: the shadow runs from them;
     - ended ok, read nothing: the shadow runs with `{}`; any read it asks for is a miss. For this
       to be told from a record that was not kept, `RunWithin` now writes an `input_reads` row
       (empty, complete) for every live run that returned no error and whose version declares
       reads, not only for one with reads or an incomplete one;
     - a record with `complete = false`, a live run that did not end ok (any status but ok, or not
       found), no row (the write failed, or the live version declares no reads while the shadow's
       does), or lines a replay cannot serve: not run, with the reason in the shadow row ("the
       live run's reads are not complete", "the live run did not end ok (error): ...", "the live
       run's reads were not recorded ...");
     - a store error reading the record or the run: not run, "the live run's reads could not be
       read", logged.
   - **A miss fails the run end to end.** `runtime/recorded.py` raises `RecordedMiss` from the
     tool call, and `harness.run_json_step` does not catch it. The triage cycle did:
     `orchestrator.run_poll` isolates a source pass that crashes and exits 0 below two failures,
     which turned a miss into a passing run. `run_poll` now re-raises `RecordedMiss` from both
     passes. Without that, a shadow that asked for an unrecorded read exited 0 (the new
     `TestAShadowsRecordedMissFailsTheCycle`).
   - **The station key** is the one `reads.py` writes (`station` = `decide` for triage, passed
     only with reads) and the replay looks up (`recorded.key(station, tool, args)`);
     `ShadowReplay` in `runtime/test_reads.py` replays a live run's own lines through the loop,
     and a different argument misses.
5. **Compose and the e2e** (done, 2026-11-23): `worker-reads` behind its own proxy (`egress-reads`),
   the shadow's worker without it; a stub MCP server in `deploy/e2e` (a result over 8192 characters,
   an undeclared tool, the server down for a replay, a shadow served the record); the section
   `reads`. Built as the "Step 5 as built" section below says.
6. **Docs**: RFC-0014 §4.8 amended (the three points above), `fleets/README.md`.

## Needs the operator

- n8n's MCP Server Trigger (which transport it speaks), its read-only workflows and credentials,
  separate from the `plumbing.call` flows'.
- The new worker, its proxy ACL with the real host, the token file, and `flotillad.yaml`'s `mcp:`.
  Concretely, on the host (`deploy/compose/README.md`, "The reads worker"):
  - n8n's MCP Server Trigger URL for each read server, in `config/worker-reads.env` as
    `ROUTINE_MCP_<SERVER>_URL=<url>` (`<SERVER>` the `server:` name upper-cased, `-` as `_`), with
    `ROUTINE_MCP_<SERVER>_TOKEN_FILE=/run/flotilla/mcp-<server>.token`;
  - the bearer token, alone in `config/mcp/<server>.token` (mode 600); the worker's own
    `config/worker-reads.token`; and `config/reads-model.env` (`KEY=value`: the read-only model
    key the endpoint's `key_env` names; the shadow worker needs the same key in its read
    environment, because the shadow of a station with reads calls the model too);
  - the proxy host: `config/egress-reads-acl.yaml` (`allowed_domains`: the n8n host and the model
    gateway's) and `config/smokescreen-reads.yaml` (`allow_ranges`: the n8n network's range and the
    gateway's only); `NET_MCP` in `.env` if n8n's Docker network is not named `mcp`;
  - `COMPOSE_PROFILES` gains `reads`; `flotillad.yaml` gains the worker (`reads: {url:
    http://worker-reads:8081, token_file: /run/flotilla/worker-reads.token}`) and a model alias for
    the station, and the routine says `worker: reads` (and a `shadow_worker`);
  - n8n's side (above): the read-only workflows and credentials behind the trigger, and the check of
    which transport it speaks (the client speaks streamable HTTP only).

## Deviations (step 4)

- **The harness's recorded wrap now covers a reads station's reads, and the fleet's own tools only
  where the record has them.** The wrap sat over every tool of the station, so a shadow of the
  triage `decide` station (reads plus `probe`) would have missed on its first probe: a live run's
  record holds reads only, and a shadow runs the fleet's own tools live on its read credentials.
  For a station with reads, a name the record has nothing for (`Replayer.knows`) goes to the
  fleet's tool; a read, or a tool the record does hold, is answered from the record. A station
  without reads is as before (every tool recorded). Only a shadow does this: an offline run
  (`offline_run.py` sets `FLOTILLA_OFFLINE=1`, which its contract cannot unset) answers every tool
  from its recordings, a miss failing it, as before.
- A shadow of a version with reads whose live version declares none is not run (no row), rather
  than run and missing every read.

## Step 5 as built (2026-11-23)

- **Compose** (`deploy/compose/docker-compose.yml`, profile `reads`, off by default):
  `worker-reads` (the triage image, which carries `runtime/`; `FLOTILLA_WORKER: reads`; its own
  secrets volume `worker-reads-secrets`; env_file `worker-reads.env` holding
  `ROUTINE_MCP_<SERVER>_URL` and `..._TOKEN_FILE`; `FLOTILLA_ENV_FILES` its model key) and
  `egress-reads` (the shared smokescreen image, its own ACL and settings files, on the `flotilla`
  network, the new external `mcp` network (`NET_MCP`) and the gateway's). flotillad gains
  `depends_on` (`required: false`) and `NO_PROXY` for the worker. `secrets.sh` fills the volume
  when `worker-reads.token` exists; `deploy.sh` hashes the new files so a changed ACL or token
  recreates the service. `cmd/flotillad/compose_test.go` checks that only `worker-reads` has
  `worker-reads.env` or its volume, only `egress-reads` joins the `mcp` network, and no service
  sets an `MCP_*` variable in the compose file.
- **The e2e** (`deploy/e2e/run.sh`, section `reads`, after `stations`): `deploy/e2e/mcp_stub.py`
  (stdlib streamable HTTP, SSE answers, bearer token from a file, a session id, tools `get_page`,
  `get_log` with 12000 characters, and `delete_page` as a write nothing declares; one JSON log line
  per request), `deploy/e2e/fleets/e2e/reads/` (v1 live on the reads worker, v2 a shadow with the
  same reads, v3 a shadow asking for a page the live run never read), the scripted model's turns in
  `deploy/e2e/stub.py`, `deploy/e2e/reads_check.py` (the store and stub-log checks). Assertions:
  1. the reads worker holds `MCP_LAB_*`; the shadow worker, the live triage worker and ops-read
     hold no `MCP_*`; the worker is on the flotilla network alone, and the `mcp` network holds
     only the stub and `egress-reads`; the reads proxy refuses a host off its list (407);
  2. a live run ended ok and its `input_reads` row is complete, with the two reads in order and
     `get_log` whole (12000 characters, `chars` equal, not cut, not hashed at 8192); `flotilla input
     show` carries them as `recorded_tool_results.answer`;
  3. the stub saw `get_page` and `get_log` only, every request authorised, every request after
     `initialize` with its session id; `lab.delete_page` (undeclared) was refused before any call and
     the stub never saw it; the reads proxy logged the stub's host;
  4. shadow v2 ran on the shadow worker and ended ok with the same three `PROBE` lines (lengths,
     hashes) as the live run and no `input_reads` row; the stub's log is unchanged after both
     shadows; the shadow worker cannot resolve the stub at all;
  5. shadow v3 failed as a miss (`RecordedMiss ... {"id":2}`, run status error);
  6. with the stub stopped, `flotillad -stations` and `runtime/offline_run.py` run the live run's
     exported input and print the same three `PROBE` lines; v3 offline fails as a miss, with no read
     attempted; the stub's log is still the live run's two calls after the replays.
- **Run:** `E2E_SECTIONS=reads deploy/e2e/run.sh` (the build, the config and `up` always run; the section
  needs nothing of the others). A full local run (2026-11-23): 421 PASS, 2 FAIL, both the browser
  check "the cases page lists the chain's case"; the tree without this change fails the same two
  (401 PASS) on this machine, and the check passes in `E2E_SECTIONS=cases,ui`: cause not found.

### Deviations (step 5)

- **The proxy's ACL admits the model gateway as well as the MCP hosts.** The station's model call
  goes through the worker's only proxy, so an ACL of the MCP hosts alone would stop the run. The
  worker has the gateway through `egress-reads` and nothing else; no write host, no GitHub, no
  GlitchTip, no Langfuse.
- **The worker uses the triage image,** not the ops image: the harness, the model client and the MCP
  client are in `runtime/`, which only the triage image carries.
- **The model key is the endpoint's `key_env`,** read from `model.env` in the worker's volume
  (`FLOTILLA_ENV_FILES`), the same shape as the shadow worker's read environment; a key file
  (`key_file`) would leave the shadow worker, which also calls the model, without the key.
- **`reads` is not in the `noeffect` group** of `E2E_SECTIONS` (CI's no-effect workflow runs that group
  only; the full `make test-integration` run includes `reads`). Adding it is the operator's call:
  it adds a section and a worker's start to that workflow's five builds.
- **`free_routine` moved** out of the `shadows` section to the helpers above `model`, unchanged, so
  `E2E_SECTIONS=reads` can use it; `reads` appends `shadows: {enabled: true}` itself when the `shadows`
  section did not.
- **Not mutation-tested:** the section's assertions were seen to fail once for real (a first run in
  which the stub could not find its token file recorded `unauthorised (401)` results and the record
  check refused them); none of the mutation checks of `no-effect.yml` was extended to a shadow
  holding an MCP setting (the worker refuses it, unit-tested in `worker/test_worker.py`).
- **Telemetry:** the e2e routine does not call `telemetry.setup()`; its reads are not traced.

## Not covered

- The legacy HTTP+SSE MCP transport, MCP resources and prompts, server-initiated requests.
- A coding harness's own tools (pi): reads are for `harness: flotilla` stations only.
- A routine with reads on a remote worker: shadows do not run on remote workers yet.
