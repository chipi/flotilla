"""The UI listener end to end (ADR-0014), run inside the worker container (it shares flotillad's network).

  docker exec -i flotilla-worker-triage python3 - ADMIN_TOKEN OPERATOR_TOKEN AGENT_TOKEN < deploy/e2e/ui_check.py

Prints one PASS or FAIL line per check; exits 1 on any FAIL.
"""
import http.client
import json
import sys
import urllib.parse

HOST, PORT, NAME = "flotillad", 8486, "flotilla.e2e"
admin, operator, agent = sys.argv[1], sys.argv[2], sys.argv[3]
bad = 0


def req(method, path, host=NAME, headers=None, body=None):
    c = http.client.HTTPConnection(HOST, PORT, timeout=10)
    h = {"Host": host}
    h.update(headers or {})
    c.request(method, path, body=body, headers=h)
    r = c.getresponse()
    data = r.read().decode("utf-8", "replace")
    return r.status, r.getheader, data


def check(cond, what):
    global bad
    print(("PASS " if cond else "FAIL ") + what)
    bad += 0 if cond else 1


code, hdr, _ = req("GET", "/signin")
check(code == 200 and "frame-ancestors 'none'" in (hdr("Content-Security-Policy") or ""), f"the sign-in page, with its CSP ({code})")
code, _, _ = req("GET", "/signin", host="evil.example")
check(code == 421, f"another Host is refused ({code})")
code, _, _ = req("GET", "/v1/status", headers={"Authorization": "Bearer " + operator})
check(code == 403, f"the callers-file token is refused on the UI listener ({code})")
code, _, _ = req("GET", "/v1/ledger/counts?ns=x", headers={"Authorization": "Bearer " + admin})
check(code == 404, f"the ledger is not on the UI listener ({code})")
code, _, _ = req("GET", "/v1/status", headers={"Authorization": "Bearer " + admin})
check(code == 200, f"a person's token reaches /v1 ({code})")
form = urllib.parse.urlencode({"token": admin})
code, hdr, _ = req("POST", "/signin", headers={"Content-Type": "application/x-www-form-urlencoded"}, body=form)
cookie = (hdr("Set-Cookie") or "").split(";")[0]
flags = hdr("Set-Cookie") or ""
check(code == 303 and cookie.startswith("__Host-flotilla=") and "HttpOnly" in flags and "Secure" in flags
      and "SameSite=Strict" in flags, f"sign-in sets the session cookie ({code})")
code, _, page = req("GET", "/", headers={"Cookie": cookie})
check(code == 200 and "ada" in page, f"the home page with the session ({code})")
code, _, _ = req("GET", "/")
check(code == 303, f"without a session, the home page sends you to sign in ({code})")

# MCP (RFC-0008): /mcp on the UI listener, a caller's bearer token only.
def mcp(body, tok=agent, extra=None):
    h = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if tok:
        h["Authorization"] = "Bearer " + tok
    h.update(extra or {})
    code, _, data = req("POST", "/mcp", headers=h, body=json.dumps(body))
    try:
        return code, json.loads(data)
    except ValueError:
        return code, data


code, _ = mcp({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, tok="")
check(code == 401, f"MCP without a token is refused ({code})")
code, _ = mcp({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, extra={"Cookie": cookie})
check(code == 400, f"MCP refuses the UI's session cookie ({code})")
code, out = mcp({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18",
                 "capabilities": {}, "clientInfo": {"name": "e2e", "version": "1"}}})
check(code == 200 and isinstance(out, dict) and out.get("result", {}).get("serverInfo", {}).get("name") == "flotilla",
      f"MCP initialize as the agent ({code})")
code, out = mcp({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
names = [t["name"] for t in (out.get("result", {}).get("tools", []) if isinstance(out, dict) else [])]
check(code == 200 and "status" in names and "routine_dry_run" in names and not any("approve" in n for n in names),
      f"MCP lists its tools, no approve ({len(names)} tools)")
code, out = mcp({"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "routine_dry_run",
                 "arguments": {"routine": "platform/heartbeat"}}})
res = out.get("result", {}) if isinstance(out, dict) else {}
check(code == 200 and not res.get("isError") and (res.get("structuredContent") or {}).get("routine") == "platform/heartbeat",
      f"MCP dry-runs a routine as the agent ({code})")
sys.exit(1 if bad else 0)
