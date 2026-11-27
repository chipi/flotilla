---
name: e2e-known
description: Use when an e2e_sop case reports the alert e2e-known.
applies_to: {"kinds":["e2e_sop"],"hosts":["*"],"alerts":["e2e-known"]}
diagnosis: [{"read":"metric","name":"e2e_age"}]
fix: [{"action":"e2e.sop_fix"}]
verify: {"check":"e2e_nothing","within":"5m"}
rollback: []
rung: 1
evidence: ["case-e2e-0"]
---
The SOP e2e's fixture, never shipped: the one SOP the e2e fleet has. Do the fix; the registry's check verifies it.
