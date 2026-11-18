package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
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

// A database written before ADR-0007 keeps its runs: step 2 renames the column, it does not copy.
// ...and step 3 (ADR-0008) turns its stage into autonomy, "live" into "auto", in runs and controls.
func TestMigratesStepOneDatabase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL); INSERT INTO schema_version (n) VALUES (1);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs (id, job, spec_hash, trigger, actor, stage, started_at, status) VALUES ('r1', 'triage/cycle', 'h', 'schedule', 'flotillad', 'live', '2026-11-10T00:00:00Z', 'ok');
		INSERT INTO controls (subject, key, value, actor, at) VALUES ('platform/heartbeat', 'stage', 'live', 'operator', '2026-11-10T00:00:00Z'),
		  ('triage/cycle', 'stage', 'shadow', 'operator', '2026-11-10T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatalf("migrating a step-1 database: %v", err)
	}
	defer s.Close()
	runs, err := s.Runs(context.Background(), "triage/cycle", 10)
	if err != nil || len(runs) != 1 || runs[0].Routine != "triage/cycle" || runs[0].Autonomy != "auto" {
		t.Fatalf("the old run must survive under routine, with autonomy auto, got %+v %v", runs, err)
	}
	for subject, want := range map[string]string{"platform/heartbeat": "auto", "triage/cycle": "shadow"} {
		if v, err := s.Control(context.Background(), subject, "autonomy"); err != nil || v != want {
			t.Fatalf("%s: the stage control must become autonomy %q, got %q %v", subject, want, v, err)
		}
		if v, _ := s.Control(context.Background(), subject, "stage"); v != "" {
			t.Fatalf("%s: the old stage control must be gone, got %q", subject, v)
		}
	}
}

// flotillad's container has a read-only root and no writable temp directory. On 2026-11-11 schema
// step 3, an UPDATE over the host's 835 runs, failed there with "disk I/O error (6410)", while every
// test passed: outside the container SQLite falls back to /var/tmp, so no unit test can take the temp
// directory away. Reproduced with the deployed image, read-only, on a copy of the host's store; the
// guard here is the setting that removes the need: temporary data in memory.
func TestTempStoreInMemory(t *testing.T) {
	s, _ := open(t)
	var v int
	if err := s.db.QueryRow(`PRAGMA temp_store`).Scan(&v); err != nil || v != 2 {
		t.Fatalf("temp_store = %d (%v), want 2 (memory)", v, err)
	}
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
	if err := s.SetControl(ctx, "triage", "stop", "1", "claude-example", "api"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetControl(ctx, "triage", "stop", "0", "operator", "cli"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Control(ctx, "triage", "stop"); v != "0" {
		t.Fatalf("control = %q", v)
	}
	ev, _ := s.Events(ctx, 10)
	if len(ev) != 2 || ev[0].Actor != "operator" || ev[1].Actor != "claude-example" || ev[0].Kind != "control.stop" {
		t.Fatalf("every change is an event with its actor: %+v", ev)
	}
}

func TestRunLifecycle(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	r := Run{ID: "r1", Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad", Autonomy: "record",
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
	if l, _ := s.LastSuccess(ctx, "other/routine"); !l.IsZero() {
		t.Fatal("a routine with no runs has no last success")
	}
}

func TestInterrupted(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	_ = s.StartRun(ctx, Run{ID: "r1", Routine: "j/a", SpecHash: "h", Trigger: "schedule", Actor: "x", Autonomy: "record",
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

func bumpSchema(t *testing.T, p string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (?)`, n); err != nil {
		t.Fatal(err)
	}
}

// -check of an older flotillad must fail on a newer store before anything is stopped (a rollback dies
// there), with the message Open gives at start.
func TestCheckSchemaRefusesNewerStore(t *testing.T) {
	s, p := open(t)
	s.Close()
	bumpSchema(t, p, len(schema)+1)
	err := CheckSchema(p)
	if err == nil || !strings.Contains(err.Error(), "newer than this flotillad") {
		t.Fatalf("CheckSchema on a newer store = %v, want the \"newer than this flotillad\" refusal", err)
	}
	if want := fmt.Sprintf("(%d)", len(schema)); !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal must name this binary's schema %s (deploy.sh reads it): %v", want, err)
	}
}

func TestCheckSchemaAcceptsCurrentOlderAndMissing(t *testing.T) {
	if err := CheckSchema(filepath.Join(t.TempDir(), "absent.db")); err != nil {
		t.Fatalf("a missing store is a first install: %v", err)
	}
	s, p := open(t)
	s.Close()
	if err := CheckSchema(p); err != nil {
		t.Fatalf("a store at this schema: %v", err)
	}
	old := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL); INSERT INTO schema_version (n) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := CheckSchema(old); err != nil {
		t.Fatalf("an older store is migrated at start, not refused: %v", err)
	}
}

// -check must not migrate or write: the older store stays at its schema, byte for byte, and no
// schema_version table appears in a database that had none.
func TestCheckSchemaWritesNothing(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.db")
	db, err := sql.Open("sqlite", old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL); INSERT INTO schema_version (n) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(old); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(old)
	if !bytes.Equal(before, after) {
		t.Fatal("CheckSchema changed the database file")
	}
	var n int
	db, _ = sql.Open("sqlite", old)
	defer db.Close()
	if err := db.QueryRow(`SELECT MAX(n) FROM schema_version`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("schema after CheckSchema = %d %v, want 1", n, err)
	}

	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(empty); err != nil {
		t.Fatalf("an empty store file: %v", err)
	}
	if fi, _ := os.Stat(empty); fi.Size() != 0 {
		t.Fatalf("CheckSchema wrote %d bytes to an empty store", fi.Size())
	}
}

// A live flotillad holds the store open in WAL mode while a deploy's -check runs in another container.
func TestCheckSchemaWhileTheStoreIsOpen(t *testing.T) {
	s, p := open(t)
	defer s.Close()
	if err := CheckSchema(p); err != nil {
		t.Fatalf("-check beside a running flotillad: %v", err)
	}
	bumpSchema(t, p, len(schema)+1)
	if err := CheckSchema(p); err == nil {
		t.Fatal("a newer store beside a running flotillad must still be refused")
	}
}

// A database at schema 22 (before run_calls had finish_reason, step and decision_key) keeps its calls: the new
// columns read as their defaults, and the new eval table works on it.
func TestMigrationAddsCallColumnsWithDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 22; i++ {
		if _, err := db.Exec(schema[i]); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO run_calls (run_id, routine, at, alias) VALUES ('old', 'triage/cycle', '2026-11-16T00:00:00Z', 'a')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatalf("migrating a schema-22 database: %v", err)
	}
	defer s.Close()
	got, err := s.RunCalls(context.Background(), []string{"old"})
	if err != nil || len(got["old"]) != 1 {
		t.Fatalf("the old call must survive: %+v %v", got, err)
	}
	if c := got["old"][0]; c.FinishReason != "" || c.Step != nil || c.DecisionKey != "" {
		t.Fatalf("the old call must read with the defaults: %+v", c)
	}
	if _, err := s.RecordEval(context.Background(), EvalResult{Routine: "r", PromptSHA: "p", Model: "m", Metric: "x", Value: 1, RecordedBy: "u"}); err != nil {
		t.Fatalf("eval_results on a migrated database: %v", err)
	}
}

// A database at schema 23 keeps its calls: role, episode, episode_seq and tools read as their defaults.
func TestMigrationAddsRoleEpisodeAndTools(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 23; i++ {
		if _, err := db.Exec(schema[i]); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO run_calls (run_id, routine, at, alias, finish_reason, step, decision_key) VALUES ('old', 'triage/cycle', '2026-11-16T00:00:00Z', 'a', 'stop', 2, 'fp')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatalf("migrating a schema-23 database: %v", err)
	}
	defer s.Close()
	got, err := s.RunCalls(context.Background(), []string{"old"})
	if err != nil || len(got["old"]) != 1 {
		t.Fatalf("the old call must survive: %+v %v", got, err)
	}
	c := got["old"][0]
	if c.FinishReason != "stop" || c.DecisionKey != "fp" || c.Role != "" || c.Episode != "" || c.EpisodeSeq != nil || c.Tools != "" {
		t.Fatalf("the old call must keep its columns and read the new ones as defaults: %+v", c)
	}
}

// Schema 25 adds run_calls.kind: a database at 24 keeps its calls, as kind 'call', with the old array-of-names tools.
func TestMigrationAddsCallKind(t *testing.T) {
	if len(schema) != 25 {
		t.Fatalf("this test is for schema 25, the schema has %d steps", len(schema))
	}
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		if _, err := db.Exec(schema[i]); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO run_calls (run_id, routine, at, alias, role, tools) VALUES ('old', 'bugfix/fix', '2026-11-17T00:00:00Z', 'a', 'coder', '["bash","read"]')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatalf("migrating a schema-24 database: %v", err)
	}
	defer s.Close()
	got, err := s.RunCalls(context.Background(), []string{"old"})
	if err != nil || len(got["old"]) != 1 {
		t.Fatalf("the old call must survive: %+v %v", got, err)
	}
	c := got["old"][0]
	if c.Kind != KindCall || c.Role != "coder" || c.Tools != `["bash","read"]` || len(c.ToolCalls()) != 2 {
		t.Fatalf("the old call reads as a call and keeps its tools: %+v", c)
	}
}
