package sched

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// cancelRunner blocks a run until a cancel arrives (its answer: "cancelled", with spend). The first
// notYet cancels are answered "no such run": the request has not reached the worker yet.
type cancelRunner struct {
	mu      sync.Mutex
	started chan struct{}
	stopped chan struct{}
	asked   int
	notYet  int
}

func (c *cancelRunner) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	close(c.started)
	<-c.stopped
	return &worker.Result{Status: "cancelled", DurationS: 4, SpendUSD: 0.2, OutputTail: "terminated"}, nil
}

func (c *cancelRunner) Cancel(ctx context.Context, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked++
	if c.asked <= c.notYet {
		return fmt.Errorf("worker: %w", worker.ErrNoSuchRun)
	}
	select {
	case <-c.stopped:
	default:
		close(c.stopped)
	}
	return nil
}

func startCancellable(t *testing.T, notYet int) (*Scheduler, *cancelRunner, *fakeMetrics, chan *store.Run) {
	t.Helper()
	s, _, fm := setup(t, 2)
	cr := &cancelRunner{started: make(chan struct{}), stopped: make(chan struct{}), notYet: notYet}
	s.Workers["triage"] = cr
	done := make(chan *store.Run, 1)
	go func() {
		r, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	<-cr.started
	return s, cr, fm, done
}

// A cancelled run is recorded with its own status, who and why, and its spend; it is neither ok nor a failure.
func TestCancelRecordsACancelledRun(t *testing.T) {
	s, cr, fm, done := startCancellable(t, 0)
	runID, err := s.Cancel(context.Background(), "triage/cycle", "claude-homelab", "fleet triage STOPped")
	if err != nil || runID == "" {
		t.Fatalf("cancel: %q %v", runID, err)
	}
	r := <-done
	if r.ID != runID || r.Status != store.StatusCancelled || r.Detail != "cancelled: claude-homelab, fleet triage STOPped" || r.SpendUSD != 0.2 {
		t.Fatalf("the run: %+v", r)
	}
	runs, _ := s.Store.Runs(context.Background(), "triage/cycle", 5)
	if len(runs) != 1 || runs[0].Status != "cancelled" || runs[0].Detail != r.Detail {
		t.Fatalf("recorded: %+v", runs)
	}
	if last, _ := s.Store.LastSuccess(context.Background(), "triage/cycle"); !last.IsZero() || fm.ok != 0 {
		t.Fatalf("a cancelled run counted as a success: %v, %d", last, fm.ok)
	}
	if len(fm.runs) != 1 || fm.runs[0] != "cancelled" {
		t.Fatalf("metrics: %v", fm.runs)
	}
	if cr.asked != 1 {
		t.Fatalf("the worker was asked %d times", cr.asked)
	}
	days, _ := s.Store.RunDays(context.Background(), "2000-01-01")
	if len(days) != 1 || days[0].Runs != 1 || days[0].Failed != 0 {
		t.Fatalf("a cancelled run is not a failed one: %+v", days)
	}
	if _, err := s.Cancel(context.Background(), "triage/cycle", "x", "y"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a second cancel, the run over: %v", err)
	}
}

// The run is registered here before the worker has its request: the cancel is asked again until it lands.
func TestCancelWaitsForTheWorkerToHaveTheRun(t *testing.T) {
	s, cr, _, done := startCancellable(t, 2)
	if _, err := s.Cancel(context.Background(), "triage/cycle", "operator", "r"); err != nil {
		t.Fatalf("a worker that does not know the run yet is not an error: %v", err)
	}
	select {
	case r := <-done:
		if r.Status != store.StatusCancelled {
			t.Fatalf("ended %q", r.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retried cancel never landed")
	}
	if cr.asked != 3 {
		t.Fatalf("asked %d times, want 3 (two refusals, then the cancel)", cr.asked)
	}
}

func TestCancelNothingInFlight(t *testing.T) {
	s, _, _ := setup(t, 2)
	if _, err := s.Cancel(context.Background(), "triage/cycle", "operator", "r"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("nothing running: %v", err)
	}
	if _, err := s.Cancel(context.Background(), "nope/x", "operator", "r"); err == nil || errors.Is(err, ErrNotRunning) {
		t.Fatalf("an unknown routine: %v", err)
	}
}

// A worker that reports "cancelled" without flotillad having asked (someone cancelled at the worker) is
// still a cancelled run, and says so.
func TestCancelledAtTheWorker(t *testing.T) {
	s, fr, _ := setup(t, 2)
	fr.res = &worker.Result{Status: "cancelled", DurationS: 1, OutputTail: "the worker's own cancel"}
	r, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if err != nil || r.Status != store.StatusCancelled || r.Detail != "cancelled at the worker: the worker's own cancel" {
		t.Fatalf("%+v %v", r, err)
	}
}
