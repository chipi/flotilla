"""runtime/mcp_client.py: an MCP client over the streamable HTTP transport, on urllib and json (RFC-0014 section 4.8, reads only).

  c = McpClient(url, token_file="/run/secrets/mcp-wiki", timeout=15, max_body=65536 + (1 << 20))
  c.list_tools()                              # [{"name", ...}]
  c.call_tool("get_page", {"id": "7"})        # the result object: {"content": [...], "isError": false}
  text(result)                                # its text blocks joined (else structuredContent as JSON)

Each call is one POST of one JSON-RPC message. The first call starts the session: `initialize` (protocol 2025-06-18; a server may answer
2025-06-18 or 2025-03-26), then `notifications/initialized` (answered 202). The `Mcp-Session-Id` the server gives is sent back, with
`MCP-Protocol-Version`, on every later request. A 404 on a request that carries a session means the server lost it (n8n keeps sessions in
memory): the client starts a new session once and repeats the request. An answer is `application/json`, or `text/event-stream` read
line by line (`data:` lines up to a blank line are one event; other fields and every message that is not the answer to this request are
ignored) until the matching id arrives. The bearer token is read from a file at each request: it is in no argument, environment
variable or message here. The proxy comes from the environment as runtime/model_client.py's does. Not covered: the legacy HTTP+SSE
transport, resources, prompts and requests the server makes. Every failure is an McpError whose text is safe to show. Stdlib only.
"""
import json
import socket
import time
import urllib.error
import urllib.request

PROTOCOL = "2025-06-18"
ACCEPTED = (PROTOCOL, "2025-03-26")
ACCEPT = "application/json, text/event-stream"
CLIENT_INFO = {"name": "flotilla", "version": "1"}
MAX_PAGES = 10          # tools/list pages followed


class McpError(Exception):
    """A request the server did not answer with a result: transport, HTTP status, protocol or a JSON-RPC error."""


class McpClient:
    def __init__(self, url, token_file=None, timeout=15.0, max_body=1 << 20):
        if not isinstance(url, str) or not url.startswith(("http://", "https://")):
            raise McpError("the server address is not an http(s) URL")
        self.url, self.token_file, self.timeout, self.max_body = url, token_file, timeout, max_body
        self.session, self.version, self._id = None, None, 0

    def list_tools(self):
        out, cursor = [], None
        for _ in range(MAX_PAGES):
            res = self.request("tools/list", {"cursor": cursor} if cursor else {})
            out += [t for t in res.get("tools") or [] if isinstance(t, dict)]
            cursor = res.get("nextCursor")
            if not cursor:
                break
        return out

    def call_tool(self, name, args):
        return self.request("tools/call", {"name": name, "arguments": args or {}})

    def request(self, method, params):
        """The `result` of one JSON-RPC request, starting the session first and once more when the server drops it."""
        if self.session is None and self.version is None:
            self._initialize()
        try:
            return self._rpc(method, params)
        except _SessionLost:
            self._reset()
            self._initialize()
            try:
                return self._rpc(method, params)
            except _SessionLost:
                raise McpError("the server dropped the session again (404)") from None

    def _reset(self):
        self.session, self.version = None, None

    def _initialize(self):
        try:
            res = self._rpc("initialize", {"protocolVersion": PROTOCOL, "capabilities": {}, "clientInfo": CLIENT_INFO}, starting=True)
            if res.get("protocolVersion") not in ACCEPTED:
                raise McpError(f"the server speaks MCP {res.get('protocolVersion')!r}, not {' or '.join(ACCEPTED)}")
            self.version = res["protocolVersion"]
            status, _, _ = self._post({"jsonrpc": "2.0", "method": "notifications/initialized"}, expect_body=False)
            if status != 202:
                raise McpError(f"notifications/initialized answered {status}, not 202")
        except _SessionLost:
            self._reset()
            raise McpError("the server answered 404 to initialize") from None
        except McpError:
            self._reset()  # a half-started session is not one: the next request starts again
            raise

    def _rpc(self, method, params, starting=False):
        self._id += 1
        rid = self._id
        status, ctype, resp = self._post({"jsonrpc": "2.0", "id": rid, "method": method, "params": params}, rid=rid, starting=starting)
        if resp.get("error") is not None:
            e = resp["error"]
            if isinstance(e, dict):
                raise McpError(f"{method}: JSON-RPC error {e.get('code')}: {e.get('message')}")
            raise McpError(f"{method}: JSON-RPC error: {e}")
        if not isinstance(resp.get("result"), dict):
            raise McpError(f"{method}: the answer has no result")
        return resp["result"]

    def _headers(self):
        h = {"Content-Type": "application/json", "Accept": ACCEPT}
        if self.version:
            h["MCP-Protocol-Version"] = self.version
        if self.session:
            h["Mcp-Session-Id"] = self.session
        if self.token_file:
            try:
                with open(self.token_file) as f:
                    token = f.read().strip()
            except OSError as e:
                raise McpError(f"the token file is unreadable ({type(e).__name__})") from None
            if not token:
                raise McpError("the token file is empty")
            h["Authorization"] = "Bearer " + token
        return h

    def _post(self, message, rid=None, starting=False, expect_body=True):
        """POST one message. (status, content type, the answer to `rid` as a dict) - the dict is {} when no answer is expected."""
        req = urllib.request.Request(self.url, data=json.dumps(message).encode(), method="POST", headers=self._headers())
        deadline = time.monotonic() + self.timeout
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as r:
                sid = r.headers.get("Mcp-Session-Id")
                if starting and sid:
                    self.session = sid
                ctype = (r.headers.get("Content-Type") or "").split(";")[0].strip().lower()
                if not expect_body:
                    r.read(self.max_body + 1)
                    return r.status, ctype, {}
                if ctype == "text/event-stream":
                    return r.status, ctype, self._sse(r, rid, deadline)
                body = r.read(self.max_body + 1)
                if len(body) > self.max_body:
                    raise McpError(f"the answer is over {self.max_body} bytes")
                return r.status, ctype, _match(_loads(body), rid)
        except urllib.error.HTTPError as e:
            e.close()
            if e.code == 404 and self.session and not starting:
                raise _SessionLost() from None
            if e.code in (401, 403):
                raise McpError(f"unauthorised ({e.code})") from None
            raise McpError(f"HTTP {e.code}") from None
        except (socket.timeout, TimeoutError):
            raise McpError(f"no answer in {self.timeout:g}s") from None
        except urllib.error.URLError as e:
            if isinstance(e.reason, (socket.timeout, TimeoutError)):
                raise McpError(f"no answer in {self.timeout:g}s") from None
            raise McpError(f"cannot reach the server ({type(e.reason).__name__})") from None
        except OSError as e:
            raise McpError(f"connection failed ({type(e).__name__})") from None

    def _sse(self, r, rid, deadline):
        read, data = 0, []
        while True:
            if time.monotonic() > deadline:
                raise McpError(f"no answer in {self.timeout:g}s")
            line = r.readline(self.max_body + 1)
            read += len(line)
            if read > self.max_body:
                raise McpError(f"the answer is over {self.max_body} bytes")
            if not line:
                break                                   # end of stream: a last event may lack its blank line
            line = line.decode("utf-8", "replace").rstrip("\r\n")
            if line == "":
                got = self._event(data, rid)
                data = []
                if got is not None:
                    return got
            elif line.startswith("data:"):
                data.append(line[5:][1:] if line[5:6] == " " else line[5:])
        got = self._event(data, rid)
        if got is None:
            raise McpError("the stream ended without an answer")
        return got

    @staticmethod
    def _event(data, rid):
        if not data:
            return None
        try:
            msg = json.loads("\n".join(data))
        except ValueError:
            return None
        return _find(msg, rid)


class _SessionLost(Exception):
    pass


def _loads(body):
    try:
        return json.loads(body.decode("utf-8", "replace"))
    except ValueError:
        raise McpError("the answer is not JSON") from None


def _find(msg, rid):
    """The response with id `rid` in a message (or a batch), else None; notifications and other ids are not it."""
    for m in msg if isinstance(msg, list) else [msg]:
        if isinstance(m, dict) and m.get("id") == rid and ("result" in m or "error" in m):
            return m
    return None


def _match(msg, rid):
    got = _find(msg, rid)
    if got is None:
        raise McpError("the answer does not answer the request")
    return got


def text(result):
    """A tools/call result as text: its text blocks joined by a newline, else structuredContent as JSON, else the blocks as JSON."""
    blocks = [b.get("text") for b in result.get("content") or [] if isinstance(b, dict) and b.get("type") == "text" and isinstance(b.get("text"), str)]
    if blocks:
        return "\n".join(blocks)
    if result.get("structuredContent") is not None:
        return json.dumps(result["structuredContent"], ensure_ascii=False, sort_keys=True)
    return json.dumps(result.get("content") or [], ensure_ascii=False)
