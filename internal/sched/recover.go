package sched

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// OrphanCanceller is a runner that can stop a run this flotillad did not start: one the previous flotillad
// left in flight. A local worker knows its runs by id, so worker.Client's Cancel does; a remote worker's
// runner needs its own, because the link's table of runs died with the old flotillad. queued: the link was
// not connected, so the cancel waits for it to connect.
type OrphanCanceller interface {
	CancelOrphan(ctx context.Context, runID string) (queued bool, err error)
}

// RecoverInterrupted is flotillad's start, after MarkInterrupted: it asks each orphaned run's worker to
// cancel it, as an operator's cancel does (the worker would otherwise run it to its end or timeout, beside
// the case's next run), and frees the leases those routines hold on handed-over cases, so the case is
// dispatched at once. A cancel is best effort: it is logged and recorded as a run.cancel event by
// flotillad, delivered or not. Cases in executing keep their lease (the action never runs twice), as do
// cases in any state but handed_over.
func (s *Scheduler) RecoverInterrupted(ctx context.Context, runs []store.RunRef) error {
	s.init()
	holders := map[string]bool{}
	for _, r := range runs {
		holders[r.Routine] = true
		s.cancelOrphan(ctx, r)
	}
	names := make([]string, 0, len(holders))
	for h := range holders {
		names = append(names, h)
	}
	sort.Strings(names)
	n, err := s.Store.FreeHandedOverLeases(ctx, names)
	if err != nil {
		return err
	}
	if n > 0 {
		s.Log.Warn("case leases of interrupted runs freed", "count", n)
	}
	return nil
}

// chargeTime puts the time an orphaned run spent on its case: it ran until now, when it is cancelled, and no
// result will ever report it. Its spend is not known here: the run that resumes it reports it (ADR-0019).
func (s *Scheduler) chargeTime(ctx context.Context, r store.RunRef) {
	from, err := time.Parse(store.TimeFormat, r.StartedAt)
	if err != nil {
		return
	}
	id, err := s.Store.CaseOfRun(ctx, r.ID)
	if err != nil || id == "" {
		return
	}
	if err := s.Store.AddCaseSpend(ctx, id, 0, max(s.Now().Sub(from).Seconds(), 0)); err != nil {
		s.Log.Error("recording an interrupted run's time on its case", "case", id, "run_id", r.ID, "err", err)
	}
}

func (s *Scheduler) cancelOrphan(ctx context.Context, r store.RunRef) {
	s.chargeTime(ctx, r)
	why := "flotillad restarted while it ran"
	outcome := "sent"
	var err error
	base, shadow := r.Routine, store.IsShadowKey(r.Routine)
	if shadow {
		base = r.Routine[:strings.Index(r.Routine, "@v")]
	}
	j := s.Routine(base)
	workerName := ""
	if j != nil {
		workerName = j.Worker
		if shadow {
			workerName = j.ShadowWorkerName()
		}
	}
	switch {
	case j == nil:
		err = fmt.Errorf("no routine %s in the specs", r.Routine)
	case s.Workers[workerName] == nil:
		err = fmt.Errorf("no worker %s", workerName)
	default:
		switch c := s.Workers[workerName].(type) {
		case OrphanCanceller:
			var queued bool
			if queued, err = c.CancelOrphan(ctx, r.ID); queued {
				outcome = "queued until the worker's link connects"
			}
		case Canceller:
			err = c.Cancel(ctx, r.ID)
			if errors.Is(err, worker.ErrNoSuchRun) {
				err, outcome = nil, "the worker no longer has it"
			}
		default:
			err = fmt.Errorf("worker %s cannot cancel a run", workerName)
		}
	}
	detail := "run " + r.ID + ": " + why
	if err != nil {
		detail += " (not delivered: " + err.Error() + ")"
		s.Log.Error("cancel of an orphaned run", "routine", r.Routine, "run_id", r.ID, "err", err)
	} else {
		detail += " (" + outcome + ")"
		s.Log.Warn("orphaned run cancelled", "routine", r.Routine, "run_id", r.ID, "outcome", outcome)
	}
	if err := s.Store.Event(ctx, "flotillad", "flotillad", "run.cancel", r.Routine, detail); err != nil {
		s.Log.Error("recording the cancel of an orphaned run", "run_id", r.ID, "err", err)
	}
}
