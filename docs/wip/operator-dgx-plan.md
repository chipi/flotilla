# The DGX case (2026-11-28)

The operator's next case after disk and the performance watch, named on 2026-11-28: "the DGX is
not serving prod". It follows [RFC-0016](../rfc/RFC-0016-blocks-and-flows.md) §8's model: blocks,
a flow, everything at record first.

## Signal

- Measured on 2026-11-28 in the installation's metrics store:
  - a gauge per DGX service, 1 up and 0 down, with the host, the instance and the service;
  - a gauge for the GPU mode, 1 for the current one (prod, code, robotics, free).
- The prod model server is down exactly when the mode is not prod:
  - 11-20, code mode, 13.5 hours;
  - 11-26, robotics and free, 5.2 hours;
  - 11-27, 16 hours.
- Prod demand is continuous, so the server down means prod is impacted.
- No alert fired on 11-26, and the GlitchTip flood of those days is the other face of the same gap
  (history 0010).

## Step 1 (built, at record)

- `service-monitor@1`: code only. One case per instance whose watched service read down for N
  readings in a row (default 2 at a 10m step, the 15 to 20 minutes an alert would wait). The service
  is the one the registry action's `service_down(<service>)` precondition names, so the condition has
  one home. The mode gauge's label rides on the detection.
  - An absent series is unknown, never up. A group that was down is held until `clear_after`
    readings in a row actually read up. It replays the last day of the gauge's own history, so a
    host that stops reporting keeps its case.
  - A failed query fails the run: nothing is reported, so no case is cleared by a broken read.
- `service_down(<service>)` in `ops/sop_act.py`: a fresh read of the case's instance; no reading
  escalates and is never "resolved".
- `service-verify@1`: the check `service_up_again`, the service up throughout five minutes. A new
  block rather than a version of `metric-verify`, because a block has one command.
- `sop-act@3` and `sop-investigate@3`: the new params; the station gains a `series` read (every
  series of a metric with its labels), for the service gauges, the mode and the GPU's memory and
  processes.
- `dgx.gpu_mode_prod`: a record-only registry action holding the condition and the check.
- An example flow in `fleets/jobs/flows/service` with placeholder names, expanded and committed; the
  installation's own flow and SOP are in its private fleets repo.

## Step 2 (not built)

`gpu-mode-swap.sh prod` on the DGX through a host agent, held at propose: a robotics or code session
may be deliberate, so a person approves each swap. The action stops being record-only then, and the
case's host and its approval need the same decisions as disk's step 3.

## Not covered

- Nothing is deployed, and the installation's flow and SOP are not committed to the private repo.
- No host agent on the DGX, and no swap.
- The metric names and the instance of the installation are in the private flow only.
- `metric-monitor@2` still reads a vanished series as clear (the open thread in history 0010). A
  `metric-monitor@3` can adopt the same rule: read the last day of history, treat an absent point as
  unknown, close only after `clear_after` readings that read clear.
- The case's target for several groups per host (`host:<instance>/<value>`) is read by the SOP
  matcher as a host of its own; step 1 uses one case per instance, `host:<instance>`.
