package api

import (
	"context"
)

// A dry run (RFC-0008 Q5) says what a run of the routine starting now would be, and whether it would
// start, without starting it: the scheduler's own skip check, the autonomy it would get, its worker,
// command and models, and its budget left. Nothing is recorded but the request's event.

type DryRun struct {
	Routine    string   `json:"routine"`
	WouldStart bool     `json:"would_start"`
	Why        string   `json:"why,omitempty"` // why it would not start
	Autonomy   string   `json:"autonomy"`      // what the run would get (shadow while frozen)
	Worker     string   `json:"worker"`
	Command    string   `json:"command"`
	Timeout    string   `json:"timeout"`
	Models     []string `json:"models,omitempty"`
	BudgetDay  float64  `json:"budget_day_usd"`
	SpentToday float64  `json:"spent_today_usd"`
}

func (s *Server) DryRunNow(ctx context.Context, c Caller, via, routineID string) (DryRun, error) {
	var d DryRun
	if err := s.Check(ctx, c, ActRun); err != nil {
		return d, err
	}
	j, err := s.routineByID(routineID)
	if err != nil {
		return d, err
	}
	d = DryRun{Routine: j.ID(), Worker: j.Worker, Command: j.Run.Command, Timeout: j.Limit.String(), Models: j.Models,
		BudgetDay: j.Budget.DayUSD}
	if d.Autonomy, err = s.Sched.Autonomy(ctx, j); err != nil {
		return d, err
	}
	day := s.Sched.Now().In(s.Sched.Location).Format("2006-01-02")
	if d.SpentToday, err = s.Store.Spend(ctx, day, j.ID()); err != nil {
		return d, err
	}
	why := ""
	switch {
	case !j.Enabled:
		why = "the routine is disabled"
	case s.Sched.Running()[j.ID()] != "":
		why = "already running (" + s.Sched.Running()[j.ID()] + ")"
	default:
		if why, err = s.Sched.SkipReason(ctx, j); err != nil {
			return d, err
		}
	}
	if why == "" {
		if _, ok := s.Sched.Workers[j.Worker]; !ok {
			why = "worker " + j.Worker + " is not in this installation's config"
		}
	}
	d.WouldStart, d.Why = why == "", why
	_ = s.Store.Event(ctx, c.Name, via, "run.dry_run", j.ID(), why)
	return d, nil
}
