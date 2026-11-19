"""runtime/stations.py: a version's stations as a run receives them (v2-design.md section 2, FLOTILLA_STATIONS).

flotillad hands a run of a version that declares stations FLOTILLA_STATIONS: per station its harness, model alias, tools, limits and its
texts (name -> path and sha256), and the version's flow limits. This module reads it, loads each text under the prompt rules
(runtime/prompts.py: a regular file, no symlink, inside the fleet's folder, UTF-8, not empty, at most 64 KiB) and REFUSES a text whose
file does not hash to what flotillad checked, renders `{{slot}}`s, and says what each rendered text was so a call can record it.

  st = stations.load(environ)                    # None when the run was not started with stations
  r = st.render("decide", "task", {"case": ...}, fleet="triage")      # Rendered(text, ...), or StationsError before any model call
  client.chat(alias, messages, station="decide", texts=[r.ref()])      # the calls line says station and which texts

A text the version does not declare, a slot the text needs and the caller did not give, and a slot given that the text does not use all
fail here, before the first model call. The texts are `loaded` records, as a prompt is (prompts.read records each). Stdlib only.
"""
import hashlib
import json
import os
import re
import sys
from dataclasses import dataclass

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import prompts  # noqa: E402
import toolrec  # noqa: E402

SLOT = re.compile(r"\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}")
TEXT_CAP = prompts.MAX_BYTES   # a rendered text above this is kept as its hash and length only, never cut


class StationsError(Exception):
    """A station, text or slot the version does not declare or the caller did not fill: the run stops before a model call."""


@dataclass
class Rendered:
    station: str
    name: str
    text: str
    template_sha256: str

    def ref(self):
        """What a calls line says of this text: its name, hash and length in bytes."""
        raw = self.text.encode("utf-8")
        return {"name": self.name, "sha256": hashlib.sha256(raw).hexdigest(), "bytes": len(raw)}

    def record(self):
        """What is stored of the text, once per hash: the ref and the text with credentials masked, or the ref alone when the text
        is over TEXT_CAP (flagged, never cut)."""
        r = self.ref()
        if r["bytes"] > TEXT_CAP:
            return dict(r, text=None, redacted=False, over_cap=True)
        return dict(r, text=toolrec.redact_credentials(self.text), redacted=True, over_cap=False)


class Stations:
    def __init__(self, doc):
        if not isinstance(doc, dict) or doc.get("schema_version") != 1 or not isinstance(doc.get("stations"), dict):
            raise StationsError("FLOTILLA_STATIONS is not a stations document of schema_version 1")
        self.doc = doc

    def names(self):
        return sorted(self.doc["stations"])

    def station(self, name):
        s = self.doc["stations"].get(name)
        if s is None:
            raise StationsError(f"the version declares no station {name!r} (it has {self.names()})")
        return s

    def limits(self):
        """The version's own flow limits."""
        return dict(self.doc.get("version_limits") or {})

    def render(self, station, text_name, slots, fleet, root=None, environ=None):
        s = self.station(station)
        ref = (s.get("texts") or {}).get(text_name)
        if not isinstance(ref, dict) or not ref.get("path") or not ref.get("sha256"):
            raise StationsError(f"the station {station!r} declares no text {text_name!r} (it has {sorted(s.get('texts') or {})})")
        got = prompts.read(f"{station}.{text_name}", fleet, ref["path"], ref["sha256"], root=root, environ=environ)
        template = got["text"]
        used = set(SLOT.findall(template))
        missing, extra = sorted(used - set(slots)), sorted(set(slots) - used)
        if missing:
            raise StationsError(f"{station}.{text_name} needs the slot(s) {', '.join(missing)}, which the code did not fill")
        if extra:
            raise StationsError(f"{station}.{text_name}: the code filled the slot(s) {', '.join(extra)}, which the text does not use")
        text = SLOT.sub(lambda m: str(slots[m.group(1)]), template)
        r = Rendered(station, text_name, text, got["sha256"])
        return r


def load(environ=None):
    """The run's Stations, or None when FLOTILLA_STATIONS is not set (a run of a version without stations, an evaluation run)."""
    environ = os.environ if environ is None else environ
    raw = environ.get("FLOTILLA_STATIONS")
    if not raw:
        return None
    try:
        return Stations(json.loads(raw))
    except ValueError as e:
        raise StationsError(f"FLOTILLA_STATIONS is not JSON: {e}") from e
