"""bugfix/spend.py: what one bugfix run cost, for flotillad (the routine's spend_file: one number, USD).

The bill is the gateway's: LiteLLM's spend of the key each endpoint the run's aliases use, read before the run and
after it (/key/info). pi's own figure is its catalog's list price, not what the gateway charged: on 2026-11-15 it was
5 to 7 times the bill (fleet-eval-data projects/bugfix-cost/PLAN.md). The spend after a run lags its last calls, so it
is read until two reads in a row agree.

A key that another run spends on meanwhile makes the difference larger, never smaller: a routine's runs never
overlap (the worker runs one per routine), and a key per routine makes the difference exact. When the gateway cannot
be read, the run reports pi's figure, which is high, so a limit acts early rather than late.
"""
import glob
import json
import os
import time
import urllib.request


def read_spend(key, base_url, timeout=15.0):
    """The key's spend from LiteLLM's /key/info, None when it cannot be read. The key goes in the Authorization
    header only: LiteLLM's access log prints URLs."""
    url = base_url.rstrip("/").removesuffix("/v1") + "/key/info"
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {key}"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            doc = json.load(r)
        spend = (doc.get("info") or doc).get("spend")
        return float(spend) if spend is not None else None
    except (OSError, ValueError, AttributeError, TypeError):
        return None


def keys_of(cfg, aliases, environ):
    """{base_url: key} for the endpoints these aliases use (FLOTILLA_MODELS, parsed); an endpoint without a key
    is left out, it cannot be asked."""
    out = {}
    for name in aliases:
        ep = (cfg.get("endpoints") or {}).get(((cfg.get("aliases") or {}).get(name) or {}).get("endpoint"))
        if not ep or not ep.get("base_url"):
            continue
        key = environ.get(ep["key_env"], "") if ep.get("key_env") else ""
        if not key and ep.get("key_file"):
            try:
                with open(ep["key_file"]) as f:
                    key = f.read().strip()
            except OSError:
                key = ""
        if key:
            out[ep["base_url"]] = key
    return out


def pi_cost(agent_dir):
    """pi's own figure for every session under agent_dir (usage.cost.total of each reply)."""
    total = 0.0
    for f in glob.glob(os.path.join(agent_dir, "sessions", "*", "*.jsonl")):
        try:
            with open(f) as fh:
                for line in fh:
                    m = (json.loads(line).get("message") or {})
                    if m.get("role") == "assistant":
                        total += float(((m.get("usage") or {}).get("cost") or {}).get("total") or 0)
        except (OSError, ValueError, TypeError):
            continue
    return total


class Meter:
    def __init__(self, keys, poll_s=5.0, settle_s=60.0, read=read_spend):
        self.keys, self.poll_s, self.settle_s, self.read = keys, poll_s, settle_s, read
        self.before = {b: read(k, b) for b, k in keys.items()}

    def settled(self, base):
        """The key's spend once two reads in a row agree (or the last read at settle_s), None when unreadable."""
        t0 = time.monotonic()
        last = self.read(self.keys[base], base)
        while last is not None and time.monotonic() - t0 < self.settle_s:
            time.sleep(self.poll_s)
            cur = self.read(self.keys[base], base)
            if cur is None or cur == last:
                return last
            last = cur
        return last

    def cost(self, agent_dir):
        """(USD, source, note): the gateway's bill when every key could be read before and after, else pi's figure."""
        if not self.keys:
            return pi_cost(agent_dir), "pi", "no endpoint key to read the gateway's spend with"
        total = 0.0
        for base in self.keys:
            before, after = self.before[base], (self.settled(base) if self.before[base] is not None else None)
            if before is None or after is None or after < before:
                why = "unreadable" if before is None or after is None else "went down (a budget reset?)"
                return pi_cost(agent_dir), "pi", f"the gateway's spend at {base} {why}"
            total += after - before
        return total, "gateway", ""


def write(path, usd):
    """The routine's spend_file: one number, read and truncated by the worker."""
    with open(path, "w") as f:
        f.write(f"{usd:.6f}\n")
