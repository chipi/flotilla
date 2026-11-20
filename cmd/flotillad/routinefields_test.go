package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// movedWorld is a store that ran the flat triage/cycle (versions and routine fields recorded, as the start does), and the same routine moved to the
// folder layout with the given day_usd in its routine.yaml (the flat spec's is 2). withSnapshot false leaves out the snapshot, as a store from before it.
func movedWorld(t *testing.T, budget string, withSnapshot bool) (state string, routines []*spec.Routine, plan versions.Plan) {
	t.Helper()
	flatDir := t.TempDir()
	writeFile(t, filepath.Join(flatDir, "triage/cycle.yaml"), strings.Replace(cycleRoutine, "shadow", "record", 1)+"run: {command: \"python3 x.py\", timeout: 20m}\nprompts:\n  system: playbooks/triage.md\n")
	writeFile(t, filepath.Join(flatDir, "triage/playbooks/triage.md"), "one\n")
	state = t.TempDir()
	st, err := store.Open(filepath.Join(state, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	flat, err := spec.LoadVersions(flatDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := versions.Reconcile(ctx, st, flat); err != nil {
		t.Fatal(err)
	}
	if withSnapshot {
		if err := recordRoutineFields(ctx, st, quiet(), flat); err != nil {
			t.Fatal(err)
		}
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
	if err != nil || len(plan.Updates) != 1 || len(plan.Updates[0].Moved) == 0 {
		t.Fatalf("the move must be re-recorded: %v %+v", err, plan.Updates)
	}
	return state, routines, plan
}

func TestACheckAfterAStartSaysNothingUntilAFieldMoves(t *testing.T) {
	ctx := context.Background()
	d := fleetsDir(t)
	routines, err := spec.LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if lines, err := checkRoutineFields(state, routines, versions.Plan{}); err != nil || len(lines) != 1 || !strings.HasPrefix(lines[0], "routine fields: no snapshot yet") {
		t.Fatalf("no store: %v %q", err, lines)
	}
	st, err := store.Open(filepath.Join(state, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := recordRoutineFields(ctx, st, quiet(), routines); err != nil {
		t.Fatal(err)
	}
	if lines, err := checkRoutineFields(state, routines, versions.Plan{}); err != nil || len(lines) != 0 {
		t.Fatalf("nothing moved: %v %q", err, lines)
	}
	writeFile(t, filepath.Join(d, "triage/cycle/routine.yaml"), strings.Replace(cycleRoutine, "day_usd: 2", "day_usd: 5", 1))
	routines, _ = spec.LoadVersions(d)
	lines, err := checkRoutineFields(state, routines, versions.Plan{})
	if err != nil || len(lines) != 1 || lines[0] != `routine triage/cycle: budget {"day_usd":2} -> {"day_usd":5}` {
		t.Fatalf("a changed budget: %v %q", err, lines)
	}
	// the start records it as one event, and a second check is quiet again
	if err := recordRoutineFields(ctx, st, quiet(), routines); err != nil {
		t.Fatal(err)
	}
	if es, _ := st.RoutineChanges(ctx, "triage/cycle", 5); len(es) != 1 || !strings.Contains(es[0].Detail, `budget {"day_usd":2} -> {"day_usd":5}`) {
		t.Fatalf("%+v", es)
	}
	if lines, _ := checkRoutineFields(state, routines, versions.Plan{}); len(lines) != 0 {
		t.Fatalf("%q", lines)
	}
}

func TestAMovedRoutineSaysItsFieldsAreIdenticalToTheSnapshotOrWhatMoved(t *testing.T) {
	state, routines, plan := movedWorld(t, "2", true)
	lines, err := checkRoutineFields(state, routines, plan)
	want := "versions: triage/cycle v1 routine fields: identical to the stored snapshot " + routineFieldsNote
	if err != nil || len(lines) != 1 || lines[0] != want {
		t.Fatalf("%v %q, want %q", err, lines, want)
	}
	state, routines, plan = movedWorld(t, "7", true) // not refused: the plan above is the same re-record
	lines, _ = checkRoutineFields(state, routines, plan)
	if len(lines) != 2 || lines[0] != `routine triage/cycle: budget {"day_usd":2} -> {"day_usd":7}` ||
		lines[1] != `versions: triage/cycle v1 routine fields: changed since the stored snapshot: budget {"day_usd":2} -> {"day_usd":7} `+routineFieldsNote {
		t.Fatalf("%q", lines)
	}
	state, routines, plan = movedWorld(t, "2", false)
	lines, _ = checkRoutineFields(state, routines, plan)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "routine fields: no snapshot yet") || lines[1] != "versions: triage/cycle v1 routine fields: no snapshot yet to compare with "+routineFieldsNote {
		t.Fatalf("%q", lines)
	}
}
