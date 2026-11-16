// Package dispatch starts a case's acting routine when the case is handed over to its kind (RFC-0004
// §3.7 and §7, pipeline v1 step 5): it leases the case to the routine, runs it with the case, and acts
// on what the run decided. The run never moves the case itself: flotillad does, through the transition
// table and policy, so a job cannot skip either.
//
// What a run writes to its plan file (FLOTILLA_PLAN_FILE), one JSON object:
//
//	{"outcome": "plan", "action": "backup.rerun_observability", "params": {"host": "mini"}}
//	{"outcome": "resolved_itself", "detail": "backup 2h old after a fresh look"}
//	{"outcome": "escalate", "detail": "the re-check contradicts the diagnosis"}
//	{"outcome": "not_reproducible", "detail": "the failing call is not on main"}
//	{"outcome": "hand_over", "stage": "clarify", "detail": "needs info", "data": {"questions": [...]}}
//
// A hand_over (RFC-0013 §2) keeps the case handed over and moves it to the stage its run named: the
// routine for that kind and stage acts next, and reads the data as stage_data in FLOTILLA_CASE. A case
// may be handed on twice; the third escalates it. A kind whose first-stage routine declares a limit
// (trigger.case.limit) shares it across all of its case's runs: a case past it escalates instead of
// running again, and FLOTILLA_CASE says what is left of it.
//
// A plan moves the case to planned (by the run's routine) and goes to policy. Anything else (a failed
// run, no file, a bad file, an unknown outcome, another case's id) leaves the case handed over with a
// run.no_decision event, and the case waits out the backoff before it is tried again; the stuck-state
// timer escalates it if that goes on (internal/casetimer).
//
// A case in checking goes the same way to its action's check routine (trigger `check: {verify: ...}`),
// with FLOTILLA_CHECK beside FLOTILLA_CASE; its result file is
//
//	{"outcome": "verified", "detail": "backup at 10:42, after the execution at 10:31"}
//	{"outcome": "not_yet", "detail": "the newest backup is still 09:00"}
//
// verified closes the case; not_yet waits for the backoff and looks again; failed escalates it (what
// the action did is not what was wanted: a PR closed without merging); the check's window ending first
// escalates it as not verified (checks).
//
// A repo action a person approved goes to its executing routine (trigger `execute: {action: ...}`,
// RFC-0010 §3), with FLOTILLA_PLAN (the approved plan and its hash) beside FLOTILLA_CASE; its file is
//
//	{"outcome": "executed", "detail": "draft PR https://github.com/o/r/pull/7", "ref": "o/r#7"}
//	{"outcome": "escalate", "detail": "the base commit is gone"}
//
// executed moves the case to checking, escalate escalates it. It runs at most once per plan: anything
// else (a failed run, no file) escalates the case as unknown, since the run may have pushed. A repo
// action with no executing routine is applied by a person, who records the commit (RecordApplied).
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// DefaultBackoff is how long a case waits after a defer or a run with no decision before it is
// dispatched again.
const DefaultBackoff = 10 * time.Minute

type Dispatcher struct {
	API     *api.Server
	Backoff time.Duration
	Log     *slog.Logger

	wg sync.WaitGroup
}

// The run's view of its case: FLOTILLA_CASE.
type caseJSON struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Target  string          `json:"target"`
	Project string          `json:"project"`
	Keys    []store.Key     `json:"keys"`
	Data    json.RawMessage `json:"data,omitempty"` // the opening detection's data (ADR-0016)
	// RFC-0013 §2: the stage the case is at, what the routine that handed it on left for this one, and
	// what is left of the case's limit (absent when its kind has none)
	Stage     string          `json:"stage,omitempty"`
	StageData json.RawMessage `json:"stage_data,omitempty"`
	Left      *caseLeft       `json:"left,omitempty"`
}

type caseLeft struct {
	USD     float64 `json:"usd"`
	Seconds float64 `json:"seconds"`
}

// openingData is the data its first detection gave the case, for its routines.
func (d *Dispatcher) openingData(ctx context.Context, id string) json.RawMessage {
	evs, err := d.API.Store.CaseEvents(ctx, id)
	if err != nil {
		return nil
	}
	for _, e := range evs {
		if e.Kind == "case.detected" && e.Data != "" {
			return json.RawMessage(e.Data)
		}
	}
	return nil
}

// Decision is a run's plan file.
type Decision struct {
	Outcome string            `json:"outcome"`
	CaseID  string            `json:"case_id,omitempty"` // optional; when present it must be the run's case
	Action  string            `json:"action,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Detail  string            `json:"detail,omitempty"`
	Ref     string            `json:"ref,omitempty"` // an executing run's: what it made (a PR, owner/repo#n)
	// hand_over (RFC-0013 §2): the stage the case goes to and what its routine should read
	Stage string          `json:"stage,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Loop runs a pass every interval until ctx ends, then waits for the runs it started.
func (d *Dispatcher) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := d.Once(ctx); err != nil {
			d.Log.Error("dispatch", "err", err)
		}
		// executing cases on a host go to it, signed (ADR-0006 §4)
		if _, err := d.API.SendExecutions(ctx); err != nil {
			d.Log.Error("dispatch: host executions", "err", err)
		}
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return
		case <-t.C:
		}
	}
}

// Wait waits for the runs Once started.
func (d *Dispatcher) Wait() { d.wg.Wait() }

// Once frees expired leases, then starts the acting routine for each handed-over case nobody holds.
func (d *Dispatcher) Once(ctx context.Context) error {
	st, sc := d.API.Store, d.API.Sched
	now := sc.Clock()
	if n, err := st.ExpireLeases(ctx, now); err != nil {
		return err
	} else if n > 0 {
		d.Log.Warn("case leases expired", "count", n)
	}
	open, err := st.Cases(ctx, store.CaseFilter{Status: cases.HandedOver, Limit: 500})
	if err != nil {
		return err
	}
	busy := sc.Running()
	for _, c := range open {
		if c.Holder != "" { // a fast path: LeaseCase below is the guard
			continue
		}
		j := actingRoutine(sc.Routines, c.Kind, c.Stage)
		if j == nil {
			continue // no routine acts on this kind and stage here: the stuck-state timer escalates it
		}
		spent, err := d.bugSpend(ctx, c, now)
		if err != nil {
			return err
		}
		if why := overLimit(spent, caseLimit(sc.Routines, c.Kind)); why != "" {
			if err := st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByPolicy, "", "flotillad", why); err != nil && !errors.Is(err, store.ErrBadTransition) {
				return err
			}
			continue
		}
		if _, running := busy[j.ID()]; running {
			continue // one run of a routine at a time: the case waits its turn, with nothing to record
		}
		if why, err := sc.SkipReason(ctx, j); err != nil {
			return err
		} else if why != "" {
			continue // paused, its fleet STOPped, its day's budget spent: the case waits, with nothing to record
		}
		if wait, err := d.backingOff(ctx, c.ID, now); err != nil || wait {
			continue
		}
		ok, err := st.LeaseCase(ctx, c.ID, j.ID(), now.Add(j.Limit+2*time.Minute))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		busy[j.ID()] = "started by this pass"
		d.wg.Add(1)
		go func(c store.Case, j *spec.Routine) {
			defer d.wg.Done()
			bg := context.WithoutCancel(ctx)
			defer st.ReleaseCase(bg, c.ID, j.ID())
			if err := d.run(bg, c, j); err != nil {
				d.Log.Error("acting run", "case", c.ID, "routine", j.ID(), "err", err)
			}
		}(c, j)
	}
	if err := d.executions(ctx); err != nil {
		return err
	}
	if err := d.deadlines(ctx, now); err != nil {
		return err
	}
	return d.checks(ctx, now)
}

// ExecutingGrace is how long, beyond its action's timeout, an executing case waits for a result: the
// host's not-connected limit (api.NotConnectedLimit, 10m) and the answer's own margin, with room.
const ExecutingGrace = 15 * time.Minute

// defaultActionTimeout is the timeout of an action the registry gives none (as api.sendOne's).
const defaultActionTimeout = 30 * time.Minute

// deadlines is RFC-0004 §3.5's executing deadline: a case still executing past its action's timeout
// and ExecutingGrace, with no execution in flight, has had no result that anyone will deliver (no
// executor for its target, no signing key, a result lost): it is unknown. The timer escalates it, and
// the target stays held until a person closes it. An execution still in flight is left to its own
// bound, which ends it first.
func (d *Dispatcher) deadlines(ctx context.Context, now time.Time) error {
	st := d.API.Store
	open, err := st.Cases(ctx, store.CaseFilter{Status: cases.Executing, Limit: 500})
	if err != nil {
		return err
	}
	for _, c := range open {
		plans, err := st.Plans(ctx, c.ID)
		if err != nil {
			return err
		}
		timeout, action, execID := defaultActionTimeout, "", ""
		if len(plans) > 0 {
			p := plans[len(plans)-1]
			action, execID = p.Action, api.ExecutionID(c.ID, p.ID)
			if d.API.Registry != nil {
				if a, ok := d.API.Registry.Action(p.Action); ok && a.Timeout > 0 {
					timeout = time.Duration(a.Timeout)
				}
			}
		}
		since, err := time.Parse(time.RFC3339Nano, c.StatusAt)
		if err != nil {
			return fmt.Errorf("case %s: status_at %q: %w", c.ID, c.StatusAt, err)
		}
		if len(plans) > 0 && d.API.Registry != nil && executeRoutine(d.API.Sched.Routines, action) != nil {
			if a, ok := d.API.Registry.Action(action); ok && a.Repo() {
				// an approved repo action waits for its executing routine (its machine may be off: RFC-0011
				// §4); the deadline counts from the run's start, and there is none before it starts
				started, at, err := d.executeStarted(ctx, c.ID, plans[len(plans)-1].ID)
				if err != nil {
					return err
				}
				if !started {
					continue
				}
				since = at
			}
		}
		limit := timeout + ExecutingGrace
		if now.Sub(since) < limit {
			continue
		}
		if execID != "" {
			x, err := st.HostExecution(ctx, execID)
			if err != nil {
				return err
			}
			if x != nil && x.Status == store.ExecSent {
				continue // in flight: its result, or its own bound, ends it
			}
		}
		detail := fmt.Sprintf("unknown: no result for %s within %s of its start (its timeout %s and %s); the target stays held",
			action, limit, timeout, ExecutingGrace)
		if err := st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByTimer, "", "flotillad", detail); err != nil {
			return err
		}
		d.Log.Error("a case had no result past its executing deadline", "case", c.ID, "action", action, "target", c.Target, "limit", limit.String())
	}
	return nil
}

// checks is the check step (RFC-0004 §3.5 and §7): a case in checking is verified by its action's check
// routine (the registry's `check.verify`), run again after each "not yet" at the backoff, until it reports
// verified, which closes the case and releases its target. Past the check's window (`check.within`,
// counted from the move into checking) the case is not verified: the timer escalates it, target held.
func (d *Dispatcher) checks(ctx context.Context, now time.Time) error {
	st := d.API.Store
	open, err := st.Cases(ctx, store.CaseFilter{Status: cases.Checking, Limit: 500})
	if err != nil {
		return err
	}
	for _, c := range open {
		if c.Holder != "" {
			continue
		}
		chk, p, err := d.checkFor(ctx, c.ID)
		if err != nil {
			return err
		}
		since, err := time.Parse(time.RFC3339Nano, c.StatusAt)
		if err != nil {
			return fmt.Errorf("case %s: status_at %q: %w", c.ID, c.StatusAt, err)
		}
		if chk == nil {
			// no check declared for the action (a registry changed under it): nothing can verify it
			if err := st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByTimer, "", "flotillad",
				fmt.Sprintf("not verified: the registry declares no check for %q", p.Action)); err != nil {
				return err
			}
			continue
		}
		if now.Sub(since) >= time.Duration(chk.Within) {
			detail := fmt.Sprintf("not verified: %s did not hold within %s of the execution", chk.Verify, time.Duration(chk.Within))
			if err := st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByTimer, "", "flotillad", detail); err != nil {
				return err
			}
			d.Log.Warn("case escalated: not verified in its window", "case", c.ID, "action", p.Action, "verify", chk.Verify)
			continue
		}
		j := checkRoutine(d.API.Sched.Routines, chk.Verify)
		if j == nil {
			continue // no routine checks this here: the window's end escalates it
		}
		if wait, err := d.backingOff(ctx, c.ID, now); err != nil || wait {
			continue
		}
		ok, err := st.LeaseCheck(ctx, c.ID, j.ID(), now.Add(j.Limit+2*time.Minute))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		d.wg.Add(1)
		go func(c store.Case, j *spec.Routine, chk registry.Check, p store.PlanRecord) {
			defer d.wg.Done()
			bg := context.WithoutCancel(ctx)
			defer st.ReleaseCase(bg, c.ID, j.ID())
			if err := d.check(bg, c, j, chk, p); err != nil {
				d.Log.Error("check run", "case", c.ID, "routine", j.ID(), "err", err)
			}
		}(c, j, *chk, p)
	}
	return nil
}

// checkFor is the check of the action the case executed: its latest plan's (as the execution's).
func (d *Dispatcher) checkFor(ctx context.Context, id string) (*registry.Check, store.PlanRecord, error) {
	plans, err := d.API.Store.Plans(ctx, id)
	if err != nil || len(plans) == 0 {
		return nil, store.PlanRecord{}, err
	}
	p := plans[len(plans)-1]
	if d.API.Registry == nil {
		return nil, p, nil
	}
	a, ok := d.API.Registry.Action(p.Action)
	if !ok || a.Check.Verify == "" {
		return nil, p, nil
	}
	return &a.Check, p, nil
}

func checkRoutine(rs []*spec.Routine, verify string) *spec.Routine {
	for _, j := range rs {
		if j.Enabled && j.OnCheck() == verify {
			return j
		}
	}
	return nil
}

// The check run's view of what it verifies: FLOTILLA_CHECK.
type checkJSON struct {
	Verify   string            `json:"verify"`
	Action   string            `json:"action"`
	Params   map[string]string `json:"params"`
	Since    string            `json:"since"` // when the case went to checking (the actuator's done)
	Deadline string            `json:"deadline"`
}

const notYetEvent = "check.not_yet"

func (d *Dispatcher) check(ctx context.Context, c store.Case, j *spec.Routine, chk registry.Check, p store.PlanRecord) error {
	st := d.API.Store
	full, err := st.Case(ctx, c.ID)
	if err != nil {
		return err
	}
	since, _ := time.Parse(time.RFC3339Nano, c.StatusAt)
	cj, _ := json.Marshal(caseJSON{ID: full.ID, Kind: full.Kind, Target: full.Target, Project: full.Project, Keys: full.Keys,
		Data: d.openingData(ctx, full.ID)})
	kj, _ := json.Marshal(checkJSON{Verify: chk.Verify, Action: p.Action, Params: p.Params, Since: c.StatusAt,
		Deadline: since.Add(time.Duration(chk.Within)).UTC().Format(time.RFC3339)})
	r, out, err := d.API.Sched.RunWith(ctx, j.ID(), "check", "flotillad", map[string]string{"FLOTILLA_CASE": string(cj), "FLOTILLA_CHECK": string(kj)})
	if err != nil {
		return d.noDecision(ctx, c.ID, j.ID(), "the check run did not start: "+err.Error())
	}
	if r.Status != store.StatusOK || out == nil {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("check run %s ended %s", r.ID, r.Status))
	}
	if out.PlanError != "" || len(out.Plan) == 0 {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("check run %s wrote no result %s", r.ID, out.PlanError))
	}
	var dec Decision
	if err := json.Unmarshal(out.Plan, &dec); err != nil {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("check run %s: result file: %v", r.ID, err))
	}
	if dec.CaseID != "" && dec.CaseID != c.ID {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("check run %s reported for case %s, not this one", r.ID, dec.CaseID))
	}
	switch dec.Outcome {
	case "verified":
		if err := st.MoveCase(ctx, c.ID, cases.Closed, cases.ByCheck, cases.Verified, j.ID(), strings.TrimSpace("run "+r.ID+": "+dec.Detail)); err != nil {
			return err
		}
		d.Log.Info("case verified", "case", c.ID, "run_id", r.ID, "verify", chk.Verify)
		return nil
	case "not_yet":
		// the window decides "not verified", not one look: checked again after the backoff
		return st.AddCaseEvent(ctx, c.ID, notYetEvent, j.ID(), strings.TrimSpace("run "+r.ID+": "+dec.Detail))
	case "failed":
		// what the action did is not what was wanted (a PR closed without merging): a person looks
		return st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByCheck, "", j.ID(), strings.TrimSpace("not verified: run "+r.ID+": "+dec.Detail))
	default:
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("check run %s: outcome %q is not verified, not_yet or failed", r.ID, dec.Outcome))
	}
}

func executeRoutine(rs []*spec.Routine, action string) *spec.Routine {
	for _, j := range rs {
		if j.Enabled && j.OnExecute() == action {
			return j
		}
	}
	return nil
}

// The executing run's view of what it carries out: FLOTILLA_PLAN, the plan the person approved.
type planJSON struct {
	ID       int64             `json:"id"`
	Action   string            `json:"action"`
	Params   map[string]string `json:"params"`
	PlanHash string            `json:"plan_hash"`
}

const (
	executeStartedEvent    = "execute.started"
	executeNotStartedEvent = "execute.not_started" // the scheduler refused or skipped it: nothing ran
)

// executions starts the executing routine of each approved repo action (RFC-0010 §3), once per plan:
// the execute.started event, written before the run, is the at-most-once guard, with the lease.
func (d *Dispatcher) executions(ctx context.Context) error {
	st := d.API.Store
	if d.API.Registry == nil {
		return nil
	}
	open, err := st.Cases(ctx, store.CaseFilter{Status: cases.Executing, Limit: 500})
	if err != nil {
		return err
	}
	busy := d.API.Sched.Running()
	for _, c := range open {
		if c.Holder != "" {
			continue
		}
		plans, err := st.Plans(ctx, c.ID)
		if err != nil {
			return err
		}
		if len(plans) == 0 {
			continue
		}
		p := plans[len(plans)-1]
		if a, ok := d.API.Registry.Action(p.Action); !ok || !a.Repo() {
			continue // a host action: its host agent executes it (SendExecutions)
		}
		j := executeRoutine(d.API.Sched.Routines, p.Action)
		if j == nil {
			continue // applied by a person (RecordApplied)
		}
		// a run that cannot start now must not spend the plan's one attempt: busy (two approvals in one
		// pass), paused, its fleet STOPped, its budget spent: the case waits, with nothing recorded
		if _, running := busy[j.ID()]; running {
			continue
		}
		if why, err := d.API.Sched.SkipReason(ctx, j); err != nil {
			return err
		} else if why != "" {
			continue
		}
		started, _, err := d.executeStarted(ctx, c.ID, p.ID)
		if err != nil {
			return err
		}
		if started {
			continue
		}
		ok, err := st.LeaseExecution(ctx, c.ID, j.ID(), d.API.Sched.Clock().Add(j.Limit+2*time.Minute))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := st.AddCaseEvent(ctx, c.ID, executeStartedEvent, j.ID(), fmt.Sprintf("plan %d %s", p.ID, shortHash(p.PlanHash))); err != nil {
			_ = st.ReleaseCase(ctx, c.ID, j.ID())
			return err
		}
		busy[j.ID()] = "started by this pass"
		d.wg.Add(1)
		go func(c store.Case, j *spec.Routine, p store.PlanRecord) {
			defer d.wg.Done()
			bg := context.WithoutCancel(ctx)
			defer st.ReleaseCase(bg, c.ID, j.ID())
			if err := d.execute(bg, c, j, p); err != nil {
				d.Log.Error("executing run", "case", c.ID, "routine", j.ID(), "err", err)
			}
		}(c, j, p)
	}
	return nil
}

// executeStarted is whether the plan's executing run started (its latest execute.started is not followed
// by an execute.not_started: a run the scheduler refused or skipped did nothing), and when.
func (d *Dispatcher) executeStarted(ctx context.Context, id string, plan int64) (bool, time.Time, error) {
	evs, err := d.API.Store.CaseEvents(ctx, id)
	if err != nil {
		return false, time.Time{}, err
	}
	prefix := fmt.Sprintf("plan %d ", plan)
	started, at := false, time.Time{}
	for _, e := range evs {
		if !strings.HasPrefix(e.Detail, prefix) {
			continue
		}
		switch e.Kind {
		case executeStartedEvent:
			started = true
			at, _ = time.Parse(time.RFC3339Nano, e.At)
		case executeNotStartedEvent:
			started = false
		}
	}
	return started, at, nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// execute runs the executing routine once and moves the case on what it reported.
func (d *Dispatcher) execute(ctx context.Context, c store.Case, j *spec.Routine, p store.PlanRecord) error {
	st := d.API.Store
	full, err := st.Case(ctx, c.ID)
	if err != nil {
		return err
	}
	unknown := func(why string) error {
		d.Log.Error("executing run made no decision: the case is unknown", "case", c.ID, "routine", j.ID(), "why", why)
		return st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByAct, "", j.ID(),
			"unknown: "+why+"; it may have acted, so it is not run again: look before you retry")
	}
	cj, _ := json.Marshal(caseJSON{ID: full.ID, Kind: full.Kind, Target: full.Target, Project: full.Project, Keys: full.Keys,
		Data: d.openingData(ctx, full.ID)})
	pj, _ := json.Marshal(planJSON{ID: p.ID, Action: p.Action, Params: p.Params, PlanHash: p.PlanHash})
	r, out, err := d.API.Sched.RunWith(ctx, j.ID(), "execute", "flotillad", map[string]string{"FLOTILLA_CASE": string(cj), "FLOTILLA_PLAN": string(pj)})
	notStarted := func(why string) error {
		// nothing ran: the attempt is given back, and the case waits for the next pass
		d.Log.Warn("executing run did not start: the plan waits", "case", c.ID, "routine", j.ID(), "why", why)
		return st.AddCaseEvent(ctx, c.ID, executeNotStartedEvent, j.ID(), fmt.Sprintf("plan %d %s: %s", p.ID, shortHash(p.PlanHash), why))
	}
	if err != nil {
		return notStarted(err.Error())
	}
	if r.Status == store.StatusSkipped {
		return notStarted("skipped: " + r.Detail)
	}
	if r.Status != store.StatusOK || out == nil {
		return unknown(fmt.Sprintf("executing run %s ended %s", r.ID, r.Status))
	}
	if out.PlanError != "" || len(out.Plan) == 0 {
		return unknown(fmt.Sprintf("executing run %s wrote no result %s", r.ID, out.PlanError))
	}
	var dec Decision
	if err := json.Unmarshal(out.Plan, &dec); err != nil {
		return unknown(fmt.Sprintf("executing run %s: result file: %v", r.ID, err))
	}
	if dec.CaseID != "" && dec.CaseID != c.ID {
		return unknown(fmt.Sprintf("executing run %s reported for case %s, not this one", r.ID, dec.CaseID))
	}
	detail := strings.TrimSpace("run " + r.ID + ": " + dec.Detail)
	switch dec.Outcome {
	case "executed":
		if dec.Ref != "" {
			detail += " (" + dec.Ref + ")"
		}
		if err := st.MoveCase(ctx, c.ID, cases.Checking, cases.ByAct, "", j.ID(), detail); err != nil {
			return err
		}
		d.Log.Info("repo action executed", "case", c.ID, "run_id", r.ID, "action", p.Action, "ref", dec.Ref)
		return nil
	case "escalate":
		return st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByAct, "", j.ID(), detail)
	default:
		return unknown(fmt.Sprintf("executing run %s: outcome %q is not executed or escalate", r.ID, dec.Outcome))
	}
}

// vanished closes, as resolved itself, each handed-over case this monitor opened whose kind no routine
// acts on (none in the specs, enabled or not) and whose key its run no longer reports: nothing else
// would ever move it, and the monitor's own check now passes (operator, 2026-11-15). A case a routine
// acts on is left to that routine; a held case is left alone.
func (d *Dispatcher) vanished(ctx context.Context, j *spec.Routine, r *store.Run, present map[string]bool) {
	open, err := d.API.Store.HandedOverBy(ctx, j.ID())
	if err != nil {
		d.Log.Error("vanished", "routine", j.ID(), "err", err)
		return
	}
	for _, c := range open {
		if present[c.Key] || c.Holder != "" || !contains(j.Detects, c.Kind) || anyActs(d.API.Sched.Routines, c.Kind) {
			continue
		}
		if err := d.API.Store.MoveCase(ctx, c.ID, cases.Closed, cases.ByDetector, cases.ResolvedItself, j.ID(),
			"run "+r.ID+": the monitor no longer finds it"); err != nil {
			d.Log.Error("vanished", "case", c.ID, "err", err)
			continue
		}
		d.Log.Info("case resolved itself", "case", c.ID, "kind", c.Kind, "routine", j.ID(), "run_id", r.ID)
	}
}

func anyActs(rs []*spec.Routine, kind string) bool {
	for _, j := range rs {
		if j.OnCase() == kind {
			return true
		}
	}
	return false
}

func actingRoutine(rs []*spec.Routine, kind, stage string) *spec.Routine {
	for _, j := range rs {
		if j.Enabled && j.OnCase() == kind && j.OnStage() == stage {
			return j
		}
	}
	return nil
}

// caseLimit is the limit a kind's cases share, declared on its first-stage routine (nil: none).
func caseLimit(rs []*spec.Routine, kind string) *spec.CaseLimit {
	for _, j := range rs {
		if j.OnCase() == kind && j.OnStage() == "" && j.Trigger.Case.Limit != nil {
			return j.Trigger.Case.Limit
		}
	}
	return nil
}

// overLimit says why a case may not run again: its acting runs have spent its limit (RFC-0013 §2).
func overLimit(c store.Case, l *spec.CaseLimit) string {
	switch {
	case l == nil:
		return ""
	case c.SpentUSD >= l.USD:
		return fmt.Sprintf("the case's runs have spent $%.2f of its $%.2f", c.SpentUSD, l.USD)
	case c.SpentS >= float64(l.Minutes)*60:
		return fmt.Sprintf("the case's runs have taken %.0f of its %d minutes", c.SpentS/60, l.Minutes)
	}
	return ""
}

// LimitWindow is how far back the cases of one bug (one canonical key) count against its limit together: a
// person closing an escalated case and the next detection opening a new one does not start the limit over.
const LimitWindow = 7 * 24 * time.Hour

// bugSpend is the case with what every case of its canonical key opened within LimitWindow has spent, the
// case's own runs included.
func (d *Dispatcher) bugSpend(ctx context.Context, c store.Case, now time.Time) (store.Case, error) {
	usd, secs, err := d.API.Store.KeySpend(ctx, c.CanonicalKey, now.Add(-LimitWindow).UTC().Format(store.TimeFormat))
	if err != nil {
		return c, err
	}
	c.SpentUSD, c.SpentS = max(usd, c.SpentUSD), max(secs, c.SpentS)
	return c, nil
}

// backingOff: a case's last word was a defer (planned -> handed_over), a run with no decision or a check
// that said not yet, less than the backoff ago.
func (d *Dispatcher) backingOff(ctx context.Context, id string, now time.Time) (bool, error) {
	ev, err := d.API.Store.CaseEvents(ctx, id)
	if err != nil || len(ev) == 0 {
		return false, err
	}
	last := ev[len(ev)-1]
	if last.Kind != "run.no_decision" && last.Kind != notYetEvent && !(last.Kind == "case."+cases.HandedOver && last.From == cases.Planned) {
		return false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, last.At)
	if err != nil {
		return false, nil
	}
	b := d.Backoff
	if b <= 0 {
		b = DefaultBackoff
	}
	return now.Sub(at) < b, nil
}

func (d *Dispatcher) run(ctx context.Context, c store.Case, j *spec.Routine) error {
	st := d.API.Store
	full, err := st.Case(ctx, c.ID) // the list leaves out the keys
	if err != nil {
		return err
	}
	cv := caseJSON{ID: full.ID, Kind: full.Kind, Target: full.Target, Project: full.Project, Keys: full.Keys,
		Data: d.openingData(ctx, full.ID), Stage: full.Stage}
	if sd, err := st.StageData(ctx, full.ID); err == nil && sd != "" {
		cv.StageData = json.RawMessage(sd)
	}
	if l := caseLimit(d.API.Sched.Routines, full.Kind); l != nil {
		bug, err := d.bugSpend(ctx, full, d.API.Sched.Clock())
		if err != nil {
			return err
		}
		cv.Left = &caseLeft{USD: max(l.USD-bug.SpentUSD, 0), Seconds: max(float64(l.Minutes)*60-bug.SpentS, 0)}
	}
	cj, _ := json.Marshal(cv)
	r, out, err := d.API.Sched.RunWith(ctx, j.ID(), "case", "flotillad", map[string]string{"FLOTILLA_CASE": string(cj)})
	if err != nil {
		return d.noDecision(ctx, c.ID, j.ID(), "the run did not start: "+err.Error())
	}
	if r != nil {
		secs := 0.0
		if r.DurationS != nil {
			secs = *r.DurationS
		}
		if err := st.AddCaseSpend(ctx, c.ID, r.SpendUSD, secs); err != nil {
			d.Log.Error("recording the case's spend", "case", c.ID, "run_id", r.ID, "err", err)
		}
	}
	if r.Status != store.StatusOK || out == nil {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s ended %s", r.ID, r.Status))
	}
	if out.PlanError != "" {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s: %s", r.ID, out.PlanError))
	}
	if len(out.Plan) == 0 {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s wrote no plan file", r.ID))
	}
	var dec Decision
	if err := json.Unmarshal(out.Plan, &dec); err != nil {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s: plan file: %v", r.ID, err))
	}
	if dec.CaseID != "" && dec.CaseID != c.ID {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s planned for case %s, not this one", r.ID, dec.CaseID))
	}
	switch dec.Outcome {
	case "plan":
		// the re-check's numbers go on the case with the move: the plan cites them (RFC-0004 §5.1)
		if err := st.MoveCase(ctx, c.ID, cases.Planned, cases.ByAct, "", j.ID(), strings.TrimSpace("run "+r.ID+": "+dec.Detail)); err != nil {
			return err
		}
		pd, pid, err := d.API.DecidePlan(ctx, policy.Plan{CaseID: c.ID, RunID: r.ID, Action: dec.Action, Params: dec.Params})
		if err != nil {
			return err
		}
		d.Log.Info("case planned", "case", c.ID, "run_id", r.ID, "plan", pid, "outcome", pd.Outcome, "would", pd.Would)
		return nil
	case "resolved_itself":
		return st.MoveCase(ctx, c.ID, cases.Closed, cases.ByAct, cases.ResolvedItself, j.ID(), "run "+r.ID+": "+dec.Detail)
	case "not_reproducible":
		return st.MoveCase(ctx, c.ID, cases.Closed, cases.ByAct, cases.NotReproducible, j.ID(), "run "+r.ID+": "+dec.Detail)
	case "escalate":
		return st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByAct, "", j.ID(), "run "+r.ID+": "+dec.Detail)
	case "hand_over":
		return d.handOver(ctx, c, j, r.ID, dec)
	default:
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s: outcome %q is not plan, resolved_itself, not_reproducible, escalate or hand_over", r.ID, dec.Outcome))
	}
}

// handOver moves the case to the stage its run named (RFC-0013 §2). A stage no enabled routine acts on, or
// data that is not a JSON object within maxData, is no decision; a hand-over past MaxStageChanges
// escalates, so a case cannot loop between routines.
func (d *Dispatcher) handOver(ctx context.Context, c store.Case, j *spec.Routine, runID string, dec Decision) error {
	st := d.API.Store
	if !stageRe.MatchString(dec.Stage) || actingRoutine(d.API.Sched.Routines, c.Kind, dec.Stage) == nil {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s: hand_over to stage %q, which no enabled routine of %s acts on", runID, dec.Stage, c.Kind))
	}
	data, ok := detectionData(dec.Data)
	if !ok {
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s: hand_over data is not a JSON object of at most %d bytes", runID, maxData))
	}
	err := st.StageCase(ctx, c.ID, dec.Stage, j.ID(), strings.TrimSpace("run "+runID+": "+dec.Detail), data)
	if errors.Is(err, store.ErrStageLimit) {
		return st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByAct, "", j.ID(),
			fmt.Sprintf("run %s: a hand_over to %q past the %d a case may have: %s", runID, dec.Stage, store.MaxStageChanges, dec.Detail))
	}
	if err == nil {
		d.Log.Info("case handed on", "case", c.ID, "run_id", runID, "from", c.Stage, "to", dec.Stage)
	}
	return err
}

// stageRe is a stage name as specs write it (spec's trigger.case.stage); "" is the kind's first stage.
var stageRe = regexp.MustCompile(`^([a-z][a-z0-9_]{0,40})?$`)

func (d *Dispatcher) noDecision(ctx context.Context, id, routine, why string) error {
	d.Log.Warn("acting run made no decision", "case", id, "routine", routine, "why", why)
	return d.API.Store.AddCaseEvent(ctx, id, "run.no_decision", routine, why)
}

// Detection is one thing a monitor found (pipeline v1 step 6). The monitor says what and where; the
// case's keys are computed here, from the kind and the target, never taken from the run (RFC-0004 §3.5).
type Detection struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	// Title is the problem in one sentence, for people (G3): "The newest offsite backup on mini is 27
	// hours old". Optional; cut to one line of maxTitle runes.
	Title string `json:"title,omitempty"`
	// Data is structured: a measure ({name, value, threshold, cycle}) or a proposal (ADR-0016). A JSON
	// object of at most maxData bytes; anything else is dropped and logged.
	Data json.RawMessage `json:"data,omitempty"`
}

const maxTitle = 200

const maxData = 8192

// detectionData is a detection's data as stored: a JSON object within maxData, else "".
func detectionData(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	var obj map[string]any
	if len(raw) > maxData || json.Unmarshal(raw, &obj) != nil {
		return "", false
	}
	b, _ := json.Marshal(obj)
	return string(b), true
}

// oneLine keeps a title to one line of at most maxTitle runes.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxTitle {
		s = string(r[:maxTitle-1]) + "…"
	}
	return s
}

// maxDetections bounds one run's report: a monitor gone wrong cannot flood the store.
const maxDetections = 50

var targetRe = regexp.MustCompile(`^[a-z][a-z0-9_]*:[A-Za-z0-9._/-]{1,120}$`)

// Detected opens a case for each detection a monitor's run reported, or attaches it to the open case
// with the same key; a case it opens is handed over at once (detected -> handed_over by the detector: a
// code detector with a known kind). A kind the routine does not declare in `detects` is refused, as is a
// malformed report; the refusals are logged as errors, so they reach GlitchTip.
func (d *Dispatcher) Detected(ctx context.Context, j *spec.Routine, r *store.Run, out *sched.CaseOutcome) {
	var rep struct {
		Outcome    string      `json:"outcome"`
		Detections []Detection `json:"detections"`
		Summary    []string    `json:"summary"`
	}
	switch {
	case r.Status != store.StatusOK:
		return // its run is recorded as failed; a failed monitor's findings are not findings
	case out.PlanError != "":
		d.Log.Error("monitor report refused", "routine", j.ID(), "run_id", r.ID, "why", out.PlanError)
		return
	case len(out.Plan) == 0:
		d.Log.Error("monitor report refused", "routine", j.ID(), "run_id", r.ID, "why", "no plan file")
		return
	}
	if err := json.Unmarshal(out.Plan, &rep); err != nil || rep.Outcome != "detections" {
		d.Log.Error("monitor report refused", "routine", j.ID(), "run_id", r.ID, "why", `not {"outcome": "detections", ...}`)
		return
	}
	if len(rep.Detections) > maxDetections {
		d.Log.Error("monitor report refused", "routine", j.ID(), "run_id", r.ID, "why", fmt.Sprintf("%d detections, more than %d", len(rep.Detections), maxDetections))
		return
	}
	present := map[string]bool{} // the keys this run reports: an incident whose key is absent has cleared
	for _, x := range rep.Detections {
		if !contains(j.Detects, x.Kind) || !targetRe.MatchString(x.Target) {
			d.Log.Error("detection refused", "routine", j.ID(), "run_id", r.ID, "kind", x.Kind, "target", x.Target,
				"why", "a kind the routine does not declare in detects, or a malformed target")
			continue
		}
		k := store.Key{Kind: "target", Value: x.Kind + "/" + x.Target}
		present[k.String()] = true
		detail := strings.TrimSpace("run " + r.ID + ": " + x.Detail)
		data, ok := detectionData(x.Data)
		if !ok {
			d.Log.Error("detection data dropped", "routine", j.ID(), "run_id", r.ID, "kind", x.Kind,
				"why", fmt.Sprintf("not a JSON object of at most %d bytes", maxData))
		}
		id, how, err := d.API.Store.DetectIncident(ctx, store.NewCase{Title: oneLine(x.Title), Kind: x.Kind, Target: x.Target, Project: j.Fleet,
			Keys: []store.Key{k}, Canonical: k, Data: data}, j.ID(), detail)
		if err != nil {
			d.Log.Error("open case", "routine", j.ID(), "kind", x.Kind, "target", x.Target, "err", err)
			continue
		}
		if how != store.Opened {
			continue // attached to the open case, or absorbed into the incident's: one case per incident
		}
		if err := d.API.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", j.ID(), "a code detector with a known kind"); err != nil {
			d.Log.Error("hand over", "case", id, "err", err)
			continue
		}
		d.Log.Info("case opened", "case", id, "kind", x.Kind, "target", x.Target, "routine", j.ID(), "run_id", r.ID)
	}
	d.summary(ctx, j, r, rep.Summary)
	d.vanished(ctx, j, r, present)
	cleared, err := d.API.Store.ClearIncidents(ctx, j.ID(), present)
	if err != nil {
		d.Log.Error("clear incidents", "routine", j.ID(), "err", err)
	}
	for _, id := range cleared {
		d.Log.Info("incident cleared", "case", id, "routine", j.ID(), "run_id", r.ID)
	}
}

// maxSummaryLines bounds a report's summary, and each line is cut to one line of maxSummaryRunes.
const (
	maxSummaryLines = 20
	maxSummaryRunes = 200
)

func (d *Dispatcher) summary(ctx context.Context, j *spec.Routine, r *store.Run, lines []string) {
	if len(lines) == 0 {
		return
	}
	if j.BriefSection == "" {
		d.Log.Error("summary dropped", "routine", j.ID(), "run_id", r.ID, "why", "the routine's spec names no brief_section")
		return
	}
	s := store.Summary{Run: r.ID, Section: j.BriefSection}
	for _, ln := range lines {
		if len(s.Lines) == maxSummaryLines {
			break
		}
		ln = strings.Join(strings.Fields(ln), " ")
		if rs := []rune(ln); len(rs) > maxSummaryRunes {
			ln = string(rs[:maxSummaryRunes-1]) + "…"
		}
		if ln != "" {
			s.Lines = append(s.Lines, ln)
		}
	}
	b, _ := json.Marshal(s)
	if err := d.API.Store.Event(ctx, j.ID(), "flotillad", store.SummaryKind, j.ID(), string(b)); err != nil {
		d.Log.Error("summary", "routine", j.ID(), "err", err)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
