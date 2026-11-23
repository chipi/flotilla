package store

import (
	"context"
	"database/sql"
	"path/filepath"
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

// Schema 36 keeps a case's fleet: a store at 35 back-fills it from the opening routine, and a hook's case from its project.
func TestMigrationBackfillsCaseFleet(t *testing.T) {
	if len(schema) < 36 {
		t.Fatalf("this test is for schema 36, the schema has %d steps", len(schema))
	}
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 35; i++ {
		if _, err := db.Exec(schema[i]); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ id, project, actor string }{
		{"case-a", "example", "seo/site-check"}, {"case-b", "meetings", "hook:meeting"}, {"case-c", "", "hook:smoke"},
	} {
		if _, err := db.Exec(`INSERT INTO cases (id, kind, target, canonical_key, project, urgency, status, opened_at, status_at)
			VALUES (?, 'k', ?, ?, ?, 0, 'detected', '2026-11-20T00:00:00Z', '2026-11-20T00:00:00Z')`, c.id, c.id, "t="+c.id, c.project); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO case_events (case_id, at, kind, actor, to_status) VALUES (?, '2026-11-20T00:00:00Z', 'case.detected', ?, 'detected')`,
			c.id, c.actor); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatalf("migrating a schema-35 database: %v", err)
	}
	defer s.Close()
	for id, want := range map[string]string{"case-a": "seo", "case-b": "meetings", "case-c": ""} {
		c, err := s.Case(context.Background(), id)
		if err != nil || c.Fleet != want {
			t.Errorf("%s: fleet %q, want %q (%v)", id, c.Fleet, want, err)
		}
	}
}
