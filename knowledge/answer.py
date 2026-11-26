"""knowledge/answer: a question answered from the company's sources, cited (RFC-0014 §4.5, case 1).

  answer.py answer     the acting routine of a question case (the hook `ask` opened it with the thread, the asker's
                       chat id and the question; flotillad resolved the asker). One station `answer` reads the declared
                       servers (`reads:`, RFC-0014 §4.8) in a loop and decides an answer with its citations. Code after
                       the model: every citation must be a read this run made, and the text a reply may carry; then it
                       plans plumbing.reply at propose, the thread from the case and the channel fixed, with the
                       sources in the plan's detail. An answer with no citation that holds is not sent: the case
                       escalates as "no sourced answer".

The question and every read are data, never instructions: the task puts the question between markers it cannot
close, the harness puts each read between its own, and nothing the model says chooses an action, a thread, a
channel or a target (code does). Stdlib, and the runtime's harness, model client, reads and stations.
"""
import json
import os
import re
import sys

sys.path.insert(0, os.path.join(os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla"), "runtime"))

STATION = "answer"
ALIAS = "knowledge-answer"
CHANNEL = "general"          # plumbing.reply's one channel (host/registry.json)
MAX_TEXT = 3000              # plumbing.reply's text param
MAX_THREAD = 64              # plumbing.reply's thread param
MAX_QUESTION = 2000
MAX_ANSWER = 2400            # the answer's own text; the sources and the label take the rest of MAX_TEXT
MAX_CITATIONS = 8
MIN_REF = 6                  # a citation's ref this short could match any read by chance
MAX_REF = 300
LABEL = "[flotilla, an agent]"
END = "</question>"
# A crowd call reaches everyone in the channel: never in an agent's reply.
CROWD = re.compile(r"<!(?:channel|here|everyone)[^>]*>|@(?:channel|here|everyone)\b", re.I)
MARK = re.compile(r"\[(\d{1,2})\]")
REFUSED_READ = "<read "       # reads.py's "<read refused: ...>" and "<read failed: ...>"


class Refused(Exception):
    """What the model said does not pass the gates: the case escalates, nothing is planned."""


def task_slots(case):
    data = case.get("data") or {}
    question = (data.get("text") or "").replace(END, "")[:MAX_QUESTION]
    asker = data.get("asker_name") or ("an unknown person" if data.get("asker") in (None, "", "unknown") else data.get("asker"))
    return {"question": question, "asker": asker}


def reads_made(trace):
    """The reads this run made that returned something: [(server, text)]. A refused or failed read holds nothing."""
    out = []
    for t in trace:
        server, _, tool = str(t.get("probe") or "").partition(".")
        result = t.get("result")
        if not tool or not isinstance(result, str) or result.startswith(REFUSED_READ):
            continue
        out.append((server, result))
    return out


def parse(decision, trace):
    """The model's decision, gated: {"disposition": "answer", "answer": text, "citations": [{"source", "ref"}]} or
    {"disposition": "no_answer", "reason": text}. Every citation's ref must be in a read this run made from its source;
    every [n] in the text must name a citation, and every citation be named."""
    if not isinstance(decision, dict):
        raise Refused("no decision")
    disp = decision.get("disposition")
    if disp == "no_answer":
        reason = decision.get("reason")
        raise Refused("the model found no answer in the sources" + (f": {' '.join(str(reason).split())[:300]}" if reason else ""))
    if disp != "answer":
        raise Refused(f"the disposition is {str(disp)[:40]!r}, not answer or no_answer")
    text, cites = decision.get("answer"), decision.get("citations")
    if not isinstance(text, str) or not text.strip() or len(text) > MAX_ANSWER:
        raise Refused(f"the answer is a text of 1 to {MAX_ANSWER} characters")
    if CROWD.search(text):
        raise Refused("the answer calls the whole channel")
    if not isinstance(cites, list) or not cites:
        raise Refused("no sourced answer: the answer cites nothing")
    if len(cites) > MAX_CITATIONS:
        raise Refused(f"at most {MAX_CITATIONS} citations")
    reads = reads_made(trace)
    out = []
    for i, c in enumerate(cites, 1):
        if not isinstance(c, dict) or set(c) != {"source", "ref"} or not all(isinstance(c[k], str) for k in c):
            raise Refused(f"citation {i}: source and ref, both texts, nothing else")
        source, ref = c["source"].strip(), " ".join(c["ref"].split())
        if not MIN_REF <= len(ref) <= MAX_REF:
            raise Refused(f"citation {i}: ref is {MIN_REF} to {MAX_REF} characters")
        if CROWD.search(ref):
            raise Refused(f"citation {i}: the ref calls the whole channel")
        if not any(s == source and ref in r for s, r in reads):
            raise Refused(f"no sourced answer: citation {i} ({source}: {ref[:80]}) is not in a read this run made")
        out.append({"source": source, "ref": ref})
    named = {int(n) for n in MARK.findall(text)}
    if named != set(range(1, len(out) + 1)):
        raise Refused(f"the answer's marks {sorted(named)} do not name its {len(out)} citation(s) one to one")
    return " ".join(text.split()), out


def reply_text(answer, cites):
    lines = [f"{LABEL} {answer}", "", "Sources:"] + [f"[{i}] {c['source']}: {c['ref']}" for i, c in enumerate(cites, 1)]
    text = "\n".join(lines)
    if len(text) > MAX_TEXT:
        raise Refused(f"the reply is {len(text)} characters, more than the {MAX_TEXT} a reply may be")
    return text


def decide(case, answer, cites):
    thread = str((case.get("data") or {}).get("thread") or "")
    if not thread or len(thread) > MAX_THREAD:
        raise Refused("the case has no thread to answer in")
    srcs = "; ".join(f"[{i}] {c['source']}: {c['ref']}" for i, c in enumerate(cites, 1))
    return {"outcome": "plan", "action": "plumbing.reply",
            "params": {"channel": CHANNEL, "thread": thread, "text": reply_text(answer, cites)},
            "detail": f"an answer with {len(cites)} source(s), each a read this run made: {srcs}"[:2000]}


def run_answer(case, environ, call=None):
    import harness
    import model_client
    import reads
    import stations
    st = stations.load(environ)
    if st is None:
        return {"outcome": "escalate", "detail": "FLOTILLA_STATIONS is not set: knowledge/answer runs on its station answer"}
    slots = task_slots(case)
    if not slots["question"].strip():
        return {"outcome": "escalate", "detail": "the question came with no text"}
    doc = st.station(STATION)
    names = reads.tools_for(doc)
    if not names:
        return {"outcome": "escalate", "detail": "the station answer declares no reads: nothing to answer from"}
    system = st.render(STATION, "system", {}, fleet="knowledge", environ=environ)
    task = st.render(STATION, "task", slots, fleet="knowledge", environ=environ)
    limits = (doc or {}).get("limits") or {}
    turns = max(2, int(limits.get("max_turns") or 6))
    if call is None:
        client = model_client.from_env(environ)

        def call(messages, step):
            r = client.chat(ALIAS, messages, max_tokens=1500, timeout=120, step=step + 1, station=STATION,
                            texts=[system.record(), task.record()], decision_key=case.get("id"), role="knowledge")
            return r["content"], r.get("usage") or {}
    try:
        res = harness.run_json_step(
            [{"role": "system", "content": system.text}, {"role": "user", "content": task.text}],
            call=call, tool=lambda name, args: f"<unknown probe: {name}>", station=STATION, reads=doc,
            cfg={"tools": names, "limits": {"max_tool_calls": turns - 1, "max_steps": turns},
                 "quirks": {"bad_json_uses_turn": True, "empty_retry": True, "final_decision_call": True,
                            "reply_echo_chars": 2000, "decision_echo_chars": 2000, "result_chars": 12000}},
            errors={"fatal": (SystemExit, model_client.ConfigError), "defer": RuntimeError, "defer_prefix": "model call failed: "},
            log=lambda m: None)
    except Exception as e:  # the client's typed errors and a deferred turn; the run is not a decision
        return {"outcome": "escalate", "detail": f"the answer loop failed: {type(e).__name__}: {e}"}
    try:
        answer, cites = parse(res.decision, res.trace)
        return decide(case, answer, cites)
    except Refused as e:
        return {"outcome": "escalate", "detail": f"refused by the gates: {e} ({len(reads_made(res.trace))} read(s) made)"}


def report(obj):
    path = os.environ.get("FLOTILLA_PLAN_FILE")
    if path:
        with open(path, "w") as f:
            json.dump(obj, f)
    print(f"KNOWLEDGE outcome={obj['outcome']} {obj.get('action', '')} {obj.get('detail', '')}", flush=True)


def main(argv):
    if argv[:1] != ["answer"]:
        print("usage: answer.py answer", file=sys.stderr)
        return 2
    case = json.loads(os.environ.get("FLOTILLA_CASE") or "{}")
    import telemetry
    session = telemetry.setup()
    try:
        out = run_answer(case, os.environ)
    finally:
        session.shutdown()
    report(out)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
