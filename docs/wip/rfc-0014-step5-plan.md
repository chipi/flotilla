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
3. **Worker and store**: the worker returns the reads file as `reads`; flotillad stores them
   (`input_reads`, with a complete flag); the input's export and an offline run carry them.
4. **Shadow**: a read the record lacks fails the shadow run (the replay path in `runtime/recorded.py`
   must not fall through to `reads.py`, which only refuses without the record); waits for the live
   run; `notRun` without a complete record; the record goes to the
   shadow worker as a file; a shadow worker never holds a read server's address or token.
5. **Compose and the e2e**: `worker-reads` behind its own proxy (`egress-reads`), the shadow's
   worker without it; a stub MCP server in `deploy/e2e` (a result over 8192 characters, an
   undeclared tool, the server down for a replay, a shadow served the record); the section `reads`.
6. **Docs**: RFC-0014 §4.8 amended (the three points above), `fleets/README.md`.

## Needs the operator

- n8n's MCP Server Trigger (which transport it speaks), its read-only workflows and credentials,
  separate from the `plumbing.call` flows'.
- The new worker, its proxy ACL with the real host, the token file, and `flotillad.yaml`'s `mcp:`.

## Not covered

- The legacy HTTP+SSE MCP transport, MCP resources and prompts, server-initiated requests.
- A coding harness's own tools (pi): reads are for `harness: flotilla` stations only.
- A routine with reads on a remote worker: shadows do not run on remote workers yet.
