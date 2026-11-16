package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/chipi/flotilla/internal/store"
)

// flotillad -check against a store a later flotillad moved forward: it must fail with the refusal Open
// gives at start, so a rollback dies in the check and not at `up`.
func TestCheckRefusesAStoreNewerThanTheBinary(t *testing.T) {
	dir := t.TempDir()
	if err := checkStore(dir); err != nil {
		t.Fatalf("no store yet (a first install): %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := checkStore(dir); err != nil {
		t.Fatalf("a store at this binary's schema: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (999)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := checkStore(dir); err == nil || !strings.Contains(err.Error(), "newer than this flotillad") {
		t.Fatalf("a newer store: -check = %v, want the \"newer than this flotillad\" refusal", err)
	}
}
