"""An old, real-sized flotilla store for the e2e stack, and what flotillad made of it.

  old_store.py make <path> <runs>   a store at schema step 1 (the step-1 SQL read from
                                    internal/store/store.go: shipped steps never change), with
                                    <runs> runs, half at stage "live", and a "stage" control
  old_store.py check <path>         one JSON line: schema step, runs kept, stages left, controls
  old_store.py make25 <path>        a store at schema step 25 (the last before routine versions), with the
                                    word shadow in every place step 28 renames it and in places it must not
                                    touch; make25 also writes <path>.counts.json (every table's row count)
  old_store.py check28 <path>        one JSON line: every post-condition of steps 26 to 31 on a migrated
                                    make25 store, "problems" empty when all hold; needs <path>.counts.json
  old_store.py check28 <path> started  the same on a store flotillad then STARTED on: its version records and the runs it made
                                     are there, the history is still version 1, every routine has one live version

On 2026-11-11 schema step 3 passed every test on small stores and failed on a production store's 835 runs
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
        "old_record": q("SELECT COUNT(*) FROM runs WHERE id LIKE 'old-%' AND autonomy = 'record'"),
        "shadow_left": q("SELECT (SELECT COUNT(*) FROM runs WHERE autonomy = 'shadow') + (SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'shadow')"),
    })


T = "2026-11-17T00:00:00Z"

# Rows holding shadow where step 28 renames it (controls, runs, plans, cases, hosts) and where it must not
# (a control that is not an autonomy, the events log, the case timeline's text). The same rows as
# internal/store/versions_test.go makeSchema25, so the Go test and the real migration are checked alike.
ROWS_25 = [
    f"""INSERT INTO controls (subject, key, value, actor, at) VALUES
      ('triage/cycle', 'autonomy', 'shadow', 'operator', '{T}'),
      ('action:backup.rerun', 'autonomy', 'shadow', 'operator', '{T}'),
      ('ops/backup-monitor', 'autonomy', 'propose', 'operator', '{T}'),
      ('platform/heartbeat', 'autonomy', 'auto', 'operator', '{T}'),
      ('triage', 'note', 'shadow', 'operator', '{T}')""",
    f"""INSERT INTO runs (id, routine, spec_hash, trigger, actor, autonomy, started_at, status, detail) VALUES
      ('r1', 'triage/cycle', 'h', 'schedule', 'flotillad', 'shadow', '{T}', 'ok', 'ran at shadow'),
      ('r2', 'triage/cycle', 'h', 'schedule', 'flotillad', 'shadow', '{T}', 'ok', ''),
      ('r3', 'ops/backup-monitor', 'h', 'schedule', 'flotillad', 'shadow', '{T}', 'ok', ''),
      ('r4', 'ops/rerun-backup', 'h', 'case', 'flotillad', 'propose', '{T}', 'ok', ''),
      ('r5', 'ops/rerun-backup', 'h', 'case', 'flotillad', 'propose', '{T}', 'ok', ''),
      ('r6', 'platform/heartbeat', 'h', 'schedule', 'flotillad', 'auto', '{T}', 'ok', '')""",
    f"""INSERT INTO cases (id, kind, target, canonical_key, status, resolution, opened_at, status_at) VALUES
      ('c1', 'failed_backup', 'host:a', 'k1', 'closed', 'shadow_plan_recorded', '{T}', '{T}'),
      ('c2', 'failed_backup', 'host:b', 'k2', 'closed', 'shadow_plan_recorded', '{T}', '{T}'),
      ('c3', 'failed_backup', 'host:c', 'k3', 'closed', 'verified', '{T}', '{T}')""",
    f"""INSERT INTO plans (case_id, run_id, action, params, plan_hash, outcome, autonomy, would, reasons, decided_at) VALUES
      ('c1', 'r4', 'backup.rerun', '{{}}', 'h', 'shadow', 'shadow', 'propose', '[]', '{T}'),
      ('c2', 'r4', 'backup.rerun', '{{}}', 'h', 'shadow', 'shadow', 'propose', '[]', '{T}'),
      ('c3', 'r5', 'backup.rerun', '{{}}', 'h', 'propose', 'propose', '', '[]', '{T}')""",
    f"""INSERT INTO case_events (case_id, at, kind, actor, detail) VALUES
      ('c1', '{T}', 'case.detected', 'ops/backup-monitor', 'found'),
      ('c1', '{T}', 'case.planned', 'ops/rerun-backup', 'planned'),
      ('c1', '{T}', 'case.closed', 'flotillad', 'plan 1 backup.rerun: shadow at shadow, would propose'),
      ('c2', '{T}', 'case.closed', 'flotillad', 'plan 2 backup.rerun: shadow at shadow, would propose'),
      ('c3', '{T}', 'case.verified', 'ada', 'looks right')""",
    f"""INSERT INTO events (at, actor, via, kind, subject, detail) VALUES
      ('{T}', 'operator', 'cli', 'control.autonomy', 'triage/cycle', 'shadow'),
      ('{T}', 'operator', 'cli', 'control.autonomy', 'action:backup.rerun', 'shadow: the new action starts at shadow'),
      ('{T}', 'operator', 'cli', 'control.stop', 'triage', '1')""",
    f"INSERT INTO hosts (name, created_at, ceiling) VALUES ('host-a', '{T}', 'shadow'), ('host-b', '{T}', 'propose')",
    f"INSERT INTO run_calls (run_id, routine, at, alias) VALUES ('r1', 'triage/cycle', '{T}', 'a'), ('r2', 'triage/cycle', '{T}', 'a')",
    f"INSERT INTO run_loaded (run_id, routine, at, kind, trust, title, source) VALUES ('r1', 'triage/cycle', '{T}', 'playbook', 'source', 't', 's')",
    f"INSERT INTO ledger_filings (namespace, fingerprint, row, run_id, written_at) VALUES ('triage/cycle', 'fp', '{{}}', 'r1', '{T}')",
    f"INSERT INTO ledger_entries (namespace, kind, row, run_id, written_at) VALUES ('triage/cycle', 'decision', '{{}}', 'r1', '{T}'), ('triage/cycle', 'decision', '{{}}', 'r2', '{T}')",
]


def _counts(db):
    names = [r[0] for r in db.execute(
        "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'"
        " AND name NOT IN ('routine_versions', 'schema_version')")]
    return {n: db.execute(f"SELECT COUNT(*) FROM {n}").fetchone()[0] for n in names}


def make25(path):
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    for f in (path, path + ".counts.json"):
        if os.path.exists(f):
            os.remove(f)
    db = sqlite3.connect(path)
    db.execute("CREATE TABLE schema_version (n INTEGER NOT NULL)")
    for i, step in enumerate(steps()[:25]):
        db.executescript(step)
        db.execute("INSERT INTO schema_version (n) VALUES (?)", (i + 1,))
    for q in ROWS_25:
        db.execute(q)
    db.commit()
    json.dump(_counts(db), open(path + ".counts.json", "w"))
    db.close()


def check28(path, started=False):
    """Every post-condition of steps 26 to 31 on a migrated make25 store; one JSON line. started: flotillad ran on it
    after the migration, so the version table is filled, and tables that record what a running flotillad does may
    have grown; the migrated rows themselves are asserted all the same."""
    db = sqlite3.connect(path)
    q = lambda sql: db.execute(sql).fetchone()[0]
    problems = []

    def want(sql, n):
        got = q(sql)
        if got != n:
            problems.append(f"{sql} = {got}, want {n}")

    before = json.load(open(path + ".counts.json"))
    after = _counts(db)
    for table, n in before.items():
        got = after.get(table)
        # a started flotillad adds rows (a run, an event, a brief); it never removes one
        if (got is None or got < n) if started else got != n:
            problems.append(f"{table}: {n} rows before, {got} after")
    want("SELECT MAX(n) FROM schema_version", len(steps()))
    live = None
    if started:
        live = q("SELECT COUNT(*) FROM routine_versions WHERE state = 'live'")
        want("SELECT COUNT(*) FROM (SELECT routine FROM routine_versions GROUP BY routine HAVING SUM(state = 'live') != 1)", 0)
        want("SELECT COUNT(*) FROM routine_versions WHERE state = 'live' AND version != 1", 0)
        want("SELECT COUNT(*) FROM runs WHERE id IN ('r1', 'r2', 'r3', 'r4', 'r5', 'r6') AND version = 1", 6)
        want("SELECT COUNT(*) FROM routine_versions WHERE promote_evidence != ''", 0)
        want("SELECT COUNT(*) FROM events WHERE kind = 'version.deploy' AND detail LIKE 'live:%'", live)
    else:
        want("SELECT COUNT(*) FROM routine_versions", 0)
    want("SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'shadow'", 0)
    want("SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'record'", 2)
    want("SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'propose'", 1)
    want("SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'auto'", 1)
    want("SELECT COUNT(*) FROM controls WHERE key = 'note' AND value = 'shadow'", 1)
    want("SELECT COUNT(*) FROM runs WHERE autonomy = 'shadow'", 0)
    want("SELECT COUNT(*) FROM runs WHERE autonomy = 'record'", 3)
    want("SELECT COUNT(*) FROM runs WHERE autonomy = 'propose'", 2)
    want("SELECT COUNT(*) FROM runs WHERE autonomy = 'auto'", 1)
    want("SELECT COUNT(*) FROM plans WHERE autonomy = 'shadow' OR outcome = 'shadow'", 0)
    want("SELECT COUNT(*) FROM plans WHERE autonomy = 'record' AND outcome = 'record' AND would = 'propose'", 2)
    want("SELECT COUNT(*) FROM plans WHERE autonomy = 'propose' AND outcome = 'propose'", 1)
    want("SELECT COUNT(*) FROM cases WHERE resolution = 'shadow_plan_recorded'", 0)
    want("SELECT COUNT(*) FROM cases WHERE resolution = 'plan_recorded'", 2)
    want("SELECT COUNT(*) FROM cases WHERE resolution = 'verified'", 1)
    want("SELECT COUNT(*) FROM hosts WHERE ceiling = 'shadow'", 0)
    want("SELECT COUNT(*) FROM hosts WHERE ceiling = 'record'", 1)
    want("SELECT COUNT(*) FROM hosts WHERE ceiling = 'propose'", 1)
    want("SELECT COUNT(*) FROM events WHERE detail LIKE '%shadow%' AND kind = 'control.autonomy'", 2)
    want("SELECT COUNT(*) FROM case_events WHERE detail LIKE '%shadow%'", 2)
    for table in ("runs", "run_calls", "run_loaded", "plans", "ledger_filings", "ledger_entries"):
        if started:  # what the running flotillad added is its own; the migrated rows are checked above and below
            continue
        want(f"SELECT COUNT(*) FROM {table} WHERE version = 1", before[table])
    want("SELECT COUNT(*) FROM case_events WHERE actor LIKE '%/%' AND version = 1", 2)
    want("SELECT COUNT(*) FROM case_events WHERE actor NOT LIKE '%/%' AND version = 0", 3)
    want("SELECT COUNT(*) FROM plans WHERE rolled_back_at IS NOT NULL OR rolled_back_by IS NOT NULL", 0)
    want("SELECT COUNT(*) FROM pragma_index_list('routine_versions') WHERE \"unique\" = 1 AND partial = 1", 2)
    # steps 30 and 31 (shadows): the old runs are live runs with no input key, the shadow tables exist and a migration
    # filled none of them; a started flotillad may add live inputs, never a shadow run or record (shadows start off)
    want("SELECT COUNT(*) FROM runs WHERE shadow <> 0 OR input_key <> ''", 0)
    want("SELECT COUNT(*) FROM shadow_runs", 0)
    want("SELECT COUNT(*) FROM shadow_records", 0)
    want("SELECT COUNT(*) FROM pragma_index_list('runs') WHERE name = 'runs_routine_shadow'", 1)
    want("SELECT COUNT(*) FROM pragma_table_info('shadow_records') WHERE name IN ('input_key', 'subject', 'kind', 'payload')", 4)
    if not started:
        want("SELECT COUNT(*) FROM run_inputs", 0)
    integrity = q("PRAGMA integrity_check")
    if integrity != "ok":
        problems.append(f"integrity_check: {integrity}")
    print(json.dumps({"schema": q("SELECT MAX(n) FROM schema_version"), "tables": len(after), "live_routines": live, "problems": problems}))


if __name__ == "__main__":
    cmd = sys.argv[1]
    if cmd == "make":
        make(sys.argv[2], int(sys.argv[3]))
    elif cmd == "make25":
        make25(sys.argv[2])
    elif cmd == "check28":
        check28(sys.argv[2], started=sys.argv[3:] == ["started"])
    else:
        check(sys.argv[2])
