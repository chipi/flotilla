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
HOSTILE = ["e2e/hostile-brief"]   # its v2 reports a detection whose plan would wait for approval (offered to Slack) and a brief line
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


def table_state(db, tables, queries=()):
    """(rows, hash) of whole tables of a store copy, and of the rows some queries select, in canonical form: what the brief and the Slack asks are."""
    import hashlib
    con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    rows = []
    for t in tables:
        rows += [json.dumps([t, list(r)], default=str) for r in con.execute('SELECT * FROM "%s"' % t)]
    for q in queries:
        rows += [json.dumps([q, list(r)], default=str) for r in con.execute(q)]
    rows.sort()
    return len(rows), hashlib.sha256("\n".join(rows).encode()).hexdigest()[:16]


def brief_state():
    return table_state(DB, ["briefs", "brief_marks"], ["SELECT subject, detail FROM events WHERE kind = 'routine.summary'"])


def slack_state():
    return table_state(DB, ["slack_asks"], ["SELECT id, kind, status FROM cases WHERE status = 'awaiting_approval'"])


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


def case_hash(kind=None):
    """The cases (their ids and kinds; their status moves on its own as other routines work), of one kind or all."""
    rc, out = cli("case", "list", "--all")
    rows = [l.split()[:2] for l in out.split("\n")[1:] if l.strip()]
    return "\n".join(" ".join(x) for x in sorted(rows) if kind is None or x[1] == kind)


def settle(limit=240, quiet=20):
    """Waits until the cases and the Slack asks are the same twice, quiet seconds apart (flotillad plans a handed-over case and offers a
    waiting plan to Slack every 15 s); False if they still move after limit seconds."""
    prev, end = None, time.time() + limit
    while time.time() < end:
        snapshot()
        cur = (slack_state(), sorted(l for l in cli("case", "list", "--all")[1].split("\n")[1:] if l.strip()))
        if cur == prev:
            return True
        prev = cur
        time.sleep(quiet)
    return False


def live_input(r, taken=()):
    """The newest live run that ended ok and has no shadow run on it yet (a shadow run on an input is one per version; flotillad refuses a second)."""
    runs = cli_json("runs", r, "-json", "-n", "30")
    for x in (runs or {}).get("runs") or []:
        if not x.get("shadow") and x.get("status") == "ok" and x["id"] not in taken:
            return x["id"]
    return ""


def shadow_ends(r):
    rc, out = docker("logs", "flotillad")
    return sum(1 for l in out.split("\n") if '"msg":"shadow run end"' in l and ('"routine":"%s@v2"' % r) in l)


def newest_run(r):
    runs = (cli_json("runs", r, "-json", "-n", "5") or {}).get("runs") or []
    return runs[0] if runs else {}


def wait_run(r, before, timeout=120):
    for _ in range(timeout):
        run = newest_run(r)
        if run.get("id") != before and run.get("status") not in (None, "running", "queued"):
            return run
        time.sleep(1)
    return None


def run_by_hand(r):
    before = newest_run(r).get("id")
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


EXAMPLE_KIND_SKIP = ("e2e_", "failed_backup", "code_bug", "dependency_update", "signal", "process_change")
# The platform allows 10 executions an hour (policy's default; flotillad has no setting for it) and the one stack's sections spend them: the host
# agent 3, this drive 4, the meeting's two plumbing sends, then section `ask`'s first answer. Measured with case.executing: 10 before `ask` when the
# email plan below is approved, and `ask`'s plans were deferred ("10 of 10 executions platform-wide this hour"). crm/fix plans the contact's second
# case about ten seconds after the first round's plans, so approving it raced the loop's own exit (one CI run approved it, the next run on the same
# code did not). Its plan stays waiting, so the drive always executes 4.
UNAPPROVED_TARGET_SUFFIX = ("/email",)


def acting(r):
    """Whether the routine acts on cases (its routine.yaml caps its autonomy)."""
    with open(os.path.join(WORK, "config", "specs", r, "routine.yaml")) as f:
        return any(l.startswith("max_autonomy:") for l in f)


def drive(routines):
    """Give the routines that wait for a case a live run: their autonomy and their actions at propose, the monitors run, and every plan that
    waits for a person approved, round after round, so the acting routines run and so do their checks (the stand-in systems are JSON files).
    Done before the matrix's before-picture, so the shadows are compared on a settled stack."""
    with open(os.path.join(ROOT, "deploy/e2e/clone_records.py")) as f:
        rc, out = docker("exec", "-i", "flotilla-worker-ops-read", "python3", "-", "/tmp/flotilla-example", stdin=f.read())
    print("drive: " + out.strip())
    rc, out = cli("action", "list")
    for l in out.split("\n")[1:]:
        name = l.split()[0] if l.split() else ""
        if name and not name.startswith(("host.", "backup.", "github.", "repo.")):
            cli("action", "autonomy", name, "propose", "--reason", "e2e matrix: the examples' actions run, so their routines have a live run")
    for r in routines:
        if acting(r):
            cli("autonomy", r, "propose", "--reason", "e2e matrix: so a plan waits for approval and the routine after it runs")
    for r in routines:
        if scheduled(r):
            run_by_hand(r)
    for _ in range(8):
        rc, out = cli("case", "list", "--all")
        todo = [l.split()[0] for l in out.split("\n")[1:] if "awaiting_approval" in l and len(l.split()) > 2
                and not l.split()[1].startswith(EXAMPLE_KIND_SKIP) and not l.split()[2].endswith(UNAPPROVED_TARGET_SUFFIX)]
        if not todo:
            break
        for c in todo:
            cli("case", "approve", c)
        time.sleep(10)
    time.sleep(10)


def triage_state():
    """Backward-readable state for the triage fleet: its ledgers (dispositions and filed rows, tsv) gain a column v1 does not know, and the cycle
    runs again and decides as it did on the state before. The ledger entries with extra columns are these files."""
    r = "triage/cycle"
    def lines():
        rc, out = docker("exec", "flotilla-worker-triage", "sh", "-c", "cat /home/worker/signal-fleet/results/*.tsv | wc -l")
        return int(out.split()[0]) if out.split() and out.split()[0].isdigit() else -1
    a = run_by_hand(r)
    n0, c0 = lines(), case_hash()
    b = run_by_hand(r)
    n1, c1 = lines(), case_hash()
    with open(os.path.join(ROOT, "deploy/e2e/extend_state.py")) as f:
        rc, out = docker("exec", "-i", "flotilla-worker-triage", "python3", "-", "/home/worker/signal-fleet/results", stdin=f.read())
    ext = [int(x) for x in out.split() if x.isdigit()]
    n2 = lines()
    c = run_by_hand(r)
    n3, c3 = lines(), case_hash()
    if not (a and b and c):
        cell(r, "state", FAIL, "a run did not end")
    elif not ext or ext[1] == 0:
        cell(r, "state", FAIL, "extend_state changed nothing (%s)" % out.strip()[:100])
    elif not (a["status"] == b["status"] == c["status"] == "ok"):
        cell(r, "state", FAIL, "run statuses %s %s %s" % (a["status"], b["status"], c["status"]))
    elif c1 != c3 or (n3 - n2) != (n1 - n0):
        cell(r, "state", FAIL, "v1 on the extended ledgers decided differently (cases %r vs %r, lines added %d vs %d)" % (c1 != c3, False, n3 - n2, n1 - n0))
    else:
        cell(r, "state", PASS, "%d rows extended; the cycle added %d lines as before" % (ext[1], n3 - n2))


def main(argv):
    if argv[1] == "drive":
        import make_shadows
        drive(make_shadows.generate(os.path.join(WORK, "config", "specs"), {"ops-read"}, dry=True))
        return 0
    routines = [l.strip() for l in open(argv[1]) if l.strip()] + HOSTILE
    ip = shadow_ip()

    # versions
    for r in routines:
        v = versions(r)
        ok = bool(v) and v.get(1, {}).get("state") == "live" and v.get(2, {}).get("state") == "shadow" and v[1].get("hash") != v[2].get("hash") and v[2].get("hash")
        cell(r, "versions", PASS if ok else FAIL, "" if ok else "v1/v2 states: %s" % {k: x.get("state") for k, x in v.items()})

    # the shadow runs, all of them, beside one before and one after of everything the live side holds. The live runs of the cells above
    # still have work in flight (a case they opened is planned by its acting routine, then offered to Slack): the "before" waits until the
    # cases and the asks stop moving, or a live plan lands between the two and reads as a shadow's (2026-11-22: crm/fix planned 30 s after
    # crm/scan's case, inside the window)
    print("live side settled: %s" % settle())
    with open(os.path.join(ROOT, "deploy/e2e/plant_contact.py")) as f:   # the hostile shadow's detection has a contact to be about, no scan has seen
        rc, out = docker("exec", "-i", "flotilla-worker-ops-read", "python3", "-", "/tmp/flotilla-example", stdin=f.read())
    print("hostile: " + out.strip())
    snapshot()
    con0 = sqlite3.connect("file:%s?mode=ro" % DB, uri=True)
    taken = {row[0] for row in con0.execute("SELECT input_key FROM shadow_runs WHERE version = 2")}
    con0.close()
    inputs = {r: live_input(r, taken) for r in routines}
    before = {r: storediff.dump(DB, r) for r in routines}
    brief0, slack0, host0 = brief_state(), slack_state(), host_state()
    state0, writes0, cases0 = state_hash(), world_writes(ip), case_hash()
    ends0 = {r: shadow_ends(r) for r in routines}
    queued = []
    for r in routines:
        if not inputs[r]:
            cell(r, "shadow", SKIP, "no live run without a shadow beside it to take as its input (a routine that waits for a case runs only when its case reaches it)")
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
    time.sleep(20)   # flotillad offers the plans that wait for a person to Slack every 15 s
    snapshot()
    after = {r: storediff.dump(DB, r) for r in routines}
    con = sqlite3.connect("file:%s?mode=ro" % DB, uri=True)
    state1, writes1, cases1 = state_hash(), world_writes(ip), case_hash()
    brief1, slack1, host1 = brief_state(), slack_state(), host_state()
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
    kinds = {"crm": "crm_fix", "campaigns": "campaign_ready", "outbound": "prospect_new", "renewals": "renewal_risk"}
    for r in routines:
        fleet = r.split("/")[0]
        if detectors.get(fleet) != r:
            cell(r, "state", SKIP, "the fleet's stores are read by its scan, whose row covers them" if fleet in detectors else "it keeps no state store of its own")
            continue
        a = run_by_hand(r)
        c1 = case_hash(kinds[fleet])
        b = run_by_hand(r)
        c2 = case_hash(kinds[fleet])
        with open(os.path.join(ROOT, "deploy/e2e/extend_state.py")) as f:
            rc, out = docker("exec", "-i", "flotilla-worker-ops-read", "python3", "-", "/tmp/flotilla-example/" + fleet, stdin=f.read())
        ext = [int(x) for x in out.split() if x.isdigit()]
        c = run_by_hand(r)
        c3 = case_hash(kinds[fleet])
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

    cells[:] = [c for c in cells if c[0] != "triage/cycle"]
    triage_state()
    return report(routines + ["triage/cycle", "(all)"])


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
