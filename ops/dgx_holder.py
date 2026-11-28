"""ops/dgx-holder: is a job holding the host? (the DGX case, the operator's decision of 2026-11-28). When the prod model server is down because
a job HOLDS the host (the robotics mode with its trainer running, a code session, a judging sweep), the right action is to wait: no mode
switch. The switch is for a mode that was LEFT BEHIND: a non-prod mode whose holder has ended.

The reading is metrics only, the installation's own names (block params, never the repo's): a mode gauge whose `mode` label is the current
mode where it reads 1, the number of processes on the GPU, and, when the installation has one, a signal that the trainer's container is up.
Every other mode switch stops the robotics trainer after a checkpoint grace, so the robotics mode is the dangerous one and is read the
strictest way: a trainer reading that is missing or unreadable counts as HELD, never as free. Anywhere else an unreading is unknown, and
unknown never permits a switch (the caller escalates).

  held     a job holds the host: wait
  free     a non-prod mode nothing holds: it was left behind, the switch may be planned
  None     unknown (no mode reading, two modes at once, prod mode, no process reading outside the robotics mode)

Environment (set by the flow's params): HOLDER_MODE_METRIC, HOLDER_MODE_LABEL, HOLDER_PROCESS_METRIC, HOLDER_CONTAINER_METRIC (optional),
HOLDER_RESIDENT and HOLDER_PROCESS_LABEL (optional: services that stay on the GPU in every mode, named by a per-process metric's label;
only a process outside them holds the host),
HOLDER_PROD_MODE (default prod), HOLDER_TRAINER_MODE (default robotics), VM_URL. Python stdlib only.
"""
import os

import common
import free_ratio
import service_up

WINDOW = service_up.WINDOW
NOT_CONFIGURED = "not-configured"   # the installation names no container signal for its trainer


def _bad(x):
    return x is None or (isinstance(x, float) and x != x)


def classify(mode, procs, trainer, prod_mode="prod", trainer_mode="robotics"):
    """(held | free | None, why) from the mode (a name or None), the GPU process count (a number or None), and the trainer's container
    (True up, False down, None unreadable, NOT_CONFIGURED). Pure."""
    procs = None if _bad(procs) else procs
    if mode is None:
        return None, "no reading of the host's mode"
    if mode == prod_mode:
        return None, f"the mode is {prod_mode}: a switch to it would change nothing"
    if mode == trainer_mode:
        if trainer is True:
            return "held", f"mode {mode}: the trainer's container is up"
        if procs is not None and procs > 0:
            return "held", f"mode {mode}: {procs:g} process(es) on the GPU"
        if procs == 0 and trainer in (False, NOT_CONFIGURED):
            return "free", f"mode {mode}: no process on the GPU" + ("" if trainer == NOT_CONFIGURED else ", the trainer's container is down")
        # an unreadable trainer is held, never free: any other mode's switch stops it after a checkpoint grace
        return "held", f"mode {mode}: the trainer cannot be confirmed down (treated as held)"
    if procs is None:
        return None, f"mode {mode}: no reading of the processes on the GPU"
    if procs > 0:
        return "held", f"mode {mode}: {procs:g} process(es) on the GPU"
    return "free", f"mode {mode}: no process on the GPU (left behind)"


def holder_processes(inst, metric, label, resident, query_vector):
    """How many GPU processes of the instance are not resident services, from a per-process metric (one series per process, its
    container in `label`): None when it has no series at all (the collector is gone: unknown, never "nothing runs"). A process without
    the label is counted: it cannot be shown to be resident."""
    rows = query_vector(f'last_over_time({metric}{{instance="{inst}"}}[{WINDOW}])')
    if not rows:
        return None
    return float(sum(1 for lab, _ in rows if lab.get(label) not in resident))


def _need(environ, name):
    v = (environ.get(name) or "").strip()
    if not v:
        raise common.ConfigError(f"{name} is not set")
    return v


def read(case, environ=None, query=None, query_vector=None):
    """(held | free | None, why) of the case's own instance, read now. A query that fails raises (no decision on a broken read)."""
    environ = os.environ if environ is None else environ
    query, query_vector = query or common.query, query_vector or common.query_vector
    mode_metric = service_up.plain_metric(_need(environ, "HOLDER_MODE_METRIC"))
    mode_label = service_up.plain_label(_need(environ, "HOLDER_MODE_LABEL"))
    proc_metric = service_up.plain_metric(_need(environ, "HOLDER_PROCESS_METRIC"))
    container = (environ.get("HOLDER_CONTAINER_METRIC") or "").strip()
    if container:
        service_up.plain_metric(container)
    prod = (environ.get("HOLDER_PROD_MODE") or "prod").strip()
    trainer_mode = (environ.get("HOLDER_TRAINER_MODE") or "robotics").strip()
    for m in (prod, trainer_mode):
        service_up.plain_service(m)
    inst = service_up.host_of((case or {}).get("target"))
    if inst is None:
        return None, "the case is not on a host"
    free_ratio.plain_instance(inst)
    rows = query_vector(f"max by (instance, {mode_label}) (last_over_time({mode_metric}[{WINDOW}]) == 1)")
    modes = sorted({lab.get(mode_label) for lab, v in rows if lab.get("instance") == inst and v == 1.0 and lab.get(mode_label)})
    mode = modes[0] if len(modes) == 1 else None
    if len(modes) > 1:
        return None, f"two modes read 1 at once ({', '.join(modes)})"
    resident = [s.strip() for s in (environ.get("HOLDER_RESIDENT") or "").split(",") if s.strip()]
    if resident:
        procs = holder_processes(inst, proc_metric, service_up.plain_label(_need(environ, "HOLDER_PROCESS_LABEL")),
                                 [service_up.plain_service(s) for s in resident], query_vector)
    else:
        procs = query(f'max(last_over_time({proc_metric}{{instance="{inst}"}}[{WINDOW}]))')
    if container:
        c = query(f'min(last_over_time({container}{{instance="{inst}"}}[{WINDOW}]))')
        trainer = None if _bad(c) else c >= 1
    else:
        trainer = NOT_CONFIGURED
    return classify(mode, procs, trainer, prod, trainer_mode)
