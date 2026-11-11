"""The one model client (ADR-0004, RFC-0002 §3): any OpenAI-compatible Chat Completions endpoint.

LiteLLM is the reference deployment, not a requirement. Specs name aliases; the installation's config
(flotillad.yaml `models:`, validated by flotillad and handed to the routine as JSON in FLOTILLA_MODELS)
maps each alias to an endpoint and a model, with the alias's capabilities:

  endpoints:
    gateway: {base_url: http://litellm:4000/v1, key_env: OPENROUTER_API_KEY, kind: litellm,
              defaults: {tools: native, json: object, cost: reported}}
  aliases:
    triage-default: {endpoint: gateway, model: fleet-triage-pro}
    local-dgx: {endpoint: gateway, model: NVFP4/Qwen3-30B-A3B-Instruct-2507-FP4, tools: none, cost: zero}
  prices:          # for aliases with cost: table, per million tokens
    some-alias: {input_per_mtok: 0.14, output_per_mtok: 0.28}

The key is named, never stored here: `key_env` (a variable in the routine's environment, which the worker
fills from the fleet's secrets file) or `key_file`. An endpoint with neither sends no Authorization.

Capabilities (an alias overrides its endpoint's defaults):
  tools  native (tools / tool_calls) | json (the model answers in JSON; the caller's protocol) | none
  json   schema (response_format json_schema) | object (json_object; OpenAI requires the word "JSON"
         in the prompt) | none
  cost   reported (the body's usage.cost, else the x-litellm-response-cost header: extensions of
         OpenRouter and LiteLLM) | table (the prices above) | zero (a declared local model) | unknown.
         A cost that is unknown, or reported but absent, is None: never counted as zero.

Every call returns a record of what an exact replay needs: the request exactly as sent, the raw reply,
usage (and whether the endpoint reported it), cost and its source, the model the response names and, on
LiteLLM, the deployment that answered (x-litellm-model-id). Errors are typed (AuthOrBilling, Transient,
RequestRejected, EmptyCompletion; ConfigError for the installation's mistakes), so the harness can fail
closed or defer as triage does today. chat_policy() adds a routine's declared fallback; the client never
falls back on its own. Each call is an OpenTelemetry span when the SDK is configured (the worker image
has it); without it, nothing is emitted and nothing breaks. Stdlib otherwise. No streaming (a non-goal).

Not wired into any fleet yet: the triage fleet moves onto it in RFC-0002 §9 step 2, after the switch.
"""
import contextlib
import json
import os
import re
import time
import urllib.error
import urllib.request

TOOLS = ("native", "json", "none")
JSON_MODES = ("schema", "object", "none")
COSTS = ("reported", "table", "zero", "unknown")
KINDS = ("litellm", "openai")
MAX_RESPONSE_BYTES = 4 * 1024 * 1024     # a runaway generation must not take the worker's memory


class ModelError(Exception):
    """Base of the client's typed errors. `record` is the failed call's record."""

    def __init__(self, msg, record=None):
        super().__init__(msg)
        self.record = record or {}


class AuthOrBilling(ModelError):
    """401, 403, 402: persistent; every call fails until someone fixes the key or the credit."""


class Transient(ModelError):
    """Timeout, connection failure, 429, 5xx, a broken or oversized response: retry at the next run."""


class RequestRejected(ModelError):
    """Another 4xx: this request is wrong (a bad parameter, an unknown model); retrying it won't help."""


class EmptyCompletion(ModelError):
    """A 200 with neither content nor tool calls: the provider flake triage retries within the turn."""


class ConfigError(ValueError):
    """The installation's or the caller's mistake: an unknown alias, a missing key, tools on an alias without them."""


class Alias:
    def __init__(self, name, endpoint, model, base_url, key_file, key_env, kind, tools, json_mode, cost, price):
        self.name, self.endpoint, self.model = name, endpoint, model
        self.base_url, self.key_file, self.key_env, self.kind = base_url.rstrip("/"), key_file, key_env, kind
        self.tools, self.json_mode, self.cost, self.price = tools, json_mode, cost, price


def load(config):
    """{alias name: Alias} from the `models` section (already parsed). Refuses anything it cannot honour."""
    if not isinstance(config, dict):
        raise ConfigError("models: a mapping with endpoints, aliases and prices")
    endpoints, aliases, prices = config.get("endpoints") or {}, config.get("aliases") or {}, config.get("prices") or {}
    # the same rules as flotillad's internal/models (both tested on runtime/testdata/models-*.json)
    for name, ep in endpoints.items():
        if not ep.get("base_url"):
            raise ConfigError(f"endpoint {name}: base_url is required")
        if ep.get("key_file") and ep.get("key_env"):
            raise ConfigError(f"endpoint {name}: key_file or key_env, not both")
        if ep.get("kind", "openai") not in KINDS:
            raise ConfigError(f"endpoint {name}: kind {ep.get('kind')!r}: litellm or openai")
    out = {}
    for name, a in aliases.items():
        extra = set(a) - {"endpoint", "model", "tools", "json", "cost"}
        if extra:
            raise ConfigError(f"alias {name}: unknown field(s) {sorted(extra)}")
        ep = endpoints.get(a.get("endpoint"))
        if ep is None:
            raise ConfigError(f"alias {name}: endpoint {a.get('endpoint')!r} is not defined")
        d = dict(ep.get("defaults") or {})
        d.update({k: v for k, v in a.items() if k in ("tools", "json", "cost")})
        tools, json_mode, cost = d.get("tools", "none"), d.get("json", "none"), d.get("cost", "unknown")
        kind = ep.get("kind", "openai")
        if tools not in TOOLS or json_mode not in JSON_MODES or cost not in COSTS or kind not in KINDS:
            raise ConfigError(f"alias {name}: tools {tools!r} / json {json_mode!r} / cost {cost!r} / kind {kind!r} not allowed")
        if not a.get("model"):
            raise ConfigError(f"alias {name}: model is required")
        if cost == "table":
            p = prices.get(name)
            if not isinstance(p, dict) or not all(isinstance(p.get(k), (int, float)) and p[k] >= 0
                                                  for k in ("input_per_mtok", "output_per_mtok")):
                raise ConfigError(f"alias {name}: cost: table needs prices.{name}.input_per_mtok and output_per_mtok")
        out[name] = Alias(name, a["endpoint"], a["model"], ep["base_url"], ep.get("key_file"), ep.get("key_env"),
                          kind, tools, json_mode, cost, prices.get(name))
    return out


def from_env(environ=None, **kw):
    """A Client from FLOTILLA_MODELS: the models section flotillad hands every run, as JSON."""
    environ = os.environ if environ is None else environ
    raw = environ.get("FLOTILLA_MODELS")
    if not raw:
        raise ConfigError("FLOTILLA_MODELS is not set: this installation's flotillad.yaml has no models section")
    try:
        cfg = json.loads(raw)
    except ValueError as e:
        raise ConfigError(f"FLOTILLA_MODELS is not JSON: {e}") from e
    return Client(load(cfg), environ=environ, **kw)


class _Response:
    def __init__(self, status, headers, text):
        self.status, self.headers, self.text = status, headers, text


def _urllib_transport(url, data, headers, timeout):
    """POST; returns (status, {lower-case header: value}, body text). HTTP errors are returned, not raised.
    The proxy comes from the environment (HTTPS_PROXY/HTTP_PROXY/NO_PROXY), as the worker sets it."""
    req = urllib.request.Request(url, data=data, method="POST")
    for k, v in headers.items():
        req.add_header(k, v)

    def read(r):
        body = r.read(MAX_RESPONSE_BYTES + 1)
        if len(body) > MAX_RESPONSE_BYTES:
            raise ValueError(f"response over {MAX_RESPONSE_BYTES} bytes")
        return body.decode("utf-8", "replace")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, {k.lower(): v for k, v in r.headers.items()}, read(r)
    except urllib.error.HTTPError as e:
        return e.code, {k.lower(): v for k, v in (e.headers or {}).items()}, read(e)


_THINK = re.compile(r"<think>.*?</think>\s*", re.S)


def strip_reasoning(content):
    """The answer without a thinking model's inline <think>…</think> (reasoning_content is never read)."""
    return _THINK.sub("", content).strip() if isinstance(content, str) else content


def _default_tracer():
    try:
        from opentelemetry import trace
        return trace.get_tracer("flotilla.model_client")
    except ImportError:
        return None


class Client:
    def __init__(self, aliases, transport=None, environ=None, clock=time.time, tracer="default"):
        self.aliases = aliases
        self.transport = transport or _urllib_transport
        self.environ = os.environ if environ is None else environ
        self.clock = clock
        self.tracer = _default_tracer() if tracer == "default" else tracer

    def _alias(self, alias):
        a = self.aliases.get(alias)
        if a is None:
            raise ConfigError(f"unknown model alias {alias!r} (configured: {', '.join(sorted(self.aliases)) or 'none'})")
        return a

    def _key(self, a):
        if a.key_env:
            v = self.environ.get(a.key_env, "").strip()
            if not v:
                raise ConfigError(f"endpoint {a.endpoint}: {a.key_env} is not set in the routine's environment")
            return v
        if a.key_file:
            try:
                with open(a.key_file) as f:
                    v = f.read().strip()
            except OSError as e:
                raise ConfigError(f"endpoint {a.endpoint}: key file {a.key_file}: {e.strerror}") from e
            if not v:
                raise ConfigError(f"endpoint {a.endpoint}: key file {a.key_file} is empty")
            return v
        return ""

    def body(self, alias, messages, tools=None, json_schema=None, temperature=0, trace_id=None, generation_name=None,
             max_tokens=None, seed=None, stop=None):
        """The request body, keys in the order triage sends them today (a byte-identical request); the optional
        parameters are added only when set, so a call without them is the same bytes as before."""
        a = self._alias(alias)
        if tools and a.tools == "none":
            raise ConfigError(f"alias {alias} has no tool calling, but tools were offered")
        b = {"model": a.model, "messages": messages}
        if a.json_mode == "object":
            b["response_format"] = {"type": "json_object"}
        elif a.json_mode == "schema" and json_schema is not None:
            b["response_format"] = {"type": "json_schema", "json_schema": json_schema}
        b["temperature"] = temperature
        for k, v in (("max_tokens", max_tokens), ("seed", seed), ("stop", stop)):
            if v is not None:
                b[k] = v
        if tools and a.tools == "native":
            b["tools"] = tools
        if trace_id and a.kind == "litellm":
            # LiteLLM's Langfuse logger adds its generation to the run's trace
            b["metadata"] = {"existing_trace_id": trace_id, "generation_name": generation_name}
        return b

    def chat(self, alias, messages, timeout=90, **kw):
        """One call. Returns its record; raises a ModelError (with .record) or ConfigError."""
        a = self._alias(alias)
        body = self.body(alias, messages, **kw)
        with self._span(a) as span:
            try:
                rec = self._call(a, body, timeout)
            except ModelError as e:
                self._annotate(span, e.record, error=e)
                raise
            self._annotate(span, rec)
            return rec

    def chat_policy(self, policy, messages, **kw):
        """A routine's model policy: {"model": alias, "fallback": [alias, ...], "fallback_on": ["transient", "empty"]}.
        Tries the model, then each fallback in order, only on the error kinds named (default: transient). The
        record says which alias answered and what failed before it (`attempts`)."""
        on = set(policy.get("fallback_on") or ["transient"])
        unknown = on - {"transient", "empty", "auth", "rejected"}
        if unknown:
            raise ConfigError(f"model policy: fallback_on {sorted(unknown)} not allowed")
        kinds = {"transient": Transient, "empty": EmptyCompletion, "auth": AuthOrBilling, "rejected": RequestRejected}
        chain = [policy["model"]] + list(policy.get("fallback") or [])
        attempts = []
        for i, alias in enumerate(chain):
            try:
                rec = self.chat(alias, messages, **kw)
            except ModelError as e:
                attempts.append({"alias": alias, "error": type(e).__name__, "detail": str(e)[:200]})
                if i == len(chain) - 1 or not any(isinstance(e, kinds[k]) for k in on):
                    e.record["attempts"] = attempts
                    raise
                continue
            rec["attempts"], rec["fallback"] = attempts, i > 0
            return rec

    def _call(self, a, body, timeout):
        request = json.dumps(body)
        rec = {"alias": a.name, "request": request, "model_id": None}
        headers = {"Content-Type": "application/json"}
        key = self._key(a)
        if key:
            headers["Authorization"] = f"Bearer {key}"
        t0 = self.clock()
        try:
            status, rh, text = self.transport(a.base_url + "/chat/completions", request.encode(), headers, timeout)
        except Exception as e:  # noqa: BLE001 — a timeout, a refused connection, an oversized response
            rec.update(error=f"{type(e).__name__}: {e}"[:300], latency_s=round(self.clock() - t0, 3))
            raise Transient(f"{a.name}: {rec['error']}", rec) from e
        rec.update(status=status, latency_s=round(self.clock() - t0, 3), model_id=rh.get("x-litellm-model-id"))
        if status != 200:
            rec["error"] = text[:300]
            cls = (AuthOrBilling if status in (401, 402, 403) else
                   Transient if status == 429 or status >= 500 else RequestRejected)
            raise cls(f"{a.name}: HTTP {status}: {text[:200]}", rec)   # the endpoint's words: a 403 can mean "this key may not use this model"
        try:
            resp = json.loads(text)
            msg = (resp.get("choices") or [{}])[0].get("message") or {}
        except (ValueError, AttributeError, IndexError) as e:
            rec["error"] = f"unparseable response: {text[:200]}"
            raise Transient(f"{a.name}: {rec['error']}", rec) from e
        usage = resp.get("usage")
        content, calls = msg.get("content"), msg.get("tool_calls") or []
        rec.update(reply=content, tool_calls=calls, usage=usage or {}, usage_reported=bool(usage),
                   model=resp.get("model"), finish_reason=((resp.get("choices") or [{}])[0] or {}).get("finish_reason"))
        rec["cost_usd"], rec["cost_source"] = self._cost(a, usage, rh)
        if not (isinstance(content, str) and content.strip()) and not calls:
            raise EmptyCompletion(f"{a.name}: empty completion", rec)
        rec["content"] = strip_reasoning(content)
        return rec

    @staticmethod
    def _cost(a, usage, headers):
        if a.cost == "zero":
            return 0.0, "zero"
        if a.cost == "reported":
            for v, src in (((usage or {}).get("cost"), "reported:body"),
                           (headers.get("x-litellm-response-cost"), "reported:header")):
                try:
                    if v is not None:
                        return float(v), src
                except (TypeError, ValueError):
                    pass
            return None, "unknown"      # the endpoint said nothing: never counted as zero
        if a.cost == "table":
            if not usage:
                return None, "unknown"  # no token counts to price
            p = a.price
            return ((usage.get("prompt_tokens") or 0) * p["input_per_mtok"]
                    + (usage.get("completion_tokens") or 0) * p["output_per_mtok"]) / 1e6, "table"
        return None, "unknown"

    @contextlib.contextmanager
    def _span(self, a):
        if self.tracer is None:
            yield None
            return
        with self.tracer.start_as_current_span("model.chat", attributes={
                "gen_ai.operation.name": "chat", "gen_ai.system": a.kind, "gen_ai.request.model": a.model,
                "flotilla.model.alias": a.name, "flotilla.model.endpoint": a.endpoint}) as span:
            yield span

    @staticmethod
    def _annotate(span, rec, error=None):
        if span is None:
            return
        u = rec.get("usage") or {}
        for k, v in (("gen_ai.response.model", rec.get("model")), ("gen_ai.usage.input_tokens", u.get("prompt_tokens")),
                     ("gen_ai.usage.output_tokens", u.get("completion_tokens")),
                     ("gen_ai.response.finish_reasons", [rec["finish_reason"]] if rec.get("finish_reason") else None),
                     ("http.response.status_code", rec.get("status")), ("flotilla.model.deployment_id", rec.get("model_id")),
                     ("flotilla.model.cost_usd", rec.get("cost_usd")), ("flotilla.model.cost_source", rec.get("cost_source")),
                     ("flotilla.model.latency_s", rec.get("latency_s"))):
            if v is not None:
                span.set_attribute(k, v)
        if error is not None:
            span.set_attribute("error.type", type(error).__name__)
            span.record_exception(error)
            try:
                from opentelemetry.trace import Status, StatusCode
                span.set_status(Status(StatusCode.ERROR, str(error)[:200]))
            except ImportError:
                pass
