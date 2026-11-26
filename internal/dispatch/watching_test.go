package dispatch

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/casetimer"
	"github.com/chipi/flotilla/internal/spec"
)

// ADR-0023: the acting routine of the fixture's kind records its plan and watches.
func (f *fixture) watch(t *testing.T) {
	t.Helper()
	j := f.d.API.Sched.Routine("ops/rerun")
	if j == nil {
		t.Fatal("no acting routine")
	}
	j.Trigger.Case.OnRecord = spec.OnRecordWatch
}

const (
	stale = `{"outcome": "detections", "detections": [{"kind": "failed_backup", "target": "host:host-01", "detail": "age 20.4h", "data": {"name": "backup_age_h", "value": 20.4}}]}`
	rerun = `{"outcome": "plan", "action": "backup.rerun", "params": {"host": "host-01"}}`
)

func (f *fixture) timers(t *testing.T) {
	t.Helper()
	if err := casetimer.Run(context.Background(), f.st, f.now, casetimer.Config{}, f.d.Log, nil); err != nil {
		t.Fatal(err)
	}
}

// The 2026-11-21 shape, synthetic: one detection, a recorded plan, 136 unchanged detections, then the
// monitor reports it clear. One case, told once, every repeat counted, closed by the monitor.
func TestTheBackupShapeIsNeverSilent(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	f.watch(t)
	f.monitor(t, "ok", stale)
	id := f.all(t)[0].ID
	f.w.mu.Lock()
	f.w.plan = rerun
	f.w.mu.Unlock()
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Watching || c.Resolution != "" {
		t.Fatalf("after the recorded plan: %+v", c)
	}
	f.timers(t)
	for i := 0; i < 136; i++ {
		f.monitor(t, "ok", stale)
	}
	f.timers(t)
	if n := len(f.all(t)); n != 1 {
		t.Fatalf("%d cases, want 1", n)
	}
	if c := f.cs(t, id); c.Status != cases.Watching || c.Repeats != 136 {
		t.Fatalf("while it lasts: status %s repeats %d", c.Status, c.Repeats)
	}
	notices, err := f.st.Notices(ctx, id)
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices %+v %v, want one due", notices, err)
	}
	f.monitor(t, "ok", fine)
	if c := f.cs(t, id); c.Status != cases.Closed || c.Resolution != cases.ResolvedItself || c.Repeats != 136 {
		t.Fatalf("after the clear: %+v", c)
	}
	if ev := lastEvent(t, f, id); ev.Kind != "case.closed" || ev.By != cases.ByDetector || ev.Actor != "ops/backup-monitor" {
		t.Fatalf("the close: %+v", ev)
	}
}

func TestRecordedWatchStaysOpen(t *testing.T) {
	f := setup(t)
	f.watch(t)
	f.monitor(t, "ok", stale)
	id := f.all(t)[0].ID
	f.w.mu.Lock()
	f.w.plan = rerun
	f.w.mu.Unlock()
	f.pass(t)
	c := f.cs(t, id)
	if c.Status != cases.Watching || c.Resolution != "" || c.ClosedAt != nil {
		t.Fatalf("a recorded plan under on_record: watch: %+v", c)
	}
	ev := lastEvent(t, f, id)
	if ev.Kind != "case.watching" || ev.By != cases.ByPolicy || ev.From != cases.Planned || ev.Actor != "flotillad" {
		t.Fatalf("the move's event: %+v", ev)
	}
	plans, _ := f.st.Plans(context.Background(), id)
	if len(plans) != 1 || plans[0].Outcome != "record" {
		t.Fatalf("plans %+v", plans)
	}
}

// The default is unchanged: without on_record a recorded plan closes its case.
func TestRecordedCloseIsTheDefault(t *testing.T) {
	f := setup(t)
	f.monitor(t, "ok", stale)
	id := f.all(t)[0].ID
	f.w.mu.Lock()
	f.w.plan = rerun
	f.w.mu.Unlock()
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Closed || c.Resolution != cases.PlanRecorded {
		t.Fatalf("%+v", c)
	}
}

func TestWatchingClosesWhenMonitorReportsClear(t *testing.T) {
	f := setup(t)
	f.watch(t)
	f.monitor(t, "ok", stale)
	id := f.all(t)[0].ID
	f.w.mu.Lock()
	f.w.plan = rerun
	f.w.mu.Unlock()
	f.pass(t)
	f.monitor(t, "ok", fine)
	if c := f.cs(t, id); c.Status != cases.Closed || c.Resolution != cases.ResolvedItself {
		t.Fatalf("%+v", c)
	}
	f.monitor(t, "ok", stale)
	if cs := f.all(t); len(cs) != 2 || cs[0].ID == id || cs[0].Status != cases.HandedOver {
		t.Fatalf("after the clear, a new detection is a new case: %+v", cs)
	}
}

// A failed or refused run knows nothing: it clears no watched case.
func TestFailedRunDoesNotClearWatching(t *testing.T) {
	f := setup(t)
	f.watch(t)
	f.monitor(t, "ok", stale)
	id := f.all(t)[0].ID
	f.w.mu.Lock()
	f.w.plan = rerun
	f.w.mu.Unlock()
	f.pass(t)
	f.monitor(t, "error", fine)
	f.monitor(t, "ok", `{"outcome": "nonsense"}`)
	if c := f.cs(t, id); c.Status != cases.Watching {
		t.Fatalf("a failed or refused run cleared it: %+v", c)
	}
	f.monitor(t, "ok", stale)
	if c := f.cs(t, id); c.Status != cases.Watching || len(f.all(t)) != 1 {
		t.Fatalf("a detection while watching: %+v", c)
	}
}
