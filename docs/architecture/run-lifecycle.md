# A run, end to end

What happens from a trigger to a measured outcome. The example is the SEO fleet's weekly regression
job; the triage fleet has the same shape with an alert as the subject.

```
trigger ─► checks ─► start ─► collect ─► detect ─► for each candidate: agent loop ─► gates ─► decision
                                                                                              │
             outcome measured later ◄─ output runs ◄─ approved by the operator ◄─ proposal ◄──┘
```

## 1. Trigger

The scheduler fires `seo/orrery-regressions` on its cron (Mondays 07:00). Before starting, it
checks:
- the job is not paused and its fleet has no STOP flag;
- today's budget is not spent;
- no run of this job is still going (one at a time per job unless the spec allows more).

A failed check is recorded as a skipped run with its reason, so it is visible, not silent.

## 2. Start

flotillad creates the run:
- a run id and a trace id;
- the spec version and the job identity.

It asks the SEO fleet's worker to start it, with a deadline.

## 3. Collect and detect

The worker runs the job's `collect` steps. They read the latest Search Console data (written daily
by a separate collector job), the crawl snapshot and Umami's page numbers. Then the `detect` steps
run: SQL over the data, producing candidates such as "page X: clicks −27% over 28 days, position
6.1 → 8.4". No model has been involved yet.

## 4. Agent loop, per candidate

1. **Context:**
   - the playbook (`playbooks/seo-regression.md`) as the system prompt;
   - the site profile;
   - the candidate's evidence;
   - the prior decisions about this page from the ledger, including any the operator rejected and why.
2. **Offer the declared tools** (`seo.page_history`, `seo.queries_for_page`, `seo.crawl_changes`,
   `findings.existing`).
3. **The model asks; the tool registry answers.** It checks scope (this site, this time range), caps
   the size, and attaches provenance.
4. **It stops** when the model answers in the output schema, or at `max_steps`. A malformed answer
   gets one retry.

Every call is traced.

## 5. Gates

Deterministic checks on the answer:
- every claim cites evidence the tools actually returned;
- not a duplicate of an open finding (`dedup_key`);
- in scope;
- plus the fleet's own gates.

A failed gate records the decision with status `gated` and the reason. It is never dropped silently.

## 6. Decision and proposal

The worker reports the decision to flotillad, which writes it to the ledger: subject, evidence
references, model, playbook and spec versions, cost, and actor = the job.

If the decision wants an outward action, here "open an issue in the Orrery repo with this proposed
change", it becomes a **proposal**:
- **`shadow`:** recorded, nothing else.
- **`propose`:** queued for the operator, unless the spec allows this exact output at this stage
  (the triage fleet's issue filing is allowed in `propose` today).
- **`live`:** it runs within the spec's declared limits.

## 7. Approval

The operator sees the proposal in the UI's queue, with its evidence, and approves or rejects it,
with a reason. Agents can see the queue through MCP but cannot approve. The ledger records the
operator as the actor.

## 8. Output

flotillad asks the fleet's worker to apply the approved output. The worker has the credentials;
the control plane does not. The output checks its scope again (the repo is in the fleet's repo map)
and records the result (issue number, PR URL) on the decision.

## 9. Outcome

Later, a separate job measures what happened: the issue closed, the PR merged, the page's clicks
four weeks after the change. It writes the outcome onto the decision. The next run's context
includes it, which is how the fleet learns without rewriting itself.

## 10. End

The run reports its status (succeeded, failed, timed out, partially gated), its cost and its
tokens. flotillad updates the job's freshness metric. A job that has not succeeded within its
declared freshness raises its stale alert.

## Failure behaviour

| Failure | Behaviour |
|---|---|
| The model endpoint is down | the run fails loudly and is retried at the next trigger; never a silent switch to another model |
| A tool errors | the error is given to the model as the tool's answer; after `max_steps` the candidate is recorded as `incomplete` |
| A malformed answer twice | the candidate is recorded as `invalid_output` with the raw answer referenced in the trace |
| The worker crashes or hangs | the deadline kills the run; its decisions so far stay in the ledger, marked with the run's failed status |
| flotillad restarts | the scheduler resumes; a run that was in flight is marked `interrupted`; missed triggers run once at start-up, or not at all, as the spec declares |
| Budget spent mid-run | the run finishes the candidate in hand and stops; the remaining candidates are recorded as `deferred` |
