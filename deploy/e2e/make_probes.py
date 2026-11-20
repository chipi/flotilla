#!/usr/bin/env python3
"""The e2e's run-token probes, one routine per worker class: fleets/e2e/token-probe-<worker>.yaml in SPECS_DIR.

  make_probes.py SPECS_DIR WORKER[:WORKER_TOKEN_FILE] ...

A probe is a routine run by hand (enabled: false, so no timer starts it) on that worker. Its command, under run_tokens: required (the default),
fails unless all of these hold:
  - FLOTILLA_API_TOKEN_FILE names the run's own file (.flotilla-token-*, mode 600), not the worker's;
  - the worker's own token on flotillad's workers' listener is refused (401);
  - the run's token reads the ledger (200);
  - a filing that names another namespace and another run's id is filed under the run's own routine.
It prints PROBE PASS or the problems. The remote worker's probe is the same with its own token file (/remote/worker.token).
"""
import os
import sys

TEMPLATE = '''fleet: e2e
routine: token-probe-{name}
description: >-
  The e2e's run-token probe on the {worker} worker, never shipped: its run reads FLOTILLA_API_TOKEN_FILE, which must be its own file and not the
  worker's; the worker's own token must be refused on flotillad's workers' listener and the run's own accepted; a filing naming another routine's
  namespace and another run's id must land under this routine's own. Disabled, so no timer starts it; the e2e runs it by hand.
enabled: false
worker: {worker}
trigger:
  every: 60m
run:
  command: |
    python3 - <<'PY'
    import json, os, stat, sys, urllib.error, urllib.request
    base = "http://flotillad:8485"
    path = os.environ["FLOTILLA_API_TOKEN_FILE"]
    wt_path = "{worker_token}"
    tok = open(path).read().strip()
    problems = []
    if path == wt_path or not os.path.basename(path).startswith(".flotilla-token-"):
        problems.append("the run was given the worker's token file")
    if stat.S_IMODE(os.stat(path).st_mode) != 0o600:
        problems.append("the run's token file is not mode 600")
    def call(method, url, token, body=None):
        r = urllib.request.Request(base + url, method=method, data=json.dumps(body).encode() if body is not None else None,
                                   headers={{"Authorization": "Bearer " + token, "Content-Type": "application/json"}})
        try:
            with urllib.request.urlopen(r, timeout=10) as x:
                return x.status, json.load(x)
        except urllib.error.HTTPError as e:
            return e.code, {{}}
    wt = open(wt_path).read().strip() if os.path.exists(wt_path) else ""
    if not wt:
        problems.append("the worker's own token is unreadable here, so its refusal cannot be shown")
    elif call("GET", "/v1/ledger/counts", wt)[0] != 401:
        problems.append("the worker's own token opened the listener")
    if call("GET", "/v1/ledger/counts", tok)[0] != 200:
        problems.append("the run's own token was refused")
    call("POST", "/v1/ledger/filings?ns=bugfix/fix", tok, {{"row": {{"fingerprint": "probe-{name}", "repo": "o/r", "issue": "1"}}, "run_id": "someone-elses"}})
    own = call("GET", "/v1/ledger/filings", tok)[1].get("filings", [])
    named = call("GET", "/v1/ledger/filings?ns=bugfix/fix", tok)[1].get("filings", [])
    if len(own) != 1 or len(named) != 1:
        problems.append("the namespace the run named was used (own %d, named %d)" % (len(own), len(named)))
    print("PROBE {name}", "PASS" if not problems else "FAIL " + "; ".join(problems))
    sys.exit(1 if problems else 0)
    PY
  workdir: .
  timeout: 1m
autonomy: record
budget:
  day_usd: 0
freshness: 120m
'''


def main(argv):
    if len(argv) < 3:
        print(__doc__)
        return 2
    out = os.path.join(argv[1], "e2e")
    os.makedirs(out, exist_ok=True)
    for arg in argv[2:]:
        worker, _, token = arg.partition(":")
        name = worker
        with open(os.path.join(out, "token-probe-%s.yaml" % name), "w") as f:
            f.write(TEMPLATE.format(name=name, worker=worker, worker_token=token or "/run/flotilla/worker.token"))
        print(name)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
