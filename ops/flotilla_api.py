"""flotillad's worker listener, for an ops routine that reads cases or proposes notes (ADR-0016): the
worker's own token, read from its file, never printed. Stdlib only.

  FLOTILLA_API_URL         http://flotillad:8485 (set by the worker from ROUTINE_FLOTILLA_API_URL)
  FLOTILLA_API_TOKEN_FILE  the worker's token file
"""
import json
import os
import urllib.parse
import urllib.request


class APIError(Exception):
    pass


class Client:
    def __init__(self, url, token, timeout=20):
        self.url, self.token, self.timeout = url.rstrip("/"), token, timeout

    def _do(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.url + path, data=data, method=method,
                                     headers={"Authorization": "Bearer " + self.token, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            raise APIError(f"{method} {path}: HTTP {e.code} {e.read()[:200]!r}") from e

    def cases(self, **q):
        return self._do("GET", "/v1/cases?" + urllib.parse.urlencode(q))["cases"]

    def case(self, case_id):
        return self._do("GET", "/v1/cases/" + urllib.parse.quote(case_id))

    def propose_note(self, case_id, text):
        return self._do("POST", "/v1/cases/" + urllib.parse.quote(case_id) + "/notes", {"text": text})


def from_env(environ=None):
    environ = os.environ if environ is None else environ
    url, tok_file = environ.get("FLOTILLA_API_URL", ""), environ.get("FLOTILLA_API_TOKEN_FILE", "")
    if not url or not tok_file:
        raise APIError("FLOTILLA_API_URL and FLOTILLA_API_TOKEN_FILE are required")
    with open(tok_file) as f:
        return Client(url, f.read().strip())
