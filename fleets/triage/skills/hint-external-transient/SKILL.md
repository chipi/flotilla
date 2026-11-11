---
name: hint-external-transient
description: Use when triage's code classes the signal as an external transient: a named third-party dependency timed out or was briefly unavailable.
---

PRE-CLASSIFICATION HINT (deterministic, advisory): this looks like an EXTERNAL TRANSIENT — a named third-party dependency timed out or was briefly unavailable. If so it is NOT a code invariant: file as work_type=config-enhancement (retry-with-backoff, then WARN + degrade on exhaustion). BUT if the evidence shows OUR code mishandles the failure, file work_type=bug. You decide.
