# The operator fleet: a plan backed by what broke (2026-11-26)

**Decisions (the operator, 2026-11-26):** the order below, and the ops fleet becomes
**operator**. Its job is to diagnose and fix what can be fixed through service management,
networking and configuration management, on every host: the mini, the VPS (prod), the DGX and the
X3. Its work comes from the monitors' alerts and from triage's escalations. Every case below is
backed by incidents in the 60 days to 2026-11-26; a case with no incident behind it stays out.

Sources: Grafana's alert annotations on the mini (524 firing transitions across 62 rules,
2026-09-27 to 2026-11-26), the 127 alert rules in homelab's
`infra/observability/backend/grafana/provisioning/alerting/rules.yaml`, flotilla's ledger,
triage's 51 escalated issues (15 bodies read), and homelab's runbooks and handover notes. Firing
counts are episodes (they flap), with distinct days in brackets.

## What exists today

- **One host action, never run.** `backup.rerun_observability` on the mini (`launchctl kickstart`
  of the db-backup job) is at `record`. It planned twice (2026-11-14, 2026-11-21) and ran nothing.
- **One host agent.** The mini has one. The VPS, the DGX and the X3 as a node have none. The
  X3's bugfix workers are refused since sha-07c2272, on a version mismatch.
- **A silent gap.**
  - On 2026-11-21 the mini's backup was missed. It reached 47.8h old, and the off-box run failed
    too ("no files for stamp 20261121-043004").
  - flotilla filed the rerun plan, closed the case within 3 seconds, and then absorbed 136 more
    readings over 23 hours without telling anyone.
  - The same happened on 2026-11-14.
- **Alerts reach the operator only by email,** except `critical` ones, which also go to Slack
  `#ops-critical`. Warnings never reach Slack.
- **Triage's escalations are mostly not host incidents.**
  - About 24 of the 51 are LiteLLM key or model denials and provider transients.
  - About 15 are client-side noise.
  - Real operational ones are few (homelab #73, #112; podcast #2271).
  - The operational signal that matters comes from Grafana.

## The cases, by actual need

Ranked by how often it happened and whether a fix through a host is possible. Rung is where the
action starts. The rungs:

- **0 observe:** diagnose and explain, change nothing.
- **1 auto:** safe and idempotent; it acts and reports.
- **2 approve:** reversible; it acts on the operator's one-tap approval.
- **3 prepare:** risky; it prepares the change and the operator applies it.

| # | Case | Host | 60 days | Detected by | Fix | Starts at |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Backup stale | mini (2 cases, off-box fail); VPS (Postgres, player, operator, corpus) | mini 2; VPS 7 (5 days) | backup-monitor; Grafana 26h/30h | rerun the backup job; check that a new one landed | 2, then 1 after evidence |
| 2 | Disk low or critical | mini (31), macbook (23) | 54 (10 days) | disk alerts | diagnose what grew, then a named cleanup (old logs, a known cache, images unused for N days) | 0 (diagnose), then 2 |
| 3 | Docker VM forward or egress broken | mini | 29 (8 days) | forward and egress heartbeats | the watchdog already restarts colima; the operator fleet confirms recovery, and escalates when it does not recover | 0, with a restart at 2 |
| 4 | Prod OOM kill or crashloop | VPS | 15 OOM (7 days), 4 crashloop, 2 limit hits | OOM classifier, restart counter | diagnose; a memory-limit change as a PR to the infra repo; a restart of the crashlooping container | 3 (PR), 2 (restart) |
| 5 | DGX memory and GPU overcommit | DGX | about 36 (mem 27, GPU 8, NVRM 15) | memory and GPU alerts | stop or swap a model container (`gpu-mode-swap.sh`); the known cause is `vllm-translate` co-resident since 2026-09-29 | 2 |
| 6 | Podcast pipeline, ingestion or enrichment failed | VPS | about 40 (jobs 14, ingestion 5, enricher 8, recap 11) | pipeline metrics and logs | diagnose; rerun the job when the cause is transient; a code fault goes to bugfix | 0, rerun at 2 |
| 7 | flotilla's own workers stale | X3 | 15 stale, 1 silent (11-11 to 11-21) | flotilla's staleness rules | update the X3 workers to flotillad's version | 2 |

**Out, because no host fix exists:**

- the DGX's hard power-offs (the physical button);
- provider and Cloudflare incidents (observe only);
- LiteLLM key and model denials (a config change, reviewed like code);
- scrape targets flapping on the DGX's pyannote and moss exporters (63 episodes): an alert to fix,
  not an incident;
- logs going dark (one cause, fixed 2026-10-14).

## What it takes

- **The platform first:**
  - Never silent: while the condition holds and no action may run, the case stays open and the
    operator is told in Slack. It closes only when the condition clears.
  - Slack approval buttons for operator actions. The host agent's Slack connector exists
    (ADR-0006): this is wiring and policy.
  - Alerts from Grafana as a source of operator cases, beside the code detectors.
- **Host agents on the VPS and the DGX** (the X3 later), installed with `deploy/host/install.sh`,
  each with its own allowlist of exact commands (ADR-0006).
- **New precondition kinds in `internal/hostexec/check.go`.** Today it knows only
  `launchd_not_running`. Needed: a container running or stopped, a disk's free space, a file's age.
- **Each action earns its rung:** a count of plans the operator agreed with, then promotion, as
  the backup rerun's gate (5 agreed plans) already says.

## The knowledge base: standard operating procedures that grow

The operator's model (2026-11-26):

1. **Match.** For each case, the agent looks for a standard operating procedure (SOP) that matches
   it: the alert, the host, the symptoms.
2. **Follow.** If one matches, it follows it. An SOP holds:
   - when it applies;
   - the diagnosis steps (read-only commands);
   - the fix (allowlisted host actions only);
   - how to verify;
   - how to roll back.

   It runs at the rung the SOP has earned.
3. **Investigate.** If none matches, the agent investigates on its own, read-only (logs, metrics,
   the host's state), writes down what it found, and proposes a fix. The fix is at rung 2: the
   operator approves it.
4. **Grow.** After every fix, the agent drafts the SOP, a new one or an update to the one it used,
   with the case as its evidence.
   - Under rule 3 (nothing rewrites its own playbooks at runtime) the draft is a proposal: the
     operator accepts it with one tap, and only then does it join the knowledge base.
   - A wrong diagnosis cannot become a procedure that later runs on its own.

**An SOP earns its rung:** each run the operator agreed with, and each verified fix, counts
towards promotion (2 to 1). A failed verification or a rollback sets it back.

**Where it lives:** the knowledge base is the installation's (host names, paths, services). It
lives in the private fleets repo, seeded from homelab's runbooks (the colima runbook among them),
and is read by the operator fleet like triage's playbooks.

**It applies to every operator case** (the operator, 2026-11-26): all seven cases above, and any
case added later, go through match, follow or investigate, and the proposed SOP. The case table's
fixes are the first SOPs to write, each one from its incidents.

## Proposed order

1. **The platform gap:**
   - never silent, plus Slack approvals;
   - the knowledge base's loop (match, follow, investigate, propose).

   Without it, nothing below is safe to rely on.
2. **The mini, disk first** (the most frequent need, the operator, 2026-11-26):
   - disk diagnosis and its first SOP (case 2);
   - the backup rerun to rung 2 (case 1);
   - confirming the colima watchdog's recoveries (case 3).
   - Also find why the backup was missed on 2026-11-21: a fault in the backup job, found by an
     operator reading its logs.
3. **The VPS:**
   - its host agent;
   - the prod backups (case 1);
   - pipeline diagnosis and rerun (case 6);
   - OOM diagnosis and the limit PR (case 4).
4. **The DGX:** its host agent, and the model-container swap (case 5).
5. **The X3:** its workers' version (case 7), and a host agent if a case needs one.

The rename (`ops` to `operator`: the fleet folder, its routines, and the mini's config) ships with
step 1, in one deploy. Agree the order before building.
