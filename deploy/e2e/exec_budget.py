#!/usr/bin/env python3
"""The platform's executions in the last hour in a copy of flotillad's store: `python3 exec_budget.py store.db` prints the count.

The same count policy reads (store.PlatformHour: case.executing events in the last hour) against its cap (policy.Limits.PlatformPerHour: the default 10; the e2e's flotillad.yaml limits block sets 12). The
e2e stack runs every section in one hour, and section `ask` needs its two plans proposed (below the cap) and its first answer's approval to execute.
"""
import datetime
import sqlite3
import sys

cutoff = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%S")
db = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
print(db.execute("SELECT COUNT(*) FROM case_events WHERE kind = 'case.executing' AND at >= ?", (cutoff,)).fetchone()[0])
