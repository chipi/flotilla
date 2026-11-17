#!/usr/bin/env python3
"""The store's snapshot and restore, run by deploy.sh inside a throwaway container of the worker image
(python3 and its sqlite3 are in it; flotillad's own image is distroless): no network, the state volume at
STORE_DIR (default /v), uid 65532 so every file stays the one flotillad opens.

  store_snapshot.py backup    flotilla.db, consistent (SQLite's online backup, safe beside a running
                              flotillad) to stdout; exit 3 when there is no store yet
  store_snapshot.py schema    the store's newest schema step on the volume, one number (0: none)
  store_snapshot.py inspect   a database on stdin: integrity check, then "SCHEMA <n>"
  store_snapshot.py restore   a database on stdin replaces flotilla.db (flotillad stopped), its
                              -wal and -shm removed; checked before it is moved into place

Nothing but the database bytes goes to stdout in backup; the other modes print one line.
"""
import os
import shutil
import sqlite3
import sys
import tempfile

STORE_DIR = os.environ.get("STORE_DIR", "/v")
DB = os.path.join(STORE_DIR, "flotilla.db")


def schema_of(path):
    uri = "file:%s?mode=ro" % path
    con = sqlite3.connect(uri, uri=True)
    try:
        try:
            return con.execute("SELECT COALESCE(MAX(n), 0) FROM schema_version").fetchone()[0]
        except sqlite3.OperationalError as e:
            if "no such table" in str(e):
                return 0
            raise
    finally:
        con.close()


def check(path):
    con = sqlite3.connect("file:%s?mode=ro" % path, uri=True)
    try:
        r = con.execute("PRAGMA integrity_check").fetchall()
    finally:
        con.close()
    if r != [("ok",)]:
        sys.exit("integrity_check: %s" % r)


def stdin_to(path):
    with open(path, "wb") as f:
        shutil.copyfileobj(sys.stdin.buffer, f)
    if os.path.getsize(path) == 0:
        sys.exit("empty database on stdin")


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else ""
    if mode == "backup":
        if not os.path.exists(DB):
            sys.exit(3)
        with tempfile.TemporaryDirectory() as t:
            out = os.path.join(t, "snap.db")
            src = sqlite3.connect("file:%s?mode=ro" % DB, uri=True)
            dst = sqlite3.connect(out)
            try:
                src.backup(dst)
                dst.execute("PRAGMA journal_mode=DELETE")   # a standalone file: reading it leaves no -wal/-shm beside it
            finally:
                dst.close()
                src.close()
            check(out)
            with open(out, "rb") as f:
                shutil.copyfileobj(f, sys.stdout.buffer)
    elif mode == "schema":
        print(schema_of(DB) if os.path.exists(DB) else 0)
    elif mode == "inspect":
        with tempfile.TemporaryDirectory() as t:
            p = os.path.join(t, "in.db")
            stdin_to(p)
            check(p)
            print("SCHEMA %d" % schema_of(p))
    elif mode == "restore":
        tmp = DB + ".restore"
        stdin_to(tmp)
        try:
            check(tmp)
            for ext in ("-wal", "-shm"):
                if os.path.exists(DB + ext):
                    os.remove(DB + ext)
            os.replace(tmp, DB)
        finally:
            for ext in ("", "-wal", "-shm"):
                if os.path.exists(tmp + ext):
                    os.remove(tmp + ext)
        print("RESTORED schema %d" % schema_of(DB))
    else:
        sys.exit("usage: store_snapshot.py backup|schema|inspect|restore")


if __name__ == "__main__":
    main()
