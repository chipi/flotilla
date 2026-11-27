"""ops/free-ratio: what the disk routines share (RFC-0016 §8, step 1): the free ratio of a filesystem as one expression, the case target a
group (instance and mountpoint) becomes and back, and the threshold read from the registry action's precondition.

The free ratio is avail / size per series (node_filesystem_avail_bytes over node_filesystem_size_bytes, both read), and a series of no size
is dropped. Every label value that reaches an expression is plain: no quote, backslash, space or newline, so a value can never close a
string or add a matcher. Python stdlib only.
"""
import re

import common

AVAIL_METRIC = "node_filesystem_avail_bytes"
SIZE_METRIC = "node_filesystem_size_bytes"
CONDITION = "free_ratio_below"
WINDOW = "10m"                      # the newest sample within it is the reading: a scrape or two missed is not a missing disk
METRIC_RE = re.compile(r"^[a-zA-Z_:][a-zA-Z0-9_:]{0,127}$")
LABEL_RE = re.compile(r"^[a-zA-Z_][a-zA-Z0-9_]{0,63}$")
INSTANCE_RE = re.compile(r"^[A-Za-z0-9._-]{1,63}$")
MOUNT_RE = re.compile(r"^/[A-Za-z0-9._/-]{0,119}$")
# an exclusion's regex: what a Prometheus regex needs, never a quote, a backslash, a space, a brace or a control character
REGEX_RE = re.compile(r"^[A-Za-z0-9._/:@%+=,|^$*()\[\]?-]{1,200}$")
FRACTION_RE = re.compile(r"^0?\.[0-9]+$")
MAX_TARGET = 120                    # flotillad's target: <kind>:<up to 120 characters>


def plain_instance(v):
    if not isinstance(v, str) or not INSTANCE_RE.match(v):
        raise common.ConfigError(f"an instance is letters, digits and . _ -, not {str(v)[:40]!r}")
    return v


def plain_mountpoint(v):
    if not isinstance(v, str) or not MOUNT_RE.match(v):
        raise common.ConfigError(f"a mountpoint is a path of letters, digits and . _ / -, not {str(v)[:40]!r}")
    return v


def check_exclude(exclude):
    """The exclusion map, as a block param gives it ({label: regex}), or ConfigError."""
    if not isinstance(exclude, dict) or not exclude:
        raise common.ConfigError("exclude is a JSON object of label to regex, with at least one label")
    for k, v in exclude.items():
        if not isinstance(k, str) or not LABEL_RE.match(k) or k.startswith("__"):
            raise common.ConfigError(f"exclude label {str(k)[:40]!r} is not a label name")
        if not isinstance(v, str) or not REGEX_RE.match(v):
            raise common.ConfigError(f"exclude {k}: a regex of letters, digits and . _ / : @ % + = , | ^ $ * ( ) [ ] ? - only")
        try:
            re.compile(v)
        except re.error as e:
            raise common.ConfigError(f"exclude {k}: not a regex ({e})")
    return exclude


def _selector(metric, exclude, instance, mountpoint):
    if not METRIC_RE.match(metric):
        raise common.ConfigError(f"{metric[:40]!r} is not a metric name")
    parts = []
    if instance is not None:
        parts.append(f'instance="{plain_instance(instance)}"')
    if mountpoint is not None:
        parts.append(f'mountpoint="{plain_mountpoint(mountpoint)}"')
    for k in sorted(exclude or {}):
        parts.append(f'{k}!~"{check_exclude({k: exclude[k]})[k]}"')
    return f"last_over_time({metric}{{{','.join(parts)}}}[{WINDOW}])"


def ratio_expr(avail, size, exclude=None, instance=None, mountpoint=None):
    """avail / size per series, the series that match no exclusion; a series of no size is dropped (the `> 0`)."""
    if instance is not None and mountpoint is not None and len(instance) + len(mountpoint) > MAX_TARGET:
        raise common.ConfigError("instance and mountpoint are too long for a case target")
    return f"{_selector(avail, exclude, instance, mountpoint)} / ({_selector(size, exclude, instance, mountpoint)} > 0)"


def encode_target(labels):
    """The case target of a group: host:<instance><mountpoint> (the mountpoint's own leading /), or None when the labels cannot make
    one (a space, a character outside letters, digits and . _ / -, or too long). The host is what is before the first /."""
    inst, mount = labels.get("instance"), labels.get("mountpoint", "")
    if not isinstance(inst, str) or not INSTANCE_RE.match(inst):
        return None
    if mount != "" and (not isinstance(mount, str) or not MOUNT_RE.match(mount)):
        return None
    t = f"{inst}{mount}"
    return f"host:{t}" if len(t) <= MAX_TARGET else None


def decode_target(target):
    """(instance, mountpoint) of a target encode_target made, or None for any other target."""
    if not isinstance(target, str) or not target.startswith("host:"):
        return None
    inst, slash, rest = target[5:].partition("/")
    mount = slash + rest
    if not INSTANCE_RE.match(inst) or not MOUNT_RE.match(mount) or len(target) - 5 > MAX_TARGET:
        return None
    return inst, mount


def limit_of(action, stage):
    """The threshold as a fraction: the registry action's free_ratio_below precondition for the run's stage (its one home)."""
    for c in common.preconditions(action, stage):
        name, arg = common.parse(c)
        if name == CONDITION:
            if not FRACTION_RE.match(arg or "") or not 0 < float(arg) < 1:
                raise common.ConfigError(f"{CONDITION} takes a fraction above 0 and below 1, such as 0.15, not {arg!r}")
            return float(arg)
    raise common.ConfigError(f"{action['name']} has no {CONDITION} precondition")


def read_ratio(instance, mountpoint, avail=AVAIL_METRIC, size=SIZE_METRIC, query=None):
    """The newest free ratio of one mountpoint of one instance, or None when it reports nothing. Two series for one mountpoint
    (a bind mount) read as the smaller ratio, the fuller one."""
    return (query or common.query)(f"min({ratio_expr(avail, size, None, instance, mountpoint)})")
