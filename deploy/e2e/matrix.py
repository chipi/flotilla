#!/usr/bin/env python3
"""The no-effect matrix of the example fleets (docs/wip/v2-design.md section 4, build step 5), run by deploy/e2e/run.sh on the host.

  matrix.py ROUTINES_FILE        one fleet/routine per line: the routines that have a generated shadow v2 (make_shadows.py)

Environment: DOCKER_CONTEXT_DEV, TAG (the images'), WORK (the e2e's folder), ROOT (the repo), E2E_MUTANT (a mutation check's name).

For every routine, one row of cells, each PASS, SKIP (with its reason) or FAIL:

  versions   v1 is live and v2 is a shadow with a hash of its own
  shadow     a shadow run of v2 on the live run's input ended done and recorded a decision of its own (its report, plan or action: not only that it ran), so the
             equality below is not the shadow doing nothing
  unchanged  across every shadow run together, nothing the routine's live side holds changed: its rows in every store table that are not a
             shadow's own (storediff.py), the worker's state files, the cases, what the outward stand-ins saw from the shadow's proxy (no write)
  promote    v2 promoted: v1 previous, v2 live
  rollback   rolled back: v1 live again, v2 marked rolled back
  previous   the image runs the previous version: a run of the routine after the rollback is v1's and ends ok
  state      backward-readable state: the routine's stores gain a key and a column v1 does not know (extend_state.py), v1 runs again, and its exit
             and the cases it decides are those of the run before

and the ones that are not routines' alone: the brief, the Slack asks and the host agent's log, as before and after.
Output: the rows, then "NOEFFECT PASS n / SKIP m (reasons) / FAIL k"; exit 1 on a FAIL.
"""
import json
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import sqlite3  # noqa: E402
import storediff  # noqa: E402

CTX = os.environ.get("DOCKER_CONTEXT_DEV", "dev")
TAG = os.environ.get("TAG", "e2e-local")
WORK = os.environ["WORK"]
ROOT = os.environ["ROOT"]
MUTANTS = [m for m in os.environ.get("E2E_MUTANT", "").split(",") if m]
WORKER_ROUTINE_STATE = ["/tmp/flotilla-example"] + (["/mut"] if "matrix-live-write" in MUTANTS else [])
DB = os.path.join(WORK, "matrix-store.db")

PASS, SKIP, FAIL = "PASS", "SKIP", "FAIL"
cells = []   # (routine, step, verdict, note)
STEPS = ["versions", "shadow", "unchanged", "promote", "rollback", "previous", "state", "brief", "slack", "hosts"]


def docker(*args, stdin=None, timeout=300):
    p = subprocess.run(["docker", "--context", CTX] + list(args), capture_output=True, text=True, input=stdin, timeout=timeout)
    return p.returncode, p.stdout + p.stderr


def cli(*args, timeout=120):
    return docker("exec", "flotillad", "/usr/local/bin/flotilla", *args, timeout=timeout)


def cli_json(*args):
    rc, out = cli(*args)
    try:
        return json.loads(out)
    except ValueError:
        return None


def cell(routine, step, verdict, note=""):
    cells.append((routine, step, verdict, note))


def versions(r):
    v = cli_json("versions", r, "-json")
    return {x["version"]: x for x in v["versions"]} if v else {}


def snapshot():
    p = subprocess.run(["docker", "--context", CTX, "run", "--rm", "--network", "none", "--user", "65532:65532", "-e", "STORE_DIR=/v",
                        "-v", "flotilla-e2e-state:/v", "-v", ROOT + "/deploy/server/store_snapshot.py:/s.py:ro",
                        "local/flotilla-worker-triage:" + TAG, "python3", "/s.py", "backup"], capture_output=True)
    if p.returncode != 0:
        raise RuntimeError("store snapshot: " + p.stderr.decode()[:200])
    with open(DB, "wb") as f:
        f.write(p.stdout)


def state_hash():
    script = "for d in %s; do (cd $d 2>/dev/null && find . -type f | sort | xargs -r sha256sum); done" % " ".join(WORKER_ROUTINE_STATE)
    rc, out = docker("exec", "flotilla-worker-ops-read", "sh", "-c", script)
    return out


def table_state(db, tables):
    """(rows, hash) of whole tables of a store copy, in canonical form: what the brief and the Slack asks are."""
    import hashlib
    con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    rows = []
    for t in tables:
        rows += [json.dumps([t, list(r)], default=str) for r in con.execute('SELECT * FROM "%s"' % t)]
    rows.sort()
    return len(rows), hashlib.sha256("\n".join(rows).encode()).hexdigest()[:16]


def host_state():
    """The host agent stand-in's record, from the volume its logs are on (the stand-in is removed at the end of its section; the volume stays): the
    lines of its audit log (every execution) and the plans it was asked to post to Slack (it is no connector, and logs each one)."""
    rc, out = docker("run", "--rm", "--network", "none", "--user", "0", "--entrypoint", "sh", "-v", "flotilla-e2e-host-audit:/a:ro",
                     "local/flotilla-worker-triage:" + TAG, "-c", "wc -l < /a/audit.jsonl; grep -c 'not its Slack connector' /a/agent.log")
    parts = out.split()
    return (int(parts[0]), int(parts[1])) if rc == 0 and len(parts) == 2 and all(x.isdigit() for x in parts) else None


def shadow_ip():
    rc, out = docker("inspect", "flotilla-egress-shadow", "--format", '{{(index .NetworkSettings.Networks "e2e-backend").IPAddress}}')
    return out.strip()


def world_writes(ip):
    rc, out = docker("logs", "e2e-world")
    return sorted(l for l in out.split("\n") if "world WRITE" in l and ("client=%s " % ip) in l)


def case_hash():
    rc, out = cli("case", "list", "--all")
    return "\n".join(" ".join(l.split()[:3]) for l in out.split("\n")[1:] if l.strip())


def live_input(r):
    runs = cli_json("runs", r, "-json", "-n", "30")
    for x in (runs or {}).get("runs") or []:
        if not x.get("shadow") and x.get("status") == "ok":
            return x["id"]
    return ""


def shadow_ends(r):
    rc, out = docker("logs", "flotillad")
    return sum(1 for l in out.split("\n") if '"msg":"shadow run end"' in l and ('"routine":"%s@v2"' % r) in l)


def wait_run(r, before, timeout=90):
    for _ in range(timeout):
        runs = (cli_json("runs", r, "-json", "-n", "5") or {}).get("runs") or []
        if len(runs) > before and runs[0].get("status") not in ("running", "queued"):
            return runs[0]
        time.sleep(1)
    return None


def run_by_hand(r):
    before = len(((cli_json("runs", r, "-json", "-n", "200")) or {}).get("runs") or [])
    cli("run", r)
    return wait_run(r, before)


# What a shadow's own decision looks like in shadow_records (internal/dispatch/shadow.go): the monitor's report and what it would open, the case
# routine's plan, the action it would have run, a note. The run's start and end are written for every shadow run, a refused report says the
# routine wrote none, and a case routine's decision "none" says it wrote no plan: none of those shows the shadow did the routine's work.
DECISION_KINDS = ("report", "detect.open", "detect.attach", "detect.clear", "plan", "would_run", "note")


def decisions(con, r):
    n = 0
    for kind, payload in con.execute("SELECT kind, payload FROM shadow_records WHERE routine IN (?, ?)", (r, r + "@v2")):
        if kind in DECISION_KINDS:
            n += 1
        elif kind == "decision":
            try:
                n += 0 if json.loads(payload).get("outcome") == "none" else 1
            except ValueError:
                pass
    return n


def scheduled(r):
    """Whether a timer, not a case, starts the routine (its routine.yaml has trigger: every:)."""
    with open(os.path.join(WORK, "config", "specs", r, "routine.yaml")) as f:
        return any(l.strip().startswith("every:") for l in f)


def main(argv):
    routines = [l.strip() for l in open(argv[1]) if l.strip()]
    ip = shadow_ip()

    # versions
    for r in routines:
        v = versions(r)
        ok = bool(v) and v.get(1, {}).get("state") == "live" and v.get(2, {}).get("state") == "shadow" and v[1].get("hash") != v[2].get("hash") and v[2].get("hash")
        cell(r, "versions", PASS if ok else FAIL, "" if ok else "v1/v2 states: %s" % {k: x.get("state") for k, x in v.items()})

    # the shadow runs, all of them, beside one before and one after of everything the live side holds
    inputs = {r: live_input(r) for r in routines}
    snapshot()
    before = {r: storediff.dump(DB, r) for r in routines}
    brief0, slack0, host0 = table_state(DB, ["briefs", "brief_marks"]), table_state(DB, ["slack_asks"]), host_state()
    state0, writes0, cases0 = state_hash(), world_writes(ip), case_hash()
    ends0 = {r: shadow_ends(r) for r in routines}
    queued = []
    for r in routines:
        if not inputs[r]:
            cell(r, "shadow", SKIP, "no live run to take as its input: a routine that waits for a case has none on this stack")
            cell(r, "unchanged", SKIP, "no shadow run")
            continue
        rc, out = cli("versions", "shadow-run", r, "v2", "--input", inputs[r])
        if rc != 0:
            cell(r, "shadow", FAIL, "shadow-run refused: %s" % out.strip()[:160])
            cell(r, "unchanged", SKIP, "no shadow run")
            continue
        queued.append(r)
    deadline = time.time() + 300
    while time.time() < deadline and any(shadow_ends(r) <= ends0[r] for r in queued):
        time.sleep(2)
    snapshot()
    after = {r: storediff.dump(DB, r) for r in routines}
    con = sqlite3.connect("file:%s?mode=ro" % DB, uri=True)
    state1, writes1, cases1 = state_hash(), world_writes(ip), case_hash()
    brief1, slack1, host1 = table_state(DB, ["briefs", "brief_marks"]), table_state(DB, ["slack_asks"]), host_state()
    for name, a, b, what in (("brief", brief0, brief1, "the day's brief rows"), ("slack", slack0, slack1, "the Slack asks"),
                              ("hosts", host0, host1, "the host agent's audit log and the plans it was asked to post")):
        if a is None or b is None:
            cell("(all)", name, SKIP, "the host agent stand-in's log volume is not in this run")
        elif a[0] == 0 if name != "hosts" else a[0] == 0 or a[1] == 0:
            cell("(all)", name, SKIP, "%s: none exists in this stack before the shadows ran, so a comparison would prove nothing" % what)
        else:
            cell("(all)", name, PASS if a == b else FAIL, "%s: %s before, %s after" % (what, a, b) if a != b else "%s: %s" % (what, a))
    for r in queued:
        if shadow_ends(r) <= ends0[r]:
            cell(r, "shadow", FAIL, "the shadow run did not end in 300s")
            cell(r, "unchanged", SKIP, "no shadow run")
            continue
        done = con.execute("SELECT COUNT(*) FROM shadow_runs WHERE routine = ? AND version = 2 AND status = 'done'", (r,)).fetchone()[0]
        recs = decisions(con, r)
        if done >= 1 and recs >= 1:
            cell(r, "shadow", PASS, "%d records" % recs)
        else:
            cell(r, "shadow", FAIL, "shadow_runs done %d, decision records %d: a shadow that decided and recorded nothing proves nothing" % (done, recs))
        bad = []
        if [l for l in before[r] if not l.startswith("SHADOW")] != [l for l in after[r] if not l.startswith("SHADOW")]:
            bad.append("its rows in the store changed")
        if state0 != state1:
            bad.append("a worker state file changed")
        if cases0 != cases1:
            bad.append("the cases changed")
        if writes0 != writes1:
            bad.append("the shadow's proxy carried a write the stand-ins saw")
        cell(r, "unchanged", FAIL if bad else PASS, "; ".join(bad))

    # promote, roll back, and the previous version runs
    for r in routines:
        rc, out = cli("promote", r, "v2", "--autonomy", "record", "--reason", "e2e matrix: the shadow is promoted and rolled back", "--no-pairs")
        v = versions(r)
        ok = rc == 0 and v.get(1, {}).get("state") == "previous" and v.get(2, {}).get("state") == "live"
        cell(r, "promote", PASS if ok else FAIL, "" if ok else "%s / %s" % (out.strip()[:120], {k: x.get("state") for k, x in v.items()}))
        rc, out = cli("rollback", r, "--autonomy", "record", "--reason", "e2e matrix: back to v1")
        v = versions(r)
        ok = rc == 0 and v.get(1, {}).get("state") == "live" and v.get(2, {}).get("state") not in ("live", "shadow", "")
        cell(r, "rollback", PASS if ok else FAIL, "" if ok else "%s / %s" % (out.strip()[:120], {k: x.get("state") for k, x in v.items()}))
        if not scheduled(r):
            cell(r, "previous", SKIP, "it runs on a case: run by hand it has none, and would end in error whichever version it is")
            continue
        run = run_by_hand(r)
        if run is None:
            cell(r, "previous", FAIL, "no run after the rollback")
        else:
            ok = run.get("status") == "ok" and run.get("version") == 1
            cell(r, "previous", PASS if ok else FAIL, "" if ok else "status %s version %s" % (run.get("status"), run.get("version")))

    # backward-readable state: the business fleets' stores
    detectors = {"crm": "crm/scan", "campaigns": "campaigns/scan", "outbound": "outbound/scan", "renewals": "renewals/scan"}
    for r in routines:
        fleet = r.split("/")[0]
        if detectors.get(fleet) != r:
            cell(r, "state", SKIP, "the fleet's stores are read by its scan, whose row covers them" if fleet in detectors else "it keeps no state store of its own")
            continue
        a = run_by_hand(r)
        c1 = case_hash()
        b = run_by_hand(r)
        c2 = case_hash()
        with open(os.path.join(ROOT, "deploy/e2e/extend_state.py")) as f:
            rc, out = docker("exec", "-i", "flotilla-worker-ops-read", "python3", "-", "/tmp/flotilla-example/" + fleet, stdin=f.read())
        ext = [int(x) for x in out.split() if x.isdigit()]
        c = run_by_hand(r)
        c3 = case_hash()
        if not (a and b and c):
            cell(r, "state", FAIL, "a run did not end")
        elif not ext or ext[0] == 0 or ext[1] == 0:
            cell(r, "state", FAIL, "extend_state changed nothing (%s)" % out.strip()[:100])
        elif not (a["status"] == b["status"] == c["status"] == "ok"):
            cell(r, "state", FAIL, "run statuses %s %s %s" % (a["status"], b["status"], c["status"]))
        elif c1 != c2:
            cell(r, "state", SKIP, "two runs on the same state decided differently, so the comparison has no baseline")
        elif c2 != c3:
            cell(r, "state", FAIL, "v1 on the extended state decided differently")
        else:
            cell(r, "state", PASS, "%d records extended" % ext[1])

    return report(routines + ["(all)"])


def report(routines):
    npass = sum(1 for c in cells if c[2] == PASS)
    nskip = sum(1 for c in cells if c[2] == SKIP)
    nfail = sum(1 for c in cells if c[2] == FAIL)
    for r in routines:
        row = {c[1]: c for c in cells if c[0] == r}
        print("NOEFFECT %-26s %s" % (r, " ".join("%s=%s" % (s, row[s][2]) for s in STEPS if s in row)))
        for s in STEPS:
            if s in row and row[s][2] != PASS and row[s][3]:
                print("   %s %s: %s" % (s, row[s][2], row[s][3]))
    reasons = sorted({c[3] for c in cells if c[2] == SKIP})
    print("NOEFFECT PASS %d / SKIP %d (%s) / FAIL %d" % (npass, nskip, "; ".join(reasons), nfail))
    return 1 if nfail else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
