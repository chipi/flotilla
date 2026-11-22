package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func fieldsOfBudget(day string) map[string]string {
	return map[string]string{"budget": `{"day_usd":` + day + `}`, "worker": `"triage"`, "enabled": "true"}
}

func updates(t *testing.T, s *Store, routine string) []Event {
	t.Helper()
	es, err := s.RoutineChanges(context.Background(), routine, 10)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// The first start writes a snapshot and no event (nothing to compare it with); an unchanged start writes nothing; a changed field writes one
// routine.update event naming it with old and new value, and the snapshot follows.
func TestRoutineFieldsAreRecordedAtStartAndAChangeIsOneEvent(t *testing.T) {
	s, p := open(t)
	ctx := context.Background()
	got, err := s.RecordRoutineFields(ctx, map[string]map[string]string{"triage/cycle": fieldsOfBudget("1")}, "flotillad")
	if err != nil || len(got) != 1 || !got[0].First {
		t.Fatalf("first: %+v %v", got, err)
	}
	if n := len(updates(t, s, "triage/cycle")); n != 0 {
		t.Fatalf("the first snapshot wrote %d events", n)
	}
	e0, _ := s.Events(ctx, 1000)
	if got, err = s.RecordRoutineFields(ctx, map[string]map[string]string{"triage/cycle": fieldsOfBudget("1")}, "flotillad"); err != nil || len(got) != 0 {
		t.Fatalf("an unchanged start: %+v %v", got, err)
	}
	if e1, _ := s.Events(ctx, 1000); len(e1) != len(e0) {
		t.Fatal("an unchanged start wrote an event")
	}
	got, err = s.RecordRoutineFields(ctx, map[string]map[string]string{"triage/cycle": fieldsOfBudget("2")}, "flotillad")
	if err != nil || len(got) != 1 || got[0].First || len(got[0].Changes) != 1 || got[0].Changes[0] != `budget {"day_usd":1} -> {"day_usd":2}` {
		t.Fatalf("a changed budget: %+v %v", got, err)
	}
	es := updates(t, s, "triage/cycle")
	if len(es) != 1 || es[0].Actor != "flotillad" || es[0].Via != "deploy" || es[0].Detail != `budget {"day_usd":1} -> {"day_usd":2}` {
		t.Fatalf("events %+v", es)
	}
	read, err := ReadRoutineFields(p)
	if err != nil || read.Fields["triage/cycle"]["budget"] != `{"day_usd":2}` {
		t.Fatalf("the snapshot did not follow: %+v %v", read, err)
	}
	// each field that moved is named; the others are not
	got, _ = s.RecordRoutineFields(ctx, map[string]map[string]string{"triage/cycle": {"budget": `{"day_usd":2}`, "worker": `"ops"`, "enabled": "false"}}, "flotillad")
	if len(got) != 1 || strings.Join(got[0].Changes, "|") != "enabled true -> false|worker triage -> ops" {
		t.Fatalf("%+v", got)
	}
}

// the store at the schema before this one (every store until this deploy) migrates: the table is there, nothing is in it, and -check reads it as no snapshot
func TestAStoreBeforeRoutineSnapshotsMigratesAndReadsAsNoSnapshot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "flotilla.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < FieldsSchema-1; i++ {
		if _, err := db.Exec(schema[i]); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	read, err := ReadRoutineFields(p)
	if err != nil || !read.Exists || read.Schema != FieldsSchema-1 || len(read.Fields) != 0 {
		t.Fatalf("an old store reads as none: %+v %v", read, err)
	}
	s, err := Open(p)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	defer s.Close()
	if got, err := s.RecordRoutineFields(context.Background(), map[string]map[string]string{"a/b": fieldsOfBudget("1")}, "flotillad"); err != nil || len(got) != 1 || !got[0].First {
		t.Fatalf("%+v %v", got, err)
	}
	if read, err = ReadRoutineFields(p); err != nil || read.Schema != len(schema) || len(read.Fields) != 1 {
		t.Fatalf("after the migration: %+v %v", read, err)
	}
}
