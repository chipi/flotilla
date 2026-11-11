---
name: hint-external
description: Use when triage's code classes the signal as an upstream rejection: the provider refused on its own policy or safety rules.
---

PRE-CLASSIFICATION HINT (deterministic, advisory): this looks like an UPSTREAM REJECTION — the provider refused on its own policy/safety, not our defect. If so, work_type=config-enhancement (catch → skip/fallback that item + WARN). File work_type=bug only if our code should have handled it and does not. You decide.
