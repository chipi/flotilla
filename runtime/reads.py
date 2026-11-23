"""runtime/reads.py: a station's live reads through the MCP servers its spec declares (RFC-0014 section 4.8, `reads:` in a station).

  names = reads.tools_for(station_doc)                    # ["wiki.get_page", ...]: add them to the station's tool names
  tool = reads.make_tool("answer", station_doc, fallback_tool)    # tool(name, args) -> text; never raises

A name `<server>.<tool>` of a declared server goes to the MCP client only when the tool is on that server's declared list; any other name
goes to `fallback_tool` (the fleet's own tools). A server's address and token file are host config, not the spec's: MCP_<SERVER>_URL and
MCP_<SERVER>_TOKEN_FILE (the server name upper-cased, - as _). What is refused is not a live read: no connection, no count, no record.

Every live read appends one JSON line to FLOTILLA_READS_FILE (mode 600):
  {"station", "tool": "<server>.<tool>", "args", "result", "ok", "chars", "cut", "at", "duration_s"}
`result` is what the model was given: credentials masked (toolrec.redact_credentials, once, before both), then cut at the declared
max_result_chars with a visible `...[cut at N of M chars]`; `chars` is M, the size before the cut. station/tool/args/result/ok are the
envelope's recorded_tool_results fields, so a replay (runtime/recorded.py) serves the line as it is. A read whose line cannot be written
fails: no read is served unrecorded. An error is `<read failed: ...>` with ok false, recorded the same way. In a shadow run
(FLOTILLA_SHADOW=1) without FLOTILLA_RECORDED_TOOLS no connection is opened: a shadow reads the live run's record, never the servers.
The calls per server are counted across stations, for the run, against the declaring entry's max_calls. Each read is noted in the run's
loaded file as untrusted. Stdlib only.
"""
import hashlib
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import loaded  # noqa: E402
import mcp_client  # noqa: E402
import toolrec  # noqa: E402

BODY_SLACK = 1 << 20     # bytes read beyond the result cap: JSON framing and encoding

_used = {}               # server -> live reads so far in this process (one run)
_clients = {}            # (url, token file, timeout) -> McpClient: one session per server


def reset():
    """Start a new run's bookkeeping (a test; a process is one run)."""
    _used.clear()
    _clients.clear()


def _reads(station_doc):
    return [r for r in (station_doc or {}).get("reads") or [] if isinstance(r, dict) and r.get("server")]


def tools_for(station_doc):
    """The read tool names a station may offer the model: `<server>.<tool>` for each declared tool."""
    return [f"{r['server']}.{t}" for r in _reads(station_doc) for t in r.get("tools") or []]


def _env_name(server, what):
    return "MCP_" + server.upper().replace("-", "_") + "_" + what


def _now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def make_tool(station, station_doc, fallback_tool, env=os.environ):
    """tool(name, args) -> text for the station's harness. It never raises."""
    by_server = {r["server"]: r for r in _reads(station_doc)}

    def tool(name, args):
        server, _, bare = str(name).partition(".")
        r = by_server.get(server) if bare else None
        if r is None:
            return fallback_tool(name, args)
        try:
            return _read(station, r, server, bare, name, args, env)
        except Exception as e:  # nothing here may fail a run's loop
            return f"<read failed: {toolrec.redact_credentials(type(e).__name__ + ': ' + str(e))}>"
    return tool


def _read(station, r, server, bare, name, args, env):
    if bare not in (r.get("tools") or []):
        return f"<read refused: {name} is not a tool this station declares for {server}>"
    if not isinstance(args, dict):
        return f"<read refused: {name} takes an object of arguments>"
    if env.get("FLOTILLA_SHADOW") == "1" and not env.get("FLOTILLA_RECORDED_TOOLS"):
        return "<read refused: a shadow run without the live run's record opens no connection>"
    cap_calls = int(r.get("max_calls") or 10)
    if _used.get(server, 0) >= cap_calls:
        return f"<read refused: {server} is at its {cap_calls} reads for this run>"
    url, token_file = env.get(_env_name(server, "URL")), env.get(_env_name(server, "TOKEN_FILE")) or None
    if not url:
        return f"<read refused: {_env_name(server, 'URL')} is not set on this worker>"
    path = env.get("FLOTILLA_READS_FILE")
    if not path:
        return "<read refused: FLOTILLA_READS_FILE is not set, so the read could not be recorded>"
    try:
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    except OSError as e:
        return f"<read refused: the reads file cannot be opened ({type(e).__name__})>"
    try:
        cap = int(r.get("max_result_chars") or 65536)
        timeout = float(r.get("timeout_s") or 15)
        _used[server] = _used.get(server, 0) + 1
        t0 = time.monotonic()
        ok = True
        try:
            client = _clients.get((url, token_file, timeout))
            if client is None:
                client = _clients[(url, token_file, timeout)] = mcp_client.McpClient(url, token_file, timeout, cap + BODY_SLACK)
            res = client.call_tool(bare, args)
            body = mcp_client.text(res)
            ok = not res.get("isError")
            if not ok:
                body = f"<read failed: {body}>"
        except mcp_client.McpError as e:
            ok, body = False, f"<read failed: {e}>"
        body = toolrec.redact_credentials(body)
        chars = len(body)
        cut = chars > cap
        if cut:
            body = body[:cap] + f"…[cut at {cap} of {chars} chars]"
        line = {"station": station, "tool": name, "args": args, "result": body, "ok": ok, "chars": chars, "cut": cut, "at": _now(),
                "duration_s": round(time.monotonic() - t0, 3)}
        try:
            os.write(fd, (json.dumps(line, ensure_ascii=False) + "\n").encode())
        except OSError as e:
            return f"<read failed: the read could not be recorded ({type(e).__name__})>"
        loaded.record("probe", "untrusted", name, f"mcp:{server}", hashlib.sha256(body.encode()).hexdigest(), environ=env)
        return body
    finally:
        os.close(fd)
