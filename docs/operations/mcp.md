# MCP for other agents

Other agents reach flotilla through MCP at `https://flotilla.tail6d0ed4.ts.net/mcp`
([ADR-0015](../adr/ADR-0015-mcp-server.md)): the API's own read and tightening tools, under each
caller's own token. Nothing there approves, rejects or loosens.

## Tools

| Read | Act (as the caller, under the API's checks) |
| --- | --- |
| `status`, `runs_list`, `run_show`, `cases_list`, `case_show`, `approvals_list`, `hosts_list`, `models_calls`, `brief_show`, `ledger_query` | `routine_run`, `routine_dry_run`, `routine_pause`, `fleet_stop`, `freeze`, `hold`, `note_propose` |

`fleet_stop` also cancels the fleet's runs in flight (an agent may; cancelling one run is an
operator's `flotilla cancel`, not a tool).

Each call is an event under the caller's name (`mcp.<tool>`, `via=mcp`); a caller may make 60 calls
a minute.

## Add a caller

1. **The token, on the caller's host, never printed:**

   ```bash
   install -d -m 700 ~/.config/flotilla
   ( umask 077; printf 'flt_%s' "$(openssl rand -hex 32)" > ~/.config/flotilla/mcp-token )
   sha256sum < ~/.config/flotilla/mcp-token | cut -d' ' -f1     # the hash, for step 2
   ```

2. **The caller in flotillad's `callers.yaml`** on the mini, role `agent`, one entry per caller:

   ```yaml
   - {name: claude-x3, role: agent, token_sha256: <the hash>}
   ```

   flotillad reads the file at start: deploy (`deploy.sh` with the running tag) to load it.
3. **The client.** Claude Code runs a headers helper on each connection
   ([Claude Code MCP docs](https://code.claude.com/docs/en/mcp.md), `headersHelper`), so the token stays
   in its file and never on a command line or in `~/.claude.json`:

   ```bash
   cat > ~/.config/flotilla/mcp-headers.sh <<'EOF'
   #!/usr/bin/env bash
   jq -n --rawfile t "$HOME/.config/flotilla/mcp-token" '{Authorization: ("Bearer " + $t)}'
   EOF
   chmod 700 ~/.config/flotilla/mcp-headers.sh
   claude mcp add-json --scope user flotilla \
     '{"type":"http","url":"https://flotilla.tail6d0ed4.ts.net/mcp","headersHelper":"'"$HOME"'/.config/flotilla/mcp-headers.sh"}'
   ```

## Verify

- `claude mcp get flotilla` shows it connected; in a session, `/mcp` lists 17 tools.
- `routine_dry_run` on `platform/heartbeat` answers `would_start: true`.
- The UI's History shows the call as an `mcp.*` event under the caller's name.

## Remove a caller

Delete its line in `callers.yaml` and deploy; its token stops working at once.

## Not covered

- Rotating a token is removing and adding the caller again.
- The session label RFC-0008 §2 mentioned is not recorded: a stateless request carries no session, so
  an event names the caller only.
- A call refused by the API comes back as a tool error with the API's reason; nothing else is logged.
