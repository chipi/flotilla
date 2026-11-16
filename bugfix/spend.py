"""bugfix/spend.py: what one bugfix run cost, for flotillad (the routine's spend_file: one number, USD).

The bill is the gateway's: LiteLLM's spend of each key the run's aliases use, read before the run and after it
(/key/info). pi's own figure is its catalog's list price, not what the gateway charged: on the six orrery sessions of
2026-11-15 it was 5 to 7 times the bill, on #335's run of 2026-11-16 1.8 times (fleet-eval-data
projects/bugfix-cost/PLAN.md). Not established for every day and provider: the 11-16 K0 run's harness total
($1.49, pi's figures, a timed-out item counted as 0) was under the gateway's $3.12. The spend after a run lags its
last calls, so it is read until two reads in a row agree.

A key that another run spends on meanwhile makes the difference larger, never smaller: a routine's runs never
overlap (the worker runs one per routine), and a key per routine makes the difference exact. When the gateway cannot
be read in time, the run reports pi's figure.
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
    """[(base_url, key)] for the endpoints these aliases use (FLOTILLA_MODELS, parsed), each pair once; an endpoint
    without a key is left out, it cannot be asked. Two endpoints on one gateway with different keys are two pairs."""
    out = []
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
        if key and (ep["base_url"], key) not in out:
            out.append((ep["base_url"], key))
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
    def __init__(self, keys, poll_s=5.0, settle_s=60.0, read=read_spend, read_timeout=15.0):
        self.keys, self.poll_s, self.settle_s, self.read, self.read_timeout = list(keys), poll_s, settle_s, read, read_timeout
        self.before = [read(k, b, read_timeout) for b, k in self.keys]

    def hurry(self, within_s=6.0, read_timeout=2.0, poll_s=1.0):
        """The worker's SIGTERM, before or during cost(): what is left (reads, the settle, the fallback) must end
        within within_s of now, inside the worker's 10 s grace before its SIGKILL. Safe from a signal handler."""
        self.read_timeout, self.poll_s = min(self.read_timeout, read_timeout), min(self.poll_s, poll_s)
        self.deadline = min(self.deadline, time.monotonic() + within_s)

    deadline = float("inf")

    def left(self):
        return self.deadline - time.monotonic()

    def settled(self, base, key):
        """The key's spend once two reads in a row agree (or the last read at settle_s), None when unreadable or past
        the deadline."""
        t0 = time.monotonic()
        last = self.read(key, base, max(min(self.read_timeout, self.left()), 0.1))
        while last is not None and time.monotonic() - t0 < self.settle_s:
            if self.left() <= self.poll_s:
                return None
            time.sleep(self.poll_s)
            cur = self.read(key, base, max(min(self.read_timeout, self.left()), 0.1))
            if cur is None or cur == last:
                return last
            last = cur
        return last if self.left() > 0 else None

    def cost(self, agent_dir):
        """(USD, source, note): the gateway's bill when every key could be read before and after, before the deadline
        (hurry), else pi's figure."""
        if not self.keys:
            return pi_cost(agent_dir), "pi", "no endpoint key to read the gateway's spend with"
        total = 0.0
        for (base, key), before in zip(self.keys, self.before):
            after = self.settled(base, key) if before is not None else None
            if before is None or after is None or after < before:
                why = "unreadable in time" if before is None or after is None else "went down (a budget reset?)"
                return pi_cost(agent_dir), "pi", f"the gateway's spend at {base} {why}"
            total += after - before
        return total, "gateway", ""


def write(path, usd):
    """The routine's spend_file: one number, read and truncated by the worker."""
    with open(path, "w") as f:
        f.write(f"{usd:.6f}\n")
