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
	return nil
}

func actingRoutine(rs []*spec.Routine, kind string) *spec.Routine {
	for _, j := range rs {
		if j.Enabled && j.OnCase() == kind {
			return j
		}
	}
	return nil
}

// backingOff: a case's last word was a defer (planned -> handed_over) or a run with no decision, less
// than the backoff ago.
func (d *Dispatcher) backingOff(ctx context.Context, id string, now time.Time) (bool, error) {
	ev, err := d.API.Store.CaseEvents(ctx, id)
	if err != nil || len(ev) == 0 {
		return false, err
	}
	last := ev[len(ev)-1]
	if last.Kind != "run.no_decision" && !(last.Kind == "case."+cases.HandedOver && last.From == cases.Planned) {
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
	for _, x := range rep.Detections {
		if !contains(j.Detects, x.Kind) || !targetRe.MatchString(x.Target) {
			d.Log.Error("detection refused", "routine", j.ID(), "run_id", r.ID, "kind", x.Kind, "target", x.Target,
				"why", "a kind the routine does not declare in detects, or a malformed target")
			continue
		}
		k := store.Key{Kind: "target", Value: x.Kind + "/" + x.Target}
		detail := strings.TrimSpace("run " + r.ID + ": " + x.Detail)
		id, opened, err := d.API.Store.OpenCase(ctx, store.NewCase{Kind: x.Kind, Target: x.Target, Project: j.Fleet,
			Keys: []store.Key{k}, Canonical: k}, j.ID(), detail)
		if err != nil {
			d.Log.Error("open case", "routine", j.ID(), "kind", x.Kind, "target", x.Target, "err", err)
			continue
		}
		if !opened {
			continue // attached to the open case for this key: a storm is one case
		}
		if err := d.API.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", j.ID(), "a code detector with a known kind"); err != nil {
			d.Log.Error("hand over", "case", id, "err", err)
			continue
		}
		d.Log.Info("case opened", "case", id, "kind", x.Kind, "target", x.Target, "routine", j.ID(), "run_id", r.ID)
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
