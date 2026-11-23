package ui

import (
	"testing"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// RFC-0013 §2 on the case page: the stage and what the case's runs spent of its kind's limit.
func TestCasePath(t *testing.T) {
	fix := &spec.Routine{Fleet: "bugfix", Routine: "fix", Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "code_bug",
		Limit: &spec.CaseLimit{USD: 1, Minutes: 90}}}}
	clarify := &spec.Routine{Fleet: "bugfix", Routine: "clarify", Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "code_bug", Stage: "clarify"}}}
	sch := &sched.Scheduler{}
	sch.SetRoutines([]*spec.Routine{clarify, fix})
	u := &Server{API: &api.Server{Sched: sch}}
	got := u.casePath(store.Case{Kind: "code_bug", Stage: "clarify", StageChanges: 1, SpentUSD: 0.25, SpentS: 1830})
	want := "Handed on 1 of 2 times. Its runs have spent $0.25 of $1.00 and 30 of 90 minutes."
	if got != want {
		t.Fatalf("got %q", got)
	}
	if got := u.casePath(store.Case{Kind: "failed_backup"}); got != "" {
		t.Fatalf("a kind with neither: %q", got)
	}
}
