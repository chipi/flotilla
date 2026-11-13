package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// actionSubject is where an action's earned autonomy is kept in the controls (ADR-0008): a new action
// has none, so it is shadow.
func actionSubject(name string) string { return "action:" + name }

// hostCeiling is the highest autonomy a host accepts. ADR-0006 §5 puts it in each host's reviewed config,
// installed with the host agent, which is not built: until then every host is a new host, and a new host
// starts at propose.
const hostCeiling = spec.AutonomyPropose

// incidentWindow is the host incident rule's window (RFC-0004 §5.3).
const incidentWindow = 15 * time.Minute

// DecidePlan gathers the state for a plan on a planned case, asks policy, and applies its decision in one
// transaction (ADR-0005 decision 4): refuse escalates, defer hands the case back, shadow closes it with
// the plan recorded, propose awaits approval and auto executes, those two holding the target's lock.
func (s *Server) DecidePlan(ctx context.Context, p policy.Plan) (policy.Decision, int64, error) {
	c, err := s.Store.Case(ctx, p.CaseID)
	if err != nil {
		return policy.Decision{}, 0, err
	}
	if c.Status != cases.Planned {
		return policy.Decision{}, 0, fmt.Errorf("%w (it is %s)", store.ErrNotPlanned, c.Status)
	}
	st, err := s.planState(ctx, c, p)
	if err != nil {
		return policy.Decision{}, 0, err
	}
	d := policy.Decide(p, st, s.Limits)
	id, err := s.applyDecision(ctx, c, p, d)
	if errors.Is(err, store.ErrLocked) {
		// Another case took the lock between the read and the write: decided again with the holder.
		if st.LockHeldBy, err = s.Store.LockHolder(ctx, c.Target); err != nil {
			return d, 0, err
		}
		d = policy.Decide(p, st, s.Limits)
		id, err = s.applyDecision(ctx, c, p, d)
	}
	return d, id, err
}

func (s *Server) applyDecision(ctx context.Context, c store.Case, p policy.Plan, d policy.Decision) (int64, error) {
	to, resolution, lock := "", "", false
	switch d.Outcome {
	case policy.Refuse:
		to = cases.Escalated
	case policy.Defer:
		to = cases.HandedOver
	case policy.Shadow:
		to, resolution = cases.Closed, cases.ShadowPlan
	case policy.Propose:
		to, lock = cases.AwaitingApproval, true
	case policy.Auto:
		to, lock = cases.Executing, true
	default:
		return 0, fmt.Errorf("policy returned %q", d.Outcome)
	}
	return s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: c.ID, RunID: p.RunID, Action: p.Action, Params: p.Params,
		PlanHash: d.PlanHash, Outcome: d.Outcome, Autonomy: d.Autonomy, Would: d.Would, Reasons: d.Reasons}, to, resolution, c.Target, lock)
}

func (s *Server) planState(ctx context.Context, c store.Case, p policy.Plan) (policy.State, error) {
	st := policy.State{CaseKind: c.Kind, CaseTarget: c.Target, HostCeiling: hostCeiling, ActionAutonomy: spec.AutonomyShadow}
	if s.Registry != nil {
		st.Action, st.ActionFound = s.Registry.Action(p.Action)
	}
	// The plan cites the run that moved the case to planned: that run's routine is the acting routine.
	ev, err := s.Store.CaseEvents(ctx, c.ID)
	if err != nil {
		return st, err
	}
	planner := ""
	for _, e := range ev {
		if e.Kind == "case."+cases.Planned {
			planner = e.Actor
		}
	}
	run, err := s.Store.Run(ctx, p.RunID) // nil when there is no such run
	if err != nil {
		return st, err
	}
	var j *spec.Routine
	if run != nil && run.Routine == planner {
		j = s.Sched.Routine(run.Routine)
	}
	st.RunOnRecord = j != nil
	if j != nil {
		if st.RoutineAutonomy, err = s.Sched.Autonomy(ctx, j); err != nil {
			return st, err
		}
		stopped, _ := s.Store.Control(ctx, j.Fleet, "stop")
		st.FleetStopped = stopped == "1"
	}
	if v, _ := s.Store.Control(ctx, actionSubject(p.Action), "autonomy"); v != "" {
		st.ActionAutonomy = v
	}
	if st.Frozen, err = s.Sched.Frozen(ctx); err != nil {
		return st, err
	}
	if st.LockHeldBy, err = s.Store.LockHolder(ctx, c.Target); err != nil {
		return st, err
	}
	if st.LockHeldBy == c.ID {
		st.LockHeldBy = ""
	}
	host := policy.HostOf(c.Target)
	now := s.Sched.Clock()
	y, m, d := now.In(s.Sched.Location).Date()
	n, err := s.Store.Counts(ctx, p.Action, c.Target, host, time.Date(y, m, d, 0, 0, 0, 0, s.Sched.Location), now, incidentWindow)
	if err != nil {
		return st, err
	}
	st.TargetToday, st.HostHour, st.PlatformHour, st.FailedHour, st.HostNewCases = n.TargetToday, n.HostHour, n.PlatformHour, n.FailedHour, n.HostNewCases
	holds, err := s.Store.Holds(ctx, now, false)
	if err != nil {
		return st, err
	}
	for _, h := range holds {
		if h.Scope == c.Target || (host != "" && h.Scope == "host:"+host) {
			st.Holds = append(st.Holds, h.String())
		}
	}
	return st, nil
}

// AddHold holds a host or a target for a while (`flotilla hold`): tightening, so an agent may too.
func (s *Server) AddHold(ctx context.Context, c Caller, via, scope string, d time.Duration, reason string) (int64, error) {
	if err := s.Check(ctx, c, ActHold); err != nil {
		return 0, err
	}
	scope, reason = strings.TrimSpace(scope), strings.TrimSpace(reason)
	if !strings.Contains(scope, ":") || strings.ContainsAny(scope, " \t\n") {
		return 0, problem(400, "scope is host:<name> or a case target such as host:mini")
	}
	if d <= 0 || d > 30*24*time.Hour {
		return 0, problem(400, "a hold lasts more than 0 and at most 30 days")
	}
	if reason == "" {
		return 0, problem(400, "a hold needs a reason: what it protects")
	}
	return s.Store.AddHold(ctx, scope, s.Sched.Clock().Add(d), c.Name, via, reason)
}

// LiftHold ends a hold early: loosening, so an operator's, with a reason (ADR-0014).
func (s *Server) LiftHold(ctx context.Context, c Caller, via string, id int64, reason string) error {
	if err := s.Check(ctx, c, ActUnhold); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return problem(400, "lifting a hold needs a reason")
	}
	if err := s.Store.LiftHold(ctx, id, c.Name, via, strings.TrimSpace(reason)); errors.Is(err, store.ErrNoSuchHold) {
		return problem(404, "%s", err)
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Server) holds(w http.ResponseWriter, r *http.Request, _ Caller) {
	hs, err := s.Store.Holds(r.Context(), s.Sched.Clock(), r.URL.Query().Get("all") == "1")
	if err != nil {
		writeErr(w, err)
		return
	}
	if hs == nil {
		hs = []store.Hold{}
	}
	writeJSON(w, 200, map[string]any{"holds": hs})
}

func (s *Server) holdAdd(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Scope  string `json:"scope"`
		For    string `json:"for"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": `body: {"scope": "host:mini", "for": "2h", "reason": "..."}`})
		return
	}
	d, err := time.ParseDuration(body.For)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "for: a duration such as 2h"})
		return
	}
	id, err := s.AddHold(r.Context(), c, "api", body.Scope, d, body.Reason)
	writeAction(w, err, 201, map[string]string{"id": strconv.FormatInt(id, 10), "scope": body.Scope, "by": c.Name})
}

func (s *Server) holdLift(w http.ResponseWriter, r *http.Request, c Caller) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "a hold's id is a number"})
		return
	}
	err = s.LiftHold(r.Context(), c, "api", id, reason(r, w))
	writeAction(w, err, 200, map[string]string{"id": r.PathValue("id"), "lifted_by": c.Name})
}
