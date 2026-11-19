"""e2e/station-monitor: render the station's texts, call the model through the client with the station and the texts, report one detection."""
import json
import os
import sys

sys.path.insert(0, os.path.join(os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla"), "runtime"))
import model_client  # noqa: E402
import stations  # noqa: E402
import telemetry  # noqa: E402

telemetry.setup()   # the model call's span (flotilla.station); exported at exit

st = stations.load()
if st is None:
    sys.exit("FLOTILLA_STATIONS is not set")
system = st.render("decide", "system", {}, fleet="e2e")
task = st.render("decide", "task", {"target": "host:e2e-stations"}, fleet="e2e")
r = model_client.from_env().chat("triage-flash", [{"role": "system", "content": system.text}, {"role": "user", "content": task.text}],
                                 max_tokens=256, timeout=30, station="decide", texts=[system.record(), task.record()])
print("STATION-MONITOR", os.environ.get("FLOTILLA_ROUTINE"), "model", r.get("model"), flush=True)
plan = os.environ.get("FLOTILLA_PLAN_FILE")
if plan:
    with open(plan, "w") as f:
        json.dump({"outcome": "detections", "detections": [{"kind": "e2e_stations", "target": "host:e2e-stations", "detail": "e2e"}]}, f)
