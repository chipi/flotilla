"""meetings: a meeting's notes into its follow-ups and decisions (RFC-0014 §4.5, case 3).

  notes.py notes       meetings/notes, the acting routine of a meeting case (the hook `meeting` opened it with the
                       notes' file name and text): one model call on the station `extract` reads the decisions and
                       the follow-ups; code gates check their shape; it plans plumbing.create_tasks with the
                       follow-ups and, as the plan's then, the decisions for the next stage. No follow-ups: it hands
                       the case straight to that stage. Neither: the case resolves itself.
  notes.py decisions   meetings/decisions, the stage `decisions`: plans plumbing.post_decisions with the decisions
                       the first stage left (stage_data), as one labelled text. No model.

The notes are data, never instructions: the task text puts them between markers the notes cannot close, and
nothing the model says chooses an action, a stage or a target (code does). A follow-up is a task in the
tracker, not a flotilla case. Stdlib, and the runtime's model client and stations.
"""
import datetime
import json
import os
import re
import sys

sys.path.insert(0, os.path.join(os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla"), "runtime"))

MAX_ITEMS = 20          # plumbing/call.py's MAX_ITEMS
MAX_ITEMS_CHARS = 4000  # plumbing.create_tasks' items param (host/registry.json)
MAX_TEXT_CHARS = 8000   # plumbing.post_decisions' text param
MAX_DECISIONS = 30
MAX_FIELD = {"who": 120, "what": 300, "by": 10}
MAX_DECISION = 500
STAGE = "decisions"
ALIAS = "meetings-notes"
END = "</meeting-notes>"
DATE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


class Refused(Exception):
    """What the model said does not pass the gates: the case escalates, nothing is planned."""


def task_slots(case, today=None):
    """The task text's slots: the file, the meeting's date (the hook's `date` when it is one, else today) and the notes,
    which can never close their own markers."""
    data = case.get("data") or {}
    text = (data.get("text") or "").replace(END, "")
    date = str(data.get("date") or "")
    try:
        datetime.date.fromisoformat(date)
        if not DATE.match(date):
            raise ValueError
    except ValueError:
        date = (today or datetime.datetime.now(datetime.timezone.utc).date()).isoformat()
    return {"file": (data.get("file") or case.get("target") or "")[:120], "date": date, "notes": text}


def parse(raw):
    """The model's answer, gated: {"decisions": [text], "follow_ups": [{who, what, by}]}, nothing else."""
    try:
        doc = json.loads(raw)
    except (TypeError, ValueError):
        raise Refused("the model's answer is not JSON") from None
    if not isinstance(doc, dict) or set(doc) != {"decisions", "follow_ups"}:
        raise Refused("the answer is not exactly decisions and follow_ups")
    decisions, items = doc["decisions"], doc["follow_ups"]
    if not isinstance(decisions, list) or len(decisions) > MAX_DECISIONS:
        raise Refused(f"decisions is a list of at most {MAX_DECISIONS}")
    if not isinstance(items, list) or len(items) > MAX_ITEMS:
        raise Refused(f"follow_ups is a list of at most {MAX_ITEMS}")
    out_d = []
    for i, d in enumerate(decisions):
        if not isinstance(d, str) or not d.strip() or len(d) > MAX_DECISION:
            raise Refused(f"decision {i + 1}: a text of 1 to {MAX_DECISION} characters")
        out_d.append(" ".join(d.split()))
    out_i = []
    for i, it in enumerate(items):
        if not isinstance(it, dict) or set(it) != {"who", "what", "by"}:
            raise Refused(f"follow-up {i + 1}: who, what and by, nothing else")
        row = {}
        for k in ("who", "what", "by"):
            v = it[k]
            if not isinstance(v, str) or not v.strip() or len(v) > MAX_FIELD[k]:
                raise Refused(f"follow-up {i + 1}: {k} is a text of 1 to {MAX_FIELD[k]} characters")
            row[k] = " ".join(v.split())
        if not DATE.match(row["by"]):
            raise Refused(f"follow-up {i + 1}: by is a date, YYYY-MM-DD")
        try:
            datetime.date.fromisoformat(row["by"])
        except ValueError:
            raise Refused(f"follow-up {i + 1}: by {row['by']} is not a date") from None
        out_i.append(row)
    return out_d, out_i


def decide(case, decisions, items):
    """The first stage's decision on what was extracted, as the plan file says it."""
    file = task_slots(case)["file"]
    then = {"decisions": decisions, "file": file}
    if items:
        raw = json.dumps(items, separators=(",", ":"), ensure_ascii=False)
        if len(raw) > MAX_ITEMS_CHARS:
            raise Refused(f"the follow-ups are {len(raw)} characters, more than the {MAX_ITEMS_CHARS} a list may be")
        plan = {"outcome": "plan", "action": "plumbing.create_tasks", "params": {"items": raw},
                "detail": f"{len(items)} follow-up(s) from {file}, {len(decisions)} decision(s) next"}
        if decisions:
            plan["then"] = {"stage": STAGE, "data": then}
        return plan
    if decisions:
        return {"outcome": "hand_over", "stage": STAGE, "data": then, "detail": f"no follow-ups in {file}, {len(decisions)} decision(s)"}
    return {"outcome": "resolved_itself", "detail": f"no decisions and no follow-ups in {file}"}


def decisions_text(case):
    """The stage `decisions`: one labelled text for the decisions page, from what the first stage left."""
    sd = case.get("stage_data") or {}
    decisions = sd.get("decisions") if isinstance(sd, dict) else None
    if not isinstance(decisions, list) or not decisions or not all(isinstance(d, str) and d.strip() for d in decisions):
        return None
    file = str(sd.get("file") or case.get("target") or "")[:120]
    lines = [f"[flotilla] Decisions from {file} (case {case.get('id', '')}):"] + [f"- {d}" for d in decisions]
    text = "\n".join(lines)
    if len(text) > MAX_TEXT_CHARS:
        raise Refused(f"the decisions are {len(text)} characters, more than the {MAX_TEXT_CHARS} the page takes at once")
    return text


def run_notes(case, environ, chat=None):
    import stations
    st = stations.load(environ)
    if st is None:
        return {"outcome": "escalate", "detail": "FLOTILLA_STATIONS is not set: meetings/notes runs on its station extract"}
    slots = task_slots(case)
    if not slots["notes"].strip():
        return {"outcome": "escalate", "detail": f"the meeting {slots['file']} came with no text"}
    system = st.render("extract", "system", {}, fleet="meetings", environ=environ)
    task = st.render("extract", "task", slots, fleet="meetings", environ=environ)
    if chat is None:
        import model_client
        chat = model_client.from_env(environ).chat
    try:
        r = chat(ALIAS, [{"role": "system", "content": system.text}, {"role": "user", "content": task.text}],
                 max_tokens=2048, timeout=120, station="extract", texts=[system.record(), task.record()],
                 decision_key=case.get("id"), role="meetings")
    except Exception as e:  # the client's typed errors; the run is not a decision
        return {"outcome": "escalate", "detail": f"the model call failed: {type(e).__name__}: {e}"}
    try:
        decisions, items = parse(r.get("content"))
        return decide(case, decisions, items)
    except Refused as e:
        return {"outcome": "escalate", "detail": f"refused by the gates: {e}"}


def run_decisions(case):
    try:
        text = decisions_text(case)
    except Refused as e:
        return {"outcome": "escalate", "detail": str(e)}
    if text is None:
        return {"outcome": "resolved_itself", "detail": "the first stage left no decisions"}
    return {"outcome": "plan", "action": "plumbing.post_decisions", "params": {"page": "decisions", "text": text},
            "detail": "the meeting's decisions for the decisions page"}


def report(obj):
    path = os.environ.get("FLOTILLA_PLAN_FILE")
    if path:
        with open(path, "w") as f:
            json.dump(obj, f)
    print(f"MEETINGS outcome={obj['outcome']} {obj.get('action', '')} {obj.get('detail', '')}", flush=True)


def main(argv):
    case = json.loads(os.environ.get("FLOTILLA_CASE") or "{}")
    if argv[:1] == ["notes"]:
        import telemetry
        session = telemetry.setup()
        try:
            out = run_notes(case, os.environ)
        finally:
            session.shutdown()
    elif argv[:1] == ["decisions"]:
        out = run_decisions(case)
    else:
        print("usage: notes.py notes | decisions", file=sys.stderr)
        return 2
    report(out)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
