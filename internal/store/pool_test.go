package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Reads run on the writer's one connection (2026-11-25): one connection in all, so a read never runs between a writer's steps.
func TestReadsShareTheWritersConnection(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	if st.rdb != st.db || st.db.Stats().MaxOpenConnections != 1 {
		t.Fatalf("reads on their own handle, or more than one connection: %+v", st.db.Stats())
	}
	if _, err := st.Events(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing the one connection once: %v", err)
	}
}

// A run's end and its spend commit together: a reader never sees the run ended without the spend in the day's total.
func TestEndRunRecordsTheSpendWithTheEnd(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"r1", "r2"} {
		if err := st.StartRun(ctx, Run{ID: id, Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
			Autonomy: "record", StartedAt: "2026-11-23T10:00:00.000000000Z", Status: StatusRunning}); err != nil {
			t.Fatal(err)
		}
	}
	total, err := st.EndRun(ctx, "r1", StatusCancelled, nil, 0.3, 2, "cancelled", "2026-11-23", "triage/cycle")
	if err != nil || total != 0.3 {
		t.Fatalf("end: %v %v", total, err)
	}
	if total, _ = st.EndRun(ctx, "r2", StatusOK, nil, 0, 1, "", "2026-11-23", "triage/cycle"); total != 0.3 {
		t.Errorf("a run with no spend: the day's total %v, want 0.3", total)
	}
	if _, err := st.EndRun(ctx, "r1", StatusOK, nil, 0.5, 1, "", "2026-11-23", "triage/cycle"); err == nil {
		t.Fatal("an ended run ended again")
	}
	if spent, _ := st.Spend(ctx, "2026-11-23", "triage/cycle"); spent != 0.3 {
		t.Errorf("a refused end added its spend: %v", spent)
	}
}
