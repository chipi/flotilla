# RFC-0008 — The MCP server: other agents see and steer flotilla

**Status:** Accepted → [ADR-0015](../adr/ADR-0015-mcp-server.md) (operator, 2026-11-15): Q1 the official
SDK, Q2 HTTP on the UI node, Q3 and Q4 yes, Q5 build the dry run now. Step 2 of the
[vision plan](../wip/vision-plan.md).
**Builds on:** [RFC-0001](RFC-0001-fleet-platform.md) §6 (interfaces: "read tools, action tools, it
cannot approve, over the tailnet, an identity per caller") and §8; [ADR-0001](../adr/ADR-0001-platform-foundations.md)
(one API under the CLI, the UI and MCP); [security](../security.md) T3 (an agent approves), T11
(exposure).

## Motivation

flotilla has a CLI for the operator on the mini and a UI for people. Other agents (Claude sessions on
the X3 and the laptop, the X3 assistant, later others) have no way in: today a Claude session reads
flotilla by SSH and `docker exec flotilla …`, with the container's own token, as one shared identity.
RFC-0001 promised an MCP server from the start; it is not built.

## Proposal

### 1. Tools: the API's own, nothing more

Every tool calls an existing API function with the caller's identity, so the API's authorisation
(`internal/api/authorize.go`) decides; MCP adds no rule of its own and bypasses none.

| Tool | API | An agent may today |
| --- | --- | --- |
| `status` | routines, workers, freeze | yes (`read`) |
| `runs_list`, `run_show` | runs, with model calls and what a run read | yes |
| `cases_list`, `case_show` | cases and their timelines | yes |
| `approvals_list` | plans waiting for a person (read only) | yes |
| `hosts_list` | hosts, their agents, executions | yes |
| `models_summary` | aliases, last checks, spend | yes |
| `brief_show` | the day's brief | yes |
| `routine_run` | start a run now | yes (`run`) |
| `routine_pause`, `fleet_stop`, `freeze` | tighten | yes (`pause`, `stop`, `freeze`) |
| `hold` | hold a host or target | yes (`hold`) |
| `ledger_query` | the ledger's decisions | **no** (`ledger-read` is people only): Q3 |
| `note_propose` | propose a note to Knowledge, from a case | **no** (`feedback` is people only): Q4 |

**Never a tool:** approve, reject, resume, start, unfreeze, unhold, autonomy, users, hosts. Loosening
and approving stay with people (T3). A test asserts the tool list holds none of them, and that each
action tool fails for an agent the API refuses.

### 2. Identity: one token per caller

Each caller gets its own agent token in `callers.yaml` (role `agent`), minted and stored like the other
credentials (mode 600 on the caller's side, never in git). Every change an agent makes is recorded with
that caller's name and `via=mcp`, plus a session label the client sends, shown as the caller's own
claim, never trusted. Revoking one caller's token cuts that caller only.

### 3. Transport: Streamable HTTP on the UI's tailnet node

`POST /mcp` on flotillad's UI listener, reached at `https://flotilla.<tailnet>.ts.net/mcp` through
the caddy-tailscale node that already serves the UI (TLS there; the tailnet ACL decides who reaches
it). Bearer token per caller. No new port, no new node. Claude Code and the X3 assistant both speak
it (`claude mcp add --transport http flotilla https://…/mcp --header "Authorization: Bearer …"`, with
the header read from a file by a wrapper, never typed).

Read-only, idempotent tools first; action tools in the same release, behind the API's checks.

### 4. Audit and limits

- Each tool call is an event (`mcp.<tool>`, caller, outcome), so History and the API's events show it.
- A rate limit per caller (60 calls a minute) so a looping agent cannot flood the store.
- Results are bounded (runs and cases paged, 50 at most per call).

### 5. Tests

- The protocol: initialize, `tools/list`, `tools/call`, errors, against a recorded exchange and against
  Claude Code itself in the e2e (`claude mcp` is not in the e2e image; the e2e drives the HTTP
  exchange with the same JSON a client sends).
- Authorisation: every action tool with an agent token that the API refuses, and the never-tools
  absent.
- Done when (vision plan): a Claude session lists open cases and runs a routine through it, and a
  refused approve is tested.

## Open questions (the operator's)

1. **Library or our own code.** The official Go SDK (`github.com/modelcontextprotocol/go-sdk` v1.8.0)
   brings JWT, OAuth2, a JSON-schema library, `segmentio/encoding` and `golang.org/x/tools`, most for
   client and OAuth features we would not use. A server needs four JSON-RPC methods (`initialize`,
   `notifications/initialized`, `tools/list`, `tools/call`) and `ping`. **Proposed:** our own small
   server (stdlib), tested against the spec's examples and a real client, as with the host envelope
   (ADR-0006 decided 3). The alternative is the SDK, approved as a dependency (ADR-0003).
2. **Transport.** **Proposed:** HTTP on the UI node (§3). The alternative, stdio through
   `ssh mini docker exec -i flotillad flotilla mcp`, opens nothing new, but every caller then shares
   the container's token unless each passes its own, and it needs SSH to the mini.
3. **Agents reading the ledger.** Today `ledger-read` is people only. **Proposed:** allow it for agents
   (read only): the ledger is how an agent learns what triage decided and why.
4. **Agents proposing notes.** Today `feedback` (verdicts and notes) is people only. **Proposed:**
   agents may propose a note (never a verdict, never activate one): a person still activates it in
   Knowledge, so nothing an agent proposes reaches another agent without a person.
5. **A dry-run tool.** RFC-0001 lists "dry-run a job"; the API has no dry run. **Proposed:** leave it
   out of this release.

## Alternatives considered

- **No MCP, the CLI over SSH:** works today for a session with SSH to the mini, not for the X3 assistant,
  and shares one identity.
- **The API itself to agents (REST with a token):** possible, but each client would need its own tool
  definitions; MCP gives every MCP client the same tools with no glue.
- **An MCP gateway in front of the API (another process):** one more thing to run and secure, for no
  rule it adds.

## Discussion

- 2026-11-15: drafted after the vision plan's step 1 (Slack deferred to later in the day).
