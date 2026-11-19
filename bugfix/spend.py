"""bugfix/spend.py: what one bugfix run cost, for flotillad (the routine's spend_file: one number, USD).

The bill is the gateway's: LiteLLM's spend of each key the run's aliases use, read before the run and after it
(/key/info). pi's own figure is its catalog's list price, not what the gateway charged: on the six shop sessions of
2026-11-15 it was 5 to 7 times the bill, on #335's run of 2026-11-16 1.8 times (the installation's research repo
projects/bugfix-cost/PLAN.md). Not established for every day and provider: the 11-16 K0 run's harness total
($1.49, pi's figures, a timed-out item counted as 0) was under the gateway's $3.12. The spend after a run lags its
last calls, so it is read until two reads in a row agree.

A key that another run spends on meanwhile makes the difference larger, never smaller: a routine's runs never
overlap (the worker runs one per routine), and a key per routine makes the difference exact. When the gateway cannot
be read in time, the run reports pi's figure, plus the estimates of the episodes of any other harness
(episode_cost: a harness's own figure for what it spent, which is never the bill and is labelled so).
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


def pi_cost(agent_dir, baseline=0.0):
    """pi's own figure for every session under agent_dir (usage.cost.total of each reply), less `baseline`: what a run
    resumed from a checkpoint found there from its earlier run (which reported it)."""
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
    return max(total - baseline, 0.0)


def episode_cost(run_dir, baseline=0.0):
    """The harness's own estimate (cost_est) summed over the episode files of the run's non-pi harnesses
    (<run_dir>/episodes/*.json, runtime/adapter.py), less `baseline`: what a run resumed from a checkpoint found there
    from its earlier run. An estimate, used only where the gateway's bill could not be read; an episode with none counts 0."""
    total = 0.0
    for f in glob.glob(os.path.join(run_dir, "episodes", "*.json")):
        try:
            with open(f) as fh:
                doc = json.load(fh)
            if doc.get("harness") != "pi":
                total += max(float(doc.get("cost_est") or 0), 0.0)
        except (OSError, ValueError, TypeError, AttributeError):
            continue
    return max(total - baseline, 0.0)


class Meter:
    def __init__(self, keys, poll_s=5.0, settle_s=60.0, read=read_spend, read_timeout=5.0, stable_s=20.0):
        self.keys, self.poll_s, self.settle_s, self.read, self.read_timeout = list(keys), poll_s, settle_s, read, read_timeout
        self.stable_s = stable_s
        self.before = [read(k, b, read_timeout) for b, k in self.keys]
        # each key's settled spend at the last episode's end (the run's start before one): the next episode's bill is
        # measured from it. None: unknown (an unreadable key), the next episode's bill is then not measurable either.
        self.mark = list(self.before)

    def hurry(self, within_s=6.0, read_timeout=2.0, poll_s=1.0):
        """The worker's SIGTERM, before or during cost(): the reads and the settle still to come end within within_s of
        now, inside the worker's 10 s grace before its SIGKILL. A read already under way keeps its own timeout
        (read_timeout, 5 s by default); fix.py writes pi's figure before the reads, so a SIGKILL during one still
        leaves a number. Safe from a signal handler."""
        self.read_timeout, self.poll_s = min(self.read_timeout, read_timeout), min(self.poll_s, poll_s)
        self.deadline = min(self.deadline, time.monotonic() + within_s)
        self.hurried = True

    deadline = float("inf")
    hurried = False

    def left(self):
        return self.deadline - time.monotonic()

    def settled(self, base, key):
        """The key's spend once it has held still for stable_s (or the last read at settle_s), None when unreadable or
        past the deadline. Two reads 5 s apart were not enough: on 2026-11-16 they agreed before LiteLLM had recorded a
        run's last call (#335's review, $0.009051 of $0.060623), which the run then did not report."""
        t0 = time.monotonic()
        last = self.read(key, base, max(min(self.read_timeout, self.left()), 0.1))
        since = time.monotonic()
        while last is not None:
            now = time.monotonic()
            if now - since >= self.stable_s or now - t0 >= self.settle_s:
                return last
            if self.left() <= self.poll_s:
                # hurried (the worker's SIGTERM): no time for stable_s; the last reading, best effort (it can miss the
                # run's last calls; the key's daily max_budget at the gateway stays the hard stop)
                return last if self.hurried else None
            time.sleep(self.poll_s)
            cur = self.read(key, base, max(min(self.read_timeout, self.left()), 0.1))
            if cur is None:
                return None
            if cur != last:
                last, since = cur, time.monotonic()
        return None

    def episode(self):
        """The gateway's bill for what ran since the last mark (the previous episode's end, or the run's start), in USD:
        each key's spend once settled (settled(): at least stable_s, 20 s by default, of wall time, the price of an
        episode's own cost), minus the key's spend at the mark; the mark moves to it. None, with the mark moved or made
        unknown, when a key could not be read in time, went down (a budget reset), or had no mark: a bill is never
        guessed, and never carried into the next episode's. A key per routine, and a run's episodes never
        overlapping, make the difference this episode's own."""
        if not self.keys:
            return None
        total, known = 0.0, True
        for i, (base, key) in enumerate(self.keys):
            before, after = self.mark[i], self.settled(base, key)
            self.mark[i] = after
            if before is None or after is None or after < before:
                known = False
            else:
                total += after - before
        return total if known else None

    def cost(self, agent_dir, quiet=False, baseline=0.0, estimate=0.0):
        """(USD, source, note): the gateway's bill when every key could be read before and after, before the deadline
        (hurry), else pi's figure plus `estimate`, the other harnesses' (episode_cost), with the source "harness-estimate"
        when there is any. `quiet` says nothing has spent since the last episode()'s mark (every episode of the
        run was measured and none is in flight): the mark is then the settled "after" and nothing is waited for again,
        so the run's total is exactly the sum of its episodes'. A key with no mark is settled as usual."""
        if not self.keys:
            return pi_cost(agent_dir, baseline) + estimate, "harness-estimate" if estimate else "pi", \
                "no endpoint key to read the gateway's spend with"
        total = 0.0
        for i, ((base, key), before) in enumerate(zip(self.keys, self.before)):
            if before is None:
                after = None
            elif quiet and self.mark[i] is not None:
                after = self.mark[i]
            else:
                after = self.settled(base, key)
            if before is None or after is None or after < before:
                why = "unreadable in time" if before is None or after is None else "went down (a budget reset?)"
                return pi_cost(agent_dir, baseline) + estimate, "harness-estimate" if estimate else "pi", \
                    f"the gateway's spend at {base} {why}"
            total += after - before
        return total, "gateway", ""


def write(path, usd):
    """The routine's spend_file: one number, read and truncated by the worker. Written to a temporary file and
    renamed over it, so a kill mid-write never leaves an empty or half-written number."""
    tmp = f"{path}.tmp"
    with open(tmp, "w") as f:
        f.write(f"{usd:.6f}\n")
    os.replace(tmp, path)
