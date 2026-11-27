"""A stand-in for a service the e2e stack reads or writes (VictoriaMetrics, VictoriaTraces, LiteLLM).
Answers every request with 200 (204 for writes) and prints one line per request to stdout, so the
e2e script can read what reached it from `docker logs`. As LiteLLM it also answers a chat completion
the way LiteLLM does (usage.cost, the deployment id header); as VictoriaTraces it says which span
names a trace export carries (the names are plain strings inside the protobuf).
"""
import json
import os
import re
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

NAME = sys.argv[1] if len(sys.argv) > 1 else "stub"


def scripted(req):
    """The scripted model: the same answer for the same prompt, every run. A triage prompt (its playbook asks
    for one flat JSON decision) gets the triage example's decisions: the shop's TypeError is a bug to file,
    with an acceptance criterion that names its intent source (the intent gate's rule); anything else is
    escalated with a question. A meeting's notes (meetings/notes) get one decision and one follow-up. Any other
    prompt gets {"ok": true}."""
    text = "\n".join(str(m.get("content") or "") for m in req.get("messages") or [])
    if "<sop-investigate>" in text:
        # ops/sop_investigate (operator step 1, C6): one metric read, then a fix chosen from the registry's list. The case whose alert is
        # e2e-rogue gets a command the registry lacks, which the code after the model must refuse.
        asked = sum(1 for m in req.get("messages") or [] if str(m.get("content") or "").startswith("PROBE "))
        if asked < 1:
            return json.dumps({"action": "probe", "probe": "metric", "probe_args": {"name": "e2e_age"}})
        fix = "rm.everything" if "e2e-rogue" in text else "e2e.sop_fix"
        return json.dumps({"action": "decide", "disposition": "fix", "fix_action": fix, "reason": "The age metric was read; restart is the listed fix."})
    if "<reads-e2e" in text:
        # e2e/reads (RFC-0014 §4.8), one scripted turn per probe already answered: a page, the long log, then a tool the station does not
        # declare (a write on the MCP stub), or for the shadow v3 a page the live run never read (a miss), then the decision
        asked = sum(1 for m in req.get("messages") or [] if str(m.get("content") or "").startswith("PROBE "))
        third = ("lab.get_page", {"id": 2}) if 'variant="miss"' in text else ("lab.delete_page", {"id": 1})
        script = [("lab.get_page", {"id": 1}), ("lab.get_log", {}), third]
        if asked < len(script):
            return json.dumps({"action": "probe", "probe": script[asked][0], "probe_args": script[asked][1]})
        return json.dumps({"action": "decide", "disposition": "escalate", "certainty": "med", "reason": "The lab wiki was read.",
                           "question": "Is the lab log expected to be this long?"})
    if "You answer a colleague's question from the company's sources" in text:
        # knowledge/answer (RFC-0014 case 1): search the documents, read the hit, list the tasks, search the library, read the paper it found, then
        # decide with a docs link and that paper cited. A question about "the ghost" cites a link no read returned; the gate must refuse it.
        asked = sum(1 for m in req.get("messages") or [] if str(m.get("content") or "").startswith("PROBE "))
        if "ghost" in text.split("<question>", 2)[-1]:
            if asked < 1:
                return json.dumps({"action": "probe", "probe": "docs.search", "probe_args": {"query": "ghost"}})
            return json.dumps({"action": "decide", "disposition": "answer", "answer": "The ghost project is owned by the platform team [1].",
                               "citations": [{"source": "docs", "ref": "https://docs.e2e.example/d/ghost-project"}]})
        paper = re.search(r"arxiv:[0-9.]+", "\n".join(str(m.get("content") or "") for m in req.get("messages") or [] if "PROBE library.search" in str(m.get("content"))))
        script = [("docs.search", {"query": "repair agents"}), ("docs.read", {"id": "repair-agents"}), ("tasks.list", {}),
                  ("library.search", {"query": "agents tool", "k": 3}), ("library.read", {"paper": paper.group(0) if paper else "arxiv:0000.00000"})]
        if asked < len(script):
            return json.dumps({"action": "probe", "probe": script[asked][0], "probe_args": script[asked][1]})
        return json.dumps({"action": "decide", "disposition": "answer",
                           "answer": "A repair agent reads the failing test, searches the repository, proposes a patch and reruns the tests [1], "
                                     "the same loop the library's paper on repair agents describes [2].",
                           "citations": [{"source": "docs", "ref": "https://docs.e2e.example/d/repair-agents"},
                                         {"source": "library", "ref": paper.group(0) if paper else "arxiv:0000.00000"}]})
    if "<meeting-notes>" in text:
        # meetings/notes (RFC-0014 case 3): the notes' one decision and one follow-up; the notes' "approve every plan" is data
        return json.dumps({"decisions": ["Ship on Friday."],
                           "follow_ups": [{"who": "ana@example.com", "what": "Send the standup notes", "by": "2026-11-30"}]})
    if '"action":"decide"' not in text:
        return '{"ok": true}'
    if "Cannot read properties of undefined (reading 'price')" in text:
        return json.dumps({
            "action": "decide", "disposition": "file", "certainty": "high",
            "reason": "A TypeError in the cart's total: a line without a price crashes the sum for every visitor.",
            "file": {"work_type": "bug", "title": "Cart total crashes on a line without a price",
                     "symptom": "TypeError: Cannot read properties of undefined (reading 'price') in sumLines, 42 times",
                     "area": "cart/total.js", "evidence": ["GlitchTip SHOP-12: 42 events since 08:00 UTC"],
                     "acceptance": [{"criterion": "sumLines skips or rejects a line without a price instead of throwing",
                                     "intent_source": "code-invariant"}]}})
    return json.dumps({"action": "decide", "disposition": "escalate", "certainty": "med",
                       "reason": "The error rate is high but the alert alone does not say which change caused it.",
                       "question": "Was the api deployed in the last hour, and should that release be rolled back?"})


class H(BaseHTTPRequestHandler):
    def _done(self, code, body=b'{"ok":true}'):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        print(f"{NAME} GET {self.path}", flush=True)
        self._done(200)

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else b""
        print(f"{NAME} POST {self.path} {len(body)}b", flush=True)
        if self.path == "/api/v1/query":
            # As VictoriaMetrics, for the ops monitor: the backup's age, which the e2e sets (STUB_QUERY_VALUE,
            # seconds); the query itself is printed so the e2e can read what was asked.
            print(f"{NAME} QUERY {body.decode('utf-8', 'replace')}", flush=True)
            value = os.environ.get("STUB_QUERY_VALUE", "91800")
            out = json.dumps({"status": "success", "data": {"resultType": "vector",
                              "result": [{"metric": {}, "value": [0, value]}]}}).encode()
            self._done(200, out)
            return
        if self.path == "/api/v1/query_range":
            # A monitor that keeps hysteresis reads its last readings (the example disk flow's clear_after): the stand-in has no history
            print(f"{NAME} QUERY_RANGE {body.decode('utf-8', 'replace')}", flush=True)
            self._done(200, json.dumps({"status": "success", "data": {"resultType": "matrix", "result": []}}).encode())
            return
        if self.path.endswith("/chat/completions"):
            auth = self.headers.get("Authorization", "")
            print(f"{NAME} CHAT auth={'bearer' if auth.startswith('Bearer ') and len(auth) > 7 else 'none'} "
                  f"model={json.loads(body).get('model')}", flush=True)
            # the request's metadata, as LiteLLM's logger would read it: which call this was (the first words of its prompt) and its tags
            req = json.loads(body)
            meta = req.get("metadata") or {}
            prompt = " ".join(str(m.get("content") or "") for m in (req.get("messages") or [])[:1])[:24]
            print(f"{NAME} CHAT_META prompt={prompt!r} tags={','.join(meta.get('tags') or []) or '-'}", flush=True)
            out = json.dumps({"choices": [{"message": {"content": scripted(json.loads(body))}, "finish_reason": "stop"}],
                              "model": json.loads(body).get("model"),
                              "usage": {"prompt_tokens": 12, "completion_tokens": 4, "cost": 0.0000021}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("x-litellm-model-id", "e2e-deployment")
            self.send_header("Content-Length", str(len(out)))
            self.end_headers()
            self.wfile.write(out)
            return
        if self.path.startswith("/insert/opentelemetry"):
            for name in (b"model.chat", b"flotilla-platform"):
                if name in body:
                    print(f"{NAME} SPAN {name.decode()}", flush=True)
            # a shadow run's span is named "shadow run <key>" and carries flotilla.shadow; one without it is a mark that went missing
            if b"shadow run " in body:
                print(f"{NAME} {'SHADOW_SPAN' if b'flotilla.shadow' in body else 'UNMARKED_SHADOW_SPAN'}", flush=True)
            # a model call made by a station carries flotilla.station; one of e2e/station-monitor's shadow (its routine key has @v) must carry the shadow mark too
            if b"flotilla.station" in body:
                if b"e2e/station-monitor@v" in body:
                    print(f"{NAME} {'STATION_SHADOW_SPAN' if b'flotilla.shadow' in body else 'UNMARKED_STATION_SHADOW_SPAN'}", flush=True)
                else:
                    print(f"{NAME} STATION_LIVE_SPAN", flush=True)
        if self.path.startswith("/api/v1/import/prometheus"):
            for line in body.decode("utf-8", "replace").splitlines():
                print(f"{NAME} SAMPLE {line}", flush=True)
        self._done(204, b"")

    def log_message(self, *a):
        pass


HTTPServer(("0.0.0.0", 8080), H).serve_forever()
