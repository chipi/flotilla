# RFC-0009 — Post-incident review: learning from every closed case

**Status:** Proposed (2026-11-15). Step 3 of the [vision plan](../wip/vision-plan.md).
**Builds on:** [RFC-0004](RFC-0004-agent-pipeline.md) (cases, policy, the memory loop: notes proposed
from cases), [ADR-0010](../adr/ADR-0010-memory-and-knowledge.md) (memory and knowledge), G8 (verdicts
and notes), G14 and T17 (the recurrence rule).

## Motivation

The first shadow case (`case-20261114-6983646f65`, 2026-11-14 23:45Z) went exactly as designed: the
backup was 20.1 h old, the chain planned a rerun on the mini, absorbed every later detection into the
one case, and cleared at 03:54Z when the daily backup landed (24.1 h at its oldest). The chain was
right within its preconditions. The check was not: a 20 h threshold on a backup that runs about every
24 h opens a case every night on a healthy backup. Nothing in flotilla said so; a person reading the
case did, and the operator changed the threshold to 25 h by hand.

In operations, closing an incident is followed by a review: what happened, and what to change so it
does not happen again. flotilla has no such step. This RFC adds it.

## Proposal

### 1. A review routine on closed cases

`ops/case-review`, code only, on the `ops-read` worker, runs when a case closes (a case trigger on
`closed`) and once a day over the last 7 days for the patterns no single case shows. It reads the case
store through the API, as any routine does, and writes nothing but proposals.

### 2. The rules (v1, code)

Each rule names the evidence it read: the case ids, the timeline lines, the numbers.

| Rule | Fires when | Proposes |
| --- | --- | --- |
| **below the cycle** | a case cleared on its own (no execution) and its detection's measure at the clear was within the source's normal cycle, as the monitor reported it (§3) | a `process_change`: the threshold above the cycle (the observed peak, rounded up) |
| **self-clearing** | the same kind and target cleared on its own 3 times in 7 days | a `process_change`: look at the threshold (no value proposed without a cycle) |
| **fix that does not hold** | a recurrence after a verified fix (G14, T17), twice in 30 days | a note: the fix treats a symptom; and a `process_change` to escalate the kind instead of rerunning |
| **wrong diagnosis** | a person's Wrong verdict | a note drafted from the case, for the person to edit or drop |
| **refused plan** | a person rejected a plan (with their reason) | a note: what the person said, against the kind and the action |

Code rules first, because they can be replayed and tested exactly. A model-written post-mortem from
the timeline comes later, as untrusted input that only proposes (RFC-0004 §4).

### 3. What monitors report

The rules need numbers, not sentences. A detection gains an optional `measure`: its name, the value,
the threshold it was compared to, and, when the monitor knows it, the source's normal cycle. The backup
monitor reads the cycle from VictoriaMetrics: the median time between successful backups over the last
14 days. These land on the case's timeline as structured fields, and so do the later absorbed
detections, so the review sees the peak.

### 4. Two outputs

- **A note** (exists, G8): proposed in Knowledge with the evidence; a person activates it before any
  agent reads it, or drops it.
- **A `process_change` case** (new kind): its plan is a change to a check, a threshold, a spec or the
  registry, as a diff against the repo at its commit, with the evidence. Always at propose, never auto,
  whatever any level says (the registry declares it so). It waits in Approvals like any plan.

### 5. Applying a process change

Approving it opens a PR on the flotilla repo with the diff and the evidence, so the change goes through
review, CI and deploy like any other change. The system never edits its own config in place. That needs
a repo write (`github.open_pr`, RFC-0004's repo actions), which step 4 (Bugfix) builds.

### 6. It feeds the record

`process_change` plans count on Autonomy like any action's: approved and merged is clean, rejected is
not. A rejected proposal's reason is kept, and the same rule on the same kind and target proposes
nothing for 30 days unless the evidence changes.

### 7. Tests

- Each rule replayed on recorded cases: last night's case, with its real measures, must give a
  `process_change` proposing 25 h with its evidence (the vision plan's done check); a healthy night
  (cleared at 3 h, far inside the cycle) must give nothing.
- Mutants for each rule's condition.
- A rejected proposal is recorded and not proposed again inside 30 days.

## Open questions (the operator's)

1. **Applying before step 4.** Until Bugfix brings `github.open_pr`: **proposed**, an approved
   `process_change` shows its diff and the operator applies it by hand (the case records the commit).
   The alternative is to build the repo write now, here, and use it again in step 4.
2. **Installation config.** Some changes are not in the repo (`flotillad.yaml` on the mini, such as
   `vlogs_url`). **Proposed:** v1 proposes only repo changes; for installation config it writes a note
   naming the key and the value.
3. **Where review runs.** **Proposed:** a routine on `ops-read` (code only, reads the API). The
   alternative is inside flotillad (no worker), simpler but outside the routine model every fleet uses.
4. **The cycle.** **Proposed:** the monitor reports it (it knows its source). The alternative is a
   cycle declared in the registry or the spec, fixed by a person.

## Alternatives considered

- **Only notes, no new case kind:** a note cannot be approved as a change; a process change would stay
  advice nobody tracks.
- **A model review from the start:** cannot be replayed exactly, and the first rule that mattered is
  arithmetic.
- **Fixing thresholds in the monitor itself (learning):** a check that tunes itself hides the change
  from the person who owns it.

## Discussion

- 2026-11-15: drafted after the operator's request ("post-incident report analysis … suggesting
  improvements to the process, which opens a case of another type that seeks my approval").
