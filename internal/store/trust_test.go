package store

import (
	"context"
	"slices"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
)

// The routines that planned an action are its live planners; a shadow run's plan names none. A recorded plan waits for a
// verdict until anyone judges it; a proposed plan, or an older plan of a case planned again, never waits.
func TestActionPlannersAndUnjudgedRecorded(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, r := range []Run{{ID: "live", Routine: "ops/rerun-backup"}, {ID: "shadow", Routine: "ops/rerun-backup@v2", Shadow: true}, {ID: "other", Routine: "ops/other"}} {
		r.SpecHash, r.Trigger, r.Actor, r.Autonomy, r.StartedAt, r.Status = "h", "schedule", "flotillad", "record", "2026-11-23T08:00:00Z", StatusRunning
		if err := s.StartRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(key, run, outcome string, to string, res string) string {
		id := plannedOn(t, s, key)
		r := rec(id)
		r.RunID, r.Outcome, r.Autonomy = run, outcome, outcome
		if _, err := s.ApplyPlan(ctx, r, to, res, "host:backup-01", false); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := plan("#a", "live", "record", cases.Closed, cases.PlanRecorded)
	b := plan("#b", "other", "record", cases.Closed, cases.PlanRecorded)
	plan("#c", "shadow", "record", cases.Closed, cases.PlanRecorded)
	plan("#d", "live", "propose", cases.AwaitingApproval, "")

	got, err := s.ActionPlanners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ops/other", "ops/rerun-backup"}; !slices.Equal(got["backup.rerun"], want) {
		t.Fatalf("planners %v, want %v (the shadow's key left out)", got["backup.rerun"], want)
	}

	ps, n, err := s.UnjudgedRecorded(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(ps) != 2 {
		t.Fatalf("%d waiting (want 3 recorded, not the proposed one), %d shown (want the limit, 2)", n, len(ps))
	}
	if err := s.SetVerdict(ctx, a, "ada", VerdictRight); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVerdict(ctx, b, "otto", VerdictWrong); err != nil {
		t.Fatal(err)
	}
	if _, n, _ = s.UnjudgedRecorded(ctx, 0); n != 1 {
		t.Fatalf("%d waiting after two verdicts, want 1: a verdict by anyone answers it", n)
	}
}
