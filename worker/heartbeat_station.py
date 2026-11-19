"""The platform heartbeat of a version that declares the `probe` station (fleets/platform/heartbeat/v2).

worker/heartbeat.py is what v1 runs and stays as it is: the allowed host answers through the proxy, a host off the list is refused by it, and
an optional one-call model check named by HEARTBEAT_MODEL_ALIAS. v2 runs the same checks and makes the model call as a station: its alias, its
text and its limits come from FLOTILLA_STATIONS and FLOTILLA_MODELS (what flotillad hands the run), and the call is recorded with the station
and the text it read (runtime/stations.py). The request is the one v1 sends: worker/test_heartbeat_station.py compares the two.
"""
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla"), "runtime"))
import heartbeat  # noqa: E402

STATION = "probe"
TEXT = "task"
FLEET = "platform"


def station_request(environ=None):
    """(alias, the rendered text, max_tokens, timeout seconds) of the probe station, or an error naming what the version lacks."""
    import stations
    environ = os.environ if environ is None else environ
    st = stations.load(environ)
    if st is None:
        raise stations.StationsError("FLOTILLA_STATIONS is not set: this entry point runs a version that declares the probe station")
    s = st.station(STATION)
    lim = s.get("limits") or {}
    tokens, timeout = lim.get("max_output_tokens"), lim.get("timeout")
    if not isinstance(tokens, int) or tokens <= 0 or not isinstance(timeout, str) or not timeout.endswith("s") or not timeout[:-1].isdigit():
        raise stations.StationsError(f"station {STATION} limits: max_output_tokens and a timeout in whole seconds (30s) are required, got {lim}")
    r = st.render(STATION, TEXT, {}, fleet=FLEET)
    text = r.text[:-1] if r.text.endswith("\n") else r.text   # the file ends in one newline; the request does not
    return s["model"], stations.Rendered(r.station, r.name, text, r.template_sha256), tokens, int(timeout[:-1])


def model_step(alias):
    """heartbeat.model_step on the station: the same call and the same report, the alias, text and limits the station's."""
    import model_client
    import prompts
    import stations
    import telemetry
    session = telemetry.setup()
    try:
        alias, rendered, tokens, timeout = station_request()
        r = model_client.from_env().chat(alias, [{"role": "user", "content": rendered.text}], max_tokens=tokens, timeout=timeout,
                                         station=STATION, texts=[rendered.record()])
        print(f"ok   model   {alias}: model={r.get('model')} deployment={r.get('model_id')} "
              f"cost={r.get('cost_usd')} ({r.get('cost_source')}) usage_reported={r.get('usage_reported')} "
              f"traced={session.active or session.why}")
        return True
    except (model_client.ModelError, model_client.ConfigError, stations.StationsError, prompts.PromptError) as e:
        print(f"FAIL model   {alias}: {type(e).__name__}: {e}")
        return False
    finally:
        session.shutdown()


def main():
    import prompts
    import stations
    try:
        alias = station_request()[0]
    except (stations.StationsError, prompts.PromptError) as e:
        print(f"FAIL: {e}")
        return 2
    os.environ["HEARTBEAT_MODEL_ALIAS"] = alias   # heartbeat.main makes its model step when an alias is named; ours is the station's
    heartbeat.model_step = model_step
    return heartbeat.main()


if __name__ == "__main__":
    sys.exit(main())
