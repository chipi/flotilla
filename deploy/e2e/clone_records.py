#!/usr/bin/env python3
"""The e2e's fresh work for the example fleets: a copy of the first record of each business store, under a new id, so each scan opens new cases.

  clone_records.py DIR      run where DIR is (the ops-read worker's /tmp/flotilla-example)

The stores are JSON files (business/store.py); a case that was closed keeps its key, so a second scan of the same data opens nothing. A clone of
a record is a new subject with the same findings (a contact to fix, a prospect to write to, a campaign to launch, an account at risk), and so its
cases take the whole path: the plan, the approval, the acting routine and the check after it. Prints "CLONED n".
"""
import json
import os
import sys

STORES = {"crm": "contacts", "campaigns": "campaigns", "outbound": "prospects", "renewals": "accounts"}


def main(root):
    n = 0
    for fleet, name in STORES.items():
        path = os.path.join(root, fleet, name + ".json")
        if not os.path.exists(path):
            continue
        with open(path) as f:
            items = json.load(f)
        new = dict(items[0])
        new["id"] = str(new["id"]) + "-m"
        if "slug" in new:
            new["slug"] = new["slug"] + "-m"
        items.append(new)
        with open(path, "w") as f:
            json.dump(items, f, indent=1)
        n += 1
    print("CLONED %d" % n)
    return 0 if n else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
