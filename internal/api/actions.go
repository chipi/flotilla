package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
)

// ADR-0014 decision 2: one path for every action. The HTTP handlers and the UI's handlers both call
// these; each checks the caller against the table and every rule (a reason for loosening, the spec's
// cap) itself, so no client can skip one.

// Problem is an action refused for a reason the caller can fix or must be told; Code is its HTTP status.
type Problem struct {
	Code int
	Msg  string
}

func (p *Problem) Error() string { return p.Msg }

func problem(code int, format string, a ...any) error {
	return &Problem{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// AsProblem returns the Problem in err, or nil.
func AsProblem(err error) *Problem {
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	return nil
}

// Check refuses a caller whose role may not do a.
func (s *Server) Check(ctx context.Context, c Caller, a Action) error {
	role, err := s.EffectiveRole(ctx, c)
	if err != nil {
		return err
	}
	if !Authorize(role, a) {
		s.Log.Warn("refused", "caller", c.Name, "role", role, "action", a)
		msg := "your role may not do this"
		switch {
		case role == RoleWorker:
			msg = "the ledger is a worker's; a worker reaches nothing else"
		case a == ActLedger:
			msg = "the ledger is a worker's"
		case role == RoleAgent:
			msg = "only an operator may do this; an agent never can"
		case role == "retired-operator-token":
			msg = "an admin exists: the callers-file operator token is for bootstrap and break-glass only; use your user token"
		}
		return problem(403, "%s", msg)
	}
	return nil
}

// RoutineStatus is one routine's state, for the API's status and the UI.
type RoutineStatus struct {
	ID           string  `json:"id"`
	Fleet        string  `json:"fleet"`
	Description  string  `json:"description"`
	Enabled      bool    `json:"enabled"`
	Worker       string  `json:"worker"`
	Every        string  `json:"every"`
	Autonomy     string  `json:"autonomy"`     // what a run starting now gets: shadow while frozen
	AutonomySet  string  `json:"autonomy_set"` // the routine's own, which the freeze leaves alone
	SpecAutonomy string  `json:"spec_autonomy"`
	MaxAutonomy  string  `json:"max_autonomy,omitempty"`
	SpecHash     string  `json:"spec_hash"`
	Paused       bool    `json:"paused"`
	Stopped      bool    `json:"fleet_stopped"`
	Running      string  `json:"running,omitempty"`
	LastSuccess  string  `json:"last_success,omitempty"`
	Stale        bool    `json:"stale"`
	SpendToday   float64 `json:"spend_today_usd"`
	BudgetDay    float64 `json:"budget_day_usd"`
}

// Status is every routine's state and each worker's health.
func (s *Server) Status(ctx context.Context) ([]RoutineStatus, map[string]any, error) {
	running := s.Sched.Running()
	now := s.Sched.Now()
	day := now.In(s.Sched.Location).Format("2006-01-02")
	var routines []RoutineStatus
	for _, j := range s.Sched.Routines {
		st, err := s.Sched.Autonomy(ctx, j)
		if err != nil {
			return nil, nil, err
		}
		set, err := s.Sched.Configured(ctx, j)
		if err != nil {
			return nil, nil, err
		}
		paused, _ := s.Store.Control(ctx, j.ID(), sched.KeyPaused)
		stopped, _ := s.Store.Control(ctx, j.Fleet, sched.KeyStop)
		last, _ := s.Store.LastSuccess(ctx, j.ID())
		spent, _ := s.Store.Spend(ctx, day, j.ID())
		js := RoutineStatus{ID: j.ID(), Fleet: j.Fleet, Description: j.Description, Enabled: j.Enabled, Worker: j.Worker,
			Every: j.Every.String(), Autonomy: st, AutonomySet: set, SpecAutonomy: j.Autonomy, MaxAutonomy: j.MaxAutonomy, SpecHash: j.Hash,
			Paused: paused == "1", Stopped: stopped == "1", Running: running[j.ID()], SpendToday: spent, BudgetDay: j.Budget.DayUSD,
			Stale: j.Enabled && (last.IsZero() || now.Sub(last) > j.Fresh)}
		if !last.IsZero() {
			js.LastSuccess = last.Format(time.RFC3339)
		}
		routines = append(routines, js)
	}
	workers := map[string]any{}
	for name, c := range s.Workers {
		if h, err := c.Health(ctx); err != nil {
			workers[name] = map[string]any{"ok": false, "error": err.Error()}
		} else {
			workers[name] = h
		}
	}
	return routines, workers, nil
}

func (s *Server) routineByID(id string) (*spec.Routine, error) {
	if j := s.Sched.Routine(id); j != nil {
		return j, nil
	}
	return nil, problem(404, "no such routine")
}

// RunNow starts a run in the background and returns the routine's id.
func (s *Server) RunNow(ctx context.Context, c Caller, via, routineID string) error {
	if err := s.Check(ctx, c, ActRun); err != nil {
		return err
	}
	j, err := s.routineByID(routineID)
	if err != nil {
		return err
	}
	if busy := s.Sched.Running()[j.ID()]; busy != "" {
		return problem(409, "already running (%s)", busy)
	}
	_ = s.Store.Event(ctx, c.Name, via, "run.requested", j.ID(), "")
	go func() {
		bg := context.WithoutCancel(ctx)
		if _, err := s.Sched.RunNow(bg, j.ID(), "manual", c.Name); err != nil {
			s.Log.Error("manual run failed", "routine", j.ID(), "caller", c.Name, "err", err)
		}
	}()
	return nil
}

// SetPaused pauses a routine or, with a reason, resumes it.
func (s *Server) SetPaused(ctx context.Context, c Caller, via, routineID string, paused bool, reason string) error {
	a, v := ActPause, "1"
	if !paused {
		a, v = ActResume, "0"
	}
	if err := s.Check(ctx, c, a); err != nil {
		return err
	}
	j, err := s.routineByID(routineID)
	if err != nil {
		return err
	}
	return s.setControl(ctx, c, via, a, j.ID(), sched.KeyPaused, v, reason)
}

// SetStopped STOPs a fleet or, with a reason, starts it again.
func (s *Server) SetStopped(ctx context.Context, c Caller, via, fleet string, stopped bool, reason string) error {
	a, v := ActStop, "1"
	if !stopped {
		a, v = ActStart, "0"
	}
	if err := s.Check(ctx, c, a); err != nil {
		return err
	}
	if !s.hasFleet(fleet) {
		return problem(404, "no such fleet")
	}
	return s.setControl(ctx, c, via, a, fleet, sched.KeyStop, v, reason)
}

func (s *Server) setControl(ctx context.Context, c Caller, via string, a Action, subject, key, value, reason string) error {
	reason = strings.TrimSpace(reason)
	if loosening(a) && reason == "" {
		return problem(400, "loosening needs a reason: body {\"reason\": \"...\"}")
	}
	if err := s.Store.SetControlBecause(ctx, subject, key, value, c.Name, via, reason); err != nil {
		return err
	}
	s.Log.Info("control set", "subject", subject, "key", key, "value", value, "caller", c.Name, "via", via, "reason", reason)
	return nil
}

// SetAutonomy sets a routine's autonomy within its spec's cap; raising it needs a reason.
func (s *Server) SetAutonomy(ctx context.Context, c Caller, via, routineID, level, reason string) error {
	if err := s.Check(ctx, c, ActAutonomy); err != nil {
		return err
	}
	j, err := s.routineByID(routineID)
	if err != nil {
		return err
	}
	switch level {
	case spec.AutonomyShadow, spec.AutonomyPropose, spec.AutonomyAuto:
	default:
		return problem(400, "autonomy must be shadow, propose or auto")
	}
	if !j.Allows(level) {
		return problem(400, "%s is capped at %s by its spec (max_autonomy)", j.ID(), j.MaxAutonomy)
	}
	cur, err := s.Sched.Configured(ctx, j) // the freeze lowers what runs, not what is set
	if err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if spec.Rank(level) > spec.Rank(cur) && reason == "" { // raising loosens (ADR-0014)
		return problem(400, "raising %s from %s to %s needs a reason", j.ID(), cur, level)
	}
	if err := s.Store.SetControlBecause(ctx, j.ID(), sched.KeyAutonomy, level, c.Name, via, reason); err != nil {
		return err
	}
	s.Log.Info("autonomy set", "routine", j.ID(), "autonomy", level, "caller", c.Name, "via", via, "reason", reason)
	return nil
}

// Freeze is the installation's freeze: whether it holds, and who set it last and when.
type Freeze struct {
	Frozen bool   `json:"frozen"`
	By     string `json:"by,omitempty"`
	At     string `json:"at,omitempty"`
}

// FreezeState reads the freeze.
func (s *Server) FreezeState(ctx context.Context) (Freeze, error) {
	cs, err := s.Store.Controls(ctx)
	if err != nil {
		return Freeze{}, err
	}
	for _, c := range cs {
		if c.Subject == sched.FreezeSubject && c.Key == sched.KeyFrozen {
			return Freeze{Frozen: c.Value == "1", By: c.Actor, At: c.At}, nil
		}
	}
	return Freeze{}, nil
}

// SetFrozen pulls the freeze (every routine runs at shadow, ADR-0014 decision 8) or, with a reason,
// lifts it. Routines' own autonomy is never touched, so lifting it restores each one.
func (s *Server) SetFrozen(ctx context.Context, c Caller, via string, frozen bool, reason string) error {
	a, v := ActFreeze, "1"
	if !frozen {
		a, v = ActUnfreeze, "0"
	}
	if err := s.Check(ctx, c, a); err != nil {
		return err
	}
	return s.setControl(ctx, c, via, a, sched.FreezeSubject, sched.KeyFrozen, v, reason)
}
