"""e2e/reads: render the station's texts and run its loop through the flotilla harness with the station's reads (RFC-0014 section 4.8), the
way a version's own code does (signal-fleet/mvp/orchestrator_station.py). The scripted model asks for a page, the long log and a tool the
station does not declare; each read's result is printed as its length and a hash, so the e2e can compare a live run, an offline run and a
shadow against what was recorded. A miss in a shadow (a read the record lacks) is not caught: it fails the run."""
import hashlib
import os
import sys

sys.path.insert(0, os.path.join(os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla"), "runtime"))
import harness  # noqa: E402
import model_client  # noqa: E402
import stations  # noqa: E402

STATION = "answer"
st = stations.load()
if st is None:
    sys.exit("FLOTILLA_STATIONS is not set")
doc = st.station(STATION)
system = st.render(STATION, "system", {}, fleet="e2e")
task = st.render(STATION, "task", {"target": "lab:e2e-reads"}, fleet="e2e")
client = model_client.from_env()
turns = {"n": 0}


def call(messages, step):
    turns["n"] += 1
    rec = client.chat("reads-answer", messages, max_tokens=256, timeout=30, step=step + 1, station=STATION, texts=[system.record(), task.record()])
    return rec["content"], rec.get("usage") or {}


res = harness.run_json_step(
    [{"role": "system", "content": system.text}, {"role": "user", "content": task.text}],
    call=call, tool=lambda name, args: "noted", station=STATION, reads=doc,
    cfg={"tools": ["note"], "limits": {"max_tool_calls": 3, "max_steps": 4},
         "quirks": {"bad_json_uses_turn": True, "empty_retry": True, "final_decision_call": True,
                    "reply_echo_chars": 600, "decision_echo_chars": 400, "result_chars": 1800}},
    errors={"fatal": (SystemExit, model_client.ConfigError), "defer": RuntimeError, "defer_prefix": "model call failed: "})
for t in res.trace:
    r = t["result"]
    print("PROBE", t["probe"], "chars", len(r), "sha", hashlib.sha256(r.encode()).hexdigest()[:16], "head", r[:40].replace("\n", " "), flush=True)
print("DECISION", res.decision.get("disposition"), "probes", len(res.trace), flush=True)
