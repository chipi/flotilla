"""The UI listener end to end (ADR-0014), run inside the worker container (it shares flotillad's network).

  docker exec -i flotilla-worker-triage python3 - ADMIN_TOKEN OPERATOR_TOKEN AGENT_TOKEN < deploy/e2e/ui_check.py

Prints one PASS or FAIL line per check; exits 1 on any FAIL.
"""
import http.client
import json
import re
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
check(not any(w in n for n in names for w in ("accept", "reject", "answer")), "MCP offers no tool that answers a proposal")

# C6b: a fleet's own knowledge. The e2e fleet declares its SOPs in fleet.yaml: the Knowledge page has their tab, the item and its
# fields, and (when the sops section ran, giving the case id) the proposal that waits, which Accept closes as the signed-in person.
proposal = sys.argv[4] if len(sys.argv) > 4 else ""
ck = {"Cookie": cookie}
code, _, page = req("GET", "/knowledge", headers=ck)
check(code == 200 and 'href="/knowledge?tab=e2e.sops"' in page and "Standard operating procedures" in page, f"the Knowledge page has the e2e fleet's tab ({code})")
code, _, tab = req("GET", "/knowledge?tab=e2e.sops", headers=ck)
check(code == 200 and "e2e-known" in tab and ">applies_to<" in tab and ">rung<" in tab and "e2e_sop" in tab,
      f"the tab lists the fleet's SOP with its declared fields ({code})")
code, _, item = req("GET", "/knowledge/e2e/sops/e2e-known", headers=ck)
check(code == 200 and "fixture, never shipped" in item, f"the item page shows its text ({code})")
if proposal:
    check("Waiting for you" in tab and proposal in tab and "e2e/sops/e2e-novel2/SKILL.md" in tab and f'action="/cases/{proposal}/accept"' in tab,
          "the sop_proposal waits on the tab with its diff, Accept and Reject")
    m = re.search(r'name="csrf" value="([0-9a-f]{64})"', tab)
    code, hdr, _ = req("POST", f"/cases/{proposal}/accept", headers={**ck, "Content-Type": "application/x-www-form-urlencoded"},
                       body=urllib.parse.urlencode({"csrf": "wrong", "back": "/knowledge?tab=e2e.sops"}))
    check(code == 403, f"Accept without the session's CSRF token is refused ({code})")
    code, hdr, _ = req("POST", f"/cases/{proposal}/accept", headers={**ck, "Content-Type": "application/x-www-form-urlencoded"},
                       body=urllib.parse.urlencode({"csrf": m.group(1) if m else "", "back": "/knowledge?tab=e2e.sops"}))
    loc = hdr("Location") or ""
    check(code == 303 and "err=" not in loc, f"Accept answers the proposal ({code} {loc[:80]})")
    code, _, tab = req("GET", "/knowledge?tab=e2e.sops", headers=ck)
    check("No proposal waits" in tab, "nothing waits on the tab after Accept")
sys.exit(1 if bad else 0)
