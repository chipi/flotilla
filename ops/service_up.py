"""ops/service-up: what the service routines share (the DGX case, step 1, on RFC-0016 §8's model): a service gauge (1 up, 0 down, one series
per service) read as an expression, the case target a group becomes and back, and the service the registry action's precondition names.

The condition is the registry action's own: `service_down(<service>)` in its pre. The monitor, the acting step's fresh re-check and the
verify read the same service from it, one home. A series that has no reading is unknown, never up and never down. Every label value that
reaches an expression is plain: no quote, backslash, space or newline. Python stdlib only.
"""
import re

import common
import free_ratio

CONDITION = "service_down"
SERVICE_RE = re.compile(r"^[A-Za-z0-9._-]{1,63}$")
WINDOW = "10m"                      # the newest sample within it is the reading: a scrape or two missed is not a missing service
MAX_GROUP_LABELS = 2                # instance, and at most one more
MAX_TARGET = free_ratio.MAX_TARGET


def plain_service(v):
    if not isinstance(v, str) or not SERVICE_RE.match(v):
        raise common.ConfigError(f"a service is letters, digits and . _ -, not {str(v)[:40]!r}")
    return v


def plain_label(v):
    if not isinstance(v, str) or not free_ratio.LABEL_RE.match(v) or v.startswith("__"):
        raise common.ConfigError(f"{str(v)[:40]!r} is not a label name")
    return v


def plain_metric(v):
    if not isinstance(v, str) or not free_ratio.METRIC_RE.match(v):
        raise common.ConfigError(f"{str(v)[:40]!r} is not a metric name")
    return v


def selector(metric, label, services, instance=None):
    """metric{instance="...",label=~"a|b"}: the services are matched exactly (a Prometheus regex is anchored)."""
    parts = []
    if instance is not None:
        parts.append(f'instance="{free_ratio.plain_instance(instance)}"')
    names = [services] if isinstance(services, str) else list(services)
    if not names:
        raise common.ConfigError("no service to read")
    parts.append(f'{plain_label(label)}=~"{"|".join(plain_service(s) for s in names)}"' if len(names) > 1
                 else f'{plain_label(label)}="{plain_service(names[0])}"')
    return f"{plain_metric(metric)}{{{','.join(parts)}}}"


def read_up(instance, service, metric, label, query=None):
    """The newest reading of one service of one instance (1 up, 0 down), or None when it reports nothing in the window: unknown."""
    return (query or common.query)(f"min(last_over_time({selector(metric, label, service, instance)}[{WINDOW}]))")


def encode_target(group_by, labels):
    """The case target of a group: host:<instance>, and /<value> for the one more label a group has, or None when a value cannot be one."""
    inst = labels.get("instance")
    if not isinstance(inst, str) or not free_ratio.INSTANCE_RE.match(inst):
        return None
    rest = [labels.get(g, "") for g in group_by[1:]]
    if any(not isinstance(v, str) or not SERVICE_RE.match(v) for v in rest):
        return None
    t = "/".join([inst] + rest)
    return f"host:{t}" if len(t) <= MAX_TARGET else None


def host_of(target):
    """The instance a case target names (what is before its first /), or None for a target that is not on a host."""
    if not isinstance(target, str) or not target.startswith("host:"):
        return None
    inst = target[5:].partition("/")[0]
    return inst if free_ratio.INSTANCE_RE.match(inst) else None


def services_of(action, stage="propose"):
    """The services the action's service_down preconditions name, in order, or ConfigError when it has none."""
    out = []
    for c in common.preconditions(action, stage):
        name, arg = common.parse(c)
        if name == CONDITION:
            out.append(plain_service(arg))
    if not out:
        raise common.ConfigError(f"{action['name']} has no {CONDITION} precondition")
    return out


def service_of(action, stage="propose"):
    """The first of them: the service a verify of the action checks."""
    return services_of(action, stage)[0]
