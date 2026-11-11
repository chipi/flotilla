package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/chipi/flotilla/internal/store"
)

func TestBackupWritesTheDayFileOnlyWhenComplete(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	day := filepath.Join(dir, "backup", "flotilla-2026-11-11.db")

	backup(context.Background(), st, dir, "2026-11-11", log)
	if _, err := os.Stat(day); err != nil {
		t.Fatalf("a completed backup must be under the day's name: %v", err)
	}
	if _, err := os.Stat(day + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("no .partial may be left after a completed backup: %v", err)
	}

	// A backup that fails (here: the store closed under it, as at shutdown) leaves neither file.
	st.Close()
	next := filepath.Join(dir, "backup", "flotilla-2026-11-12.db")
	backup(context.Background(), st, dir, "2026-11-12", log)
	for _, p := range []string{next, next + ".partial"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("a failed backup must leave no %s: %v", filepath.Base(p), err)
		}
	}
}
