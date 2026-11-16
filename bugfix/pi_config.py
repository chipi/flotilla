"""bugfix/pi_config.py: pi's model configuration for one run, from FLOTILLA_MODELS (RFC-0010 §4).

pi reads its providers from <PI_CODING_AGENT_DIR>/models.json. A run writes one into its own temporary
directory with only the aliases its routine names (security T4): each endpoint becomes a provider
`flotilla-<endpoint>` speaking OpenAI completions at the endpoint's base URL. The key is named, never
written: `$VAR` for an endpoint's key_env (the worker fills it from the fleet's secrets), `!cat <file>`
for its key_file. pi is then called with --provider and --model from `pick`.

Each model also gets what pi's own catalog knows about the model LiteLLM routes it to (`upstream`, e.g.
fleet-bugfix-flash -> deepseek/deepseek-v4-flash): its context window, output limit, prices and compat.
Without them pi assumes a 128k window and 16k output and reports no cost; a fix session on orrery #646
stopped at 124k of context with one output token (2026-11-15), where the bake-off's openrouter provider
had the model's 1M window.
"""
import json
import os
import subprocess


class ConfigError(Exception):
    pass


def models_json(cfg, aliases, meta=None, upstream=None, min_output=None):
    """The models.json for these aliases of the models section `cfg` (FLOTILLA_MODELS, parsed); `meta` is
    pi's catalog entry per upstream model, `upstream` the upstream model per LiteLLM model. `min_output` is a
    floor for a model's reply limit (maxTokens): pi's catalog gives deepseek-v4-flash 4096, the pinned providers
    allow 131072 and more, and flash spent all 4096 on reasoning and was cut off before its answer (5 eval runs of
    2026-11-16; a verdict cut off so ends the case "no verdict")."""
    meta, upstream = meta or {}, upstream or {}
    providers = {}
    for name in aliases:
        a = cfg.get("aliases", {}).get(name)
        if not a:
            raise ConfigError(f"alias {name!r} is not in FLOTILLA_MODELS: the routine's models list names it?")
        ep_name = a["endpoint"]
        ep = cfg.get("endpoints", {}).get(ep_name)
        if not ep:
            raise ConfigError(f"alias {name!r}: endpoint {ep_name!r} is not in FLOTILLA_MODELS")
        p = providers.setdefault("flotilla-" + ep_name, {"baseUrl": ep["base_url"], "api": "openai-completions", "models": []})
        if ep.get("key_env"):
            p["apiKey"] = "$" + ep["key_env"]
        elif ep.get("key_file"):
            p["apiKey"] = "!cat " + ep["key_file"]
        else:
            p["apiKey"] = "none"  # pi wants one; the endpoint takes no Authorization
        if a["model"] not in [m["id"] for m in p["models"]]:
            entry = {"id": a["model"]}
            entry.update(meta.get(upstream.get(a["model"], ""), {}))
            if min_output:
                entry["maxTokens"] = max(int(entry.get("maxTokens") or 0), min_output)
            p["models"].append(entry)
    return {"providers": providers}


def pick(cfg, alias):
    """(provider, model) to pass pi for an alias."""
    a = cfg.get("aliases", {}).get(alias)
    if not a:
        raise ConfigError(f"alias {alias!r} is not in FLOTILLA_MODELS")
    return "flotilla-" + a["endpoint"], a["model"]


def catalog(ids, environ=None):
    """pi's catalog entries for these OpenRouter model ids (bugfix/pi_catalog.mjs, from the installed pi)."""
    if not ids:
        return {}
    script = os.path.join(os.path.dirname(os.path.abspath(__file__)), "pi_catalog.mjs")
    p = subprocess.run(["node", script, *ids], capture_output=True, text=True, timeout=60, env=environ)
    if p.returncode != 0:
        raise ConfigError(f"pi's catalog: {p.stderr.strip()[:200]}")
    return json.loads(p.stdout)


def write(agent_dir, aliases, environ=None, upstream=None, meta=None, min_output=None):
    """Write models.json into agent_dir for these aliases; returns the parsed models section. `upstream`
    maps a LiteLLM model to the OpenRouter model it routes to; `meta` (tests) stands in for pi's catalog."""
    environ = os.environ if environ is None else environ
    upstream = upstream or {}
    raw = environ.get("FLOTILLA_MODELS")
    if not raw:
        raise ConfigError("FLOTILLA_MODELS is not set: this installation's flotillad.yaml has no models section")
    cfg = json.loads(raw)
    os.makedirs(agent_dir, exist_ok=True)
    path = os.path.join(agent_dir, "models.json")
    with open(path, "w") as f:
        if meta is None:
            meta = catalog(sorted(set(upstream.values())), environ)
        json.dump(models_json(cfg, aliases, meta, upstream, min_output), f, indent=2)
    os.chmod(path, 0o600)
    return cfg
