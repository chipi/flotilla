package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// Cancel is the fake worker's: it records the run ids it was asked to stop.
func (f *fakeWorker) Cancel(ctx context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, runID)
	if runID == "run-ended" {
		return worker.ErrNoSuchRun
	}
	return nil
}

func (f *fixture) leaseOf(t *testing.T, id string) string {
	t.Helper()
	return f.cs(t, id).Holder
}

func (f *fixture) startRunning(t *testing.T, id, routine string) {
	t.Helper()
	if err := f.st.StartRun(context.Background(), store.Run{ID: id, Routine: routine, Status: store.StatusRunning, StartedAt: f.now.UTC().Format(store.TimeFormat)}); err != nil {
		t.Fatal(err)
	}
}

// flotillad's start: the runs left running are read, marked interrupted and cancelled at their workers; the
// leases they held on handed-over cases are freed so the case is dispatched at once (and resumed); a case in
// executing keeps its lease (the action never runs twice) and a case in checking is left to ExpireLeases.
func TestRecoverInterruptedFreesOnlyHandedOverLeases(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	withResume(f, "interrupted")
	far := f.now.Add(3 * time.Hour)

	a := f.handOver(t, "#a")
	if ok, _ := f.st.LeaseCase(ctx, a, "ops/rerun", far); !ok {
		t.Fatal("lease a")
	}
	if err := f.st.AddCaseEvent(ctx, a, sched.EventRunStarted, "ops/rerun", "run run-a"); err != nil {
		t.Fatal(err)
	}
	f.startRunning(t, "run-a", "ops/rerun")

	b := f.handOver(t, "#b")
	_, _ = f.st.LeaseCase(ctx, b, "ops/rerun", far)
	_ = f.st.MoveCase(ctx, b, cases.Planned, cases.ByAct, "", "ops/rerun", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: b, RunID: "r", Action: "backup.rerun",
		Params: map[string]string{"host": "host-01"}, Outcome: "auto", Autonomy: "auto"}, cases.Executing, "", "host:host-01#b", true); err != nil {
		t.Fatal(err)
	}

	c := f.checking(t, "#c")
	if ok, _ := f.st.LeaseCheck(ctx, c, "ops/verify-backup", far); !ok {
		t.Fatal("check lease")
	}
	f.startRunning(t, "run-c", "ops/verify-backup")

	d := f.handOver(t, "#d") // held by a routine with no run in flight: untouched
	if ok, _ := f.st.LeaseCase(ctx, d, "ops/other", far); !ok {
		t.Fatal("lease d")
	}
	f.startRunning(t, "run-ended", "ops/backup-monitor")

	orphans, err := f.st.RunningRuns(ctx)
	if err != nil || len(orphans) != 3 {
		t.Fatalf("running runs: %+v, %v", orphans, err)
	}
	if n, err := f.st.MarkInterrupted(ctx); err != nil || n != 3 {
		t.Fatalf("interrupted %d, %v", n, err)
	}
	if err := f.d.API.Sched.RecoverInterrupted(ctx, orphans); err != nil {
		t.Fatal(err)
	}

	if len(f.w.cancelled) != 3 {
		t.Fatalf("cancelled at the worker: %v", f.w.cancelled)
	}
	evs, _ := f.st.Events(ctx, 50)
	n := 0
	for _, e := range evs {
		if e.Kind == "run.cancel" && e.Actor == "flotillad" && strings.Contains(e.Detail, "flotillad restarted while it ran") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d run.cancel events by flotillad: %+v", n, evs)
	}
	if f.leaseOf(t, a) != "" {
		t.Fatal("the handed-over case kept its lease")
	}
	if f.leaseOf(t, b) != "ops/rerun" {
		t.Fatal("an executing case lost its holder: the action could run again")
	}
	if f.leaseOf(t, c) != "ops/verify-backup" {
		t.Fatal("a case in checking was freed by the restart")
	}
	if f.leaseOf(t, d) != "ops/other" {
		t.Fatal("a lease of a routine with no interrupted run was freed")
	}

	f.pass(t) // no wait for the lease: the case is dispatched and its run resumes run-a
	if len(f.w.got) != 1 {
		t.Fatalf("%d runs started", len(f.w.got))
	}
	if rs := f.resumeOf(t, 0); rs == nil || rs.RunID != "run-a" || rs.Status != store.StatusInterrupted {
		t.Fatalf("resume %+v", rs)
	}
}

// A routine without resume gets the cancel and its lease freed too: only the resume word is missing.
func TestRecoverInterruptedWithoutResume(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a := f.handOver(t, "#a")
	_, _ = f.st.LeaseCase(ctx, a, "ops/rerun", f.now.Add(time.Hour))
	f.startRunning(t, "run-a", "ops/rerun")
	orphans, _ := f.st.RunningRuns(ctx)
	_, _ = f.st.MarkInterrupted(ctx)
	if err := f.d.API.Sched.RecoverInterrupted(ctx, orphans); err != nil {
		t.Fatal(err)
	}
	if len(f.w.cancelled) != 1 || f.leaseOf(t, a) != "" {
		t.Fatalf("cancelled %v, holder %q", f.w.cancelled, f.leaseOf(t, a))
	}
	f.pass(t)
	if len(f.w.got) != 1 || f.resumeOf(t, 0) != nil {
		t.Fatalf("%d runs, resume %+v", len(f.w.got), f.w.got[0].Env)
	}
}

// Nothing running: nothing is cancelled and nothing is freed.
func TestRecoverInterruptedNothingRunning(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a := f.handOver(t, "#a")
	_, _ = f.st.LeaseCase(ctx, a, "ops/rerun", f.now.Add(time.Hour))
	if err := f.d.API.Sched.RecoverInterrupted(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.w.cancelled) != 0 || f.leaseOf(t, a) != "ops/rerun" {
		t.Fatalf("cancelled %v, holder %q", f.w.cancelled, f.leaseOf(t, a))
	}
}

// The time an orphaned run spent is charged on its case when flotillad starts: no result will ever report it.
func TestRecoverInterruptedChargesTheRunsTimeToItsCase(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	withResume(f, "interrupted")
	a := f.handOver(t, "#a")
	if err := f.st.AddCaseEvent(ctx, a, sched.EventRunStarted, "ops/rerun", "run run-a"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.StartRun(ctx, store.Run{ID: "run-a", Routine: "ops/rerun", Status: store.StatusRunning,
		StartedAt: f.now.Add(-20 * time.Minute).UTC().Format(store.TimeFormat)}); err != nil {
		t.Fatal(err)
	}
	orphans, _ := f.st.RunningRuns(ctx)
	_, _ = f.st.MarkInterrupted(ctx)
	if err := f.d.API.Sched.RecoverInterrupted(ctx, orphans); err != nil {
		t.Fatal(err)
	}
	if got := f.cs(t, a).SpentS; got < 1199 || got > 1201 {
		t.Fatalf("charged %v s, want the 20 minutes the run ran", got)
	}
}
