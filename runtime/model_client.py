"""The one model client (ADR-0004, RFC-0002 §3): any OpenAI-compatible Chat Completions endpoint.

LiteLLM is the reference deployment, not a requirement. Specs name aliases; this installation's
config maps each alias to an endpoint and a model, with the alias's capabilities:

  endpoints:
    gateway: {base_url: http://litellm:4000/v1, key_file: /run/flotilla/litellm.key, kind: litellm,
              defaults: {tools: native, json: object, cost: reported}}
  aliases:
    triage-default: {endpoint: gateway, model: fleet-triage-pro}
    local-dgx: {endpoint: gateway, model: NVFP4/Qwen3-30B-A3B-Instruct-2507-FP4, tools: none, cost: zero}
  prices:          # for aliases with cost: table, per million tokens
    some-alias: {input_per_mtok: 0.14, output_per_mtok: 0.28}

Capabilities (an alias overrides its endpoint's defaults):
  tools  native (tools / tool_calls) | json (the model answers in JSON; the caller's protocol) | none
  json   schema (response_format json_schema) | object (json_object) | none
  cost   reported (the response says: body usage.cost, else the x-litellm-response-cost header)
         | table (the prices above) | zero (a declared local model) | unknown (never counted as zero)

Every call returns a Reply that records what an exact replay needs: the request exactly as sent, the
raw reply, usage, cost and where it came from, the model the response names and, on LiteLLM, the
deployment that answered (x-litellm-model-id). Errors are typed (AuthOrBilling, Transient,
RequestRejected, EmptyCompletion) so the harness can fail closed or defer as triage does today.
Stdlib only. No streaming (a non-goal).

Not wired into any fleet yet: the triage fleet moves onto it in RFC-0002 §9 step 2, after the switch.
"""
import json
import re
import time
import urllib.error
import urllib.request

TOOLS = ("native", "json", "none")
JSON_MODES = ("schema", "object", "none")
COSTS = ("reported", "table", "zero", "unknown")


class ModelError(Exception):
    """Base of the client's typed errors. `record` is the failed call's Reply-shaped record."""

    def __init__(self, msg, record=None):
        super().__init__(msg)
        self.record = record or {}


class AuthOrBilling(ModelError):
    """401, 403, 402: persistent; every call will fail until someone fixes the key or the credit."""


class Transient(ModelError):
    """Timeout, connection failure, 429, 5xx: retry the case at the next run."""


class RequestRejected(ModelError):
    """Another 4xx: this request is wrong (a bad parameter, an unknown model); retrying it won't help."""


class EmptyCompletion(ModelError):
    """A 200 with neither content nor tool calls: the provider flake triage retries within the turn."""


class ConfigError(ValueError):
    pass


class Alias:
    def __init__(self, name, endpoint, model, base_url, key_file, kind, tools, json_mode, cost, price):
        self.name, self.endpoint, self.model = name, endpoint, model
        self.base_url, self.key_file, self.kind = base_url.rstrip("/"), key_file, kind
        self.tools, self.json_mode, self.cost, self.price = tools, json_mode, cost, price


def load(config):
    """{alias name: Alias} from the `models` section of the installation's config (already parsed)."""
    endpoints, aliases, prices = config.get("endpoints") or {}, config.get("aliases") or {}, config.get("prices") or {}
    out = {}
    for name, a in aliases.items():
        ep = endpoints.get(a.get("endpoint"))
        if ep is None:
            raise ConfigError(f"alias {name}: endpoint {a.get('endpoint')!r} is not defined")
        d = dict(ep.get("defaults") or {})
        d.update({k: v for k, v in a.items() if k in ("tools", "json", "cost")})
        tools, json_mode, cost = d.get("tools", "none"), d.get("json", "none"), d.get("cost", "unknown")
        if tools not in TOOLS or json_mode not in JSON_MODES or cost not in COSTS:
            raise ConfigError(f"alias {name}: tools {tools!r} / json {json_mode!r} / cost {cost!r} not allowed")
        if not a.get("model") or not ep.get("base_url"):
            raise ConfigError(f"alias {name}: model and the endpoint's base_url are required")
        if cost == "table" and name not in prices:
            raise ConfigError(f"alias {name}: cost: table, but no price for it")
        out[name] = Alias(name, a["endpoint"], a["model"], ep["base_url"], ep.get("key_file"), ep.get("kind", "openai"),
                          tools, json_mode, cost, prices.get(name))
    return out


def _urllib_transport(url, data, headers, timeout):
    """POST; returns (status, {lower-case header: value}, body text). HTTP errors are returned, not raised."""
    req = urllib.request.Request(url, data=data, method="POST")
    for k, v in headers.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, {k.lower(): v for k, v in r.headers.items()}, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, {k.lower(): v for k, v in (e.headers or {}).items()}, e.read().decode("utf-8", "replace")


_THINK = re.compile(r"<think>.*?</think>\s*", re.S)


def strip_reasoning(content):
    """The answer without a thinking model's inline <think>…</think> (reasoning_content is never read)."""
    return _THINK.sub("", content).strip() if isinstance(content, str) else content


class Client:
    def __init__(self, aliases, transport=None, read_key=None, clock=time.time):
        self.aliases = aliases
        self.transport = transport or _urllib_transport
        self.read_key = read_key or (lambda path: open(path).read().strip() if path else "")
        self.clock = clock

    def body(self, alias, messages, tools=None, json_schema=None, temperature=0, trace_id=None, generation_name=None):
        """The request body, keys in the order triage sends them today (a byte-identical request)."""
        a = self.aliases[alias]
        if tools and a.tools == "none":
            raise ConfigError(f"alias {alias} has no tool calling, but tools were offered")
        b = {"model": a.model, "messages": messages}
        if a.json_mode == "object":
            b["response_format"] = {"type": "json_object"}
        elif a.json_mode == "schema" and json_schema is not None:
            b["response_format"] = {"type": "json_schema", "json_schema": json_schema}
        b["temperature"] = temperature
        if tools and a.tools == "native":
            b["tools"] = tools
        if trace_id and a.kind == "litellm":
            # LiteLLM's Langfuse logger adds its generation to the run's trace
            b["metadata"] = {"existing_trace_id": trace_id, "generation_name": generation_name}
        return b

    def chat(self, alias, messages, timeout=90, **kw):
        a = self.aliases[alias]
        body = self.body(alias, messages, **kw)
        request = json.dumps(body)
        rec = {"alias": alias, "request": request, "model_id": None}
        t0 = self.clock()
        headers = {"Content-Type": "application/json"}
        key = self.read_key(a.key_file)
        if key:
            headers["Authorization"] = f"Bearer {key}"
        try:
            status, rh, text = self.transport(a.base_url + "/chat/completions", request.encode(), headers, timeout)
        except Exception as e:  # noqa: BLE001 — a timeout or a refused connection
            rec.update(error=f"{type(e).__name__}: {e}"[:300], latency_s=round(self.clock() - t0, 3))
            raise Transient(f"{alias}: {rec['error']}", rec) from e
        rec.update(status=status, latency_s=round(self.clock() - t0, 3), model_id=rh.get("x-litellm-model-id"))
        if status != 200:
            rec["error"] = text[:300]
            cls = (AuthOrBilling if status in (401, 402, 403) else
                   Transient if status == 429 or status >= 500 else RequestRejected)
            raise cls(f"{alias}: HTTP {status}", rec)
        try:
            resp = json.loads(text)
            msg = (resp.get("choices") or [{}])[0].get("message") or {}
        except (ValueError, AttributeError) as e:
            rec["error"] = f"unparseable response: {text[:200]}"
            raise Transient(f"{alias}: {rec['error']}", rec) from e
        content, calls = msg.get("content"), msg.get("tool_calls") or []
        rec.update(reply=content, tool_calls=calls, usage=resp.get("usage") or {}, model=resp.get("model"))
        rec["cost_usd"], rec["cost_source"] = self._cost(a, rec["usage"], rh)
        if not (isinstance(content, str) and content.strip()) and not calls:
            raise EmptyCompletion(f"{alias}: empty completion", rec)
        rec["content"] = strip_reasoning(content)
        return rec

    @staticmethod
    def _cost(a, usage, headers):
        if a.cost == "zero":
            return 0.0, "zero"
        if a.cost == "reported":
            for v, src in ((usage.get("cost"), "reported:body"), (headers.get("x-litellm-response-cost"), "reported:header")):
                try:
                    if v is not None:
                        return float(v), src
                except (TypeError, ValueError):
                    pass
            return None, "unknown"      # the endpoint said nothing: never counted as zero
        if a.cost == "table":
            p = a.price
            return ((usage.get("prompt_tokens") or 0) * p["input_per_mtok"]
                    + (usage.get("completion_tokens") or 0) * p["output_per_mtok"]) / 1e6, "table"
        return None, "unknown"
