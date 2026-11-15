"""bugfix/pi_config.py: pi's model configuration for one run, from FLOTILLA_MODELS (RFC-0010 §4).

pi reads its providers from <PI_CODING_AGENT_DIR>/models.json. A run writes one into its own temporary
directory with only the aliases its routine names (security T4): each endpoint becomes a provider
`flotilla-<endpoint>` speaking OpenAI completions at the endpoint's base URL. The key is named, never
written: `$VAR` for an endpoint's key_env (the worker fills it from the fleet's secrets), `!cat <file>`
for its key_file. pi is then called with --provider and --model from `pick`.
"""
import json
import os


class ConfigError(Exception):
    pass


def models_json(cfg, aliases):
    """The models.json for these aliases of the models section `cfg` (FLOTILLA_MODELS, parsed)."""
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
            p["models"].append({"id": a["model"]})
    return {"providers": providers}


def pick(cfg, alias):
    """(provider, model) to pass pi for an alias."""
    a = cfg.get("aliases", {}).get(alias)
    if not a:
        raise ConfigError(f"alias {alias!r} is not in FLOTILLA_MODELS")
    return "flotilla-" + a["endpoint"], a["model"]


def write(agent_dir, aliases, environ=None):
    """Write models.json into agent_dir for these aliases; returns the parsed models section."""
    environ = os.environ if environ is None else environ
    raw = environ.get("FLOTILLA_MODELS")
    if not raw:
        raise ConfigError("FLOTILLA_MODELS is not set: this installation's flotillad.yaml has no models section")
    cfg = json.loads(raw)
    os.makedirs(agent_dir, exist_ok=True)
    path = os.path.join(agent_dir, "models.json")
    with open(path, "w") as f:
        json.dump(models_json(cfg, aliases), f, indent=2)
    os.chmod(path, 0o600)
    return cfg
