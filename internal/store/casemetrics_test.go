package store

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
)

// A case's fleet is its opening routine's, or for a hook's case its project; open cases count by status, and every move counts once,
// a close with its resolution.
func TestCaseMetrics(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	backup, _, err := s.OpenCase(ctx, backupCase(), "ops/backup-monitor", "age 20.4h")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct{ to, by, res string }{
		{cases.HandedOver, cases.ByDetector, ""},
		{cases.Planned, cases.ByAct, ""},
		{cases.Closed, cases.ByPolicy, cases.PlanRecorded},
	} {
		if err := s.MoveCase(ctx, backup, x.to, x.by, x.res, "test", ""); err != nil {
			t.Fatal(err)
		}
	}
	k := Key{Kind: "target", Value: "meeting/file:standup"}
	if _, _, err := s.OpenCase(ctx, NewCase{Kind: "meeting", Target: "file:standup", Project: "meetings", Keys: []Key{k}, Canonical: k},
		"hook:meeting", ""); err != nil {
		t.Fatal(err)
	}

	open, moves, err := s.CaseMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0] != (CaseCount{Fleet: "meetings", Kind: "meeting", Status: cases.Detected, N: 1}) {
		t.Fatalf("open: %+v", open)
	}
	want := map[CaseMove]bool{
		{Fleet: "meetings", Kind: "meeting", To: cases.Detected, N: 1}:                                                                         true,
		{Fleet: "ops", Kind: "failed_backup", To: cases.Detected, N: 1}:                                                                        true,
		{Fleet: "ops", Kind: "failed_backup", From: cases.Detected, To: cases.HandedOver, By: cases.ByDetector, N: 1}:                          true,
		{Fleet: "ops", Kind: "failed_backup", From: cases.HandedOver, To: cases.Planned, By: cases.ByAct, N: 1}:                                true,
		{Fleet: "ops", Kind: "failed_backup", From: cases.Planned, To: cases.Closed, By: cases.ByPolicy, Resolution: cases.PlanRecorded, N: 1}: true,
	}
	if len(moves) != len(want) {
		t.Fatalf("moves: %+v", moves)
	}
	for _, m := range moves {
		if !want[m] {
			t.Fatalf("unexpected move %+v in %+v", m, moves)
		}
	}
}
