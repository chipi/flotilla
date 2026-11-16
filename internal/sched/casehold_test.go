package sched

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

// CaseHold (casetimer.Hold): the acting routine's fleet STOPped or the routine paused holds its cases; once
// both are released, the later release is when the case's clock starts again.
func TestCaseHold(t *testing.T) {
	ctx := context.Background()
	s, _, _ := setup(t, 0)
	s.Routines = append(s.Routines, &spec.Routine{Fleet: "bugfix", Routine: "fix", Enabled: true, Worker: "triage",
		Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "code_bug"}}})
	held := func() (bool, string) {
		t.Helper()
		h, at, err := s.CaseHold(ctx, "code_bug", "")
		if err != nil {
			t.Fatal(err)
		}
		return h, at
	}
	if h, at := held(); h || at != "" {
		t.Fatalf("never held: %v %q", h, at)
	}
	_ = s.Store.SetControl(ctx, "bugfix", KeyStop, "1", "op", "test")
	if h, _ := held(); !h {
		t.Fatal("a STOPped fleet does not hold its cases")
	}
	_ = s.Store.SetControl(ctx, "bugfix", KeyStop, "0", "op", "test")
	_ = s.Store.SetControl(ctx, "bugfix/fix", KeyPaused, "1", "op", "test")
	if h, _ := held(); !h {
		t.Fatal("a paused routine does not hold its cases")
	}
	_ = s.Store.SetControl(ctx, "bugfix/fix", KeyPaused, "0", "op", "test")
	h, at := held()
	_, pat, _ := s.Store.ControlAt(ctx, "bugfix/fix", KeyPaused)
	if h || at != pat {
		t.Fatalf("released: held %v at %q, want the pause's release %q", h, at, pat)
	}
	if h, _, _ := s.CaseHold(ctx, "other_kind", ""); h {
		t.Fatal("a kind no routine acts on is held")
	}
}
