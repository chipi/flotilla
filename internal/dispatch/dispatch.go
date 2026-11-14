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
// verified closes the case; not_yet waits for the backoff and looks again; the check's window ending
// first escalates it as not verified (checks).
package dispatch

import (
	"context"
	"encoding/json"
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
	ID      string      `json:"id"`
	Kind    string      `json:"kind"`
	Target  string      `json:"target"`
	Project string      `json:"project"`
	Keys    []store.Key `json:"keys"`
}

// Decision is a run's plan file.
type Decision struct {
	Outcome string            `json:"outcome"`
	CaseID  string            `json:"case_id,omitempty"` // optional; when present it must be the run's case
	Action  string            `json:"action,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Detail  string            `json:"detail,omitempty"`
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
	for _, c := range open {
		if c.Holder != "" { // a fast path: LeaseCase below is the guard
			continue
		}
		j := actingRoutine(sc.Routines, c.Kind)
		if j == nil {
			continue // no routine acts on this kind here: the stuck-state timer escalates it
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
	cj, _ := json.Marshal(caseJSON{ID: full.ID, Kind: full.Kind, Target: full.Target, Project: full.Project, Keys: full.Keys})
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
	default:
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("check run %s: outcome %q is not verified or not_yet", r.ID, dec.Outcome))
	}
}

func actingRoutine(rs []*spec.Routine, kind string) *spec.Routine {
	for _, j := range rs {
		if j.Enabled && j.OnCase() == kind {
			return j
		}
	}
	return nil
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
	cj, _ := json.Marshal(caseJSON{ID: full.ID, Kind: full.Kind, Target: full.Target, Project: full.Project, Keys: full.Keys})
	r, out, err := d.API.Sched.RunWith(ctx, j.ID(), "case", "flotillad", map[string]string{"FLOTILLA_CASE": string(cj)})
	if err != nil {
		return d.noDecision(ctx, c.ID, j.ID(), "the run did not start: "+err.Error())
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
	case "escalate":
		return st.MoveCase(ctx, c.ID, cases.Escalated, cases.ByAct, "", j.ID(), "run "+r.ID+": "+dec.Detail)
	default:
		return d.noDecision(ctx, c.ID, j.ID(), fmt.Sprintf("run %s: outcome %q is not plan, resolved_itself or escalate", r.ID, dec.Outcome))
	}
}

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
		id, how, err := d.API.Store.DetectIncident(ctx, store.NewCase{Kind: x.Kind, Target: x.Target, Project: j.Fleet,
			Keys: []store.Key{k}, Canonical: k}, j.ID(), detail)
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
	cleared, err := d.API.Store.ClearIncidents(ctx, j.ID(), present)
	if err != nil {
		d.Log.Error("clear incidents", "routine", j.ID(), "err", err)
	}
	for _, id := range cleared {
		d.Log.Info("incident cleared", "case", id, "routine", j.ID(), "run_id", r.ID)
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
