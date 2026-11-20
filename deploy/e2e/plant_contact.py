#!/usr/bin/env python3
"""The e2e's contact for the hostile shadow: a CRM contact with fields to normalize, planted after every scan has run (so none has opened its case).

  plant_contact.py DIR      run where DIR is (the ops-read worker's /tmp/flotilla-example)

A live scan of the CRM would open a crm_fix case for it, whose plan (at propose) waits for a person and is offered to Slack. e2e/hostile-brief's
shadow version reports exactly that detection; the matrix's cells must hold while it runs. Prints "PLANTED c-901".
"""
import json
import os
import sys

CONTACT = {"id": "c-901", "name": "Hostile Test", "email": "Hostile.Test@Example.org ", "country": "Spain", "created": "2025-01-10"}

path = os.path.join(sys.argv[1], "crm", "contacts.json")
with open(path) as f:
    items = json.load(f)
if not any(x.get("id") == CONTACT["id"] for x in items):
    items.append(CONTACT)
    with open(path, "w") as f:
        json.dump(items, f, indent=1)
print("PLANTED " + CONTACT["id"])
