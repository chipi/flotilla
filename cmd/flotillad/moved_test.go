package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
)

// movedPlan is the plan -check makes for a flat triage/cycle moved to the folder layout, and the two fleets folders: the running (flat) one
// and the new one. budget is the day_usd the folder's routine.yaml has (the flat spec's is 2).
func movedPlan(t *testing.T, budget string) (plan versions.Plan, routines []*spec.Routine, flatDir string, err error) {
	t.Helper()
	flatDir = t.TempDir()
	writeFile(t, filepath.Join(flatDir, "triage/cycle.yaml"), strings.Replace(cycleRoutine, "shadow", "record", 1)+"run: {command: \"python3 x.py\", timeout: 20m}\nprompts:\n  system: playbooks/triage.md\n")
	writeFile(t, filepath.Join(flatDir, "triage/playbooks/triage.md"), "one\n")
	state := t.TempDir()
	st, err := store.Open(filepath.Join(state, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	flat, err := spec.LoadVersions(flatDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := versions.Reconcile(context.Background(), st, flat); err != nil {
		t.Fatal(err)
	}
	st.Close()
	d := t.TempDir()
	writeFile(t, filepath.Join(d, "triage/cycle/routine.yaml"), strings.Replace(strings.Replace(cycleRoutine, "shadow", "record", 1), "day_usd: 2", "day_usd: "+budget, 1))
	writeFile(t, filepath.Join(d, "triage/cycle/v1/spec.yaml"), "run: {command: \"python3 x.py\", timeout: 20m}\nprompts:\n  system: triage.md\n")
	writeFile(t, filepath.Join(d, "triage/cycle/v1/triage.md"), "one\n")
	routines, err = spec.LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err = checkVersions(state, routines)
	return plan, routines, flatDir, err
}

func TestMovedFromSaysTheRoutineFieldsAreIdentical(t *testing.T) {
	plan, routines, flatDir, err := movedPlan(t, "2")
	if err != nil || len(plan.Updates) != 1 {
		t.Fatalf("%v %+v", err, plan.Updates)
	}
	got := movedFromLines(flatDir, routines, plan)
	want := "versions: triage/cycle v1 routine fields identical to the flat spec " + movedFromNote
	if len(got) != 1 || got[0] != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if strings.Contains(got[0], "re-recorded") {
		t.Fatal("the information line must not look like the re-record line the deploy greps")
	}
}

// a changed budget is reported, never refused: the routine's own fields are outside the freeze
func TestMovedFromReportsAChangedBudgetAndDoesNotRefuse(t *testing.T) {
	plan, routines, flatDir, err := movedPlan(t, "7")
	if err != nil || len(plan.Updates) != 1 {
		t.Fatalf("a changed budget must not refuse the move: %v %+v", err, plan.Updates)
	}
	got := movedFromLines(flatDir, routines, plan)
	if len(got) != 1 || !strings.Contains(got[0], "routine fields differ from the flat spec: Budget is {7") || !strings.HasSuffix(got[0], movedFromNote) {
		t.Fatalf("%q", got)
	}
}

func TestMovedFromWithNoMoveOrNoRunningFleets(t *testing.T) {
	plan, routines, _, _ := movedPlan(t, "2")
	if got := movedFromLines(t.TempDir(), routines, versions.Plan{}); got != nil {
		t.Fatalf("no move, no line: %q", got)
	}
	got := movedFromLines(t.TempDir(), routines, plan)
	if len(got) != 1 || !strings.Contains(got[0], "not compared: cannot load ") {
		t.Fatalf("%q", got)
	}
}
