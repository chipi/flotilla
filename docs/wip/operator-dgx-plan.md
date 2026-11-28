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

## Step 1b: holder-aware (2026-11-28, built, at record)

The operator's decision: when the DGX is not serving prod because a job HOLDS it (the robotics mode
with its trainer running, a code session, a judging sweep), wait: no switch, prod keeps failing
until the holder ends. Switch only a mode LEFT BEHIND (non-prod, its holder ended). Every mode other
than robotics stops the robotics trainer after a checkpoint grace, so the robotics mode is read the
strictest way.

- `dgx.wait_for_holder`: a record-only action for `dgx_not_serving`: `service_down(model-prod)` and
  `holder_active`. `on_record: watch` keeps the case open as watching with the holder in the detail
  (ADR-0023). `dgx.gpu_mode_prod` gained `no_holder_active`.
- The holder is read from metrics (`ops/dgx_holder.py`): a mode gauge's `mode` label where it reads
  1, the processes on the GPU, and, when the installation has one, a signal that the trainer's
  container is up. No such signal is known to exist for the training container: without it the
  robotics mode rests on the mode and the process count, and a trainer reading that is missing
  counts as held. An unknown reading never permits the switch: robotics plans the wait, any other
  mode escalates. A `service_up` gauge that is a TCP-open check says the port is open, not that the
  model serves.
- `sop-act@4` (new version: its params and behaviour differ): a `fixes: first_holding` param makes
  an SOP's several fixes alternatives, the first whose preconditions hold wins, one fresh read
  serves them all, and an unknown never steps past to a later one. Without it a several-fix SOP
  escalates, as before. The example `service` flow's act and investigate steps are
  pinned to `sop-act@4` and `sop-investigate@4`, still version 1: the example is not deployed, and a
  second version needs a shadow worker per worker in every stack that runs the examples.
- The re-run of the nightly pipeline is its own case (`batch_window_failed`), opened by
  `health-probe@1` once the pipeline's `scheduled-run` capability is ready again after an outage;
  see the [health contract](health-contract.md). The nightly never sets the DGX's mode.
- The installation's flow, SOPs and registry are in its private fleets repo; the files built here
  were reconstructed from the public example, not patched from the committed ones.

## Step 2 (not built)

`gpu-mode-swap.sh prod` on the DGX through a host agent, held at propose: a robotics or code session
may be deliberate, so a person approves each swap. The action stops being record-only then, and the
case's host and its approval need the same decisions as disk's step 3.

## Not covered

- Nothing is deployed, and the installation's flow and SOP are not committed to the private repo
  (step 1b's are drafted, not committed).
- No host agent on the DGX, and no swap.
- The metric names and the instance of the installation are in the private flow only.
- `metric-monitor@2` still reads a vanished series as clear (the open thread in history 0010). A
  `metric-monitor@3` can adopt the same rule: read the last day of history, treat an absent point as
  unknown, close only after `clear_after` readings that read clear.
- The case's target for several groups per host (`host:<instance>/<value>`) is read by the SOP
  matcher as a host of its own; step 1 uses one case per instance, `host:<instance>`.
