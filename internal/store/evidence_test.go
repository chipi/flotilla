package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

// G13: five clean plans in a row make an action ready; a wrong verdict, a failed execution or a
// rejection ends the run, and holds it for 7 days; an unjudged shadow plan neither counts nor breaks.
func TestPromotionStreak(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	n := 0
	shadow := func(verdict string) {
		n++
		id := plannedOn(t, s, fmt.Sprintf("#s%d", n))
		r := rec(id)
		r.Outcome, r.Autonomy, r.Would = "shadow", "shadow", "auto"
		if _, err := s.ApplyPlan(ctx, r, cases.Closed, cases.ShadowPlan, "host:backup-01", false); err != nil {
			t.Fatal(err)
		}
		if verdict != "" {
			if err := s.SetVerdict(ctx, id, "ada", verdict); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := func(want Streak) {
		t.Helper()
		st, err := s.PromotionStreak(ctx, "backup.rerun")
		if err != nil {
			t.Fatal(err)
		}
		if st.Clean != want.Clean || st.Held != want.Held || st.BadWhat != want.BadWhat {
			t.Fatalf("streak %+v, want clean %d held %v bad %q", st, want.Clean, want.Held, want.BadWhat)
		}
	}
	got(Streak{})
	for i := 0; i < 4; i++ {
		shadow(VerdictRight)
	}
	shadow("") // unjudged: neither counts nor breaks
	got(Streak{Clean: 4})
	shadow(VerdictRight)
	if st, _ := s.PromotionStreak(ctx, "backup.rerun"); !st.Ready() {
		t.Fatalf("five judged right is ready: %+v", st)
	}
	shadow(VerdictRight) // at most PromotionNeed
	got(Streak{Clean: 5})

	shadow(VerdictWrong)
	got(Streak{Held: true, BadWhat: "wrong"})
	for i := 0; i < 5; i++ {
		shadow(VerdictRight)
	}
	st, _ := s.PromotionStreak(ctx, "backup.rerun")
	if st.Clean != 5 || !st.Held || st.Ready() {
		t.Fatalf("five clean after a wrong verdict this week is held, not ready: %+v", st)
	}
	s.now = func() time.Time { return time.Now().UTC().Add(8 * 24 * time.Hour) }
	if st, _ := s.PromotionStreak(ctx, "backup.rerun"); !st.Ready() {
		t.Fatalf("eight days later the wrong verdict no longer holds it: %+v", st)
	}
	s.now = func() time.Time { return time.Now().UTC() }

	// a proposed plan the operator rejected, and one whose execution failed
	id := plannedOn(t, s, "#rej")
	r := rec(id)
	r.Outcome, r.Autonomy = "propose", "propose"
	if _, err := s.ApplyPlan(ctx, r, cases.AwaitingApproval, "", "host:backup-01", false); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveCase(ctx, id, cases.Escalated, cases.ByOperator, "", "ada", "rejected: no"); err != nil {
		t.Fatal(err)
	}
	got(Streak{Held: true, BadWhat: "rejected"})
	id = plannedOn(t, s, "#fail")
	if _, err := s.ApplyPlan(ctx, rec(id), cases.Executing, "", "host:backup-01", false); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveCase(ctx, id, cases.Escalated, cases.ByAct, "", "ops-act-host", "failed"); err != nil {
		t.Fatal(err)
	}
	got(Streak{Held: true, BadWhat: "failed"})
	// a proposed plan approved as it stood, executed and verified is clean
	id = plannedOn(t, s, "#ok")
	r = rec(id)
	r.Outcome, r.Autonomy = "propose", "propose"
	if _, err := s.ApplyPlan(ctx, r, cases.AwaitingApproval, "", "host:backup-01", false); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ to, by, res string }{{cases.Executing, cases.ByOperator, ""}, {cases.Checking, cases.ByAct, ""},
		{cases.Closed, cases.ByCheck, cases.Verified}} {
		if err := s.MoveCase(ctx, id, m.to, m.by, m.res, "x", ""); err != nil {
			t.Fatal(m.to, err)
		}
	}
	got(Streak{Clean: 1, Held: true, BadWhat: "failed"})
}
