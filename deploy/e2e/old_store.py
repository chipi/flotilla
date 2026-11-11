"""An old, real-sized flotilla store for the e2e stack, and what flotillad made of it.

  old_store.py make <path> <runs>   a store at schema step 1 (the step-1 SQL read from
                                    internal/store/store.go: shipped steps never change), with
                                    <runs> runs, half at stage "live", and a "stage" control
  old_store.py check <path>         one JSON line: schema step, runs kept, stages left, controls

On 2026-11-11 schema step 3 passed every test on small stores and failed on the mini's 835 runs
inside flotillad's read-only container ("disk I/O error (6410)"). The e2e stack starts flotillad on
this store, so a step that cannot run there fails the e2e, not the deploy.
"""
import json
import os
import re
import sqlite3
import sys

STORE_GO = os.path.join(os.path.dirname(__file__), "..", "..", "internal", "store", "store.go")


def steps():
    src = open(STORE_GO).read()
    block = src[src.index("var schema = []string{"):]
    block = block[:block.index("\n}\n")]
    return re.findall(r"`([^`]*)`", block)


def make(path, n):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    if os.path.exists(path):
        os.remove(path)
    db = sqlite3.connect(path)
    db.executescript("CREATE TABLE schema_version (n INTEGER NOT NULL); INSERT INTO schema_version (n) VALUES (1);")
    db.executescript(steps()[0])
    db.executemany(
        "INSERT INTO runs (id, job, spec_hash, trigger, actor, stage, started_at, status, detail)"
        " VALUES (?, 'triage/cycle', 'old', 'schedule', 'flotillad', ?, '2026-11-01T00:00:00Z', 'ok', ?)",
        ((f"old-{i:06d}", "live" if i % 2 else "shadow", "x" * 400) for i in range(n)))
    db.execute("INSERT INTO controls (subject, key, value, actor, at)"
               " VALUES ('platform/heartbeat', 'stage', 'live', 'operator', '2026-11-01T00:00:00Z')")
    db.commit()
    db.close()


def check(path):
    try:
        print(json.dumps(_check(path)))
    except sqlite3.Error as e:  # an unmigrated or broken store: say so, the caller fails the check
        print(json.dumps({"error": str(e)}))


def _check(path):
    db = sqlite3.connect(path)
    q = lambda sql: db.execute(sql).fetchone()[0]
    return ({
        "schema": q("SELECT MAX(n) FROM schema_version"),
        "latest": len(steps()),
        "old_runs": q("SELECT COUNT(*) FROM runs WHERE id LIKE 'old-%'"),
        "old_auto": q("SELECT COUNT(*) FROM runs WHERE id LIKE 'old-%' AND autonomy = 'auto'"),
        "live_left": q("SELECT COUNT(*) FROM runs WHERE autonomy = 'live'"),
        "stage_controls": q("SELECT COUNT(*) FROM controls WHERE key = 'stage'"),
        "heartbeat_autonomy": q("SELECT COALESCE(MAX(value), '') FROM controls WHERE subject = 'platform/heartbeat' AND key = 'autonomy'"),
    })


if __name__ == "__main__":
    if sys.argv[1] == "make":
        make(sys.argv[2], int(sys.argv[3]))
    else:
        check(sys.argv[2])
