#!/usr/bin/env python3
"""The no-effect test's store dump: what a shadow run must leave alone, hashed table by table.

  storediff.py DB ROUTINE    DB is a consistent copy of flotillad's store (deploy/server/store_snapshot.py backup).

For every table, the rows that are not a shadow's own (not in a shadow table, and no value holding an "@v" key) and that concern ROUTINE (any
text value naming it, as its id or as the start of a run id): their count and a hash of their canonical form. One line per table, then
"SHADOW tables rows runs" counting what the shadow did leave (so a dump taken after a shadow that did nothing is visible), then "ROWS n", the
number of rows hashed (so an empty dump cannot pass). Two dumps are equal when their lines are.
"""
import hashlib
import json
import sqlite3
import sys

SHADOW_TABLES = {"shadow_runs", "shadow_records"}


def main(db, routine):
    con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    needles = (routine, routine.replace("/", "-"))
    tables = [r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
    total = 0
    for t in tables:
        if t in SHADOW_TABLES:
            continue
        cols = [c[1] for c in con.execute('PRAGMA table_info("%s")' % t)]
        rows = []
        for row in con.execute('SELECT * FROM "%s"' % t):
            text = [str(v) for v in row if isinstance(v, (str, bytes)) or v is not None]
            if any("@v" in x for x in text):
                continue
            if not any(n in x for x in text for n in needles):
                continue
            rows.append(json.dumps([cols, [v if not isinstance(v, bytes) else v.hex() for v in row]], sort_keys=True, default=str))
        rows.sort()
        total += len(rows)
        print("%s %d %s" % (t, len(rows), hashlib.sha256("\n".join(rows).encode()).hexdigest()[:16]))
    shadow_rows = sum(con.execute('SELECT COUNT(*) FROM "%s"' % t).fetchone()[0] for t in SHADOW_TABLES if t in tables)
    shadow_runs = con.execute("SELECT COUNT(*) FROM runs WHERE shadow = 1").fetchone()[0] if "runs" in tables else 0
    print("SHADOW %d %d" % (shadow_rows, shadow_runs))
    print("ROWS %d" % total)


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
