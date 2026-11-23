package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The explorer reads the three streams as one, follows one thing everywhere, and counts kinds under the other filters.
func TestExplore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, e := range [][]string{
		{"2026-11-20T10:00:00Z", "user:ada", "ui", "control.autonomy", "action:backup.rerun", "propose: ready"},
		{"2026-11-21T10:00:00Z", "flotillad", "flotillad", "run.prompts", "run-7", `{"system":"x"}`},
		{"2026-11-22T10:00:00Z", "directory:file", "directory", "person.added", "ada@example.org", "Ada; slack U0ADA"},
	} {
		if _, err := st.db.Exec(`INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`, e[0], e[1], e[2], e[3], e[4], e[5]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`INSERT INTO cases (id, kind, target, canonical_key, status, status_at, opened_at) VALUES ('case-1', 'failed_backup', 'host:a', 'target:host:a', 'closed', '2026-11-21T11:00:00Z', '2026-11-21T09:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO case_events (case_id, at, kind, actor, by_role, from_status, to_status, detail) VALUES ('case-1', '2026-11-21T11:00:00Z', 'case.closed', 'ops/verify', 'check', 'checking', 'closed', 'verified: run-7')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO ledger_entries (namespace, kind, occurrence_id, fingerprint, row, run_id, written_at) VALUES ('triage/cycle', 'decision', 'occ-1', 'grafana:abc', '{"disposition":"file"}', 'run-7', '2026-11-21T10:30:00Z')`); err != nil {
		t.Fatal(err)
	}

	rows, total, kinds, err := st.Explore(ctx, ExploreQuery{Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(rows) != 5 || rows[0].Kind != "person.added" {
		t.Fatalf("all streams, newest first: %d %+v", total, rows)
	}
	if len(kinds) != 5 {
		t.Errorf("kinds: %+v", kinds)
	}
	// Follow run-7: the platform's prompts record and the ledger's decision name it as their run.
	rows, total, _, err = st.Explore(ctx, ExploreQuery{Ref: "run-7"})
	if err != nil || total != 2 || rows[0].Stream != StreamPlatform || rows[1].Stream != StreamLedger || rows[1].Raw != `{"disposition":"file"}` {
		t.Fatalf("follow run-7: %d %+v %v", total, rows, err)
	}
	// Text finds a substring in a case event's detail; its detail leads with the status change.
	rows, _, _, _ = st.Explore(ctx, ExploreQuery{Text: "verified"})
	if len(rows) != 1 || rows[0].Detail != "checking -> closed: verified: run-7" || rows[0].CaseID != "case-1" {
		t.Fatalf("text: %+v", rows)
	}
	// A stream and a kind; the kind counts ignore the kind filter but keep the stream.
	rows, total, kinds, _ = st.Explore(ctx, ExploreQuery{Streams: []string{StreamPlatform}, Kind: "control.autonomy"})
	if total != 1 || rows[0].Actor != "user:ada" || len(kinds) != 3 {
		t.Fatalf("stream and kind: %d %+v %+v", total, rows, kinds)
	}
	// A time range.
	if _, total, _, _ = st.Explore(ctx, ExploreQuery{From: "2026-11-21T00:00:00Z", To: "2026-11-21T23:59:59Z"}); total != 3 {
		t.Errorf("one day: %d rows, want 3", total)
	}
	// Text with a LIKE wildcard is literal.
	if _, total, _, _ = st.Explore(ctx, ExploreQuery{Text: "%"}); total != 0 {
		t.Errorf("a %% matched %d rows", total)
	}
}
