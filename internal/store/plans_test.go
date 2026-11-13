package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

func plannedOn(t *testing.T, s *Store, key string) string {
	t.Helper()
	ctx := context.Background()
	k := Key{Kind: "target", Value: "failed_backup/host:mini" + key}
	id, _, err := s.OpenCase(ctx, NewCase{Kind: "failed_backup", Target: "host:mini", Keys: []Key{k}, Canonical: k}, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = s.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "triage/cycle", "")
	return id
}

func rec(id string) PlanRecord {
	return PlanRecord{CaseID: id, RunID: "r", Action: "backup.rerun_observability", Params: map[string]string{"host": "mini"},
		PlanHash: "h", Outcome: "auto", Autonomy: "auto", Reasons: []string{"ok"}}
}

// A failed or unknown execution keeps the target held until the operator clears the case; closing it
// releases the lock.
func TestLockLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a := plannedOn(t, s, "#a")
	if _, err := s.ApplyPlan(ctx, rec(a), cases.Executing, "", "host:mini", true); err != nil {
		t.Fatal(err)
	}
	_ = s.MoveCase(ctx, a, cases.Escalated, cases.ByTimer, "", "flotillad", "unknown")
	if h, _ := s.LockHolder(ctx, "host:mini"); h != a {
		t.Fatalf("after an unknown execution the lock is %q, want it held by %s", h, a)
	}
	_ = s.MoveCase(ctx, a, cases.Closed, cases.ByOperator, cases.OperatorResolved, "user:ada", "checked by hand")
	if h, _ := s.LockHolder(ctx, "host:mini"); h != "" {
		t.Fatalf("a closed case holds the target: %s", h)
	}
}

// A lock held by another case refuses the whole application: no plan recorded, the case still planned.
func TestApplyPlanWithTheTargetLocked(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a, b := plannedOn(t, s, "#a"), plannedOn(t, s, "#b")
	if _, err := s.ApplyPlan(ctx, rec(a), cases.AwaitingApproval, "", "host:mini", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPlan(ctx, rec(b), cases.Executing, "", "host:mini", true); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second lock: %v", err)
	}
	if c, _ := s.Case(ctx, b); c.Status != cases.Planned {
		t.Fatalf("the refused case moved to %s", c.Status)
	}
	if p, _ := s.Plans(ctx, b); len(p) != 0 {
		t.Fatalf("a refused plan was recorded: %+v", p)
	}
	// A move the table refuses rolls the plan back too.
	if _, err := s.ApplyPlan(ctx, rec(b), cases.Checking, "", "host:mini", false); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("planned -> checking: %v", err)
	}
	if p, _ := s.Plans(ctx, b); len(p) != 0 {
		t.Fatalf("a plan recorded with a refused move: %+v", p)
	}
}

func TestCountsAndHolds(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a := plannedOn(t, s, "#a")
	_, _ = s.ApplyPlan(ctx, rec(a), cases.Executing, "", "host:mini", true)
	_ = s.MoveCase(ctx, a, cases.Escalated, cases.ByAct, "", "ops-act-host", "failed")
	now := time.Now()
	n, err := s.Counts(ctx, "backup.rerun_observability", "host:mini", "mini", now.Add(-time.Hour), now, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n.TargetToday != 1 || n.HostHour != 1 || n.PlatformHour != 1 || n.FailedHour != 1 || n.HostNewCases != 1 {
		t.Fatalf("counts %+v", n)
	}
	if n, _ := s.Counts(ctx, "other.action", "host:mini", "mini", now.Add(-time.Hour), now, 15*time.Minute); n.TargetToday != 0 {
		t.Fatalf("another action's executions counted: %+v", n)
	}
	if n, _ := s.Counts(ctx, "backup.rerun_observability", "host:mini", "mini", now.Add(time.Minute), now.Add(2*time.Hour), time.Minute); n.TargetToday != 0 || n.HostHour != 0 || n.HostNewCases != 0 {
		t.Fatalf("old executions counted: %+v", n)
	}
	id, _ := s.AddHold(ctx, "host:mini", now.Add(time.Hour), "user:ada", "api", "disk")
	if hs, _ := s.Holds(ctx, now, false); len(hs) != 1 || hs[0].ID != id {
		t.Fatalf("active holds %+v", hs)
	}
	if hs, _ := s.Holds(ctx, now.Add(2*time.Hour), false); len(hs) != 0 {
		t.Fatalf("an expired hold is active: %+v", hs)
	}
	if err := s.LiftHold(ctx, id, "user:ada", "api", "done"); err != nil {
		t.Fatal(err)
	}
	if hs, _ := s.Holds(ctx, now, false); len(hs) != 0 {
		t.Fatalf("a lifted hold is active: %+v", hs)
	}
	if err := s.LiftHold(ctx, id, "user:ada", "api", "again"); !errors.Is(err, ErrNoSuchHold) {
		t.Fatalf("lifting twice: %v", err)
	}
}
