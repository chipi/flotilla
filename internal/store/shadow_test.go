package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func seedRun(t *testing.T, s *Store, id, routine string, shadow bool, status string) {
	t.Helper()
	r := Run{ID: id, Routine: routine, SpecHash: "h", Trigger: "schedule", Actor: "flotillad", Autonomy: "record",
		StartedAt: time.Now().UTC().Format(ts), Status: StatusRunning, Version: 1, Shadow: shadow}
	if shadow {
		r.InputKey = "live-1"
		r.Version = 2
	}
	if err := s.StartRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if status != StatusRunning {
		if err := s.FinishRun(context.Background(), id, status, nil, 0.5, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// Steps 30 and 31 on a store at schema 29 with rows in it: every row stays, runs read as live runs with no input key, the new
// tables are there and empty, and the migration adds nothing.
func TestMigrateSteps30And31(t *testing.T) {
	if len(schema) < 31 {
		t.Fatalf("steps 30 and 31 are the shadows'; the schema has %d steps", len(schema))
	}
	p := filepath.Join(t.TempDir(), "s29.db")
	makeSchemaUpTo(t, p, 29)
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO runs (id, routine, spec_hash, trigger, actor, autonomy, started_at, status, version) VALUES
			('r1', 'triage/cycle', 'h', 'schedule', 'flotillad', 'record', '2026-11-18T00:00:00Z', 'ok', 1),
			('r2', 'triage/cycle', 'h', 'schedule', 'flotillad', 'record', '2026-11-18T00:10:00Z', 'error', 3)`,
		`INSERT INTO run_calls (run_id, routine, at, alias) VALUES ('r1', 'triage/cycle', '2026-11-18T00:00:00Z', 'a')`,
		`INSERT INTO spend (day, subject, usd) VALUES ('2026-11-18', 'triage/cycle', 0.25)`,
		`INSERT INTO ledger_entries (namespace, kind, row, run_id, written_at) VALUES ('triage/cycle', 'decision', '{}', 'r1', 't')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	before := tableCounts(t, p)
	if err := CheckSchema(p); err != nil {
		t.Fatalf("a store two steps behind passes the read-only check: %v", err)
	}
	s, err := Open(p)
	if err != nil {
		t.Fatalf("migrating a schema-29 store: %v", err)
	}
	defer s.Close()
	after := tableCounts(t, p)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s: %d rows before, %d after", table, n, after[table])
		}
	}
	for _, table := range []string{"run_inputs", "shadow_runs", "shadow_records"} {
		if n, ok := after[table]; !ok || n != 0 {
			t.Errorf("%s: want an empty new table, got %d rows (exists %v)", table, n, ok)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT MAX(n) FROM schema_version`).Scan(&n); err != nil || n != len(schema) {
		t.Fatalf("schema %d %v", n, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE shadow = 0 AND input_key = ''`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the old runs read as live runs with no input key: %d %v", n, err)
	}
	r, err := s.Run(context.Background(), "r2")
	if err != nil || r == nil || r.Shadow || r.InputKey != "" || r.Version != 3 || r.Status != StatusError {
		t.Fatalf("an old run keeps its columns: %+v %v", r, err)
	}
	if _, err := s.db.Exec(`INSERT INTO shadow_runs (input_key, routine, version, status, queued_at) VALUES ('k', 'a/b', 2, 'bogus', 't')`); err == nil {
		t.Fatal("a shadow run's status is checked by the table")
	}
}

// A step 30/31 that is rerun is not rerun: a second Open leaves the new tables and their rows alone.
func TestShadowTablesSurviveAReopen(t *testing.T) {
	s, p := open(t)
	ctx := context.Background()
	if err := s.NewInput(ctx, Input{Key: "live-1", Routine: "triage/cycle", Kind: InputTick, StartedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Input(ctx, "live-1"); err != nil {
		t.Fatalf("the input survives a reopen: %v", err)
	}
}

func TestInputRoundTrip(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	in := Input{Key: "live-1", Routine: "bugfix/fix", Kind: InputCase, Version: 2, Trigger: "case", Actor: "flotillad", StartedAt: "2026-11-19T10:00:00.000000000Z",
		Handed: json.RawMessage(`{"case":{"id":"c1"},"tick_time":"2026-11-19T10:00:00Z"}`), MarkFilings: 7, MarkEntries: 9,
		LiveCaseKeys: []string{"alert=a", "alert=b"}, CommitRepo: "o/r", CommitSHA: "abc", SnapshotPaths: []string{"state/ledger"}, SnapshotState: SnapshotHeld}
	if err := s.NewInput(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.Input(ctx, "live-1")
	if err != nil {
		t.Fatal(err)
	}
	in.SchemaVersion = InputSchemaVersion
	if jsonOf(got) != jsonOf(in) {
		t.Fatalf("the input reads back as written:\n got %s\nwant %s", jsonOf(got), jsonOf(in))
	}
	// a second input of the same key (a live run has one) is refused
	if err := s.NewInput(ctx, in); err == nil {
		t.Fatal("one input per live run")
	}
	// defaults: no handed, no keys, no snapshot
	if err := s.NewInput(ctx, Input{Key: "live-2", Routine: "triage/cycle", Kind: InputTick, StartedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Input(ctx, "live-2")
	if err != nil || string(d.Handed) != "{}" || d.LiveCaseKeys == nil || len(d.LiveCaseKeys) != 0 || d.SnapshotState != SnapshotNone || d.SnapshotPaths == nil || d.Pinned {
		t.Fatalf("an input with only what it must have: %+v %v", d, err)
	}
	if _, err := s.Input(ctx, "nope"); !errors.Is(err, ErrNoSuchInput) {
		t.Fatalf("an unknown input: %v", err)
	}
	for name, bad := range map[string]Input{
		"no key":          {Routine: "a/b", Kind: InputTick},
		"no routine":      {Key: "k", Kind: InputTick},
		"bad kind":        {Key: "k", Routine: "a/b", Kind: "weekly"},
		"a shadow's":      {Key: "k", Routine: "a/b@v2", Kind: InputTick},
		"handed not JSON": {Key: "k", Routine: "a/b", Kind: InputTick, Handed: json.RawMessage(`{`)},
	} {
		if err := s.NewInput(ctx, bad); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	if err := s.SetSnapshot(ctx, "live-2", SnapshotHeld, []string{"p"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PinInput(ctx, "live-2", true); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Input(ctx, "live-2")
	if d.SnapshotState != SnapshotHeld || len(d.SnapshotPaths) != 1 || !d.Pinned {
		t.Fatalf("snapshot and pin: %+v", d)
	}
	if err := s.SetSnapshot(ctx, "live-2", "gone", nil); err == nil {
		t.Fatal("a snapshot is none, held or deleted")
	}
	if err := s.SetSnapshot(ctx, "zzz", SnapshotDeleted, nil); !errors.Is(err, ErrNoSuchInput) {
		t.Fatalf("a snapshot of no input: %v", err)
	}
	if err := s.PinInput(ctx, "zzz", true); !errors.Is(err, ErrNoSuchInput) {
		t.Fatalf("a pin of no input: %v", err)
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestLedgerMark(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	if f, e, err := s.LedgerMark(ctx, "triage/cycle"); err != nil || f != 0 || e != 0 {
		t.Fatalf("an empty ledger's mark is 0, 0: %d %d %v", f, e, err)
	}
	f1, _ := s.WriteFiling(ctx, "triage/cycle", filing("a", "1", "", "", ""), "r")
	s.AppendEntry(ctx, "other/ns", "decision", map[string]string{"fingerprint": "x"}, "r")
	e1, _ := s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "a"}, "r")
	f, e, err := s.LedgerMark(ctx, "triage/cycle")
	if err != nil || f != f1 || e != e1 {
		t.Fatalf("the mark is the namespace's newest seqs: %d %d (want %d %d) %v", f, e, f1, e1, err)
	}
}

func TestOpenCaseKeys(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	mk := func(kind, key string) string {
		id, _, err := s.OpenCase(ctx, NewCase{Kind: kind, Target: "t", Keys: []Key{{"alert", key}}, Canonical: Key{"alert", key}}, "ops/monitor", "found")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mk("backup", "b-2")
	closed := mk("backup", "b-1")
	mk("disk", "d-1")
	mk("other", "o-1")
	if _, err := s.db.Exec(`UPDATE cases SET status = 'closed' WHERE id = ?`, closed); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenCaseKeys(ctx, []string{"backup", "disk"})
	if err != nil || strings.Join(got, ",") != "alert=b-2,alert=d-1" {
		t.Fatalf("the open cases of the kinds, sorted, a closed one left out: %v %v", got, err)
	}
	none, err := s.OpenCaseKeys(ctx, nil)
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("no kinds, no keys (an empty list, not null): %v %v", none, err)
	}
}

// The copy-on-write ledger a shadow reads: live rows up to the mark, none after, its own on top, and its own row is the
// current one for a fingerprint both filed.
func TestLedgerAsOf(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	live, own := "triage/cycle", "triage/cycle@v2"
	for _, fp := range []string{"a", "b"} {
		if _, err := s.WriteFiling(ctx, live, filing(fp, "1", "g-"+fp, "n-"+fp, ""), "r1"); err != nil {
			t.Fatal(err)
		}
	}
	s.AppendEntry(ctx, live, "decision", map[string]string{"fingerprint": "a", "disposition": "file"}, "r1")
	mf, me, err := s.LedgerMark(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	// the live run goes on writing after the mark: a rewrite of a, a new c, a new decision
	s.WriteFiling(ctx, live, filing("a", "2", "g-a", "n-a", ""), "r1")
	s.WriteFiling(ctx, live, filing("c", "3", "g-c", "n-c", ""), "r1")
	s.AppendEntry(ctx, live, "decision", map[string]string{"fingerprint": "c", "disposition": "dismiss"}, "r1")
	// and the shadow writes its own: a filing of b with another issue, a filing of d, a decision
	s.WriteFiling(ctx, own, filing("b", "9", "g-b", "n-b", ""), "s1")
	s.WriteFiling(ctx, own, filing("d", "4", "g-d", "n-d", ""), "s1")
	s.AppendEntry(ctx, own, "decision", map[string]string{"fingerprint": "d", "disposition": "file"}, "s1")

	got, err := s.FilingsAsOf(ctx, live, own, mf, nil)
	if err != nil {
		t.Fatal(err)
	}
	// a at its mark-time issue 1 (the rewrite is later), b the shadow's own issue 9, no c, d the shadow's
	if want := "a#1,b#9,d#4"; fps(got) != want {
		t.Fatalf("filings as of the mark: %s, want %s", fps(got), want)
	}
	// a lookup by key reads the same view: group g-b finds the shadow's row of b, g-c finds nothing (c is after the mark)
	for key, want := range map[string]string{"group_key=g-b": "b#9", "group_key=g-c": "", "norm_key=n-a": "a#1", "fingerprint=d": "d#4"} {
		k, v, _ := strings.Cut(key, "=")
		g, err := s.FilingsAsOf(ctx, live, own, mf, map[string]string{k: v})
		if err != nil || fps(g) != want {
			t.Errorf("FilingsAsOf by %s = %q %v, want %q", key, fps(g), err, want)
		}
	}
	if _, err := s.FilingsAsOf(ctx, live, own, mf, map[string]string{"nope": "x"}); err == nil {
		t.Error("an unknown key is refused as Filings refuses it")
	}
	if g, err := s.FilingsAsOf(ctx, live, own, mf, map[string]string{"norm_key": ""}); err != nil || g != nil {
		t.Errorf("an empty key value matches nothing: %v %v", g, err)
	}
	// the live ledger itself did not change: the shadow wrote in its own namespace
	cur, _ := s.Filings(ctx, live, nil)
	if want := "b#1,a#2,c#3"; fps(cur) != want {
		t.Fatalf("the live view is the live view: %s, want %s", fps(cur), want)
	}
	ents, err := s.EntriesAsOf(ctx, live, own, me, 0, 0)
	if err != nil || len(ents) != 2 || ents[0].Row["fingerprint"] != "a" || ents[1].Row["fingerprint"] != "d" {
		t.Fatalf("entries as of the mark, then the shadow's own: %+v %v", ents, err)
	}
	page, err := s.EntriesAsOf(ctx, live, own, me, ents[0].Seq, 1)
	if err != nil || len(page) != 1 || page[0].Row["fingerprint"] != "d" {
		t.Fatalf("paging after a seq, limited: %+v %v", page, err)
	}
	if all, _ := s.Entries(ctx, live, 0, 0); len(all) != 2 {
		t.Fatalf("the live decisions are the live ones: %d", len(all))
	}
}

func TestShadowQueue(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	id, err := s.QueueShadowRun(ctx, ShadowRun{InputKey: "live-1", Routine: "triage/cycle", Version: 2, VersionHash: "h2", LiveStartedAt: "t0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueShadowRun(ctx, ShadowRun{InputKey: "live-1", Routine: "triage/cycle", Version: 2}); !errors.Is(err, ErrShadowRunExists) {
		t.Fatalf("one row per input and version: %v", err)
	}
	skipped, err := s.QueueShadowRun(ctx, ShadowRun{InputKey: "live-1", Routine: "triage/cycle", Version: 3, Status: ShadowNotRun, Reason: "queue full"})
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]ShadowRun{
		"no version":       {InputKey: "k", Routine: "a/b"},
		"not run, no why":  {InputKey: "k", Routine: "a/b", Version: 2, Status: ShadowNotRun},
		"entering as done": {InputKey: "k", Routine: "a/b", Version: 2, Status: ShadowDone},
		"no input":         {Routine: "a/b", Version: 2},
	} {
		if _, err := s.QueueShadowRun(ctx, bad); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	// the moves: queued -> running -> done; terminal rows stay as they ended
	if err := s.ShadowRunStep(ctx, id, ShadowDone, "", ""); err == nil {
		t.Fatal("a queued run does not jump to done")
	}
	if err := s.ShadowRunStep(ctx, id, ShadowRunning, "", "shadow-run-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ShadowRunStep(ctx, id, ShadowNotRun, "late", ""); err == nil {
		t.Fatal("a running run is not 'not run'")
	}
	if err := s.ShadowRunStep(ctx, id, ShadowDone, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ShadowRunStep(ctx, id, ShadowCancelled, "retired", ""); err == nil {
		t.Fatal("a done run stays done")
	}
	if err := s.ShadowRunStep(ctx, skipped, ShadowRunning, "", "x"); err == nil {
		t.Fatal("a not-run row does not start")
	}
	if err := s.ShadowRunStep(ctx, 999, ShadowRunning, "", "x"); err == nil {
		t.Fatal("a row that does not exist")
	}
	if err := s.ShadowRunStep(ctx, id, "weird", "", ""); err == nil {
		t.Fatal("an unknown state")
	}
	rows, err := s.ShadowRuns(ctx, ShadowRunFilter{InputKey: "live-1"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("both rows: %+v %v", rows, err)
	}
	if r := rows[0]; r.Status != ShadowDone || r.RunID != "shadow-run-1" || r.StartedAt == "" || r.EndedAt == "" || r.LiveStartedAt != "t0" || r.VersionHash != "h2" {
		t.Fatalf("the done row says when it started and ended, its run, and both start times: %+v", r)
	}
	if r := rows[1]; r.Status != ShadowNotRun || r.Reason != "queue full" || r.RunID != "" {
		t.Fatalf("the not-run row says why: %+v", r)
	}
	if byStatus, _ := s.ShadowRuns(ctx, ShadowRunFilter{Status: ShadowNotRun}); len(byStatus) != 1 || byStatus[0].Version != 3 {
		t.Fatalf("filter by status: %+v", byStatus)
	}
	if byV, _ := s.ShadowRuns(ctx, ShadowRunFilter{Routine: "triage/cycle", Version: 2}); len(byV) != 1 {
		t.Fatalf("filter by routine and version: %+v", byV)
	}
	// cancelled, with the reason kept
	id2, _ := s.QueueShadowRun(ctx, ShadowRun{InputKey: "live-2", Routine: "triage/cycle", Version: 2})
	if err := s.ShadowRunStep(ctx, id2, ShadowCancelled, "retired", ""); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.ShadowRuns(ctx, ShadowRunFilter{InputKey: "live-2"}); c[0].Status != ShadowCancelled || c[0].Reason != "retired" || c[0].EndedAt == "" {
		t.Fatalf("cancelled: %+v", c)
	}
}

func TestShadowRecords(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	rec := ShadowRecord{RunID: "s-run", Routine: "triage/cycle@v2", Version: 2, VersionHash: "h2", InputKey: "live-1", Subject: "fp-1", Kind: "decision",
		Payload: json.RawMessage(`{"disposition":"file"}`)}
	if _, err := s.RecordShadow(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec2 := rec
	rec2.Subject, rec2.Kind, rec2.Payload = "c-1", "plan", nil
	if _, err := s.RecordShadow(ctx, rec2); err != nil {
		t.Fatal(err)
	}
	all, err := s.ShadowRecords(ctx, ShadowRecordFilter{InputKey: "live-1"})
	if err != nil || len(all) != 2 || all[0].Subject != "fp-1" || string(all[0].Payload) != `{"disposition":"file"}` || string(all[1].Payload) != "{}" || all[0].CreatedAt == "" {
		t.Fatalf("records in the order written, an empty payload read as {}: %+v %v", all, err)
	}
	if got, _ := s.ShadowRecords(ctx, ShadowRecordFilter{Kind: "plan"}); len(got) != 1 || got[0].Subject != "c-1" {
		t.Fatalf("by kind: %+v", got)
	}
	if got, _ := s.ShadowRecords(ctx, ShadowRecordFilter{Routine: "triage/cycle@v3"}); len(got) != 0 {
		t.Fatalf("by another version's key: %+v", got)
	}
	for name, bad := range map[string]ShadowRecord{
		"a live key":       {RunID: "r", Routine: "triage/cycle", Version: 2, InputKey: "k", Kind: "x"},
		"no run":           {Routine: "a/b@v2", Version: 2, InputKey: "k", Kind: "x"},
		"no input":         {RunID: "r", Routine: "a/b@v2", Version: 2, Kind: "x"},
		"no kind":          {RunID: "r", Routine: "a/b@v2", Version: 2, InputKey: "k"},
		"no version":       {RunID: "r", Routine: "a/b@v2", InputKey: "k", Kind: "x"},
		"payload not JSON": {RunID: "r", Routine: "a/b@v2", Version: 2, InputKey: "k", Kind: "x", Payload: json.RawMessage(`nope`)},
	} {
		if _, err := s.RecordShadow(ctx, bad); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	// time window
	since := time.Now().UTC().Add(time.Hour).Format(ts)
	if got, _ := s.ShadowRecords(ctx, ShadowRecordFilter{Since: since}); len(got) != 0 {
		t.Fatalf("since the future: %+v", got)
	}
	if got, _ := s.ShadowRecords(ctx, ShadowRecordFilter{Until: since}); len(got) != 2 {
		t.Fatalf("until the future: %+v", got)
	}
}

// A pair is a subject both sides decided on one input: the live side is a plan of the input's run or a decision entry of it.
func TestPairsFor(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	if n, err := s.PairsFor(ctx, "triage/cycle", 2); err != nil || n != 0 {
		t.Fatalf("no records, no pairs: %d %v", n, err)
	}
	shadow := func(input, subject, kind string, version int) {
		t.Helper()
		if _, err := s.RecordShadow(ctx, ShadowRecord{RunID: "s", Routine: ShadowKey("triage/cycle", version), Version: version, InputKey: input, Subject: subject, Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	// live decisions on input live-1: fp-1 (subject by fingerprint), occ-2 (by occurrence id), sub-3 (the row's own subject)
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fp-1"}, "live-1")
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fpx", "occurrence_id": "occ-2"}, "live-1")
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fpy", "occurrence_id": "occ-y", "subject": "sub-3"}, "live-1")
	s.AppendEntry(ctx, "triage/cycle", "recurrence", map[string]string{"fingerprint": "fp-4"}, "live-1") // a recurrence is not a decision
	// a live plan on input live-9 for case c-1
	id, _, err := s.OpenCase(ctx, NewCase{Kind: "backup", Target: "t", Keys: []Key{{"alert", "a"}}, Canonical: Key{"alert", "a"}}, "ops/m", "d")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE cases SET status = 'planned' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPlan(ctx, PlanRecord{CaseID: id, RunID: "live-9", Action: "a.b", Outcome: "record", Autonomy: "record"}, "closed", "plan_recorded", "", false); err != nil {
		t.Fatal(err)
	}
	for _, x := range [][3]string{
		{"live-1", "fp-1", "decision"}, {"live-1", "occ-2", "decision"}, {"live-1", "sub-3", "decision"},
		{"live-1", "fp-4", "decision"}, // live only recurred it: no pair
		{"live-1", "fp-5", "decision"}, // live did not decide it: unpaired
		{"live-1", "fp-1", "decision"}, // recorded twice: one pair
		{"live-1", "fp-1", "note"},     // not a decision kind
		{"live-2", "fp-1", "decision"}, // another input: the live side did not decide it there
		{"live-9", id, "plan"},
	} {
		shadow(x[0], x[1], x[2], 2)
	}
	shadow("live-1", "fp-1", "decision", 3) // another version's
	if n, err := s.PairsFor(ctx, "triage/cycle", 2); err != nil || n != 4 {
		t.Fatalf("pairs of v2: %d %v, want 4 (fp-1, occ-2, sub-3, and the plan's case)", n, err)
	}
	if n, _ := s.PairsFor(ctx, "triage/cycle", 3); n != 1 {
		t.Fatalf("pairs of v3: %d, want 1", n)
	}
	// a decision in a shadow's own namespace is not the live side
	s.AppendEntry(ctx, "triage/cycle@v2", "decision", map[string]string{"fingerprint": "fp-5"}, "live-1")
	if n, _ := s.PairsFor(ctx, "triage/cycle", 2); n != 4 {
		t.Fatalf("a shadow's own ledger rows are not the live side: %d", n)
	}
}

// Shadow rows never reach a reader of the live system, and each reader that can show them does so only when asked.
func TestShadowRowsStayOutOfLiveReaders(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	seedRun(t, s, "live-1", "triage/cycle", false, StatusOK)
	seedRun(t, s, "shadow-1", "triage/cycle@v2", true, StatusOK)
	seedRun(t, s, "shadow-2", "triage/cycle@v2", true, StatusRunning)
	at := time.Now().UTC().Format(ts)
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "live-1", Routine: "triage/cycle", At: at, Alias: "a"}, {RunID: "shadow-1", Routine: "triage/cycle@v2", At: at, Alias: "a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSpend(ctx, "2026-11-19", "triage/cycle", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSpend(ctx, "2026-11-19", "triage/cycle@v2", 7); err != nil {
		t.Fatal(err)
	}
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "a"}, "live-1")
	s.AppendEntry(ctx, "triage/cycle@v2", "decision", map[string]string{"fingerprint": "a"}, "shadow-1")
	s.WriteFiling(ctx, "triage/cycle@v2", filing("z", "1", "", "", ""), "shadow-1")

	if rs, err := s.Runs(ctx, "", 50); err != nil || len(rs) != 1 || rs[0].ID != "live-1" {
		t.Fatalf("Runs: %+v %v", rs, err)
	}
	if rs, err := s.Runs(ctx, "triage/cycle", 50); err != nil || len(rs) != 1 {
		t.Fatalf("Runs of the live routine: %+v %v", rs, err)
	}
	if rs, err := s.Runs(ctx, "triage/cycle@v2", 50); err != nil || len(rs) != 2 || !rs[0].Shadow || rs[0].InputKey != "live-1" || rs[0].Version != 2 {
		t.Fatalf("Runs of a shadow's key asks for the shadow's: %+v %v", rs, err)
	}
	page, total, err := s.RunsPage(ctx, RunQuery{Limit: 10})
	if err != nil || total != 1 || len(page) != 1 {
		t.Fatalf("RunsPage: %+v %d %v", page, total, err)
	}
	if page, total, _ = s.RunsPage(ctx, RunQuery{Fleet: "triage", Limit: 10}); total != 1 {
		t.Fatalf("RunsPage by fleet counts live runs only: %d", total)
	}
	if page, total, _ = s.RunsPage(ctx, RunQuery{Q: "cycle", Limit: 10}); total != 1 {
		t.Fatalf("RunsPage by search: %d", total)
	}
	if page, total, _ = s.RunsPage(ctx, RunQuery{Routine: "triage/cycle@v2", IncludeShadow: true, Limit: 10}); total != 2 || !page[0].Shadow {
		t.Fatalf("RunsPage with IncludeShadow: %+v %d", page, total)
	}
	if page, total, _ = s.RunsPage(ctx, RunQuery{Routine: "triage/cycle@v2", Limit: 10}); total != 0 {
		t.Fatalf("RunsPage of a shadow's key without IncludeShadow is empty: %d", total)
	}
	if page, total, _ = s.RunsPage(ctx, RunQuery{IncludeShadow: true, Limit: 10}); total != 3 {
		t.Fatalf("RunsPage with IncludeShadow, all: %d", total)
	}
	days, err := s.RunDays(ctx, "2000-01-01")
	if err != nil || len(days) != 1 || days[0].Routine != "triage/cycle" || days[0].Runs != 1 {
		t.Fatalf("RunDays: %+v %v", days, err)
	}
	cs, err := s.CallsSince(ctx, "2000-01-01")
	if err != nil || len(cs) != 1 || cs[0].RunID != "live-1" {
		t.Fatalf("CallsSince: %+v %v", cs, err)
	}
	if cs, _ := s.RoutineCallsSince(ctx, "triage/cycle", "2000-01-01"); len(cs) != 1 {
		t.Fatalf("RoutineCallsSince of the live routine: %+v", cs)
	}
	if cs, _ := s.RoutineCallsSince(ctx, "triage/cycle@v2", "2000-01-01"); len(cs) != 1 || cs[0].RunID != "shadow-1" {
		t.Fatalf("RoutineCallsSince of a shadow's key: %+v", cs)
	}
	if v, _ := s.Spend(ctx, "2026-11-19", "triage/cycle"); v != 1 {
		t.Fatalf("the live day's spend does not include the shadow's: %v", v)
	}
	if at, err := s.LastSuccess(ctx, "triage/cycle"); err != nil || at.IsZero() {
		t.Fatalf("LastSuccess of the live routine: %v %v", at, err)
	}
	// a shadow's ok run never counts as the live routine's last success, even under a live key by mistake
	seedRun(t, s, "stray", "platform/heartbeat", true, StatusOK)
	if at, _ := s.LastSuccess(ctx, "platform/heartbeat"); !at.IsZero() {
		t.Fatalf("a run flagged shadow is never a live success: %v", at)
	}
	ns, err := s.Namespaces(ctx)
	if err != nil || strings.Join(ns, " ") != "triage/cycle" {
		t.Fatalf("Namespaces lists live ledgers only: %v %v", ns, err)
	}
	// every run is found for an interrupt, and Run reads either kind by id
	orphans, err := s.RunningRuns(ctx)
	if err != nil || len(orphans) != 1 || orphans[0].ID != "shadow-2" {
		t.Fatalf("RunningRuns includes a shadow's: %+v %v", orphans, err)
	}
	if r, _ := s.Run(ctx, "shadow-1"); r == nil || !r.Shadow {
		t.Fatalf("Run by id reads a shadow run: %+v", r)
	}
	if n, err := s.MarkInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("a restart interrupts the shadow run too: %d %v", n, err)
	}
}

// The reader guard: every statement that reads runs, run_calls or spend must say what it does with shadow rows, so the next reader
// written cannot leak them silently. Per statement, not per function: a function with one guarded query and one unguarded one is
// caught. A statement says it by naming liveRuns, liveCalls, `shadow = 0` or IncludeShadow in its own text, or by a
// `// shadow-ok: why` comment on it or on the three lines above it.
var shadowRead = regexp.MustCompile("(?i)\\b(?:FROM|JOIN)\\s+(runs|run_calls|spend)\\b")
var shadowSaid = regexp.MustCompile(`liveRuns|liveCalls|shadow = 0|IncludeShadow|shadow-ok:`)

// unguardedReaders returns "file:line: the statement reads <table>" for each statement of src that reads a shadow-bearing table
// without saying what it does with shadow rows.
func unguardedReaders(file, src string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.ParseComments)
	if err != nil {
		return []string{file + ": does not parse: " + err.Error()}
	}
	var out []string
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		m := shadowRead.FindStringSubmatch(lit.Value)
		if m == nil {
			return true
		}
		var stmt ast.Node
		for i := len(stack) - 2; i >= 0 && stmt == nil; i-- {
			switch stack[i].(type) {
			case ast.Stmt, *ast.GenDecl:
				stmt = stack[i]
			}
		}
		if stmt == nil {
			out = append(out, fmt.Sprintf("%s: a literal reads %s outside any statement", file, m[1]))
			return true
		}
		text := src[fset.Position(stmt.Pos()).Offset:fset.Position(stmt.End()).Offset]
		line := fset.Position(stmt.Pos()).Line
		said := shadowSaid.MatchString(text)
		for _, c := range f.Comments {
			if cl := fset.Position(c.End()).Line; cl >= line-3 && cl <= fset.Position(stmt.End()).Line && strings.Contains(c.Text(), "shadow-ok:") {
				said = true
			}
		}
		if !said {
			out = append(out, fmt.Sprintf("%s:%d: the statement reads %s", file, fset.Position(lit.Pos()).Line, m[1]))
		}
		return true
	})
	return out
}

func TestEveryReaderSaysWhatItDoesWithShadows(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no store sources: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range unguardedReaders(f, string(b)) {
			t.Errorf("%s: say what it does with shadow rows (liveRuns, liveCalls, IncludeShadow, or a `shadow-ok: why` comment)", bad)
		}
	}
}

// The guard itself is checked against a reader that leaks, one that is half guarded, and ones that say what they do.
func TestTheReaderGuardCatchesALeak(t *testing.T) {
	leak := "package x\n\nfunc (s *Store) Bad(ctx context.Context) {\n\ts.db.Query(`SELECT id FROM runs WHERE status = 'ok'`)\n}\n"
	if got := unguardedReaders("x.go", leak); len(got) != 1 || !strings.Contains(got[0], "runs") {
		t.Fatalf("a reader of runs with no shadow filter: %v", got)
	}
	leakCalls := "package x\n\nfunc Bad2() {\n\tq := `SELECT a FROM run_calls`\n}\n"
	if got := unguardedReaders("x.go", leakCalls); len(got) != 1 {
		t.Fatalf("a reader of run_calls: %v", got)
	}
	leakJoin := "package x\n\nfunc Bad3() {\n\tq := `SELECT 1 FROM plans p JOIN runs r ON r.id = p.run_id`\n}\n"
	if got := unguardedReaders("x.go", leakJoin); len(got) != 1 {
		t.Fatalf("a join of runs: %v", got)
	}
	// one statement guarded does not cover the next: this is the leak the guard used to miss
	half := "package x\n\nfunc Half() {\n\tq := `SELECT id FROM runs WHERE ` + liveRuns\n\tr := `SELECT id FROM runs WHERE id = ?`\n\t_, _ = q, r\n}\n"
	if got := unguardedReaders("x.go", half); len(got) != 1 || !strings.Contains(got[0], ":5:") {
		t.Fatalf("a function with a guarded and an unguarded read of runs: %v", got)
	}
	halfFlag := "package x\n\nfunc HalfFlag(q RunQuery) {\n\t_ = q.IncludeShadow\n\tx := `FROM runs`\n}\n"
	if got := unguardedReaders("x.go", halfFlag); len(got) != 1 {
		t.Fatalf("a mention of IncludeShadow elsewhere does not guard a statement: %v", got)
	}
	// a comment covers the statement under it only
	commentFar := "package x\n\nfunc Far() {\n\t// shadow-ok: by id\n\tq := `SELECT id FROM runs WHERE id = ?`\n\t_ = q\n\n\n\n\n\tr := `SELECT id FROM run_calls`\n\t_ = r\n}\n"
	if got := unguardedReaders("x.go", commentFar); len(got) != 1 || !strings.Contains(got[0], "run_calls") {
		t.Fatalf("a comment far above does not cover a later statement: %v", got)
	}
	for _, ok := range []string{
		"func A() {\n\tq := `SELECT id FROM runs WHERE ` + liveRuns\n}\n",
		"func B() {\n\t// shadow-ok: by id\n\tq := `SELECT id FROM runs WHERE id = ?`\n}\n",
		"func C() {\n\tq := `SELECT a FROM run_calls WHERE ` + liveCalls\n}\n",
		"func D(q RunQuery) {\n\tx := `FROM runs WHERE ` + liveRuns + IncludeShadow\n}\n",
		"func E() {\n\tx := `SELECT 1 FROM plans`\n}\n",
		"const c = `SELECT 1 FROM runs` // shadow-ok: by id\n",
		"func F() {\n\tif err := db.QueryRow(`SELECT 1 FROM spend WHERE x = ?`, 1); err != nil { // shadow-ok: keyed by subject\n\t}\n}\n",
	} {
		if got := unguardedReaders("x.go", "package x\n\n"+ok); len(got) != 0 {
			t.Errorf("%q: %v", ok, got)
		}
	}
}

// Q7: a shadow run's action is never enqueued. A plan of a shadow run that somehow exists cannot become a host execution, and a plan
// of a live run still can.
func TestAShadowRunsPlanNeverBecomesAHostExecution(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	seedRun(t, s, "live-run", "ops/rerun-backup", false, StatusOK)
	seedRun(t, s, "shadow-run", "ops/rerun-backup@v2", true, StatusOK)
	mk := func(key, runID string) int64 {
		t.Helper()
		id, _, err := s.OpenCase(ctx, NewCase{Kind: "backup", Target: "host:b", Keys: []Key{{"target", key}}, Canonical: Key{"target", key}}, "ops/m", "d")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE cases SET status = 'planned' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		pid, err := s.ApplyPlan(ctx, PlanRecord{CaseID: id, RunID: runID, Action: "backup.rerun", Outcome: "propose", Autonomy: "propose"}, "awaiting_approval", "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		return pid
	}
	shadowPlan, livePlan := mk("a", "shadow-run"), mk("b", "live-run")
	err := s.StartHostExecution(ctx, HostExecution{ID: "x1", CaseID: "c", PlanID: shadowPlan, Host: "h", Action: "backup.rerun", Approved: "a"})
	if !errors.Is(err, ErrShadowRun) {
		t.Fatalf("a shadow's plan was sent: %v", err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM host_executions`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d host executions for a shadow's plan", n)
	}
	if err := s.StartHostExecution(ctx, HostExecution{ID: "x2", CaseID: "c", PlanID: livePlan, Host: "h", Action: "backup.rerun", Approved: "a", Envelope: []byte("{}")}); err != nil {
		t.Fatalf("a live plan is sent as before: %v", err)
	}
	if yes, err := s.RunIsShadow(ctx, "shadow-run"); err != nil || !yes {
		t.Fatalf("%v %v", yes, err)
	}
	if yes, _ := s.RunIsShadow(ctx, "live-run"); yes {
		t.Fatal("a live run is not a shadow")
	}
	if yes, _ := s.RunIsShadow(ctx, "nope"); yes {
		t.Fatal("an unknown run is not a shadow")
	}
}
