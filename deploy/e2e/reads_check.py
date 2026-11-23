#!/usr/bin/env python3
"""The e2e's reads section's store checks (RFC-0014 section 4.8), on a consistent copy of flotillad's store (store_snapshot.py backup).

  reads_check.py record DB RUN_ID          the live run's input_reads row: complete, the two reads in order, the long result whole
  reads_check.py shadow DB ROUTINE VERSION the shadow run of that version: its row in shadow_runs and the run it made, one line (key=value)
  reads_check.py mcpcalls                  (stdin: the stub MCP server's log) which tools it was asked to call, and how many requests were refused or sessionless

Prints one line and exits 0 when the check holds, 1 when it does not (the line says what was found).
"""
import json
import sqlite3
import sys

LONG = "0123456789" * 1200   # what the stub's get_log answers (deploy/e2e/mcp_stub.py): 12000 characters, over the 8192 an episode's tool record keeps


def con(db):
    return sqlite3.connect("file:%s?mode=ro" % db, uri=True)


def record(db, run_id):
    row = con(db).execute("SELECT lines, complete FROM input_reads WHERE run_id = ?", (run_id,)).fetchone()
    if row is None:
        return False, "no input_reads row for %s" % run_id
    lines, complete = json.loads(row[0]), row[1]
    if complete != 1:
        return False, "the record is not complete (%s)" % complete
    got = [(l.get("tool"), l.get("args")) for l in lines]
    if got != [("lab.get_page", {"id": 1}), ("lab.get_log", {})]:
        return False, "the reads recorded are %s" % got
    page, log = lines
    if page.get("result") != "page 1 of the lab wiki" or page.get("ok") is not True or page.get("station") != "answer":
        return False, "the page line: %s" % json.dumps(page)[:200]
    if not (log.get("ok") is True and log.get("cut") is False and log.get("chars") == len(LONG) == len(log.get("result", "")) and log["result"] == LONG):
        return False, "the log line is not whole: chars=%s cut=%s len(result)=%s" % (log.get("chars"), log.get("cut"), len(log.get("result", "")))
    if not all(isinstance(l.get("at"), str) and isinstance(l.get("duration_s"), (int, float)) for l in lines):
        return False, "a line lacks its time"
    return True, "complete, 2 reads in order (%s), get_log whole: %d chars, not cut, not hashed" % (", ".join(l["tool"] for l in lines), log["chars"])


def shadow(db, routine, version):
    c = con(db)
    row = c.execute("SELECT status, reason, run_id FROM shadow_runs WHERE routine = ? AND version = ?", (routine, int(version))).fetchone()
    if row is None:
        return False, "status=none"
    status, reason, run_id = row
    run = c.execute("SELECT status, exit_code, detail FROM runs WHERE id = ?", (run_id,)).fetchone() if run_id else None
    n = c.execute("SELECT COUNT(*) FROM input_reads WHERE run_id = ?", (run_id,)).fetchone()[0] if run_id else 0
    detail = ((run[2] if run else "") or "").strip().splitlines()
    # the run's detail is its failure's text (a traceback): the last line says what failed; it is last here so it can hold any character
    return True, "status=%s run=%s run_status=%s exit=%s reason_chars=%d input_reads_rows=%d detail_last=%s" % (
        status, run_id or "-", run[0] if run else "-", run[1] if run else "-", len(reason), n, detail[-1] if detail else "")


def mcpcalls(text):
    """The stub's log: the tools it was asked to call, in order, how many requests it refused (no or a wrong token), and which methods came
    without a session. Always true (the line says what was found); the e2e compares it with what it expects, and with itself over time."""
    reqs = []
    for line in text.splitlines():
        try:
            d = json.loads(line)
        except ValueError:
            continue
        if isinstance(d, dict) and d.get("event") == "request":
            reqs.append(d)
    tools = [r["tool"] for r in reqs if r.get("method") == "tools/call"]
    nosession = sorted({r["method"] for r in reqs if not r.get("session")})
    return True, "CALLS %s UNAUTH %d NOSESSION %s" % (",".join(tools) or "-", sum(1 for r in reqs if not r.get("authed")), ",".join(nosession) or "-")


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "mcpcalls" and len(sys.argv) == 2:
        ok, msg = mcpcalls(sys.stdin.read())
    elif cmd == "record" and len(sys.argv) == 4:
        ok, msg = record(sys.argv[2], sys.argv[3])
    elif cmd == "shadow" and len(sys.argv) == 5:
        ok, msg = shadow(sys.argv[2], sys.argv[3], sys.argv[4])
    else:
        sys.exit(__doc__)
    print(("PASS " if ok else "FAIL ") + msg)
    sys.exit(0 if ok else 1)
