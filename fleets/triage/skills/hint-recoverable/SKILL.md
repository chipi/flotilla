---
name: hint-recoverable
description: Use when triage's code classes the signal as recoverable: the pipeline already handled or degraded through it and still logged ERROR.
---

PRE-CLASSIFICATION HINT (deterministic, advisory): this looks RECOVERABLE — the pipeline already handled/degraded through it and still logged ERROR. It is WARN-level, low-priority: file work_type=config-enhancement (track the degradation) rather than work_type=bug. File work_type=bug only if the recovery itself is broken. You decide.
