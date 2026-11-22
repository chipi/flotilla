"""plumbing.call: the shared sender of the plumbing's callbacks (RFC-0014 §4.6).

  call.py execute   the executing routine of an approved plumbing.* plan (FLOTILLA_CASE, FLOTILLA_PLAN):
                    checks the plan's hash and the payload's shape, then posts it, signed, to the action's
                    callback; reports {"outcome": "executed", "ref"} when the plumbing says what it made, and
                    escalates otherwise. It never retries: a timed-out call may have been delivered.
  call.py check     the check of a sent plan (FLOTILLA_CASE, FLOTILLA_CHECK): asks the same callback, signed,
                    whether the plan's delivery is done in the system behind it; reports verified or not_yet.

Each action's callback is the installation's, never the spec's: PLUMBING_<NAME>_URL and
PLUMBING_<NAME>_SECRET_FILE (the worker's ROUTINE_ settings; NAME is the action after "plumbing.",
upper case). The signature is the inbound hook's (internal/hook): X-Flotilla-Timestamp,
X-Flotilla-Delivery and X-Flotilla-Signature, sha256= the hex HMAC-SHA256 of timestamp.delivery.body.
The delivery is "plan-" and the plan hash's first 16 hex digits, the same for a send and its check, so
the plumbing can tell a re-send of one plan from a new one.
"""
import hashlib
import hmac
import json
import os
import re
import stat
import sys
import time
import urllib.error
import urllib.request

TIMEOUT_S = 15
MAX_ITEMS = 20
LABEL = "[flotilla]"
SECRET_MIN = 32
SECRET_MAX = 4096
ACTIONS = ("create_tasks", "post_decisions", "reply", "post", "retract")


class Refused(Exception):
    """A plan this sender does not send: nothing left the worker."""


def go_json(v):
    """json.dumps as Go's encoding/json writes it (as business/apply.py), so the hash is policy's."""
    s = json.dumps(v, separators=(",", ":"), ensure_ascii=False)
    for a, b in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), (" ", "\\u2028"), (" ", "\\u2029")):
        s = s.replace(a, b)
    return s


def plan_hash(case_id, action, params):
    """policy.Plan.Hash: sha256 of the JSON of [case id, action, [[key, value] sorted by key]]."""
    return hashlib.sha256(go_json([case_id, action, [[k, params[k]] for k in sorted(params)]]).encode()).hexdigest()


def sign(secret, ts, delivery, body):
    """internal/hook.Sign: the hex HMAC-SHA256 of timestamp.delivery.body under the secret."""
    return hmac.new(secret.encode(), f"{ts}.{delivery}.".encode() + body, hashlib.sha256).hexdigest()


def name_of(action):
    if not action.startswith("plumbing.") or action[len("plumbing."):] not in ACTIONS:
        raise Refused(f"{action!r} is not a plumbing.call action")
    return action[len("plumbing."):]


def setting(env, name):
    """The action's callback URL and secret, from the installation's settings; the secret file as a hook's."""
    key = "PLUMBING_" + name.upper()
    url, path = env.get(key + "_URL", ""), env.get(key + "_SECRET_FILE", "")
    if not re.match(r"^https?://[^\s]+$", url):
        raise Refused(f"{key}_URL is not set to an http(s) URL")
    if not path:
        raise Refused(f"{key}_SECRET_FILE is not set")
    try:
        st = os.stat(path)
        if st.st_mode & (stat.S_IRWXG | stat.S_IRWXO):
            raise Refused(f"{key}_SECRET_FILE is readable by others (mode {stat.S_IMODE(st.st_mode):o}): want 600")
        with open(path) as f:
            secret = f.read().strip()
    except OSError as e:
        raise Refused(f"{key}_SECRET_FILE: {e.strerror}") from None
    if not SECRET_MIN <= len(secret) <= SECRET_MAX:
        raise Refused(f"{key}_SECRET_FILE: the secret is {len(secret)} characters, want {SECRET_MIN} to {SECRET_MAX}")
    return url, secret


def items_of(raw):
    """create_tasks' follow-ups: a JSON list of at most MAX_ITEMS {who, what, by}, each a short text."""
    try:
        items = json.loads(raw)
    except ValueError:
        raise Refused("items is not JSON") from None
    if not isinstance(items, list) or not 1 <= len(items) <= MAX_ITEMS:
        raise Refused(f"items is a list of 1 to {MAX_ITEMS} follow-ups")
    for i, it in enumerate(items):
        if not isinstance(it, dict) or set(it) != {"who", "what", "by"}:
            raise Refused(f"item {i + 1}: who, what and by, nothing else")
        if not all(isinstance(it[k], str) and it[k].strip() for k in it):
            raise Refused(f"item {i + 1}: who, what and by are each a text")
        if not re.match(r"^\d{4}-\d{2}-\d{2}$", it["by"]):
            raise Refused(f"item {i + 1}: by is a date, YYYY-MM-DD")
    return items


def chat_text(text):
    """A chat post's text: the agent's label leads it, and it calls no crowd (RFC-0014 §4.6's gates that need nothing else)."""
    if not text.startswith(LABEL):
        raise Refused(f"the text does not start with the agent's label {LABEL}")
    if re.search(r"@(channel|here|everyone)\b|<!(channel|here|everyone)>", text):
        raise Refused("the text calls the whole channel")
    return text


def payload_params(name, params):
    """The params as the callback receives them; a list param is sent as the list it names."""
    out = dict(params)
    if name == "create_tasks":
        out["items"] = items_of(params.get("items", ""))
    elif name in ("reply", "post"):
        chat_text(params.get("text", ""))
    return out


def post(url, secret, delivery, doc, now=time.time):
    body = go_json(doc).encode()
    ts = str(int(now()))
    req = urllib.request.Request(url, data=body, method="POST", headers={
        "Content-Type": "application/json", "X-Flotilla-Timestamp": ts, "X-Flotilla-Delivery": delivery,
        "X-Flotilla-Signature": "sha256=" + sign(secret, ts, delivery, body)})
    with urllib.request.urlopen(req, timeout=TIMEOUT_S) as r:
        raw = r.read(65536)
    try:
        got = json.loads(raw or b"{}")
    except ValueError:
        got = {}
    return got if isinstance(got, dict) else {}


def ref_of(got):
    ref = got.get("ref")
    return ref.strip()[:200] if isinstance(ref, str) else ""


def execute(case, plan, env, send=post):
    try:
        name = name_of(plan.get("action", ""))
        prm = plan.get("params") or {}
        h = plan_hash(case["id"], plan["action"], prm)
        if h != plan.get("plan_hash"):
            raise Refused("the plan's hash is not the approved one")
        doc = {"mode": "send", "action": plan["action"], "case_id": case["id"], "plan_hash": h, "params": payload_params(name, prm)}
        url, secret = setting(env, name)
    except Refused as e:
        return {"outcome": "escalate", "detail": f"{e}: nothing sent"}
    delivery = "plan-" + h[:16]
    try:
        got = send(url, secret, delivery, doc)
    except urllib.error.HTTPError as e:
        e.close()
        return {"outcome": "escalate", "detail": f"the plumbing answered {e.code} for {delivery}: not sent, or not known to be"}
    except (urllib.error.URLError, OSError) as e:
        return {"outcome": "escalate", "detail": f"the call for {delivery} failed ({getattr(e, 'reason', e)}): it may have been delivered; not retried"}
    ref = ref_of(got)
    if not ref:
        return {"outcome": "escalate", "detail": f"the plumbing took {delivery} but named nothing it made"}
    return {"outcome": "executed", "detail": f"{plan['action']} sent as {delivery}", "ref": ref}


def check(case, chk, env, send=post):
    try:
        name = name_of(chk.get("action", ""))
        prm = chk.get("params") or {}
        h = plan_hash(case["id"], chk["action"], prm)
        url, secret = setting(env, name)
    except Refused as e:
        return {"outcome": "failed", "detail": str(e)}
    delivery = "plan-" + h[:16]
    try:
        got = send(url, secret, delivery, {"mode": "status", "action": chk["action"], "case_id": case["id"], "plan_hash": h})
    except (urllib.error.URLError, OSError) as e:
        if isinstance(e, urllib.error.HTTPError):
            e.close()
        return {"outcome": "not_yet", "detail": f"the status call for {delivery} failed ({getattr(e, 'reason', e)})"}
    if got.get("done") is True and ref_of(got):
        return {"outcome": "verified", "detail": f"{delivery} is done: {ref_of(got)}"}
    return {"outcome": "not_yet", "detail": f"{delivery} is not done yet"}


def report(obj):
    path = os.environ.get("FLOTILLA_PLAN_FILE")
    if path:
        with open(path, "w") as f:
            json.dump(obj, f)


def main(argv):
    if argv[:1] == ["execute"]:
        out = execute(json.loads(os.environ["FLOTILLA_CASE"]), json.loads(os.environ["FLOTILLA_PLAN"]), os.environ)
    elif argv[:1] == ["check"]:
        out = check(json.loads(os.environ["FLOTILLA_CASE"]), json.loads(os.environ["FLOTILLA_CHECK"]), os.environ)
    else:
        print("usage: call.py execute | check", file=sys.stderr)
        return 2
    report(out)
    print(f"PLUMBING {argv[0].upper()} outcome={out['outcome']} {out.get('detail', '')}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
