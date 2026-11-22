package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

func checkingOn(t *testing.T, s *Store, key string) string {
	t.Helper()
	ctx := context.Background()
	id := plannedOn(t, s, key)
	if _, err := s.ApplyPlan(ctx, rec(id), cases.Executing, "", "host:backup-01"+key, true); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveCase(ctx, id, cases.Checking, cases.ByAct, "", "flotillad", "done"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.LeaseCheck(ctx, id, "ops/check", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("lease %v %v", ok, err)
	}
	return id
}

// A verified case goes on to its plan's then only from checking, held by the check, at most
// MaxStageChanges times; the target stays the case's.
func TestHandOnVerified(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id := checkingOn(t, s, "#a")
	if err := s.HandOnVerified(ctx, id, "decisions", "ops/other", "run r: done", `{}`); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("another holder: %v", err)
	}
	if err := s.HandOnVerified(ctx, id, "", "ops/check", "run r: done", `{}`); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("the stage it is at: %v", err)
	}
	if err := s.HandOnVerified(ctx, id, "decisions", "ops/check", "run r: done", `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Case(ctx, id)
	if c.Status != cases.HandedOver || c.Stage != "decisions" || c.StageChanges != 1 {
		t.Fatalf("%s %q %d", c.Status, c.Stage, c.StageChanges)
	}
	if d, _ := s.StageData(ctx, id); d != `{"x":1}` {
		t.Fatalf("stage data %q", d)
	}
	if h, _ := s.LockHolder(ctx, "host:backup-01#a"); h != id {
		t.Fatalf("lock %q", h)
	}
	if err := s.HandOnVerified(ctx, id, "more", "ops/check", "run r: done", `{}`); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("not checking any more: %v", err)
	}

	b := checkingOn(t, s, "#b")
	if _, err := s.db.ExecContext(ctx, `UPDATE cases SET stage_changes = ? WHERE id = ?`, MaxStageChanges, b); err != nil {
		t.Fatal(err)
	}
	if err := s.HandOnVerified(ctx, b, "decisions", "ops/check", "run r: done", `{}`); !errors.Is(err, ErrStageLimit) {
		t.Fatalf("past the limit: %v", err)
	}
	if c, _ := s.Case(ctx, b); c.Status != cases.Checking {
		t.Fatalf("a refused hand-on moved the case: %s", c.Status)
	}
}

// Only a check moves a case from checking back to handed_over.
func TestCheckingToHandedOverIsTheChecks(t *testing.T) {
	for _, by := range []string{cases.ByAct, cases.ByOperator, cases.ByPolicy, cases.ByTimer} {
		if err := cases.Check(cases.Checking, cases.HandedOver, by, ""); err == nil {
			t.Fatalf("%s moved a checking case to handed_over", by)
		}
	}
	if err := cases.Check(cases.Checking, cases.HandedOver, cases.ByCheck, ""); err != nil {
		t.Fatal(err)
	}
}
