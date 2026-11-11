"""The triage fleet's ledgers and flotillad's store (RFC-0002 §9 step 5).

  python3 ledger_sync.py import     # load filed.tsv and dispositions.tsv into the routine's namespace, once
  python3 ledger_sync.py check      # render both TSVs from the store; equal to the files, byte for byte?

Run inside a worker, as its routine runs: FLOTILLA_API_URL, FLOTILLA_API_TOKEN_FILE and FLOTILLA_ROUTINE
(the namespace) name the ledger; SF_FILED_LEDGER and SF_LEDGER name the files (filing.FILED, config.LEDGER).
It prints counts only, never a row: the rows are production data and the held-out test side of the
fleet's evaluation. Exit 0 on success, 1 on a mismatch or a refusal.
"""
import os
import sys

import actions
import config
import filing


def _read(path):
    if not os.path.exists(path):
        return None, []
    with open(path, encoding="utf-8") as f:
        lines = f.read().split("\n")
    return lines[0], [ln for ln in lines[1:] if ln]


def tsv_filings():
    header, lines = _read(filing.FILED)
    return header, [dict(zip(filing.FILED_COLS, ln.split("\t"))) for ln in lines]


def tsv_entries():
    header, lines = _read(config.LEDGER)
    out = []
    for ln in lines:
        row = dict(zip(actions.LEDGER_COLS, ln.split("\t")))
        out.append({"kind": "recurrence" if row.get("disposition") == "recurrence" else "decision", "row": row})
    return header, out


def render_filings(rows):
    return "\t".join(filing.FILED_COLS) + "\n" + "".join("\t".join(r.get(c, "") for c in filing.FILED_COLS) + "\n"
                                                        for r in rows)


def render_entries(rows):
    return "\t".join(actions.LEDGER_COLS) + "\n" + "".join("\t".join(r.get(c, "") for c in actions.LEDGER_COLS) + "\n"
                                                           for r in rows)


def _file(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def main(argv):
    if len(argv) != 1 or argv[0] not in ("import", "check"):
        print(__doc__)
        return 2
    if argv[0] == "import":   # the one caller allowed an empty namespace: it fills it
        client = filing._ledger_module().from_env()
    else:
        client = filing._ledger_client()
    if argv[0] == "import":
        fh, filings = tsv_filings()
        eh, entries = tsv_entries()
        if fh != "\t".join(filing.FILED_COLS) or eh != "\t".join(actions.LEDGER_COLS):
            print("IMPORT REFUSED: a ledger's header is not the current one (migrate it first)")
            return 1
        short = sum(1 for r in filings if len(r) != len(filing.FILED_COLS))
        short += sum(1 for e in entries if len(e["row"]) != len(actions.LEDGER_COLS))
        if short:
            # a short row reads with fewer keys than the store keeps; the next ledger rewrite pads the file
            print(f"IMPORT REFUSED: {short} rows have fewer columns than the header (rewrite the ledger first)")
            return 1
        c = client.import_ledger(filings, entries)
        want = (len(filings), sum(e["kind"] == "decision" for e in entries), sum(e["kind"] == "recurrence" for e in entries))
        got = (c["filings"], c["decisions"], c["recurrences"])
        print(f"imported into {client.ns}: {got[0]} filings, {got[1]} decisions, {got[2]} recurrences "
              f"(the files: {want[0]}, {want[1]}, {want[2]})")
        return 0 if got == want else 1
    ok = True
    f_store = render_filings(client.filings())
    f_file = _file(filing.FILED)
    store_entries = client.all_entries()
    e_store = render_entries([e["row"] for e in store_entries])
    e_file = _file(config.LEDGER)
    # each stored kind matches its row: a recurrence is an entry of kind recurrence, never a decision
    wrong_kind = sum(1 for e in store_entries if (e["row"].get("disposition") == "recurrence") != (e["kind"] == "recurrence"))
    print(f"entry kinds: {wrong_kind} stored under the wrong kind")
    ok = wrong_kind == 0
    for name, a, b in (("filed.tsv", f_store, f_file), ("dispositions.tsv", e_store, e_file)):
        same = a == b
        ok &= same
        print(f"{name}: store {a.count(chr(10)) - 1} rows, file {b.count(chr(10)) - 1} rows, "
              f"{'byte-identical' if same else 'DIFFERENT'}")
    print(f"LEDGER CHECK ({client.ns}): {'PASS' if ok else 'FAIL'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
