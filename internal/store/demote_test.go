package store

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
)

const act = "backup.rerun_observability"

func actionAt(t *testing.T, s *Store, level string) {
	t.Helper()
	if err := s.SetControl(context.Background(), "action:"+act, "autonomy", level, "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
}

func level(t *testing.T, s *Store) string {
	t.Helper()
	v, _ := s.Control(context.Background(), "action:"+act, "autonomy")
	return v
}

// planned to `to` with a plan for the action (executing for auto, awaiting_approval for propose).
func decided(t *testing.T, s *Store, key, to string) string {
	t.Helper()
	id := plannedOn(t, s, key)
	if _, err := s.ApplyPlan(context.Background(), rec(id), to, "", "host:mini"+key, true); err != nil {
		t.Fatal(err)
	}
	return id
}

// RFC-0004 §5.2: a failed or unknown execution demotes auto to propose; an operator's rejection demotes
// propose to shadow; recorded on the case and in the events. Never a promotion, never from elsewhere.
func TestAutomaticDemotion(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	actionAt(t, s, "auto")
	id := decided(t, s, "#fail", cases.Executing)
	_ = s.MoveCase(ctx, id, cases.Escalated, cases.ByAct, "", "ops-act-host", "the host agent reported failed")
	if l := level(t, s); l != "propose" {
		t.Fatalf("after a failed execution the action is %q, want propose", l)
	}
	ev, _ := s.CaseEvents(ctx, id)
	if ev[len(ev)-1].Kind != "action.demoted" {
		t.Fatalf("the demotion is not on the case: %+v", ev[len(ev)-1])
	}

	id = decided(t, s, "#reject", cases.AwaitingApproval)
	_ = s.MoveCase(ctx, id, cases.Escalated, cases.ByOperator, "", "user:ada", "rejected")
	if l := level(t, s); l != "shadow" {
		t.Fatalf("after a rejection the action is %q, want shadow", l)
	}

	// An expiry is not a rejection.
	actionAt(t, s, "propose")
	id = decided(t, s, "#expire", cases.AwaitingApproval)
	_ = s.MoveCase(ctx, id, cases.Escalated, cases.ByTimer, "", "flotillad", "the approval expired")
	if l := level(t, s); l != "propose" {
		t.Fatalf("an expiry demoted the action to %q", l)
	}

	// Already at or below the target: nothing changes, and a demotion never raises.
	actionAt(t, s, "shadow")
	id = decided(t, s, "#low", cases.Executing)
	_ = s.MoveCase(ctx, id, cases.Escalated, cases.ByTimer, "", "flotillad", "unknown")
	if l := level(t, s); l != "shadow" {
		t.Fatalf("a demotion raised shadow to %q", l)
	}

	// An escalation from elsewhere demotes nothing: policy refusing a plan (planned -> escalated), where
	// nothing ran and nothing was proposed.
	actionAt(t, s, "auto")
	id = plannedOn(t, s, "#refused")
	if _, err := s.ApplyPlan(ctx, rec(id), cases.Escalated, "", "host:mini#refused", false); err != nil {
		t.Fatal(err)
	}
	if l := level(t, s); l != "auto" {
		t.Fatalf("a refused plan demoted the action to %q", l)
	}
}
