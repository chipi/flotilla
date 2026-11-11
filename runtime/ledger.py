"""The fleets' ledger client (RFC-0002 §9 step 5): a routine reads and writes its ledger through flotillad.

  client = ledger.from_env()                       # FLOTILLA_API_URL, FLOTILLA_API_TOKEN_FILE,
                                                    # FLOTILLA_ROUTINE (the namespace), FLOTILLA_RUN_ID
  client.filings({"fingerprint": fp, "norm_key": nk})   # current filed rows matching any key, ledger order
  client.filings()                                  # all current filed rows
  client.write_filing(row)                          # a new current row for row["fingerprint"]
  client.append_entry("decision" | "recurrence", row)
  client.entries(after=0, limit=5000)
  client.counts(); client.import_ledger(filings, entries)

Workers never open the database: flotillad serves the ledger on its workers' listener. Any failure is
LedgerUnavailable, and a caller that needs dedup stops on it: a fleet never files without dedup.
Every call is told to `record(kind, item)` when given (the triage fleet records ledger calls in their own
list, never among its GitHub calls). Stdlib only.
"""
import json
import os
import urllib.error
import urllib.parse
import urllib.request

MAX_RESPONSE_BYTES = 64 << 20


class LedgerUnavailable(Exception):
    """flotillad could not be reached or refused the call: the run must not act without its ledger."""


class Client:
    def __init__(self, url, token, namespace, run_id="", record=None, timeout=30, opener=None):
        if not url or not token or not namespace:
            raise LedgerUnavailable("the ledger needs FLOTILLA_API_URL, a token and a namespace (FLOTILLA_ROUTINE)")
        self.url, self.token, self.ns, self.run_id = url.rstrip("/"), token, namespace, run_id
        self.record, self.timeout = record, timeout
        self.open = opener or urllib.request.urlopen

    def _call(self, method, path, query=None, body=None):
        q = dict(query or {}, ns=self.ns)
        url = f"{self.url}{path}?{urllib.parse.urlencode(q)}"
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Authorization", f"Bearer {self.token}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with self.open(req, timeout=self.timeout) as r:
                raw = r.read(MAX_RESPONSE_BYTES + 1)
            if len(raw) > MAX_RESPONSE_BYTES:
                raise LedgerUnavailable(f"ledger {path}: response over {MAX_RESPONSE_BYTES} bytes")
            out = json.loads(raw)
        except urllib.error.HTTPError as e:
            detail = e.read()[:300].decode("utf-8", "replace") if e.fp else ""
            self._rec(method, path, query, body, error=f"HTTP {e.code}: {detail}")
            raise LedgerUnavailable(f"ledger {method} {path}: HTTP {e.code}: {detail}") from e
        except LedgerUnavailable:
            raise
        except Exception as e:  # noqa: BLE001 — a refused connection, a timeout, a broken body
            self._rec(method, path, query, body, error=f"{type(e).__name__}: {e}")
            raise LedgerUnavailable(f"ledger {method} {path}: {type(e).__name__}: {e}") from e
        self._rec(method, path, query, body, result=out)
        return out

    def _rec(self, method, path, query, body, **out):
        if self.record:
            try:
                self.record("ledger", dict({"method": method, "path": path, "query": query or {}, "body": body}, **out))
            except Exception:  # noqa: BLE001 — recording never breaks the call
                pass

    def filings(self, keys=None):
        q = {k: v for k, v in (keys or {}).items() if v}
        if keys and not q:
            return []
        return [f["row"] for f in self._call("GET", "/v1/ledger/filings", q)["filings"]]

    def write_filing(self, row):
        return self._call("POST", "/v1/ledger/filings", body={"row": row, "run_id": self.run_id})["seq"]

    def append_entry(self, kind, row):
        return self._call("POST", "/v1/ledger/entries", body={"kind": kind, "row": row, "run_id": self.run_id})["seq"]

    def entries(self, after=0, limit=5000):
        return self._call("GET", "/v1/ledger/entries", {"after": after, "limit": limit})["entries"]

    def all_entries(self):
        out, after = [], 0
        while True:
            page = self.entries(after=after)
            if not page:
                return out
            out.extend(page)
            after = page[-1]["seq"]

    def counts(self):
        return self._call("GET", "/v1/ledger/counts")

    def import_ledger(self, filings, entries):
        return self._call("POST", "/v1/ledger/import", body={"filings": filings, "entries": entries})


def from_env(environ=None, record=None):
    environ = os.environ if environ is None else environ
    tok_file = environ.get("FLOTILLA_API_TOKEN_FILE", "")
    try:
        with open(tok_file) as f:
            token = f.read().strip()
    except OSError as e:
        raise LedgerUnavailable(f"the ledger's token file {tok_file!r}: {e.strerror}") from e
    return Client(environ.get("FLOTILLA_API_URL", ""), token, environ.get("FLOTILLA_ROUTINE", ""),
                  environ.get("FLOTILLA_RUN_ID", ""), record=record)
