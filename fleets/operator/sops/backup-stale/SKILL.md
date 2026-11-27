---
name: backup-stale
description: Use when a host's off-box backup is older than its limit and the host has a backup job to rerun.
applies_to: {"kinds":["failed_backup"],"hosts":["*"],"alerts":[]}
diagnosis: [{"read":"metric","name":"backup_last_success_timestamp"}]
fix: [{"action":"backup.rerun"}]
verify: {"check":"backup_newer_than_execution","within":"60m"}
rollback: []
rung: 1
evidence: ["case-example-1"]
---
# A stale backup

An example SOP: placeholders only, an installation writes its own.

When a host's newest off-box backup is older than its limit:

1. Read the backup's last success time (the metric above) and confirm the age is real, not a gap
   in the metric.
2. Rerun the backup on that host with the registry's `backup.rerun` action. The registry's
   preconditions (the age is still above the limit, no backup is running) are re-checked by the
   acting routine and by the host.
3. The check passes when a backup newer than the rerun's execution lands within the hour.

Why: a rerun is regenerable and low risk, and a stale backup that nobody reruns stays stale. If
the rerun does not verify, the case escalates to a person: do not rerun a second time.
