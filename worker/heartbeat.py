"""The platform heartbeat (fleets/platform/heartbeat.yaml): proves a worker can run a routine and that
egress is enforced, both ways. Exit 0 only when an allowed host answers through the proxy AND a host
off the list is refused by it.

  HEARTBEAT_ALLOWED_URL  reached through the proxy, any HTTP answer below 500 counts
  HEARTBEAT_DENIED_URL   a host that exists and would answer, but is off the allowlist: the proxy
                         must refuse it (HTTP 407/403 from the proxy)

Both come from the installation (ROUTINE_HEARTBEAT_* in the worker's environment). The proxy is the
standard HTTP_PROXY / HTTPS_PROXY, which urllib honours.

  HEARTBEAT_MODEL_ALIAS  optional: one tiny call through the model client (runtime/model_client.py) to
                         this alias from FLOTILLA_MODELS, traced (runtime/telemetry.py). It proves the
                         models section, the key, the endpoint through the proxy and the routine's spans.
"""
import os
import sys
import urllib.error
import urllib.request

sys.path.insert(0, os.path.join(os.environ.get("FLOTILLA_CODE_ROOT", "/opt/flotilla"), "runtime"))


def probe(url, timeout=10):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return r.status, ""
    except urllib.error.HTTPError as e:
        return e.code, e.reason
    except (urllib.error.URLError, OSError) as e:
        return None, str(getattr(e, "reason", e))


def main():
    allowed, denied = os.environ.get("HEARTBEAT_ALLOWED_URL"), os.environ.get("HEARTBEAT_DENIED_URL")
    if not allowed or not denied:
        print("FAIL: HEARTBEAT_ALLOWED_URL and HEARTBEAT_DENIED_URL must both be set")
        return 2
    if not (os.environ.get("HTTP_PROXY") or os.environ.get("http_proxy")):
        print("FAIL: no HTTP_PROXY: this routine checks the proxy, and would check nothing without one")
        return 2
    ok = True
    code, why = probe(allowed)
    if code is None or code >= 500:
        print(f"FAIL allowed {allowed}: {code} {why}")
        ok = False
    else:
        print(f"ok   allowed {allowed}: HTTP {code}")
    # Only the proxy's own refusal counts. A connection error would also "fail" with no proxy rule at
    # all (the host does not resolve, the network is down), so it proves nothing about egress.
    code, why = probe(denied)
    if code in (403, 407):
        print(f"ok   denied  {denied}: refused by the proxy (HTTP {code})")
    elif code is None:
        print(f"FAIL denied  {denied}: no answer ({why}), so the proxy's refusal was not seen")
        ok = False
    else:
        print(f"FAIL denied  {denied}: HTTP {code}: egress is NOT enforced")
        ok = False
    alias = os.environ.get("HEARTBEAT_MODEL_ALIAS")
    if alias:
        ok = model_step(alias) and ok
    print("HEARTBEAT PASS" if ok else "HEARTBEAT FAIL")
    return 0 if ok else 1


def model_step(alias):
    import model_client
    import telemetry
    session = telemetry.setup()
    try:
        r = model_client.from_env().chat(alias, [{"role": "user", "content": "Reply with the JSON {\"ok\": true}."}],
                                         max_tokens=16, timeout=30)
        print(f"ok   model   {alias}: model={r.get('model')} deployment={r.get('model_id')} "
              f"cost={r.get('cost_usd')} ({r.get('cost_source')}) usage_reported={r.get('usage_reported')} "
              f"traced={session.active or session.why}")
        return True
    except (model_client.ModelError, model_client.ConfigError) as e:
        print(f"FAIL model   {alias}: {type(e).__name__}: {e}")
        return False
    finally:
        session.shutdown()


if __name__ == "__main__":
    sys.exit(main())
