"""What the seo routines share: the site list, JSON over HTTP, the drop rule, the report."""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
UA = "flotilla-seo/1 (+https://github.com/chipi/flotilla)"

# RFC-0012 §3, the operator's starting values (2026-11-15): a drop is more than 30 % week over week
# from at least 50 the week before; an opportunity ranks 8 to 20 with at least 100 impressions in 28 days.
DROP_SHARE = 0.30
DROP_MIN = 50
OPP_POSITION = (8.0, 20.0)
OPP_IMPRESSIONS = 100


class Error(Exception):
    """A source that cannot be read: the run fails, so no finding is made from a missing number."""


def sites_path(argv=None):
    """The sites file the routine's spec names (`--sites <path>`, in its fleet folder), or SEO_SITES. The
    code knows no fleet's name: an installation's fleets and the examples each name their own."""
    argv = sys.argv[1:] if argv is None else argv
    if "--sites" in argv and argv.index("--sites") + 1 < len(argv):
        return argv[argv.index("--sites") + 1]
    if os.environ.get("SEO_SITES"):
        return os.environ["SEO_SITES"]
    raise Error("no sites file: the routine's spec passes --sites <path>")


def sites(argv=None):
    with open(sites_path(argv)) as f:
        return json.load(f)


def env_file(path):
    out = {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                out[k] = v.strip().strip('"').strip("'")
    return out


def http_json(url, token=None, body=None, form=None, timeout=60):
    headers = {"User-Agent": UA, "Accept": "application/json"}
    data = None
    if body is not None:
        data, headers["Content-Type"] = json.dumps(body).encode(), "application/json"
    elif form is not None:
        data, headers["Content-Type"] = urllib.parse.urlencode(form).encode(), "application/x-www-form-urlencoded"
    if token:
        headers["Authorization"] = "Bearer " + token
    try:
        with urllib.request.urlopen(urllib.request.Request(url, data=data, headers=headers), timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        raise Error(f"{urllib.parse.urlsplit(url).path}: HTTP {e.code} {e.read()[:200]!r}") from None
    except (urllib.error.URLError, OSError, ValueError) as e:
        raise Error(f"{urllib.parse.urlsplit(url).netloc}: {e}") from None


def dropped(now, before):
    """The drop rule: True when `now` is more than DROP_SHARE below `before` and `before` is at least DROP_MIN."""
    return before >= DROP_MIN and now < before * (1 - DROP_SHARE)


def change(now, before):
    if not before:
        return "new" if now else "0"
    return f"{(now - before) / before * 100:+.0f}%"


def report(detections, summary):
    """The run's report: its detections, and the lines the brief's section shows (at most 20)."""
    plan = os.environ.get("FLOTILLA_PLAN_FILE")
    if plan:
        with open(plan, "w") as f:
            json.dump({"outcome": "detections", "detections": detections, "summary": summary[:20]}, f)
