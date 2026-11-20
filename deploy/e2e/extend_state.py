#!/usr/bin/env python3
"""The e2e's backward-readable-state check: what a v2 that adds a column or a key leaves behind for v1 to read.

  extend_state.py DIR       run where DIR is (the worker's own filesystem)

Every JSON store (*.json) in DIR gets one key it did not have: on each record of a list, on a dict itself and on each record of a list in a dict.
Every TSV store (*.tsv) gets one more column, in its header and every row. The value is a string no routine knows. A routine that reads its state
by name, as the examples do, must run on it as it ran on the state before: the matrix runs v1 again and compares its exit and decisions.

Prints "EXTENDED files n records m" (n and m are never 0 on a folder with a store, so an empty folder cannot pass).
"""
import json
import os
import sys

KEY = "x_added_by_a_later_version"
VALUE = "unknown-to-v1"


def extend_json(data):
    n = 0
    if isinstance(data, list):
        for item in data:
            if isinstance(item, dict):
                item.setdefault(KEY, VALUE)
                n += 1
    elif isinstance(data, dict):
        data.setdefault(KEY, VALUE)
        n += 1
        for v in data.values():
            if isinstance(v, list):
                n += extend_json(v)
    return n


def extend_tsv(text):
    lines = text.split("\n")
    out, n = [], 0
    for i, line in enumerate(lines):
        if not line:
            out.append(line)
            continue
        out.append(line + "\t" + (KEY if i == 0 else VALUE))
        n += 1
    return "\n".join(out), n


def main(argv):
    if len(argv) != 2:
        print(__doc__)
        return 2
    files = records = 0
    for root, _, names in os.walk(argv[1]):
        for name in sorted(names):
            path = os.path.join(root, name)
            if name.endswith(".json"):
                with open(path) as f:
                    data = json.load(f)
                n = extend_json(data)
                with open(path, "w") as f:
                    json.dump(data, f, indent=1)
            elif name.endswith(".tsv"):
                with open(path) as f:
                    text, n = extend_tsv(f.read())
                with open(path, "w") as f:
                    f.write(text)
            else:
                continue
            files += 1
            records += n
    print("EXTENDED files %d records %d" % (files, records))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
