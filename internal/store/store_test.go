package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func TestMigrateTwice(t *testing.T) {
	s, p := open(t)
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("reopening a migrated database: %v", err)
	}
	s2.Close()
}

func TestRefusesNewerSchema(t *testing.T) {
	s, p := open(t)
	if _, err := s.db.Exec(`INSERT INTO schema_version (n) VALUES (99)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(p); err == nil || !strings.Contains(err.Error(), "newer than this flotillad") {
		t.Fatalf("an older binary must refuse a newer database, got %v", err)
	}
}

func TestControlRecordsWho(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if v, _ := s.Control(ctx, "triage", "stop"); v != "" {
		t.Fatalf("unset control = %q", v)
	}
	if err := s.SetControl(ctx, "triage", "stop", "1", "claude-homelab", "api"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetControl(ctx, "triage", "stop", "0", "operator", "cli"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Control(ctx, "triage", "stop"); v != "0" {
		t.Fatalf("control = %q", v)
	}
	ev, _ := s.Events(ctx, 10)
	if len(ev) != 2 || ev[0].Actor != "operator" || ev[1].Actor != "claude-homelab" || ev[0].Kind != "control.stop" {
		t.Fatalf("every change is an event with its actor: %+v", ev)
	}
}

func TestRunLifecycle(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	r := Run{ID: "r1", Job: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad", Stage: "shadow",
		StartedAt: time.Now().UTC().Format(ts), Status: StatusRunning}
	if err := s.StartRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := s.FinishRun(ctx, "r1", StatusOK, &code, 0.01, 3.2, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, "r1", StatusError, nil, 0, 0, "x"); err == nil {
		t.Fatal("a finished run must not be finished again")
	}
	runs, _ := s.Runs(ctx, "triage/cycle", 5)
	if len(runs) != 1 || runs[0].Status != StatusOK || *runs[0].ExitCode != 0 || runs[0].EndedAt == nil {
		t.Fatalf("got %+v", runs)
	}
	last, _ := s.LastSuccess(ctx, "triage/cycle")
	if last.IsZero() {
		t.Fatal("last success not found")
	}
	if l, _ := s.LastSuccess(ctx, "other/job"); !l.IsZero() {
		t.Fatal("a job with no runs has no last success")
	}
}

func TestInterrupted(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	_ = s.StartRun(ctx, Run{ID: "r1", Job: "j/a", SpecHash: "h", Trigger: "schedule", Actor: "x", Stage: "shadow",
		StartedAt: time.Now().UTC().Format(ts), Status: StatusRunning})
	n, err := s.MarkInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	runs, _ := s.Runs(ctx, "", 5)
	if runs[0].Status != StatusInterrupted {
		t.Fatalf("got %s", runs[0].Status)
	}
}

func TestSpend(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if v, _ := s.Spend(ctx, "2026-11-08", "triage/cycle"); v != 0 {
		t.Fatal("no spend is zero")
	}
	s.AddSpend(ctx, "2026-11-08", "triage/cycle", 0.25)
	v, _ := s.AddSpend(ctx, "2026-11-08", "triage/cycle", 0.5)
	if v != 0.75 {
		t.Fatalf("got %v", v)
	}
	if o, _ := s.Spend(ctx, "2026-11-09", "triage/cycle"); o != 0 {
		t.Fatal("a new day starts at zero")
	}
}

func TestBackupIsAWorkingCopy(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	_ = s.SetControl(ctx, "triage", "stop", "1", "op", "api")
	bp := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, bp); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", bp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM controls WHERE subject='triage'`).Scan(&v); err != nil || v != "1" {
		t.Fatalf("backup content: %q %v", v, err)
	}
}
