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
5. **Compose and the e2e**: `worker-reads` behind its own proxy (`egress-reads`), the shadow's
   worker without it; a stub MCP server in `deploy/e2e` (a result over 8192 characters, an
   undeclared tool, the server down for a replay, a shadow served the record); the section `reads`.
6. **Docs**: RFC-0014 §4.8 amended (the three points above), `fleets/README.md`.

## Needs the operator

- n8n's MCP Server Trigger (which transport it speaks), its read-only workflows and credentials,
  separate from the `plumbing.call` flows'.
- The new worker, its proxy ACL with the real host, the token file, and `flotillad.yaml`'s `mcp:`.

## Deviations (step 4)

- **The harness's recorded wrap now covers a reads station's reads, and the fleet's own tools only
  where the record has them.** The wrap sat over every tool of the station, so a shadow of the
  triage `decide` station (reads plus `probe`) would have missed on its first probe: a live run's
  record holds reads only, and a shadow runs the fleet's own tools live on its read credentials.
  For a station with reads, a name the record has nothing for (`Replayer.knows`) goes to the
  fleet's tool; a read, or a tool the record does hold, is answered from the record. A station
  without reads is as before (every tool recorded). Not covered: an offline run (`offline_run.py`)
  of a station with reads whose scenario lacks a probe now reaches the live probe (behind its
  refusing proxy) instead of a miss.
- A shadow of a version with reads whose live version declares none is not run (no row), rather
  than run and missing every read.

## Not covered

- The legacy HTTP+SSE MCP transport, MCP resources and prompts, server-initiated requests.
- A coding harness's own tools (pi): reads are for `harness: flotilla` stations only.
- A routine with reads on a remote worker: shadows do not run on remote workers yet.
