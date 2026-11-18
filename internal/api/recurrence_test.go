package api

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// security T17: a case that came back after a verified fix tells policy so; one after any other close
// does not.
func TestPlanStateKnowsAVerifiedFixCameBack(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	open := func(key string) string {
		k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01" + key}
		id, _, err := s.Store.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k}, "m", "")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	fixed := open("#1")
	for _, m := range []struct{ to, by, res string }{{cases.HandedOver, cases.ByDetector, ""}, {cases.Planned, cases.ByAct, ""}} {
		_ = s.Store.MoveCase(ctx, fixed, m.to, m.by, m.res, "x", "")
	}
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: fixed, Action: rerun, Params: map[string]string{"host": "backup-01"},
		PlanHash: "h1", Outcome: policy.Auto, Autonomy: policy.Auto}, cases.Executing, "", "host:backup-01", true); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ to, by, res string }{{cases.Checking, cases.ByAct, ""}, {cases.Closed, cases.ByCheck, cases.Verified}} {
		if err := s.Store.MoveCase(ctx, fixed, m.to, m.by, m.res, "x", ""); err != nil {
			t.Fatal(m.to, err)
		}
	}
	again := open("#2")
	c, _ := s.Store.Case(ctx, again)
	st, err := s.planState(ctx, c, policy.Plan{CaseID: again, Action: rerun, Params: map[string]string{"host": "backup-01"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.RecursVerified != fixed {
		t.Fatalf("a recurrence of a verified fix: %q, want %s", st.RecursVerified, fixed)
	}
	// a recurrence of a case that resolved itself is not a failed fix
	_ = s.Store.MoveCase(ctx, again, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = s.Store.MoveCase(ctx, again, cases.Closed, cases.ByAct, cases.ResolvedItself, "a", "")
	third := open("#3")
	c, _ = s.Store.Case(ctx, third)
	if st, _ := s.planState(ctx, c, policy.Plan{CaseID: third, Action: rerun}); st.RecursVerified != "" {
		t.Fatalf("a recurrence of a case that resolved itself: %q", st.RecursVerified)
	}
}
